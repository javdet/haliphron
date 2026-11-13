// Package state is the controller's durable memory: one SQLite row per run,
// the equivalent of an AgentRun together with its Secret and its ConfigMap.
//
// The rules are the Kubernetes controller's, carried over unchanged.
// Reconciliation remembers nothing: every decision is a function of the row
// and of what Docker reports at that moment. Commands from outside arrive as
// column writes, never as fields in memory. Every write is a compare-and-swap
// on a version, the equivalent of resourceVersion, so the reconciler, the
// callback server and the reporter can each update a row without losing what
// the others wrote.
package state

import (
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Run is one row. There is at most one row per run: a new epoch replaces the
// row once the previous epoch's containers are gone, which is the same rule
// as "a new epoch is a new AgentRun, after the old one is deleted".
type Run struct {
	RunID runv1.ULID
	Epoch int64
	// LeaseAttempt is the attempt the lease was issued at: the first attempt
	// this controller starts. Every later one is its own decision, in Status.
	LeaseAttempt int32

	Spec runv1.RenderedRunSpec
	// SpecRawSHA256 is the digest of the spec as it arrived on the wire,
	// before decoding. It is diagnostic: the pruning guard itself runs at
	// materialisation and refuses the lease.
	SpecRawSHA256 string

	Status Status

	LeasedAt time.Time
	AckedAt  *time.Time

	CancelRequestedAt  *time.Time
	CancelGraceSeconds int32
	CancelReason       string
	AbandonedAt        *time.Time

	// CallbackToken is minted here and is what stops one agent container
	// posting a completion for another run.
	CallbackToken string
	Materials     Materials

	Version    int64
	CreatedAt  time.Time
	TerminalAt *time.Time
}

// Materials is what Kubernetes keeps in the per-run Secret and ConfigMap. It
// sits in the database in clear text, protected by the file's mode (decision
// 7 of the plan): the same as Secrets in etcd without encryption at rest.
type Materials struct {
	Prompt     string                   `json:"prompt"`
	Secrets    map[string]string        `json:"secrets,omitempty"`
	RoleConfig map[string]string        `json:"roleConfig,omitempty"`
	Artifacts  clusterv1.ArtifactBundle `json:"artifacts"`
}

// Status is the AgentRunStatus shape, less what only Kubernetes has.
type Status struct {
	Phase        runv1.Phase        `json:"phase,omitempty"`
	Reason       string             `json:"reason,omitempty"`
	Message      string             `json:"message,omitempty"`
	Attempt      int32              `json:"attempt,omitempty"`
	StartedAt    *time.Time         `json:"startedAt,omitempty"`
	FinishedAt   *time.Time         `json:"finishedAt,omitempty"`
	ExitCode     *int32             `json:"exitCode,omitempty"`
	FailureClass runv1.FailureClass `json:"failureClass,omitempty"`

	// ContainerName is recorded when the attempt's container is created. A
	// recorded container that is gone, on a run that has not finished, is
	// somebody running docker rm: ContainerDisappeared, class infra.
	ContainerName string `json:"containerName,omitempty"`
	ContainerID   string `json:"containerID,omitempty"`
	// StartupBeganAt anchors the startup deadline: the moment this attempt
	// first tried to pull or create. Persisted, so a controller restart in
	// the middle of a slow pull does not hand the attempt a fresh budget.
	StartupBeganAt *time.Time `json:"startupBeganAt,omitempty"`
	// CleanedAttempt is the last attempt whose container, volumes and secret
	// files were removed after its terminal phase was recorded. It keeps the
	// removal to once per attempt instead of once per resync.
	CleanedAttempt int32 `json:"cleanedAttempt,omitempty"`
	// PullError is the last image pull failure of this attempt.
	PullError string `json:"pullError,omitempty"`
	// Stop is every stop the controller issued for this attempt, recorded
	// before it was issued. It is how a container killed by a signal is told
	// apart: one the controller stopped, and one a daemon restart or a host
	// reboot stopped.
	Stop *StopRecord `json:"stop,omitempty"`

	Retry           Retry                `json:"retry"`
	CompletedPhases []runv1.RuntimePhase `json:"completedPhases,omitempty"`

	Result         *ResultRefs  `json:"result,omitempty"`
	ResultReported bool         `json:"resultReported,omitempty"`
	PRURL          string       `json:"prURL,omitempty"`
	Usage          *runv1.Usage `json:"usage,omitempty"`
	// ArtifactsRelayed is nil while nothing was spooled for the run, false
	// from the first spooled object, true once the spool is empty again. The
	// reaper leaves a run alone while it is false.
	ArtifactsRelayed *bool `json:"artifactsRelayed,omitempty"`

	Reported Reported `json:"reported"`
}

// Stop reasons.
const (
	StopCancel   = "cancel"
	StopDeadline = "deadline"
	StopAbandon  = "abandon"
)

// StopRecord is a stop the controller asked for.
type StopRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// Grace is how long SIGTERM gets before SIGKILL.
	GraceSeconds int64 `json:"graceSeconds"`
	// KilledAt is when SIGKILL was sent, if it was.
	KilledAt *time.Time `json:"killedAt,omitempty"`
}

// Retry is the local retry budget.
type Retry struct {
	InfraRetries  int32      `json:"infraRetries,omitempty"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
}

// ResultRefs are the pointers the completion carried.
type ResultRefs struct {
	Result     *runv1.ObjectRef `json:"result,omitempty"`
	Output     *runv1.ObjectRef `json:"output,omitempty"`
	Log        *runv1.ObjectRef `json:"log,omitempty"`
	Completion *runv1.ObjectRef `json:"completion,omitempty"`
	Relayed    bool             `json:"relayed,omitempty"`
}

// Reported is what the backend has accepted.
type Reported struct {
	Phase               runv1.Phase `json:"phase,omitempty"`
	Attempt             int32       `json:"attempt,omitempty"`
	CompletionDelivered bool        `json:"completionDelivered,omitempty"`
	LastDeliveredAt     *time.Time  `json:"lastDeliveredAt,omitempty"`
}

// IsAcknowledged reports whether the backend confirmed this epoch.
func (r *Run) IsAcknowledged() bool { return r.AckedAt != nil }

// IsCancelRequested reports whether a cancel command was recorded.
func (r *Run) IsCancelRequested() bool { return r.CancelRequestedAt != nil }

// IsAbandoned reports whether the run was taken away from this host.
func (r *Run) IsAbandoned() bool { return r.AbandonedAt != nil }

// CurrentAttempt is the attempt the controller is working on.
func (r *Run) CurrentAttempt() int32 {
	if r.Status.Attempt >= 1 {
		return r.Status.Attempt
	}
	if r.LeaseAttempt >= 1 {
		return r.LeaseAttempt
	}
	return 1
}

// ArtifactMode is the run's mode, with an empty value read as relay.
func (r *Run) ArtifactMode() runv1.ArtifactMode {
	if r.Materials.Artifacts.Mode == runv1.ArtifactModeObjectStore {
		return runv1.ArtifactModeObjectStore
	}
	return runv1.ArtifactModeRelay
}
