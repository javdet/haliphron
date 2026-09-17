package v1

// AgentType selects the runtime inside the agent image. One image serves both;
// the variable switches authentication, MCP config rendering, the command line
// and the output parser.
//
// +kubebuilder:validation:Enum=claude-code;codex
// +kubebuilder:validation:MaxLength=32
type AgentType string

const (
	AgentClaudeCode AgentType = "claude-code"
	AgentCodex      AgentType = "codex"
)

// GitProvider selects the hosting API used for the pull request. "none" is a
// legitimate case: a run without a repository.
//
// +kubebuilder:validation:Enum=github;gitlab;none
// +kubebuilder:validation:MaxLength=16
type GitProvider string

const (
	GitProviderGitHub GitProvider = "github"
	GitProviderGitLab GitProvider = "gitlab"
	GitProviderNone   GitProvider = "none"
)

// PermissionMode is the agent CLI permission level, already reconciled with the
// policy ceiling by the backend. The controller does not recompute it.
//
// +kubebuilder:validation:Enum=default;acceptEdits;bypassPermissions;plan
// +kubebuilder:validation:MaxLength=32
type PermissionMode string

// MoneyUSD is a decimal string. Money is never a float64 here, and never a
// Kubernetes Quantity either: Quantity would round 0.4231 into a form nobody
// expects to see in a bill.
//
// +kubebuilder:validation:Pattern=`^-?[0-9]{1,12}(\.[0-9]{1,6})?$`
// +kubebuilder:validation:MaxLength=32
type MoneyUSD string

// ULID is the identifier shared with the backend's runs.id. Uppercase Crockford
// base32 on the wire; lowercased when it becomes a Kubernetes object name,
// which is lossless because the alphabet is case-insensitive.
//
// +kubebuilder:validation:Pattern=`^[0-9A-HJKMNP-TV-Z]{26}$`
// +kubebuilder:validation:MaxLength=26
type ULID string

// ObjectRef points at an object in shared storage. Content never travels
// through these contracts; only the pointer does.
type ObjectRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Key string `json:"key"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=64
	SHA256 string `json:"sha256,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	ContentType string `json:"contentType,omitempty"`
}

// RenderedRunSpec is one run, fully resolved: role applied, policy intersected,
// branch name generated, nothing left that would require a call back to the
// backend to interpret (principle P4).
//
// The same value travels as Lease.spec over the Cluster API and as the body of
// AgentRunSpec in the CRD. That is the whole point: one schema on two carriers,
// so the two contracts stay synchronised by construction.
//
// What is deliberately not here: secrets, presigned URLs, the prompt text and
// the role config files. Those are materials — the controller turns them into a
// Secret and a ConfigMap — and they travel beside the spec in the lease, never
// inside it. The rule is mechanical: if `kubectl get agentrun -o yaml` must not
// show it, it is not a spec field.
type RenderedRunSpec struct {
	Agent AgentType `json:"agent"`

	// Prompt points at runs/{runID}/prompt.txt, written by the backend at
	// admission. It travels through object storage rather than the Secret
	// because a workflow step's prompt absorbs the output of previous steps
	// and has no natural ceiling, while a Secret is capped at 1 MiB for all
	// keys together. The digest lets the pod verify it is executing exactly
	// what the backend posted.
	Prompt ObjectRef `json:"prompt"`

	// Model is a provider-qualified identifier, e.g. anthropic/claude-opus-5.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Model string `json:"model"`

	// Role is the platform-level role name. Informational for the controller;
	// the role has already been resolved into runtime, toolPolicy and the role
	// config files.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Role string `json:"role,omitempty"`

	// Image should be pinned by digest: reproducing attempt 2 matters more
	// than the convenience of a moving tag.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// +optional
	// +kubebuilder:validation:Enum=Always;IfNotPresent
	ImagePullPolicy string `json:"imagePullPolicy,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=253
	ImagePullSecrets []string `json:"imagePullSecrets,omitempty"`

	Repo    RepoSpec    `json:"repo"`
	Runtime RuntimeSpec `json:"runtime"`

	// +optional
	ToolPolicy *ToolPolicy `json:"toolPolicy,omitempty"`
	// +optional
	Budget *BudgetSpec `json:"budget,omitempty"`
	// +optional
	Retry *RetrySpec `json:"retry,omitempty"`
	// +optional
	Observability *ObservabilitySpec `json:"observability,omitempty"`

	// TTLSecondsAfterFinished is how long the AgentRun survives its terminal
	// phase. The owned Job, pod, Secret and ConfigMap go with it.
	// +optional
	// +kubebuilder:default=86400
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// RepoSpec describes the repository the agent works in. An empty URL means a
// run without a repository, which is legal.
type RepoSpec struct {
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url,omitempty"`
	// +optional
	Provider GitProvider `json:"provider,omitempty"`
	// +optional
	// +kubebuilder:default=main
	// +kubebuilder:validation:MaxLength=255
	BaseBranch string `json:"baseBranch,omitempty"`

	// TargetBranch is always generated by the backend, deterministically from
	// the runID. A name invented by the agent produces a second branch and a
	// second PR on the second attempt, which is exactly the idempotency the
	// retry path depends on.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	TargetBranch string `json:"targetBranch,omitempty"`

	// CloneDepth 0 means a full clone.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CloneDepth int32 `json:"cloneDepth,omitempty"`
	// +optional
	Submodules bool `json:"submodules,omitempty"`
	// +optional
	LFS bool `json:"lfs,omitempty"`
	// +optional
	// +kubebuilder:default=true
	CreatePR *bool `json:"createPR,omitempty"`
}

