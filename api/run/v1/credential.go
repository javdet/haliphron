package v1

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ClaudeOAuthTokenPrefix starts every token `claude setup-token` mints for a
// Claude subscription. The version digits after it are left out, so the next
// generation of tokens is still recognised.
const ClaudeOAuthTokenPrefix = "sk-ant-oat"

// IsClaudeOAuthToken reports whether a model credential is a Claude
// subscription OAuth token rather than an API key.
//
// The two cannot share an environment variable. claude-code sends
// ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN ahead of CLAUDE_CODE_OAUTH_TOKEN,
// so an OAuth token exported under either of them goes out as an API key and
// comes back a 401. The backend classifies a stored credential with this
// function and the image picks the variable with it: one predicate, so a token
// the UI calls OAuth is the token the pod treats as OAuth.
func IsClaudeOAuthToken(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), ClaudeOAuthTokenPrefix)
}

// ModelEndpointMaxLen bounds a model endpoint. It matches the column the
// backend stores it in.
const ModelEndpointMaxLen = 2048

// NormalizeModelEndpoint checks the root URL of a model gateway — OpenRouter,
// LiteLLM, a corporate proxy — and returns it in the one form both sides use.
// An empty value is no endpoint: the CLIs talk to their own provider.
//
// The endpoint is a root and not an API path, and that is what lets one value
// serve both runtimes. claude-code appends /v1/messages to it; codex is given
// it with /v1 appended and adds /responses. For OpenRouter the root is
// https://openrouter.ai/api. A value that already ends in /v1 is refused
// rather than tolerated: tolerated, it becomes /v1/v1/messages and a 404 from
// inside a pod.
//
// Credentials in the URL are refused as well. The endpoint is not a secret —
// the UI shows it and the pod logs it — and a password in its userinfo would
// be published by both.
//
// The backend calls this when the credential is saved and the image again when
// it reads the lease: one predicate, so an endpoint the UI accepts is the
// endpoint the pod uses.
func NormalizeModelEndpoint(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) > ModelEndpointMaxLen {
		return "", fmt.Errorf("the model endpoint is longer than %d bytes", ModelEndpointMaxLen)
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("the model endpoint is not a URL: %v", err)
	}
	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return "", errors.New("the model endpoint must be an http or https URL, such as https://openrouter.ai/api")
	case u.Host == "":
		return "", errors.New("the model endpoint has no host")
	case u.User != nil:
		return "", errors.New("the model endpoint must not carry credentials; the key goes in the credential's value")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("the model endpoint must not have a query or a fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	if u.Path == "/v1" || strings.HasSuffix(u.Path, "/v1") {
		return "", fmt.Errorf("the model endpoint is the gateway's root, without /v1: %s",
			strings.TrimSuffix(u.String(), "/v1"))
	}
	return u.String(), nil
}
