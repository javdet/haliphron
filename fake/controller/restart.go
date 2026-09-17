package controller

import (
	"context"
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Restarting, and the status that makes it possible.
//
// The controller keeps nothing durable of its own: the AgentRun is its memory.
// That is what lets it be killed at any moment — and the two properties worth
// testing are exactly the ones that break when something is held only in RAM:
// an ack repeated under the same epoch, and a retry budget that does not reset
// into a fresh set of attempts for a run that has been failing all afternoon.

// updateStatus writes what the controller observed onto the CR. Called under
// the lock, after every phase change.
func (c *Controller) updateStatus(r *runState) {
	name := agentrunv1alpha1.ObjectName(r.runID)
	cr, ok := c.objects.getAgentRun(name)
	if !ok {
		return
	}
	cr.Status.Phase = r.phase
	cr.Status.Attempt = r.attempt
	cr.Status.JobName = agentrunv1alpha1.JobName(r.runID, r.attempt)
	cr.Status.Retry.InfraRetries = r.infraRetries
	// What the backend accepted, not what was observed. Conflating the two is
	// how a restart loses an outcome that never got through.
	cr.Status.Reported.Phase = r.reportedPhase
	cr.Status.Reported.Attempt = r.attempt
	cr.Status.Reported.CompletionDelivered = r.completionDelivered

	setCondition(cr, agentrunv1alpha1.ConditionValidated, metav1.ConditionTrue,
		agentrunv1alpha1.ReasonMaterialsReady)
	setCondition(cr, agentrunv1alpha1.ConditionJobCreated, metav1.ConditionTrue,
		agentrunv1alpha1.ReasonJobCreated)
	if r.terminal != "" {
		// Reason carries which terminal phase, so a client tells Succeeded from
		// Cancelled without parsing the phase field.
		setCondition(cr, agentrunv1alpha1.ConditionCompleted, metav1.ConditionTrue, string(r.terminal))
		reported := metav1.ConditionFalse
		reason := agentrunv1alpha1.ReasonCallbackNotReceived
		if r.completionDelivered {
			reported, reason = metav1.ConditionTrue, agentrunv1alpha1.ReasonCallbackReceived
		}
		// The absence of this condition on a finished run is precisely what the
		// backend sees as CompletedWithoutResult.
		setCondition(cr, agentrunv1alpha1.ConditionResultReported, reported, reason)
	}
	c.objects.replaceStatus(cr)
}

func setCondition(cr *agentrunv1alpha1.AgentRun, kind string, status metav1.ConditionStatus, reason string) {
	for i := range cr.Status.Conditions {
		if cr.Status.Conditions[i].Type == kind {
			cr.Status.Conditions[i].Status = status
			cr.Status.Conditions[i].Reason = reason
			return
		}
	}
	cr.Status.Conditions = append(cr.Status.Conditions, metav1.Condition{
		Type: kind, Status: status, Reason: reason,
		LastTransitionTime: metav1.Now(),
	})
}

// Restart discards everything held in memory and rebuilds from the cluster, the
// way a controller comes up after being killed. Nothing survives but the
// objects: the AgentRun carries the spec, the epoch, the attempt and the retry
// budget, and the per-run Secret carries the materials.
//
// Note what does not come back: runs whose CR was deleted. An abandoned run is
// gone for good, which is the correct reading of "report nothing further".
func (c *Controller) Restart(ctx context.Context) error {
	c.mu.Lock()
	c.runs = map[runv1.ULID]*runState{}
	snapshot := c.objects.snapshot()
	secrets := map[string]map[string][]byte{}
	for _, s := range snapshot.Secrets {
		secrets[s.Name] = s.Data
	}

	for i := range snapshot.AgentRuns {
		cr := snapshot.AgentRuns[i]
		id := cr.Spec.RunID
		state := &runState{
			runID:   id,
			epoch:   cr.Spec.LeaseEpoch,
			attempt: max32(cr.Status.Attempt, 1),
			phase:   cr.Status.Phase,
			// The budget is read back, not reset. A run that has burned two
			// attempts must not get three more because the controller was
			// rescheduled.
			infraRetries:        cr.Status.Retry.InfraRetries,
			acked:               hasCondition(cr.Status.Conditions, agentrunv1alpha1.ConditionJobCreated),
			specHash:            cr.Annotations[agentrunv1alpha1.AnnotationSpecHash],
			completionDelivered: cr.Status.Reported.CompletionDelivered,
		}
		if cr.Status.Phase.IsTerminal() {
			state.terminal = cr.Status.Phase
		}
		// status.reported is the delivery bookkeeping. Without it a restarted
		// controller either re-sends everything forever or drops the guard
		// that keeps the CR alive until the backend has heard the outcome.
		state.reportedPhase = cr.Status.Reported.Phase
		// Rebuild as much of the lease as the cluster still holds. The spec is
		// on the CR; the materials are in the Secret, which is why the bundle
		// was put there rather than into the spec.
		state.lease = clusterv1.Lease{
			RunID: id, Epoch: state.epoch, Attempt: state.attempt,
			Spec: cr.Spec.RenderedRunSpec,
		}
		if data, ok := secrets[cr.Spec.Materials.SecretName]; ok {
			state.lease.Secrets = map[string]string{}
			for k, v := range data {
				if k == runv1.SecretKeyPresigned {
					var bundle clusterv1.ArtifactBundle
					if err := json.Unmarshal(v, &bundle); err == nil {
						state.lease.Artifacts = bundle
					}
					continue
				}
				if k == runv1.SecretKeyCallbackToken {
					state.callbackToken = string(v)
					continue
				}
				state.lease.Secrets[k] = string(v)
			}
		}
		for _, cm := range snapshot.ConfigMaps {
			if cm.Name == cr.Spec.Materials.ConfigMapName {
				state.lease.RoleConfig = cm.Data
			}
		}
		c.runs[id] = state
	}
	c.noteLocked("restarted: recovered %d run(s) from the cluster", len(c.runs))
	c.mu.Unlock()

	// The ack is repeated for anything that was materialised but never
	// acknowledged. Under the same epoch: the backend treats it as idempotent,
	// and without the repeat the ack deadline expires and the work is reissued
	// even though it is sitting right here.
	return c.ackPending(ctx)
}

func hasCondition(conds []metav1.Condition, kind string) bool {
	for _, cond := range conds {
		if cond.Type == kind && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// ObserveFailure reports a failure the controller saw itself, with no exit code
// because the container never produced one: ImagePullBackOff past the startup
// deadline, an unschedulable pod, a Job deleted from under it, an eviction.
//
// These are the only failures the controller may classify. Everything else is
// the pod's to classify, because the evidence for why an agent failed exists
// once, inside it, at the moment of failure.
func (c *Controller) ObserveFailure(ctx context.Context, id runv1.ULID, reason string, class runv1.FailureClass) error {
	c.mu.Lock()
	r, ok := c.runs[id]
	if !ok || c.abandoned[id] {
		c.mu.Unlock()
		return nil
	}
	budget := int32(3)
	if r.lease.Spec.Retry != nil && r.lease.Spec.Retry.MaxInfraRetries != nil {
		budget = *r.lease.Spec.Retry.MaxInfraRetries
	}
	retriable := class.Retriable() && r.infraRetries < budget && !r.cancelled
	c.mu.Unlock()

	if retriable {
		return c.retryLocally(ctx, id, Outcome{Reason: reason}, class)
	}
	return c.reportPhase(ctx, id, runv1.PhaseFailed, func(o *clusterv1.RunObservation) {
		o.FailureClass = class
		o.Reason = reason
	})
}

// ReleaseFinalizer removes the finalizer and lets the CR go. The real
// controller does this after a bounded wait whether or not the pod has actually
// stopped: a finalizer that can wedge makes the namespace undeletable, and the
// cure for that is editing objects by hand in production, which is worse than
// the leak it was preventing.
func (c *Controller) ReleaseFinalizer(id runv1.ULID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := agentrunv1alpha1.ObjectName(id)
	cr, ok := c.objects.getAgentRun(name)
	if !ok {
		return
	}
	var kept []string
	for _, f := range cr.Finalizers {
		if f != agentrunv1alpha1.FinalizerTerminateJob {
			kept = append(kept, f)
		}
	}
	cr.Finalizers = kept
	c.objects.replaceStatus(cr)
	c.noteLocked("finalizer released on %s after a bounded wait", name)
}

// Cleanup deletes a finished run's objects, the TTL reaper's job. It refuses
// while the outcome has not reached the backend: dropping the CR then would
// lose the only remaining record that the controller holds.
func (c *Controller) Cleanup(id runv1.ULID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return false
	}
	if r.terminal == "" || !r.completionDelivered {
		c.noteLocked("holding %s past its TTL: the outcome has not been delivered", id)
		return false
	}
	c.objects.deleteAgentRun(agentrunv1alpha1.ObjectName(id))
	delete(c.runs, id)
	return true
}

// Held lists the runs this cluster currently owns.
func (c *Controller) Held() []runv1.ULID {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]runv1.ULID, 0, len(c.runs))
	for id := range c.runs {
		out = append(out, id)
	}
	sortULIDs(out)
	return out
}

func sortULIDs(ids []runv1.ULID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && strings.Compare(string(ids[j]), string(ids[j-1])) < 0; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}
