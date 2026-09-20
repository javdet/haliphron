package report

import (
	"context"
	"errors"
	"io"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/spool"
)

// Forwarding the spool: the second half of relay mode.
//
// The first half is the callback handler, which writes a pod's object to this
// controller's volume and acknowledges it. From that moment the pod is free to
// exit and the controller owes the object to the backend — for as long as that
// takes. A control plane that is down for an hour costs an hour of forwarding
// latency and nothing else, which is the same bargain the completion path
// already makes and the reason P4 is worth having.
//
// What makes it at-least-once rather than at-most-once is the order: the
// spooled copy is deleted only after the backend has acknowledged. A controller
// that deleted first would throw away the only copy of a result a pod was told
// was safe.

// Artifact implements callback.Sink. It spools one object and returns what
// landed, so the handler can put a complete reference in its acknowledgement.
func (r *Reporter) Artifact(entry spool.Entry, body io.Reader, budget int64) (spool.Entry, error) {
	if r.Spool == nil {
		// A controller in relay mode with no spool is a misconfiguration that
		// would otherwise surface as every run finishing without a result.
		return spool.Entry{}, errors.New("report: this controller has no artifact spool")
	}
	return r.Spool.Write(entry, body, budget)
}

// Forwarded implements callback.Sink: it wakes the delivery loop.
func (r *Reporter) Forwarded() { r.nudge() }

// Spent reports what a run is holding in the spool, for the acknowledgement's
// remaining-budget field.
func (r *Reporter) Spent(id runv1.ULID) int64 {
	if r.Spool == nil {
		return 0
	}
	return r.Spool.Spent(id)
}

// flushArtifacts forwards what is spooled, oldest first.
//
// Before the completions, and the order is deliberate. A completion names refs
// the backend will resolve; forwarding it first leaves a window in which the
// control plane holds a report pointing at objects it does not have, and the
// CompletedWithoutResult recovery path would read that window as a lost result.
// Artifacts first means a report never arrives ahead of what it describes.
func (r *Reporter) flushArtifacts(ctx context.Context) error {
	if r.Spool == nil {
		return nil
	}
	pending, err := r.Spool.Pending()
	if err != nil {
		return err
	}

	var firstErr error
	for _, entry := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch err := r.forwardOne(ctx, entry); {
		case err == nil:
			if err := r.Spool.Done(entry); err != nil {
				r.Log.Error("could not drop a forwarded artifact",
					"runID", entry.RunID, "key", entry.Key, "error", err)
			}
		case clusterapi.ActionOf(err) == clusterv1.ActionAbandon:
			// The run was reassigned. Its artifacts belong to whoever holds it
			// now, and forwarding ours would overwrite theirs under the same
			// key. Dropped rather than retried forever.
			r.Log.Warn("discarding the spool of a run this cluster no longer holds",
				"runID", entry.RunID)
			if err := r.Spool.Forget(entry.RunID); err != nil {
				r.Log.Error("could not discard a spool", "runID", entry.RunID, "error", err)
			}
		case clusterapi.ActionOf(err) == clusterv1.ActionFatal:
			// The backend will not take this object however many times it is
			// offered — a key it does not admit, a digest that will not match
			// because the spooled bytes are what they are. Keeping it would
			// block every later object for the same run behind it.
			r.Log.Error("the control plane refused an artifact outright; dropping it",
				"runID", entry.RunID, "key", entry.Key, "error", err)
			if err := r.Spool.Done(entry); err != nil {
				r.Log.Error("could not drop a refused artifact",
					"runID", entry.RunID, "key", entry.Key, "error", err)
			}
		default:
			r.markUnreachable()
			if firstErr == nil {
				firstErr = err
			}
			// Left in the spool. The next pass tries again, and the backoff in
			// the ingest loop keeps that from being a busy loop.
		}
	}

	if firstErr == nil {
		r.markReachable()
		r.markRelayed(ctx, pending)
	}
	return firstErr
}

func (r *Reporter) forwardOne(ctx context.Context, entry spool.Entry) error {
	body, err := r.Spool.Open(entry)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	_, err = r.API.IngestArtifact(ctx, clusterv1.ArtifactIngestRequest{
		ClusterID:   r.ClusterID,
		RunID:       entry.RunID,
		Epoch:       entry.Epoch,
		Attempt:     entry.Attempt,
		Key:         entry.Key,
		ContentType: entry.ContentType,
		SHA256:      entry.SHA256,
		SizeBytes:   entry.SizeBytes,
	}, body)
	return err
}

// markRelayed flips ConditionArtifactsRelayed to True on the runs whose spools
// are now empty.
//
// It is what releases the TTL reaper. The condition is False from the moment
// the first object is spooled, and an AgentRun with it False is not collected —
// so a run cannot have its CR removed while this controller is still holding
// artifacts that belong to it.
func (r *Reporter) markRelayed(ctx context.Context, forwarded []spool.Entry) {
	seen := map[runv1.ULID]bool{}
	for _, entry := range forwarded {
		if seen[entry.RunID] {
			continue
		}
		seen[entry.RunID] = true
		if r.Spool.Spent(entry.RunID) > 0 {
			continue
		}
		r.patchRun(ctx, entry.RunID, func(cr *agentrunv1alpha1.AgentRun) bool {
			if c := meta.FindStatusCondition(cr.Status.Conditions,
				agentrunv1alpha1.ConditionArtifactsRelayed); c != nil && c.Status == metav1.ConditionTrue {
				return false
			}
			meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
				Type:               agentrunv1alpha1.ConditionArtifactsRelayed,
				Status:             metav1.ConditionTrue,
				Reason:             agentrunv1alpha1.ReasonArtifactsForwarded,
				Message:            "every spooled artifact has been accepted by the control plane",
				ObservedGeneration: cr.Generation,
			})
			return true
		})
	}
}

// ForgetArtifacts drops a run's spool. Called on abandon and when a CR is
// collected: without it the controller keeps the artifacts of every run it ever
// lost, and a volume that only grows is a controller that eventually stops.
func (r *Reporter) ForgetArtifacts(id runv1.ULID) {
	if r.Spool == nil {
		return
	}
	if err := r.Spool.Forget(id); err != nil {
		r.Log.Error("could not discard a spool", "runID", id, "error", err)
	}
}
