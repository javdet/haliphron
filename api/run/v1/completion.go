package v1

// CompletionReport is what the pod says happened. It travels twice: once to the
// controller over the in-cluster webhook, and once into storage as
// completion.json, unchanged. The controller forwards it to the backend without
// editing it, so the three copies are byte-identical and any of them can be the
// one that survives.
//
// Nothing in here is authoritative. It is produced by the least trusted
// component in the system: the cost and token counts come out of the agent CLI
// and a compromised agent can understate them. The backend cross-checks
// Usage.DurationMs against the Job duration the controller observed and audits
// the difference; metering at an LLM proxy is the real answer and is out of
// scope for v1.
type CompletionReport struct {
	// RunID and Attempt are here as well as in the ingest envelope, because
	// completion.json is read from the bucket without an envelope when the
	// controller dies between the webhook and the ingest. A report that cannot
	// say which run it belongs to is not a fallback.
	RunID   ULID  `json:"runID"`
	Attempt int32 `json:"attempt"`

	Status   CompletionStatus `json:"status"`
	ExitCode int32            `json:"exitCode"`
	// +optional
	FailureClass FailureClass `json:"failureClass,omitempty"`

	// FailedPhase, Reason and Message are the difference between "Failed,
	// config" in the UI and a user who can fix their run. An exit code is eight
	// bits; the evidence for what those bits meant exists only in the pod, and
	// only at the moment of failure.
	// +optional
	FailedPhase RuntimePhase `json:"failedPhase,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`

	Agent AgentType `json:"agent"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Runtime *RuntimeInfo `json:"runtime,omitempty"`

	// +optional
	ResultRef *ObjectRef `json:"resultRef,omitempty"`
	// +optional
	OutputRef *ObjectRef `json:"outputRef,omitempty"`
	// +optional
	LogRef *ObjectRef `json:"logRef,omitempty"`
	// +optional
	StateRef *ObjectRef `json:"stateRef,omitempty"`

	// Summary is the first 64 KiB of result.md and the only content in this
	// contract. It is duplicated into runs.result_summary so that the run list
	// in the UI does not reach into storage once per row.
	// +optional
	Summary string `json:"summary,omitempty"`

	// +optional
	Repo *RepoResult `json:"repo,omitempty"`
	// +optional
	Usage *Usage `json:"usage,omitempty"`

	// PhaseTimings is a cheap substitute for tracing when OTLP is not
	// configured, and the only way to see that the time went into cloning a
	// monorepo rather than into the model.
	// +optional
	PhaseTimings []PhaseTiming `json:"phaseTimings,omitempty"`

	// ChildRunIDs are runs this one started through its per-run MCP token, for
	// reconciling depth and cost.
	// +optional
	ChildRunIDs []ULID `json:"childRunIDs,omitempty"`
}

// CompletionStatus is the pod's own verdict. It is derivable from the exit code
// and is sent anyway: the controller needs to distinguish a cancellation from
// an eviction, and both arrive as signal 143.
//
// +kubebuilder:validation:Enum=success;failure;timeout;cancelled
type CompletionStatus string

const (
	CompletionSuccess   CompletionStatus = "success"
	CompletionFailure   CompletionStatus = "failure"
	CompletionTimeout   CompletionStatus = "timeout"
	CompletionCancelled CompletionStatus = "cancelled"
)

// RuntimeInfo identifies what actually ran. Without it, a behaviour change
// across a fleet of clusters running different image tags is diagnosed by
// guesswork.
type RuntimeInfo struct {
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	ImageVersion string `json:"imageVersion,omitempty"`
	// ContractVersion is the runtime contract the image implements. The
	// controller rejects a major it does not speak before the Job is created,
	// so a mismatch here means the check was bypassed.
	// +optional
	ContractVersion string `json:"contractVersion,omitempty"`
	// AgentVersion is the version of the agent CLI inside the image.
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`
}

// RepoResult is what happened to the repository. Absent for a run without one.
type RepoResult struct {
	// +optional
	Pushed bool `json:"pushed,omitempty"`
	// +optional
	TargetBranch string `json:"targetBranch,omitempty"`
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
	// +optional
	PRURL string `json:"prURL,omitempty"`
	// +optional
	PRNumber int32 `json:"prNumber,omitempty"`
	// PRAction distinguishes the second attempt from the first: create-or-update
	// is one of the converging side effects that make a retry safe.
	// +optional
	PRAction PRAction `json:"prAction,omitempty"`
}

// +kubebuilder:validation:Enum=created;updated;none
type PRAction string

const (
	PRActionCreated PRAction = "created"
	PRActionUpdated PRAction = "updated"
	PRActionNone    PRAction = "none"
)

// PhaseTiming is one entrypoint phase, as measured.
type PhaseTiming struct {
	Phase      RuntimePhase `json:"phase"`
	DurationMs int64        `json:"durationMs"`
	// +optional
	Outcome PhaseOutcome `json:"outcome,omitempty"`
}
