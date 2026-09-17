package v1

// Materials are the parts of a lease that the controller turns into Kubernetes
// objects instead of copying into the AgentRun: the secret values, the
// presigned bundle and the role config files. They never appear in a spec, and
// the key names below are the only names either side may use.
//
// The constants live in the shared package for the same reason the spec types
// do: the backend fills these keys and the controller reads them, and a typo
// discovered at runtime looks like "the agent silently had no git token".
const (
	// SecretKeyGitToken is a short-lived token scoped to the single repository
	// of this run. The entrypoint re-exports it under GH_TOKEN and GL_TOKEN as
	// well, because gh and glab each insist on their own name.
	SecretKeyGitToken = "git-token"
	// SecretKeyLLMAPIKey authenticates the agent CLI to the model provider.
	SecretKeyLLMAPIKey = "llm-api-key"
	// SecretKeyMCPConfig holds the rendered MCP configuration including header
	// values, which is why it is a secret and not a ConfigMap.
	SecretKeyMCPConfig = "mcp.json"
	// SecretKeyPresigned holds the presigned bundle. Presigned URLs are bearer
	// capabilities on someone else's bucket prefix, so they belong in a Secret
	// and never in the CR or in a variable visible to `kubectl describe`.
	SecretKeyPresigned = "presigned.json"
	// SecretKeyCallbackToken authenticates the pod's completion callback to the
	// controller. Unlike every other key it is minted by the controller, not
	// the backend: without it any pod in the agents namespace could post a
	// forged completion for another run.
	SecretKeyCallbackToken = "callback-token"
)

// Object storage layout. Fixed, because three components address it
// independently: the backend writes the prompt and reads the fallback, the pod
// writes everything else, and the controller never touches storage at all.
const (
	// StoragePrefixRun is the per-run prefix; presigned capabilities are scoped
	// to it, so a compromised pod cannot read another run.
	StoragePrefixRun = "runs/%s/"

	StorageKeyPrompt     = "prompt.txt"
	StorageKeyResult     = "result.md"
	StorageKeyOutput     = "output.json"
	StorageKeyState      = "state.json"
	StorageKeyCompletion = "completion.json"
	StorageKeyAgentLog   = "logs/agent.log"
	StoragePrefixChunks  = "logs/chunks/"
)
