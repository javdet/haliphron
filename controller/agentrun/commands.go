package agentrun

import (
	"context"
	"fmt"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

// Commands arrive from the backend on an ack or a heartbeat and are recorded on
// the AgentRun as annotations rather than kept in the controller's memory.
//
// Section 14 of the CRD contract requires reconciliation to be a pure function
// of the spec and the observed cluster state, with nothing remembered between
// calls. A cancellation held in a map would break that in the way that matters
// most: a controller restarted between receiving the command and acting on it
// would forget it, and the backend — which gets no acknowledgement for a
// command by design — would have no way to tell.
//
// They are annotations rather than spec fields because the spec is immutable
// and because a command is not part of the work: it is something that happened
// to the work afterwards.
const (
	// AnnotationCancelRequested carries the RFC 3339 time the command was
	// received, so the bounded wait before the finalizer is released can be
	// computed from the object alone.
	AnnotationCancelRequested = agentrunv1alpha1.GroupName + "/cancel-requested-at"
	// AnnotationCancelGrace is the pod's shutdown budget from the command,
	// which may be longer than the pod template's: the point of a grace period
	// on a cancellation is to let the entrypoint upload a partial result.
	AnnotationCancelGrace  = agentrunv1alpha1.GroupName + "/cancel-grace-seconds"
	AnnotationCancelReason = agentrunv1alpha1.GroupName + "/cancel-reason"
	// AnnotationAbandoned means the work is no longer this cluster's. The
	// annotation exists for the instant between learning that and the object
	// being gone: a controller that crashes in between must not come back and
	// report on a run that now belongs elsewhere.
	AnnotationAbandoned = agentrunv1alpha1.GroupName + "/abandoned-at"
)

// IsCancelRequested reports whether a cancel command has been recorded.
func IsCancelRequested(cr *agentrunv1alpha1.AgentRun) bool {
	_, ok := cr.Annotations[AnnotationCancelRequested]
	return ok
}

// CancelRequestedAt is when, for the bounded wait on the finalizer.
func CancelRequestedAt(cr *agentrunv1alpha1.AgentRun) (time.Time, bool) {
	raw, ok := cr.Annotations[AnnotationCancelRequested]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// CancelGrace is the shutdown budget the command asked for, or zero.
func CancelGrace(cr *agentrunv1alpha1.AgentRun) time.Duration {
	raw, ok := cr.Annotations[AnnotationCancelGrace]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// IsAbandoned reports whether this run was taken away from the cluster.
func IsAbandoned(cr *agentrunv1alpha1.AgentRun) bool {
	_, ok := cr.Annotations[AnnotationAbandoned]
	return ok
}

// Record writes a command onto the AgentRun. Delivery of commands is idempotent
// by design — they repeat until the observed state reflects them — so recording
// one twice must be a no-op, and it is: the first annotation wins and the
// second write is skipped.
func Record(ctx context.Context, c client.Client, cr *agentrunv1alpha1.AgentRun, cmd clusterv1.Command, now time.Time) error {
	patch := client.MergeFrom(cr.DeepCopy())
	annotations := map[string]string{}

	switch cmd.Type {
	case clusterv1.CommandCancel:
		if IsCancelRequested(cr) || IsAbandoned(cr) {
			return nil
		}
		annotations[AnnotationCancelRequested] = now.UTC().Format(time.RFC3339)
		if cmd.GracePeriodSeconds > 0 {
			annotations[AnnotationCancelGrace] = strconv.Itoa(int(cmd.GracePeriodSeconds))
		}
		if cmd.Reason != "" {
			annotations[AnnotationCancelReason] = cmd.Reason
		}
	case clusterv1.CommandAbandon:
		if IsAbandoned(cr) {
			return nil
		}
		annotations[AnnotationAbandoned] = now.UTC().Format(time.RFC3339)
	default:
		// An unknown command type is a no-op and a log line, never a failure:
		// the control plane may be newer than the controller, and a new command
		// must not take down every older controller in the fleet.
		return nil
	}

	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		cr.Annotations[k] = v
	}
	if err := c.Patch(ctx, cr, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("record command %s on %s: %w", cmd.Type, cr.Name, err)
	}
	return nil
}

// AnnotationAckedAt is when the control plane accepted the acknowledgement for
// this epoch.
//
// It gates the Job. The contract's ack deadline means "the work is guaranteed
// not to have started", and the backend relies on that to reassign an
// unacknowledged lease immediately and safely. A controller that created the
// pod as soon as the CR existed would break the guarantee in exactly the case
// that matters: the ack failed to arrive, the backend reassigned the run, and
// two clusters are now running the same agent against the same branch.
const AnnotationAckedAt = agentrunv1alpha1.GroupName + "/acknowledged-at"

// IsAcknowledged reports whether the backend has confirmed this epoch.
func IsAcknowledged(cr *agentrunv1alpha1.AgentRun) bool {
	_, ok := cr.Annotations[AnnotationAckedAt]
	return ok
}

// MarkAcknowledged records the confirmation, releasing the Job.
func MarkAcknowledged(ctx context.Context, c client.Client, cr *agentrunv1alpha1.AgentRun, now time.Time) error {
	if IsAcknowledged(cr) {
		return nil
	}
	patch := client.MergeFrom(cr.DeepCopy())
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[AnnotationAckedAt] = now.UTC().Format(time.RFC3339)
	if err := c.Patch(ctx, cr, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("mark %s acknowledged: %w", cr.Name, err)
	}
	return nil
}

// LeasedAt is when the lease arrived, used to bound the wait for an ack that
// may never come.
func LeasedAt(cr *agentrunv1alpha1.AgentRun) (time.Time, bool) {
	raw, ok := cr.Annotations[agentrunv1alpha1.AnnotationLeasedAt]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
