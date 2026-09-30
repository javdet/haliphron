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

// The model credential is one stored secret, named by Limits.LLMAPIKeySecret,
// that every lease resolves into the pod's llm-api-key. It gets a use case of
// its own rather than being one row among the others because it is the one
// every run needs: without it every run fails in the pod's validate phase, and
// a generic secrets table does not tell an operator that, or what the name has
// to be.

// Model credential types, as the public API spells them.
const (
	// ModelCredentialAPIKey is a provider API key, or a gateway's token: what
	// claude-code reads from ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN and codex
	// from OPENAI_API_KEY.
	ModelCredentialAPIKey = "api_key"
	// ModelCredentialOAuthToken is a Claude subscription token from
	// `claude setup-token`. It authenticates claude-code only.
	ModelCredentialOAuthToken = "oauth_token"
)

// ModelCredential is the state of the installation's model credential, value
// excluded.
type ModelCredential struct {
	// SecretName is the stored secret leases read. Empty when the deployment
	// configured none, in which case no lease carries a model key.
	SecretName string
	Configured bool
	// Kind is managed or referenced.
	Kind string
	// Type is ModelCredentialAPIKey or ModelCredentialOAuthToken, derived from
	// the value. Empty when the value is not here to look at: a reference, or
	// a managed secret this process cannot decrypt.
	Type      string
	Ref       string
	UpdatedAt *time.Time
	// Problem is why a stored credential will not reach a pod, in words an
	// operator can act on. Empty when leases will carry it.
	Problem string
}

// ModelCredentialRequest sets the credential. Exactly one of Value and Ref.
type ModelCredentialRequest struct {
	Type  string
	Value string
	Ref   string
}

// ModelCredential reports the credential without resolving anything outside
// this process.
func (s *Service) ModelCredential(ctx context.Context) (ModelCredential, error) {
	out := ModelCredential{SecretName: s.limits.LLMAPIKeySecret}
	if out.SecretName == "" {
		out.Problem = "the deployment names no model credential secret (HALIPHRON_LLM_SECRET is empty), " +
			"so no run receives a model key"
		return out, nil
	}

	secrets, err := s.store.ListSecrets(ctx)
	if err != nil {
		return ModelCredential{}, err
	}
	for _, secret := range secrets {
		if secret.Name != out.SecretName {
			continue
		}
		out.Configured = true
		out.Kind, out.Ref = secret.Kind, secret.RefURI
		out.UpdatedAt = &secret.UpdatedAt
	}
	if !out.Configured {
		return out, nil
	}

	// The value is decrypted to classify it, and only the class leaves. The
	// lease path decrypts the same row with the same call, so a credential
	// this reports as healthy is one a lease can resolve.
	value, err := s.store.ResolveSecret(ctx, out.SecretName)
	switch {
	case err == nil:
		out.Type = modelCredentialType(string(value))
	case errors.Is(err, store.ErrSecretNotManaged):
		out.Problem = "the credential is a reference, and the backend does not resolve references; " +
			"leases cannot carry it. Store the value here instead"
	case errors.Is(err, store.ErrNoKEK):
		out.Problem = "no key encryption key is configured, so the stored value cannot be decrypted"
	default:
		// A KEK mismatch or a corrupt row. The message names the key ids,
		// which is what the operator needs, and never the value.
		out.Problem = err.Error()
	}
	return out, nil
}

// PutModelCredential stores the credential under the configured name.
//
// The declared type is checked against the value. The pod tells the two apart
// by prefix whatever is declared, so a mismatch is a paste into the wrong box —
// an API key a subscription holder meant to replace, or the reverse — and
// refusing it here is the difference between a message now and a 401 in a pod.
func (s *Service) PutModelCredential(ctx context.Context, req ModelCredentialRequest, by string) (ModelCredential, error) {
	name := s.limits.LLMAPIKeySecret
	if name == "" {
		return ModelCredential{}, &run.InvalidRequestError{Field: "",
			Detail: "the deployment names no model credential secret (HALIPHRON_LLM_SECRET is empty)"}
	}
	if req.Type != ModelCredentialAPIKey && req.Type != ModelCredentialOAuthToken {
		return ModelCredential{}, &run.InvalidRequestError{Field: "type",
			Detail: "type is " + ModelCredentialAPIKey + " or " + ModelCredentialOAuthToken}
	}

	value, ref := strings.TrimSpace(req.Value), strings.TrimSpace(req.Ref)
	switch {
	case value != "" && ref != "":
		return ModelCredential{}, &run.InvalidRequestError{Field: "value",
			Detail: "a credential is either managed or referenced, not both"}
	case value != "":
		if got := modelCredentialType(value); got != req.Type {
			return ModelCredential{}, &run.InvalidRequestError{Field: "value",
				Detail: mismatchDetail(req.Type)}
		}
		if _, err := s.store.PutManagedSecret(ctx, name, []byte(value), by); err != nil {
			return ModelCredential{}, err
		}
	case ref != "":
		// A reference cannot be checked against its type; the pod classifies
		// whatever arrives.
		if _, err := s.store.PutReferencedSecret(ctx, name, ref, by); err != nil {
			return ModelCredential{}, err
		}
	default:
		return ModelCredential{}, &run.InvalidRequestError{Field: "value",
			Detail: "either value or ref is required"}
	}
	return s.ModelCredential(ctx)
}

func modelCredentialType(value string) string {
	if runv1.IsClaudeOAuthToken(value) {
		return ModelCredentialOAuthToken
	}
	return ModelCredentialAPIKey
}

func mismatchDetail(declared string) string {
	if declared == ModelCredentialOAuthToken {
		return "this is not a Claude OAuth token: those start with " + runv1.ClaudeOAuthTokenPrefix +
			" and come from `claude setup-token`. For an API key choose " + ModelCredentialAPIKey
	}
	return "this is a Claude subscription OAuth token (" + runv1.ClaudeOAuthTokenPrefix +
		"…), not an API key; choose " + ModelCredentialOAuthToken
}
