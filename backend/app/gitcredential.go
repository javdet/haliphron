package app

import (
	"context"
	"errors"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The git credential is the stored secret StoredGitToken resolves for a run
// with a repository: Limits.GitTokenSecret, with <name>-github and
// <name>-gitlab tried first. It gets a use case of its own for the same reason
// the model credential does. Its absence is not refused at admission — a public
// repository without a pull request needs none — so nothing says it is missing
// until a run fails at clone or push, minutes later and far from the page where
// it is fixed. Most runs name a private repository or ask for a pull request,
// so the UI asks for one up front, and it can only ask for a name the backend
// tells it.

// gitCredentialProviders are the forges with a name of their own, in the order
// they are reported. The fallback, which is tried last, is reported first.
var gitCredentialProviders = []runv1.GitProvider{runv1.GitProviderGitHub, runv1.GitProviderGitLab}

// GitCredential is the state of the installation's git tokens, values excluded.
type GitCredential struct {
	// SecretName is the fallback name. Empty when the deployment configured
	// none, in which case Tokens is empty and Problem says so.
	SecretName string
	// Tokens is the fallback first, then one entry per forge, stored or not.
	Tokens  []GitToken
	Problem string
}

// GitToken is one of the names a lease may resolve.
type GitToken struct {
	// Provider is empty for the fallback, which every forge uses when it has
	// no token of its own.
	Provider   runv1.GitProvider
	SecretName string
	Configured bool
	// Kind is managed or referenced.
	Kind      string
	UpdatedAt *time.Time
	// Problem is why a stored token will not reach a pod. A lease that meets
	// one fails outright rather than leasing the run without it, so this is
	// worse than absence.
	Problem string
}

// Configured is whether any token is stored, usable or not.
func (c GitCredential) Configured() bool {
	for _, token := range c.Tokens {
		if token.Configured {
			return true
		}
	}
	return false
}

// GitCredentialRequest stores one token. Provider is empty for the fallback.
type GitCredentialRequest struct {
	Provider string
	Value    string
	Ref      string
}

// GitCredential reports every name a lease may resolve.
func (s *Service) GitCredential(ctx context.Context) (GitCredential, error) {
	base := s.limits.GitTokenSecret
	out := GitCredential{SecretName: base}
	if base == "" {
		out.Problem = "the deployment names no git credential secret (HALIPHRON_GIT_SECRET is empty), " +
			"so no run receives a git token"
		return out, nil
	}

	secrets, err := s.store.ListSecrets(ctx)
	if err != nil {
		return GitCredential{}, err
	}
	stored := make(map[string]store.Secret, len(secrets))
	for _, secret := range secrets {
		stored[secret.Name] = secret
	}

	providers := append([]runv1.GitProvider{""}, gitCredentialProviders...)
	for _, provider := range providers {
		token := GitToken{Provider: provider, SecretName: gitTokenName(base, provider)}
		if secret, ok := stored[token.SecretName]; ok {
			token.Configured = true
			token.Kind = secret.Kind
			token.UpdatedAt = &secret.UpdatedAt
			token.Problem = s.gitTokenProblem(ctx, token.SecretName)
		}
		out.Tokens = append(out.Tokens, token)
	}
	return out, nil
}

// gitTokenProblem resolves the token the way the lease path does, so a token
// this reports as healthy is one a lease can resolve. Only the outcome leaves.
func (s *Service) gitTokenProblem(ctx context.Context, name string) string {
	_, err := s.store.ResolveSecret(ctx, name)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, store.ErrNotFound):
		// Removed between the listing and this read; the next read says so.
		return ""
	case errors.Is(err, store.ErrSecretNotManaged):
		return "the token is a reference, and the backend does not resolve references; " +
			"no run with a repository on this forge can be leased until it is stored as a value"
	case errors.Is(err, store.ErrNoKEK):
		return "no key encryption key is configured, so the stored value cannot be decrypted"
	default:
		// A KEK mismatch or a corrupt row. The message names the key ids and
		// never the value.
		return err.Error()
	}
}

// PutGitCredential stores a token under the fallback name or a forge's own.
//
// Only a value is accepted. StoredGitToken fails the lease on a reference
// instead of skipping it, so accepting one here would be accepting a
// credential that stops every run with a repository from starting.
func (s *Service) PutGitCredential(ctx context.Context, req GitCredentialRequest, by string) (GitCredential, error) {
	base := s.limits.GitTokenSecret
	if base == "" {
		return GitCredential{}, &run.InvalidRequestError{Field: "",
			Detail: "the deployment names no git credential secret (HALIPHRON_GIT_SECRET is empty)"}
	}
	provider := runv1.GitProvider(req.Provider)
	if provider != "" && provider != runv1.GitProviderGitHub && provider != runv1.GitProviderGitLab {
		return GitCredential{}, &run.InvalidRequestError{Field: "provider",
			Detail: "provider is github, gitlab, or absent for the token every forge falls back to"}
	}
	if strings.TrimSpace(req.Ref) != "" {
		return GitCredential{}, &run.InvalidRequestError{Field: "ref",
			Detail: "a git token is stored as a value: the backend does not resolve references, " +
				"and a lease that meets one fails"}
	}

	value := strings.TrimSpace(req.Value)
	switch {
	case value == "":
		return GitCredential{}, &run.InvalidRequestError{Field: "value", Detail: "value is required"}
	case strings.ContainsAny(value, " \t\r\n"):
		// A pasted header or a line of a credentials file. Stored as it is,
		// it would reach the forge as a token and be refused there.
		return GitCredential{}, &run.InvalidRequestError{Field: "value",
			Detail: "a token has no whitespace in it; paste the token alone"}
	}
	if forge := gitTokenForge(value); forge != "" && provider != "" && forge != provider {
		return GitCredential{}, &run.InvalidRequestError{Field: "value",
			Detail: "this looks like a " + string(forge) + " token, stored as the " + string(provider) + " one"}
	}

	if _, err := s.store.PutManagedSecret(ctx, gitTokenName(base, provider), []byte(value), by); err != nil {
		return GitCredential{}, err
	}
	return s.GitCredential(ctx)
}

// gitTokenName is the name StoredGitToken tries for a provider.
func gitTokenName(base string, provider runv1.GitProvider) string {
	if provider == "" {
		return base
	}
	return base + "-" + string(provider)
}

// gitTokenForge recognises the documented token prefixes. Anything else —
// a self-managed forge's token, a classic GitHub OAuth token — is not
// recognised and not refused.
func gitTokenForge(value string) runv1.GitProvider {
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if strings.HasPrefix(value, prefix) {
			return runv1.GitProviderGitHub
		}
	}
	if strings.HasPrefix(value, "glpat-") {
		return runv1.GitProviderGitLab
	}
	return ""
}
