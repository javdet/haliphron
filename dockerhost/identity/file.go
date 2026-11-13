// Package identity keeps the cluster's Ed25519 key in a file on the host. It
// is the Docker host's identity.Store: the protocol — generate, persist before
// registering, adopt the cluster ID — is controller/identity's, unchanged.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	ctrlidentity "github.com/automagicops/haliphron/controller/identity"
)

// FileStore is a JSON file, mode 0600, written by rename so a crash leaves
// either the old identity or the new one and never half of each.
type FileStore struct {
	Path string
}

var _ ctrlidentity.Store = FileStore{}

type onDisk struct {
	KID       string `json:"kid"`
	Seed      []byte `json:"seed"`
	ClusterID string `json:"clusterID,omitempty"`
}

// Load implements identity.Store.
func (f FileStore) Load(_ context.Context) (ctrlidentity.Persisted, bool, error) {
	raw, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return ctrlidentity.Persisted{}, false, nil
	}
	if err != nil {
		return ctrlidentity.Persisted{}, false, fmt.Errorf("identity: read %s: %w", f.Path, err)
	}
	var d onDisk
	if err := json.Unmarshal(raw, &d); err != nil {
		return ctrlidentity.Persisted{}, false, fmt.Errorf("identity: parse %s: %w", f.Path, err)
	}
	return ctrlidentity.Persisted{KID: d.KID, Seed: d.Seed, ClusterID: ulid(d.ClusterID)}, true, nil
}

// Save implements identity.Store.
func (f FileStore) Save(_ context.Context, p ctrlidentity.Persisted) error {
	raw, err := json.Marshal(onDisk{KID: p.KID, Seed: p.Seed, ClusterID: string(p.ClusterID)})
	if err != nil {
		return fmt.Errorf("identity: encode: %w", err)
	}
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("identity: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return fmt.Errorf("identity: temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: chmod: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("identity: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return fmt.Errorf("identity: rename into %s: %w", f.Path, err)
	}
	return nil
}

func ulid(s string) runv1.ULID { return runv1.ULID(s) }
