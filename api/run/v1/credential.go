package v1

import "strings"

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
