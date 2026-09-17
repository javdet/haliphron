package v1

import runv1 "github.com/automagicops/haliphron/api/run/v1"

// Problem is RFC 9457 with two haliphron extensions: Code and Action.
//
// Action is the important one. Without it each side infers behaviour from the
// status code, and that inference is where two independently written sides
// diverge first: one reads a 409 as grounds to retry, the other as grounds to
// give up, and the disagreement only shows up when a lease expires in
// production. The backend says what to do; the controller does it and does not
// second-guess the number.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int32  `json:"status"`
	// +optional
	Detail string `json:"detail,omitempty"`
	// +optional
	Instance string `json:"instance,omitempty"`

	Code   ProblemCode `json:"code"`
	Action Action      `json:"action"`

	// +optional
	RetryAfterSeconds int32 `json:"retryAfterSeconds,omitempty"`
	// +optional
	RunID runv1.ULID `json:"runID,omitempty"`
	// +optional
	ClusterID runv1.ULID `json:"clusterID,omitempty"`

	// CurrentEpoch and CurrentStatus travel with an EpochMismatch so the
	// controller can tell "my work was reassigned" from "I am desynchronised",
	// which are the same rejection but different bugs.
	// +optional
	CurrentEpoch int64 `json:"currentEpoch,omitempty"`
	// +optional
	CurrentStatus string `json:"currentStatus,omitempty"`
	// +optional
	TraceID string `json:"traceID,omitempty"`
}

// Error lets a Problem be returned as an error by a client. The text is the
// code rather than the title: the title is prose and may be localised or
// reworded, the code is the contract.
func (p *Problem) Error() string { return string(p.Code) + ": " + p.Title }

// ProblemCode is the machine-readable cause. Stable, unlike Title.
type ProblemCode string

const (
	CodeInvalidRequest               ProblemCode = "InvalidRequest"
	CodeUnauthenticated              ProblemCode = "Unauthenticated"
	CodeClusterRevoked               ProblemCode = "ClusterRevoked"
	CodeClusterMismatch              ProblemCode = "ClusterMismatch"
	CodeClusterNameTaken             ProblemCode = "ClusterNameTaken"
	CodeBootstrapTokenInvalid        ProblemCode = "BootstrapTokenInvalid"
	CodeBootstrapTokenConsumed       ProblemCode = "BootstrapTokenConsumed"
	CodeUnsupportedControllerVersion ProblemCode = "UnsupportedControllerVersion"
	CodeRunNotFound                  ProblemCode = "RunNotFound"
	CodeEpochMismatch                ProblemCode = "EpochMismatch"
	CodeRunLeasedByAnotherCluster    ProblemCode = "RunLeasedByAnotherCluster"
	CodeRunTerminal                  ProblemCode = "RunTerminal"
	CodePhaseRegression              ProblemCode = "PhaseRegression"
	CodeAttemptRegression            ProblemCode = "AttemptRegression"
	CodeLeaseExpired                 ProblemCode = "LeaseExpired"
	CodePayloadTooLarge              ProblemCode = "PayloadTooLarge"
	CodeRateLimited                  ProblemCode = "RateLimited"
	CodeUnavailable                  ProblemCode = "Unavailable"
	CodeInternal                     ProblemCode = "Internal"
)

// Action is the instruction to the controller.
type Action string

const (
	// ActionRetry means exponential backoff with jitter, base 1s, ceiling 60s,
	// indefinitely.
	ActionRetry Action = "retry"
	// ActionBackoff is the same, but not before Retry-After.
	ActionBackoff Action = "backoff"
	// ActionAbandon means the work is no longer ours: cancel the Job, delete
	// the CR, and report nothing further about this run. Reporting anything
	// after an abandon is how a zombie controller appends its state to a run
	// that now belongs to a different cluster.
	ActionAbandon Action = "abandon"
	// ActionResync means send the next heartbeat with reportComplete: true.
	ActionResync Action = "resync"
	// ActionReregister means the credential is unusable: stop polling and raise
	// an event for a human. Retrying cannot fix it.
	ActionReregister Action = "reregister"
	// ActionFatal means a defect or an incompatibility. A repeat will not help.
	ActionFatal Action = "fatal"
)

// ProblemTypeBase is the prefix of the Type URI. The suffix is the kebab-cased
// code; it is a documentation anchor and nothing dereferences it at runtime.
const ProblemTypeBase = "https://haliphron.io/problems/"