// RuntimeSpec is everything that shapes the pod.
type RuntimeSpec struct {
	// TimeoutSeconds is the agent's own budget, enforced by the entrypoint,
	// which exits 11 when it is exceeded. The Job's activeDeadlineSeconds is
	// derived from it as a backstop and is deliberately larger, so that a
	// normal timeout still gets to upload its partial result.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int32 `json:"timeoutSeconds"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxTurns int32 `json:"maxTurns,omitempty"`
	// +optional
	PermissionMode PermissionMode `json:"permissionMode,omitempty"`

	// Env carries non-secret variables only. Secrets arrive through envFrom on
	// the per-run Secret, which is why this list is safe to read with
	// `kubectl get agentrun`.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Env []EnvVar `json:"env,omitempty"`

	// +optional
	Resources Resources `json:"resources,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	NodeSelector map[string]LabelValue `json:"nodeSelector,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Tolerations []Toleration `json:"tolerations,omitempty"`

	// MCPServers are rendered server descriptions. Headers carrying tokens are
	// not here; they live in the mcp.json key of the per-run Secret.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
}

type EnvVar struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=4096
	Value string `json:"value"`
}

// Resources are container limits. The controller sets requests equal to limits,
// which puts the pod in the Guaranteed QoS class: an agent evicted halfway
// through under node pressure costs an hour of work and a second LLM bill, and
// Burstable buys nothing in return.
//
// The values are Kubernetes quantity strings, kept as strings so that this
// package stays free of apimachinery. The controller parses them; a malformed
// value is a config failure, reported before the Job is created.
type Resources struct {
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(m)?$`
	// +kubebuilder:validation:MaxLength=32
	CPU string `json:"cpu,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|k|M|G|T)?$`
	// +kubebuilder:validation:MaxLength=32
	Memory string `json:"memory,omitempty"`

	// EphemeralStorage is not decorative. A clone plus dependencies plus logs
	// fills the node's disk, and an unbounded agent pod shows up as random
	// other pods being evicted.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|k|M|G|T)?$`
	// +kubebuilder:validation:MaxLength=32
	EphemeralStorage string `json:"ephemeralStorage,omitempty"`
}

// Toleration mirrors the Kubernetes toleration rather than importing it: k8s.io/api
// is a large dependency to hand the backend for five fields, and an explicit
// struct keeps the CRD schema structural instead of preserve-unknown-fields.
type Toleration struct {
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Exists;Equal
	// +kubebuilder:validation:MaxLength=16
	Operator string `json:"operator,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Value string `json:"value,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
	// +kubebuilder:validation:MaxLength=32
	Effect string `json:"effect,omitempty"`
	// +optional
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

type MCPServer struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=stdio;http;sse
	// +kubebuilder:validation:MaxLength=16
	Transport string `json:"transport"`
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Command string `json:"command,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=1024
	Args []string `json:"args,omitempty"`
}

// ToolPolicy is already intersected with the policy ceiling. The controller
// passes it through and never recomputes it.
type ToolPolicy struct {
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=256
	Allow []string `json:"allow,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=256
	Deny []string `json:"deny,omitempty"`
}

type BudgetSpec struct {
	// +optional
	MaxCostUSD MoneyUSD `json:"maxCostUSD,omitempty"`
}

type RetrySpec struct {
	// MaxInfraRetries is how many extra attempts the controller starts on its
	// own, without asking the backend. Only infra and git classes are retried,
	// and the retry is idempotent thanks to runs/{runID}/state.json: a run that
	// already finished the agent phase resumes at push, and does not pay for
	// the model twice.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxInfraRetries *int32 `json:"maxInfraRetries,omitempty"`
}

type ObservabilitySpec struct {
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	OTLPEndpoint string `json:"otlpEndpoint,omitempty"`
	// Traceparent is the W3C parent context: workflow instance to step to attempt.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Traceparent string `json:"traceparent,omitempty"`
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	LogChunkIntervalSeconds *int32 `json:"logChunkIntervalSeconds,omitempty"`
}

// Usage is what the run cost. Every field is self-declared by the pod, which is
// the least trusted component in the system: a compromised agent can understate
// its spend. The backend cross-checks durationMs against the Job duration
// observed by the controller and audits the difference; the real fix is
// metering at an LLM proxy, which is out of scope for v1.
type Usage struct {
	// +optional
	DurationMs int64 `json:"durationMs,omitempty"`
	// +optional
	NumTurns int32 `json:"numTurns,omitempty"`
	// +optional
	TotalCostUSD MoneyUSD `json:"totalCostUSD,omitempty"`
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// +optional
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// +optional
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	SessionID string `json:"sessionID,omitempty"`
}

// LabelValue is a Kubernetes label value. It is a named type only so that the
// schema can bound it: an unbounded map is the one shape whose comparison cost
// the API server cannot estimate, and the immutability rule on the spec is
// priced against exactly that estimate.
//
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(|[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)$`
type LabelValue string
