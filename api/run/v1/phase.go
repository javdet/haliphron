package v1

// Phase is what the controller observes in the cluster. The backend-owned
// states (Queued, Leased, Dispatched, Unknown) are deliberately absent: the
// controller never names them, and giving them a constant here would invite it.
//
// +kubebuilder:validation:Enum=Pending;Starting;Running;Succeeded;Failed;TimedOut;Cancelled
type Phase string

const (
	// PhasePending means the AgentRun exists but no Job has been created yet.
	PhasePending Phase = "Pending"
	// PhaseStarting means the Job exists and the pod has not reached Running.
	PhaseStarting Phase = "Starting"
	// PhaseRunning means the agent container is running.
	PhaseRunning Phase = "Running"
	// PhaseSucceeded means the container exited 0. Nothing more is implied:
	// whether the work was done correctly is a separate workflow node.
	PhaseSucceeded Phase = "Succeeded"
	// PhaseFailed means the container exited non-zero, or the pod never got
	// far enough to run it.
	PhaseFailed Phase = "Failed"
	// PhaseTimedOut means the run exceeded its own budget (exit 11) or the
	// Job's activeDeadlineSeconds backstop.
	PhaseTimedOut Phase = "TimedOut"
	// PhaseCancelled means a cancel command was carried out.
	PhaseCancelled Phase = "Cancelled"
)

// Phase ranks. Reports are ordered by (attempt, rank), never by time: cluster
// clocks are not synchronised, so a timestamp cannot decide which of two
// reports is newer.
const (
	rankPending  = 10
	rankStarting = 20
	rankRunning  = 30
	rankTerminal = 40
)

// Rank returns the monotonic rank of the phase. An unknown phase ranks 0, which
// makes it lose every comparison rather than overwrite known state — a newer
// peer sending a phase this build has never heard of must not be able to move
// a run backwards.
func (p Phase) Rank() int {
	switch p {
	case PhasePending:
		return rankPending
	case PhaseStarting:
		return rankStarting
	case PhaseRunning:
		return rankRunning
	case PhaseSucceeded, PhaseFailed, PhaseTimedOut, PhaseCancelled:
		return rankTerminal
	default:
		return 0
	}
}

// IsTerminal reports whether the phase admits no successor within the attempt.
func (p Phase) IsTerminal() bool { return p.Rank() == rankTerminal }

// FailureClass decides whether the controller retries on its own. Only infra
// and git are retried: replaying an agent that already pushed a branch and
// opened a PR spends the budget twice and can produce conflicting commits.
//
// +kubebuilder:validation:Enum=none;infra;agent;git;config;budget
type FailureClass string

const (
	FailureNone   FailureClass = "none"
	FailureInfra  FailureClass = "infra"
	FailureAgent  FailureClass = "agent"
	FailureGit    FailureClass = "git"
	FailureConfig FailureClass = "config"
	FailureBudget FailureClass = "budget"
)

// Retriable reports whether the controller may start another attempt locally,
// without asking the backend.
func (f FailureClass) Retriable() bool {
	return f == FailureInfra || f == FailureGit
}

// Agent exit codes, as produced by the image entrypoint. The runtime contract
// (contract 3) owns this table; it lives here because the controller maps codes
// to failure classes and the backend explains them in the UI, and the two must
// not hold different copies of it.
const (
	ExitSuccess       int32 = 0
	ExitAgentError    int32 = 10 // agent CLI exited non-zero
	ExitAgentTimeout  int32 = 11 // entrypoint killed the agent at timeoutSeconds
	ExitOutputInvalid int32 = 12 // output.json missing or fails the node schema
	ExitGit           int32 = 20 // clone, push or PR failed
	ExitStorage       int32 = 21 // a presigned GET or PUT failed
	ExitConfig        int32 = 30 // incomplete configuration, or MCP servers did not come up
)

// FailureClassForExitCode classifies a container exit code.
//
// ExitStorage is infra rather than config even though it usually means an
// expired presigned bundle: the controller reissues the bundle before it starts
// the next attempt, so this is the one failure the cluster can actually repair
// on its own. Classifying it as config would burn a run whose result the
// entrypoint had already produced.
//
// Codes above 128 are signals — 137 is SIGKILL (OOMKilled, eviction), 143 is
// SIGTERM (drain, preemption) — and all of them mean the platform stopped the
// process rather than the agent finishing badly, so they are infra and
// retriable. An unrecognised code below 128 is the agent's own, and is not
// retried: an unknown deliberate exit is not evidence that a replay would go
// any better.
func FailureClassForExitCode(code int32) FailureClass {
	switch code {
	case ExitSuccess:
		return FailureNone
	case ExitAgentError, ExitAgentTimeout, ExitOutputInvalid:
		return FailureAgent
	case ExitGit:
		return FailureGit
	case ExitStorage:
		return FailureInfra
	case ExitConfig:
		return FailureConfig
	}
	if code > 128 {
		return FailureInfra
	}
	return FailureAgent
}

// PhaseForExitCode maps an exit code to the terminal phase it implies.
// Cancellation is not derivable from the exit code — the pod is killed and
// reports 143 like any other eviction — so the controller overrides this with
// PhaseCancelled when it was the one that asked for the kill.
func PhaseForExitCode(code int32) Phase {
	switch {
	case code == ExitSuccess:
		return PhaseSucceeded
	case code == ExitAgentTimeout:
		return PhaseTimedOut
	default:
		return PhaseFailed
	}
}

// ExitCodes enumerates the codes the entrypoint produces deliberately. Signals
// are not here: they are produced by the platform, start above 128 and are
// classified by range rather than by membership.
var ExitCodes = []int32{
	ExitSuccess, ExitAgentError, ExitAgentTimeout, ExitOutputInvalid,
	ExitGit, ExitStorage, ExitConfig,
}
