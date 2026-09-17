package v1

import (
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Wire limits. Exceeding one is a 413 rather than a truncation: a silently
// dropped tail of a batch is a report that never arrives and is never missed.
const (
	MaxRequestBytes  = 1 << 20 // 1 MiB
	MaxStatusReports = 100     // one ingest batch
	MaxHeartbeatRuns = 500     // the cluster capacity ceiling, with room
	MaxSummaryBytes  = 64 << 10
)

// StatusIngestRequest is the low-latency path for a phase change. It duplicates
// what the heartbeat would have carried an interval later and obeys exactly the
// same rules — the two paths share one implementation on the backend side, or
// the periodic one starts rolling back state the fast one delivered.
//
// Batched because a reconcile burst produces dozens of events per second, and N
// requests for N events is both extra traffic and N chances of a partial
// failure.
type StatusIngestRequest struct {
	ClusterID runv1.ULID       `json:"clusterID"`
	Reports   []RunObservation `json:"reports"`
}

// StatusIngestResponse answers per row. Partial success is the normal case.
type StatusIngestResponse struct {
	Results []StatusIngestResult `json:"results"`
}

// StatusIngestResult is the outcome for one observation. A row with Accepted
// false is not resent unless its Action says retry.
type StatusIngestResult struct {
	RunID    runv1.ULID `json:"runID"`
	Accepted bool       `json:"accepted"`
	// +optional
	AppliedStatus string `json:"appliedStatus,omitempty"`
	// +optional
	Code ProblemCode `json:"code,omitempty"`
	// +optional
	Action Action `json:"action,omitempty"`
	// +optional
	CurrentEpoch int64 `json:"currentEpoch,omitempty"`
}

// CompletionIngestRequest wraps the pod's webhook in an envelope carrying the
// cluster identity and the epoch. The controller forwards the report unchanged;
// it does not get to edit what the pod said.
type CompletionIngestRequest struct {
	ClusterID runv1.ULID `json:"clusterID"`
	RunID     runv1.ULID `json:"runID"`
	Epoch     int64      `json:"epoch"`
	Attempt   int32      `json:"attempt"`
	// ReceivedAt is when the controller accepted the webhook from the pod.
	// +optional
	ReceivedAt *time.Time `json:"receivedAt,omitempty"`

	Completion runv1.CompletionReport `json:"completion"`
}

// CompletionIngestResponse confirms the report.
//
// The call is an optimisation, not a correctness condition: the result is in
// storage before the callback is made (ADR 15), so a completion that never
// arrives costs a read of runs/{runID}/ and not the result.
type CompletionIngestResponse struct {
	RunID    runv1.ULID `json:"runID"`
	Accepted bool       `json:"accepted"`
	// Duplicate means a report for this (runID, attempt) was already applied
	// and this one changed nothing. The cost is charged once.
	// +optional
	Duplicate bool `json:"duplicate,omitempty"`
	// +optional
	AppliedStatus string `json:"appliedStatus,omitempty"`
	// Commands carries anything incidental, e.g. an abandon for a run that has
	// since been reassigned.
	// +optional
	Commands []Command `json:"commands,omitempty"`
}

// Backend run statuses. The controller never names these — it reports the CR
// phases from run/v1 — but it reads them out of AckResponse.Status and
// StatusIngestResult.AppliedStatus, so the spellings belong in the shared
// package rather than in the backend alone.
const (
	// StatusQueued is admitted, not yet assigned.
	StatusQueued = "Queued"
	// StatusLeased is handed to a cluster, not yet acknowledged.
	StatusLeased = "Leased"
	// StatusDispatched is acknowledged: the CR exists in the cluster.
	StatusDispatched = "Dispatched"
	StatusStarting   = "Starting"
	StatusRunning    = "Running"
	StatusSucceeded  = "Succeeded"
	StatusFailed     = "Failed"
	StatusTimedOut   = "TimedOut"
	StatusCancelled  = "Cancelled"
	// StatusUnknown is set only by the backend, on lease expiry or from
	// unknownRuns. It means the work may or may not be running and no one can
	// say which — a state that needs a human, and is therefore worth naming
	// rather than guessing at with Failed.
	StatusUnknown = "Unknown"
	// StatusCompletedWithoutResult is a terminal phase observed with no
	// completion report. The contents are recoverable from storage, so this is
	// a reconciliation task and not a failure.
	StatusCompletedWithoutResult = "CompletedWithoutResult"
)

// StatusForPhase maps an observed CR phase to the backend status it implies.
// The backend-only statuses have no phase and are never produced here.
func StatusForPhase(p runv1.Phase) string {
	switch p {
	case runv1.PhasePending:
		return StatusDispatched
	case runv1.PhaseStarting:
		return StatusStarting
	case runv1.PhaseRunning:
		return StatusRunning
	case runv1.PhaseSucceeded:
		return StatusSucceeded
	case runv1.PhaseFailed:
		return StatusFailed
	case runv1.PhaseTimedOut:
		return StatusTimedOut
	case runv1.PhaseCancelled:
		return StatusCancelled
	default:
		return ""
	}
}
