package agentrun

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

// The status is the controller's only memory. Everything that must survive a
// restart is here — the attempt, the retry budget, what the backend has already
// accepted — and everything here is an observation or a counter, never a piece
// of the domain: no result text, no logs, no workflow context. A 64 KiB summary
// in etcd would be read on every reconcile of every run, and would make the CR
// a second place where the truth about a run lives.

// applyObservation folds an observation into the status and writes it if
// anything changed. It reports whether a write happened, which is also the
// signal to tell the backend: a status nobody changed is not news.
func (r *Reconciler) applyObservation(ctx context.Context, cr *agentrunv1alpha1.AgentRun, attempt int32, obs Observation) (bool, error) {
	before := cr.Status.DeepCopy()

	cr.Status.Attempt = attempt
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Phase = obs.Phase
	cr.Status.Reason = obs.Reason
	cr.Status.Message = truncate(obs.Message, 1024)
	if obs.PodName != "" {
		cr.Status.PodName = obs.PodName
	}
	if obs.NodeName != "" {
		cr.Status.NodeName = obs.NodeName
	}
	if obs.StartedAt != nil {
		cr.Status.StartedAt = obs.StartedAt
	}
	if obs.FinishedAt != nil {
		cr.Status.FinishedAt = obs.FinishedAt
	}
	if obs.ExitCode != nil {
		cr.Status.ExitCode = obs.ExitCode
	}
	if obs.FailureClass != "" {
		cr.Status.FailureClass = obs.FailureClass
	}

	if obs.Phase.IsTerminal() {
		setCondition(cr, agentrunv1alpha1.ConditionCompleted, metav1.ConditionTrue,
			string(obs.Phase), obs.Reason)
	}

	if equalStatus(before, &cr.Status) {
		return false, nil
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		if apierrors.IsConflict(err) {
			// Someone else wrote first; the next reconcile recomputes from the
			// object rather than from anything remembered here, so a conflict
			// costs a round trip and nothing else.
			return false, err
		}
		return false, fmt.Errorf("update status of %s: %w", cr.Name, err)
	}
	return true, nil
}

// setCondition writes a condition, leaving the transition time alone when
// nothing about it changed.
func setCondition(cr *agentrunv1alpha1.AgentRun, condType string, status metav1.ConditionStatus, reason, message string) {
	if reason == "" {
		reason = condType
	}
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             sanitizeReason(reason),
		Message:            truncate(message, 1024),
		ObservedGeneration: cr.Generation,
	})
}

// sanitizeReason keeps a condition reason inside what the API server accepts:
// a CamelCase token. Kubernetes reasons already are; a message that leaked into
// the field would otherwise fail the write and lose the condition entirely.
func sanitizeReason(reason string) string {
	out := make([]rune, 0, len(reason))
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == ',', r == ':':
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "Unknown"
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return string(out)
}

// observationFor is what the backend is told. It carries pointers and facts,
// never content: a report with the agent's output in it would put a megabyte
// through the control plane on every heartbeat, and the output is already in
// storage where the backend can read it.
func observationFor(cr *agentrunv1alpha1.AgentRun, now time.Time) clusterv1.RunObservation {
	obs := clusterv1.RunObservation{
		RunID:        cr.Spec.RunID,
		Epoch:        cr.Spec.LeaseEpoch,
		Attempt:      cr.Status.Attempt,
		Phase:        cr.Status.Phase,
		Reason:       cr.Status.Reason,
		Message:      truncate(cr.Status.Message, 1024),
		JobName:      cr.Status.JobName,
		PodName:      cr.Status.PodName,
		NodeName:     cr.Status.NodeName,
		ExitCode:     cr.Status.ExitCode,
		FailureClass: cr.Status.FailureClass,
		ObservedAt:   ptr(now.UTC()),
	}
	if obs.Attempt < 1 {
		obs.Attempt = 1
	}
	if cr.Status.StartedAt != nil {
		obs.StartedAt = ptr(cr.Status.StartedAt.Time)
	}
	if cr.Status.FinishedAt != nil {
		obs.FinishedAt = ptr(cr.Status.FinishedAt.Time)
	}
	return obs
}

// reportSettled reports whether the backend has heard everything this run owes
// it: the terminal phase for the current attempt, and the pod's completion if
// there was one. It is the gate on the TTL — a CR deleted before the outcome
// was delivered turns a finished run into an Unknown that needs a human.
func reportSettled(cr *agentrunv1alpha1.AgentRun) bool {
	if !cr.Status.Phase.IsTerminal() {
		return false
	}
	reported := cr.Status.Reported
	if reported.Attempt != cr.Status.Attempt || reported.Phase != cr.Status.Phase {
		return false
	}
	if meta.IsStatusConditionTrue(cr.Status.Conditions, agentrunv1alpha1.ConditionResultReported) {
		return reported.CompletionDelivered
	}
	return true
}

// equalStatus compares the fields the controller writes. Conditions are
// compared by (type, status, reason) rather than wholesale, because their
// timestamps would make every reconcile look like a change and turn the status
// into a write loop against etcd.
func equalStatus(a, b *agentrunv1alpha1.AgentRunStatus) bool {
	if a.Phase != b.Phase || a.Reason != b.Reason || a.Message != b.Message ||
		a.Attempt != b.Attempt || a.ObservedGeneration != b.ObservedGeneration ||
		a.JobName != b.JobName || a.PodName != b.PodName || a.NodeName != b.NodeName ||
		a.FailureClass != b.FailureClass || a.PRURL != b.PRURL {
		return false
	}
	if !equalTime(a.StartedAt, b.StartedAt) || !equalTime(a.FinishedAt, b.FinishedAt) {
		return false
	}
	if !equalInt32(a.ExitCode, b.ExitCode) {
		return false
	}
	if a.Retry.InfraRetries != b.Retry.InfraRetries || !equalTime(a.Retry.NextAttemptAt, b.Retry.NextAttemptAt) {
		return false
	}
	if a.Reported != b.Reported {
		return false
	}
	if (a.Result == nil) != (b.Result == nil) {
		return false
	}
	if (a.Usage == nil) != (b.Usage == nil) {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message {
			return false
		}
	}
	return true
}

func equalTime(a, b *metav1.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(b)
	}
}

func equalInt32(a, b *int32) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// patchStatus applies a mutation to the status with an optimistic patch, which
// is cheaper than a read-modify-write and does not lose a concurrent update to
// a field this call did not touch.
func (r *Reconciler) patchStatus(ctx context.Context, cr *agentrunv1alpha1.AgentRun, mutate func(*agentrunv1alpha1.AgentRun)) error {
	patch := client.MergeFrom(cr.DeepCopy())
	mutate(cr)
	if err := r.Status().Patch(ctx, cr, patch); err != nil {
		return fmt.Errorf("patch status of %s: %w", cr.Name, err)
	}
	return nil
}
