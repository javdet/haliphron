package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/store"
)

// Materials: the parts of a lease the controller turns into Kubernetes objects
// instead of copying into the AgentRun.
//
// The rule is mechanical and it is checked on both sides: if `kubectl get
// agentrun -o yaml` must not show it, it is not a spec field. Secret values,
// the presigned bundle and the role's config files are assembled here, travel
// beside the spec in the lease, and become a Secret and a ConfigMap with an
// ownerReference to the CR — so deleting the CR collects them and no
// long-lived secret is left in the agent namespace.

// GitTokenMinter is the GitProvider port, reduced to the one call the lease
// path makes.
//
// The target implementation mints a GitHub App installation token: one hour,
// one repository. That property is the main compensating control in the
// absence of egress filtering — a token leaked through a compromised agent
// lives an hour and opens one repository — so the port is shaped around a TTL
// and a repository rather than around "fetch the credential".
type GitTokenMinter interface {
	MintToken(ctx context.Context, repoURL string, provider runv1.GitProvider, ttl time.Duration) (string, error)
}

// StoredGitToken is the phase 1 implementation: a credential an operator put
// in the secrets table, handed out as it is.
//
// It does not narrow anything — the token is as wide and as long-lived as the
// one that was stored — and that is why it sits behind the port rather than
// being called directly. Replacing it with an App-based minter is a
// constructor change here and nothing else anywhere.
type StoredGitToken struct {
	Store *store.Store
	// Name is the secret to resolve. Per-provider names are tried first, so an
	// installation with both GitHub and GitLab repositories does not have to
	// share one credential between them.
	Name string
}

// MintToken resolves the stored credential for a repository's provider.
func (g StoredGitToken) MintToken(ctx context.Context, _ string, provider runv1.GitProvider, _ time.Duration) (string, error) {
	names := []string{g.Name + "-" + string(provider), g.Name}
	for _, name := range names {
		value, err := g.Store.ResolveSecret(ctx, name)
		switch {
		case err == nil:
			return string(value), nil
		case errors.Is(err, store.ErrNotFound):
			continue
		default:
			return "", fmt.Errorf("resolve git credential %s: %w", name, err)
		}
	}
	// No credential is not a failure: a run against a public repository with
	// createPR false needs none, and refusing here would make the absence of a
	// secret a class of run that cannot start.
	return "", nil
}

// materials builds the secret map and the role files for one lease.
func (s *Service) materials(ctx context.Context, runID runv1.ULID, spec runv1.RenderedRunSpec) (
	secrets map[string]string, roleConfig map[string]string, err error) {

	secrets = map[string]string{}

	if spec.Repo.URL != "" && s.git != nil {
		ttl := time.Duration(spec.Runtime.TimeoutSeconds) * time.Second * 2
		token, err := s.git.MintToken(ctx, spec.Repo.URL, spec.Repo.Provider, ttl)
		if err != nil {
			return nil, nil, fmt.Errorf("mint git token for %s: %w", runID, err)
		}
		if token != "" {
			secrets[runv1.SecretKeyGitToken] = token
		}
	}

	if name := s.limits.LLMAPIKeySecret; name != "" {
		value, err := s.store.ResolveSecret(ctx, name)
		switch {
		case err == nil:
			secrets[runv1.SecretKeyLLMAPIKey] = string(value)
		case errors.Is(err, store.ErrNotFound):
			// An installation using a provider that authenticates some other
			// way — a proxy with its own credentials, a self-hosted model — is
			// legitimate, and the pod fails at its auth phase with a message
			// about the provider rather than about this table.
		default:
			return nil, nil, fmt.Errorf("resolve model credential: %w", err)
		}
	}

	mcp, err := s.mcpConfig(ctx, runID, spec)
	if err != nil {
		return nil, nil, err
	}
	if mcp != "" {
		secrets[runv1.SecretKeyMCPConfig] = mcp
	}

	if spec.Role != "" {
		role, err := s.store.RoleByName(ctx, spec.Role)
		switch {
		case err == nil:
			parsed, err := parseRole(role)
			if err != nil {
				return nil, nil, err
			}
			roleConfig = parsed.ConfigFiles
		case errors.Is(err, store.ErrNotFound):
			// The role was deleted after the run was admitted. The spec is
			// frozen and still describes what to run, so the run proceeds
			// without the fallback files rather than failing on a record that
			// is no longer there.
			s.log.Warn("role named by an admitted run no longer exists",
				"run", runID, "role", spec.Role)
		default:
			return nil, nil, fmt.Errorf("read role %s: %w", spec.Role, err)
		}
	}

	return secrets, roleConfig, nil
}

// mcpConfig renders the MCP configuration, including the per-run token.
//
// The token is why this is a secret rather than a ConfigMap, and why it is
// per-run rather than global: when an agent calls run_agent, the platform has
// to know which run is calling, so the child can be bound to its parent,
// checked against the depth limit and charged to the right budget. A global
// token makes all three impossible at once.
func (s *Service) mcpConfig(ctx context.Context, runID runv1.ULID, spec runv1.RenderedRunSpec) (string, error) {
	servers := map[string]any{}

	if s.limits.MCPEndpoint != "" {
		ttl := time.Duration(float32(spec.Runtime.TimeoutSeconds)*s.limits.RunTokenTTLMultiplier) * time.Second
		token, err := s.store.CreateToken(ctx, store.Token{
			Name:      "run " + string(runID),
			Kind:      store.TokenKindRunMCP,
			Scopes:    []string{store.ScopeRunsWrite, store.ScopeRunsRead},
			RunID:     runID,
			CreatedBy: "system",
		}, ttl)
		if err != nil {
			return "", fmt.Errorf("mint per-run MCP token for %s: %w", runID, err)
		}
		servers["haliphron"] = map[string]any{
			"type":    "http",
			"url":     s.limits.MCPEndpoint,
			"headers": map[string]string{"Authorization": "Bearer " + token.Secret},
		}
	}

	// The role's own servers are already in the spec, without header values.
	// They are rendered here too so the pod reads one file rather than merging
	// two sources with different trust levels.
	for _, srv := range spec.Runtime.MCPServers {
		entry := map[string]any{"type": srv.Transport}
		if srv.URL != "" {
			entry["url"] = srv.URL
		}
		if srv.Command != "" {
			entry["command"] = srv.Command
		}
		if len(srv.Args) > 0 {
			entry["args"] = srv.Args
		}
		servers[srv.Name] = entry
	}

	if len(servers) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return "", fmt.Errorf("render MCP configuration for %s: %w", runID, err)
	}
	return string(encoded), nil
}

// bundleTTL sizes the presigned capabilities against the run's own timeout.
//
// Deliberately not "long enough to be safe": a signature that outlives the work
// it was minted for is a capability lying in a Secret, and the controller can
// ask for a fresh bundle whenever the one it holds falls short of the next
// attempt.
func (s *Service) bundleTTL(timeoutSeconds int32) time.Duration {
	multiplier := s.timings.ArtifactTTLMultiplier
	if multiplier <= 0 {
		multiplier = 2
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 3600
	}
	return time.Duration(float32(timeoutSeconds)*multiplier) * time.Second
}
