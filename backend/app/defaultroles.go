package app

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/automagicops/haliphron/backend/store"
)

// Default roles: the roles an installation starts with, so that the chat bot
// has something to run as before anyone has written a role.
//
// Each is seeded once. A default an operator edits keeps the edits, and one an
// operator deletes stays deleted: the seed looks for any row of the name, live
// or not, and inserts only when there has never been one. The spec is validated
// by the rules PutRole applies, less the check that its secrets exist — a
// default names the secret an operator is expected to store, and cannot wait
// for it.

//go:embed defaults/*.json
var defaultRoles embed.FS

// EnsureDefaultRoles seeds the default roles that were never stored, and
// returns the names it created.
func (s *Service) EnsureDefaultRoles(ctx context.Context) ([]string, error) {
	entries, err := defaultRoles.ReadDir("defaults")
	if err != nil {
		return nil, fmt.Errorf("read default roles: %w", err)
	}
	var created []string
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		spec, err := defaultRoles.ReadFile(path.Join("defaults", entry.Name()))
		if err != nil {
			return created, fmt.Errorf("read default role %s: %w", name, err)
		}
		if err := validateDefaultRole(name, spec); err != nil {
			return created, fmt.Errorf("default role %s: %w", name, err)
		}
		ok, err := s.store.InsertRoleIfNeverExisted(ctx, name, spec, "system")
		if err != nil {
			return created, err
		}
		if !ok {
			continue
		}
		if err := s.store.Audit(ctx, store.AuditEntry{
			Actor: "system", ActorKind: "system", Action: store.AuditRoleSeeded,
			SubjectKind: "role", SubjectID: name,
		}); err != nil {
			return created, err
		}
		created = append(created, name)
	}
	return created, nil
}

// validateDefaultRole holds an embedded spec to PutRole's shape rules. A
// default that would be refused when saved from the UI is a build defect, and
// is reported as one rather than seeded.
func validateDefaultRole(name string, spec []byte) error {
	var parsed roleSpec
	if err := json.Unmarshal(spec, &parsed); err != nil {
		return err
	}
	if len(parsed.Description) > MaxRoleDescriptionBytes {
		return fmt.Errorf("description is over %d bytes", MaxRoleDescriptionBytes)
	}
	for key := range parsed.ConfigFiles {
		if err := configFileKey(key); err != nil {
			return err
		}
	}
	if err := validateRoleSettings(name, parsed); err != nil {
		return err
	}
	if err := validateSecretEnv(parsed.SecretEnv); err != nil {
		return err
	}
	_, err := parseRole(store.Role{Name: name, Spec: spec})
	return err
}
