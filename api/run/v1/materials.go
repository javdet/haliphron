package v1

// Materials are the parts of a lease that the controller turns into Kubernetes
// objects instead of copying into the AgentRun: the secret values, the prompt,
// the presigned bundle of object-store mode and the role config files. They
// never appear in a spec, and the key names below are the only names either
// side may use.
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

	// SecretKeyPrompt holds the task itself, and is the one key in this Secret
	// that is not a credential.
	//
	// It is here rather than in the spec because prompts carry customer
	// context: a field in the spec would put them into
	// `kubectl get agentrun -o yaml` and into every GitOps diff. It reaches
	// the container as an environment variable through valueFrom.secretKeyRef
	// — one named key, not envFrom — so the value shows up in neither the
	// Job's spec nor `kubectl describe pod` (ADR 38).
	//
	// ADR 33 is not contradicted by that. It keeps secret *material* out of
	// the environment because the agent inherits the pod's environment and is
	// assumed able to exfiltrate whatever it can read. The prompt is the one
	// value the agent is meant to read; nothing is protected by withholding it
	// from the process whose entire purpose is to act on it.
	SecretKeyPrompt = "prompt"

	// SecretKeyPresigned holds the presigned bundle, and exists only in
	// object-store mode. Presigned URLs are bearer capabilities on someone
	// else's bucket prefix, so they belong in a Secret and never in the CR or
	// in a variable visible to `kubectl describe`. In relay mode the key is
	// absent, because the pod addresses no object store at all.
	SecretKeyPresigned = "presigned.json"

	// SecretKeyCallbackToken authenticates the pod's completion callback and
	// its artifact uploads to the controller. Unlike every other key it is
	// minted by the controller, not the backend: without it any pod in the
	// agents namespace could post a forged completion for another run.
	SecretKeyCallbackToken = "callback-token"
)

// ArtifactMode selects how a run's results reach durable storage. One port,
// two implementations, and nothing above the port knows which is in force —
// the callback path, the report ordering and the CompletedWithoutResult
// recovery are identical in both.
//
// +kubebuilder:validation:Enum=relay;object-store
// +kubebuilder:validation:MaxLength=16
type ArtifactMode string

const (
	// ArtifactModeRelay is the default: pod → controller → backend → a PVC on
	// the backend. No object storage anywhere, which is the point — an
	// installation should not have to stand up MinIO to run its first agent.
	ArtifactModeRelay ArtifactMode = "relay"
	// ArtifactModeObjectStore is pod → S3/MinIO directly, by presigned URL.
	// The better answer at volume, and never a requirement.
	ArtifactModeObjectStore ArtifactMode = "object-store"
)

// Artifact store layout. Fixed, and identical in both modes, because three
// components address it independently: the pod produces the objects, the
// controller stamps their run prefix in relay mode, and the backend reads the
// fallback back out. The mode changes the path the bytes take and nothing
// about what they are called.
//
// There is no prompt.txt and no state.json. Both moved into PostgreSQL — the
// prompt to runs.prompt, the checkpoint to run_attempts.completed_phases — and
// what remains here is what actually has no ceiling: outputs, logs and
// artifacts. That is what makes the object store optional: nothing on the path
// to *starting* a run touches this store any more, only the path to finishing
// one.
const (
	// StoragePrefixRun is the per-run prefix. In object-store mode presigned
	// capabilities are scoped to it; in relay mode the controller stamps it
	// from the CR, so the key is not the pod's to choose. Either way a
	// compromised pod cannot name another run's prefix.
	StoragePrefixRun = "runs/%s/"

	StorageKeyResult       = "result.md"
	StorageKeyOutput       = "output.json"
	StorageKeyCompletion   = "completion.json"
	StorageKeyAgentLog     = "logs/agent.log"
	StoragePrefixChunks    = "logs/chunks/"
	StoragePrefixArtifacts = "artifacts/"
)

// URI schemes for runs.result_ref. A stored row says which store wrote it, so
// an installation that migrates from one mode to the other keeps its old runs
// readable instead of orphaning them.
const (
	SchemeFile = "file"
	SchemeS3   = "s3"
)
