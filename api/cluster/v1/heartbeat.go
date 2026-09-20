package v1

import (
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// HeartbeatRequest does four things in one call: proves the cluster is alive,
// renews every listed lease, reconciles state, and collects commands. They are
// one call because they are one interval — splitting them would multiply the
// request rate by four for no added information.
type HeartbeatRequest struct {
	FreeSlots int32 `json:"freeSlots"`
	// +optional
	CapacitySlots int32 `json:"capacitySlots,omitempty"`

	// ReportComplete true means Runs holds every non-terminal run this cluster
	// owns, so the backend may treat an unmentioned run as lost and react at
	// once instead of waiting out the lease.
	//
	// False means a partial report — the controller has just started and its
	// informer cache is not warm yet. Without the flag, "I do not have this
	// run" and "I have not told you about it yet" are indistinguishable, and
	// the first needs a reaction while the second needs silence.
	ReportComplete bool `json:"reportComplete"`

	Runs []RunObservation `json:"runs"`

	// +optional
	Controller *ControllerHealth `json:"controller,omitempty"`
	// +optional
	Cluster *ClusterFacts `json:"cluster,omitempty"`
}

// RunObservation is what the controller sees in the cluster. It never carries
// result text and never logs: those go to storage, and a report that carried
// them would put a megabyte of agent output through the control plane on every
// heartbeat.
type RunObservation struct {
	RunID   runv1.ULID  `json:"runID"`
	Epoch   int64       `json:"epoch"`
	Attempt int32       `json:"attempt"`
	Phase   runv1.Phase `json:"phase"`

	// Reason is a short Kubernetes-style token: ImagePullBackOff, OOMKilled,
	// DeadlineExceeded.
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	JobName string `json:"jobName,omitempty"`
	// +optional
	PodName string `json:"podName,omitempty"`
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// +optional
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// FailureClass as the controller understands it. It reclassifies only what
	// it observes itself — OOM, eviction, cancellation; the pod owns the rest,
	// because the evidence for why an agent failed exists only inside it.
	// +optional
	FailureClass runv1.FailureClass `json:"failureClass,omitempty"`

	// CompletedPhases is the attempt checkpoint of section 9.3: the entrypoint
	// phases this attempt has got through, as the pod reported them to the
	// controller. It rides on the observation rather than on a channel of its
	// own because it is the same fact about the same attempt, and a second
	// channel would need the same epoch check, the same ordering rule and the
	// same batching.
	//
	// The backend unions it into run_attempts.completed_phases rather than
	// replacing: reports arrive reordered, and a heartbeat carrying an earlier
	// snapshot must not shorten a list the fast path already grew.
	// +optional
	CompletedPhases []runv1.RuntimePhase `json:"completedPhases,omitempty"`

	// ObservedAt is diagnostic only. Ordering is by (attempt, phase rank),
	// never by time: cluster clocks are not synchronised, so a timestamp cannot
	// decide which of two reports is newer.
	// +optional
	ObservedAt *time.Time `json:"observedAt,omitempty"`
}

// ControllerHealth is the controller reporting on itself.
type ControllerHealth struct {
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	UptimeSeconds int64 `json:"uptimeSeconds,omitempty"`
	// BackendUnreachableSeconds is accumulated time spent working blind. It is
	// reported from below because the only side that knows about a disconnect
	// is the side that was unreachable for metric scraping at the time.
	// +optional
	BackendUnreachableSeconds int64 `json:"backendUnreachableSeconds,omitempty"`
	// +optional
	LeaseQueueDepth int32 `json:"leaseQueueDepth,omitempty"`
	// +optional
	ReconcileErrorsTotal int64 `json:"reconcileErrorsTotal,omitempty"`
}

// ClusterFacts is drift in what is true about the cluster — a node upgrade, an
// added CRD version. Sent on change rather than every interval.
type ClusterFacts struct {
	// +optional
	K8sVersion string `json:"k8sVersion,omitempty"`
	// +optional
	NodeCount int32 `json:"nodeCount,omitempty"`
	// +optional
	Runtimes []runv1.AgentType `json:"runtimes,omitempty"`
	// +optional
	CRDVersions []string `json:"crdVersions,omitempty"`
	// QuotaExhausted stops the backend assigning work here. Without it an
	// exhausted ResourceQuota is visible only as a run of Failed runs with
	// "exceeded quota" in the message.
	// +optional
	QuotaExhausted bool `json:"quotaExhausted,omitempty"`
}

// HeartbeatResponse renews, commands and reconciles.
type HeartbeatResponse struct {
	ServerTime time.Time `json:"serverTime"`

	// Leases are the renewed deadlines. A run absent from this list is one the
	// backend no longer regards as the cluster's.
	Leases []LeaseRenewal `json:"leases"`

	Commands []Command `json:"commands"`

	// UnknownRuns are runs the backend considers active here that the
	// controller did not mention under ReportComplete. The controller must
	// check each: if the CR really is gone, it reports a terminal phase or
	// acknowledges the loss, and the backend acts immediately rather than after
	// staleAfter. This is what turns "the agent namespace was recreated" from a
	// batch of manual triage into one heartbeat.
	// +optional
	UnknownRuns []runv1.ULID `json:"unknownRuns,omitempty"`

	// Timings lets the control plane retune intervals without a controller
	// restart.
	// +optional
	Timings *Timings `json:"timings,omitempty"`

	// Observations reports rejected rows only. A response listing every
	// accepted row would be the heartbeat's largest field and say nothing.
	// +optional
	Observations []StatusIngestResult `json:"observations,omitempty"`
}

// LeaseRenewal is one extended deadline.
type LeaseRenewal struct {
	RunID         runv1.ULID `json:"runID"`
	Epoch         int64      `json:"epoch"`
	LeaseDeadline time.Time  `json:"leaseDeadline"`
}

// Command is the only top-down channel in the system. Delivery is idempotent
// and repeats until the observed state reflects it: commands deliberately have
// no acknowledgement, because an acknowledgement would have to be stored and
// expired, while re-sending cancel for an already cancelled run is harmless.
type Command struct {
	Type  CommandType `json:"type"`
	RunID runv1.ULID  `json:"runID"`
	// +optional
	Epoch int64 `json:"epoch,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	IssuedAt *time.Time `json:"issuedAt,omitempty"`
	// GracePeriodSeconds is how long the pod gets to upload its partial result
	// before SIGKILL.
	// +optional
	GracePeriodSeconds int32 `json:"gracePeriodSeconds,omitempty"`
}

// CommandType is what to do. An unknown value is a no-op plus a log line, never
// a panic: the control plane may be newer than the controller, and a new
// command type must not take down every older controller in the fleet.
type CommandType string

const (
	// CommandCancel means stop, drive the run to Cancelled, and still report
	// the result if there is one.
	CommandCancel CommandType = "cancel"
	// CommandAbandon means this work is not yours any more — it was reassigned
	// or deleted. Remove the Job and the CR and report nothing.
	CommandAbandon CommandType = "abandon"
)
