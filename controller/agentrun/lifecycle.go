package agentrun

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/launcher"
)

// DefaultTTL matches the schema's default: a finished run's CR lives a day, so
// that `kubectl get agentruns` is useful the morning after.
const DefaultTTL = 24 * time.Hour

// DefaultHardTTL is the ceiling for a run whose outcome the backend never
// acknowledged. Holding the CR is how the outcome survives an outage; holding
// it forever is how a cluster fills with objects nobody will ever read.
const DefaultHardTTL = 7 * 24 * time.Hour

// abandon carries out the one command that ends in silence. The work belongs to
// another cluster now — reassigned, or deleted outright — and anything this
// controller says about it from here on would be a zombie appending its state
// to somebody else's run.
func (r *Reconciler) abandon(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (ctrl.Result, error) {
	r.Notifier.Forget(cr.Spec.RunID)
	if err := r.deleteJobs(ctx, cr, 0); err != nil {
		return ctrl.Result{}, err
	}
	if cr.DeletionTimestamp == nil {
		if err := r.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete abandoned %s: %w", cr.Name, err)
		}
		r.event(cr, corev1.EventTypeWarning, agentrunv1alpha1.ReasonAbandonRequested,
			"the control plane reassigned this run; the job and the AgentRun are being removed")
		// Re-read so the finalizer is released in this pass: the copy in hand
		// predates the deletion and still looks alive, and waiting for the
		// watch event to say otherwise leaves the object Terminating for no
		// reason.
		if err := r.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return r.finalize(ctx, cr)
}

// applyCancel carries out a cancellation. The Job is deleted with the grace
// period the command asked for, so the entrypoint gets its window to upload
// whatever it has; the result is still reported, because a cancelled run that
// produced half an answer produced half an answer.
//
// The phase only becomes Cancelled if nothing terminal was observed first. A
// pod that exited zero while the command was in flight succeeded, and the
// backend applies the same rule to the report, so neither side can talk the
// other into a different outcome.
func (r *Reconciler) applyCancel(ctx context.Context, cr *agentrunv1alpha1.AgentRun, job *batchv1.Job, pod *corev1.Pod, obs Observation) (Observation, error) {
	if outranksCancellation(obs) {
		return obs, nil
	}
	if job != nil && job.DeletionTimestamp == nil {
		if err := r.deleteJobs(ctx, cr, r.cancelGrace(cr)); err != nil {
			return obs, err
		}
		r.event(cr, corev1.EventTypeNormal, agentrunv1alpha1.ReasonCancelRequested,
			fmt.Sprintf("cancelling: the job is being deleted with a %s grace period", r.cancelGrace(cr)))
	}

	// Wait for the pod to be gone before declaring the run cancelled: until
	// then it may still exit on its own, and that outcome outranks this one.
	if pod != nil && pod.DeletionTimestamp == nil {
		return obs, nil
	}
	if pod != nil && pod.DeletionTimestamp != nil && !r.cancelDeadlinePassed(cr) {
		return obs, nil
	}

	obs.Phase = runv1.PhaseCancelled
	obs.Reason = ReasonCancelled
	if reason := cr.Annotations[AnnotationCancelReason]; reason != "" {
		obs.Message = reason
	}
	obs.FailureClass = runv1.FailureNone
	if obs.FinishedAt == nil {
		obs.FinishedAt = ptr(metav1.NewTime(r.now()))
	}
	return obs, nil
}

// outranksCancellation reports whether an observation describes an outcome the
// run reached on its own.
//
// Only a deliberate exit does. A container killed by a signal exits above 128,
// and during a cancellation that signal is the one this controller asked for —
// reporting it as an infrastructure failure would turn every cancellation into
// a failure, and a run deleted by an operator would come back looking like a
// broken node. The same goes for the Job disappearing: it disappeared because
// the cancellation deleted it.
func outranksCancellation(obs Observation) bool {
	if !obs.Phase.IsTerminal() {
		return false
	}
	return obs.ExitCode != nil && *obs.ExitCode <= 128
}

func (r *Reconciler) cancelGrace(cr *agentrunv1alpha1.AgentRun) time.Duration {
	if g := CancelGrace(cr); g > 0 {
		return g
	}
	return time.Duration(r.Builder.GraceSeconds) * time.Second
}

// cancelDeadlinePassed reports whether the pod has had its whole grace period
// plus slack. Beyond it the kubelet has sent SIGKILL and the pod object is
// merely slow to disappear; waiting longer only delays the report.
func (r *Reconciler) cancelDeadlinePassed(cr *agentrunv1alpha1.AgentRun) bool {
	at, ok := CancelRequestedAt(cr)
	if !ok {
		return true
	}
	return r.now().After(at.Add(r.cancelGrace(cr)).Add(30 * time.Second))
}

// finalize releases the object once the work under it has stopped.
//
// The finalizer is removed unconditionally after a bounded wait. A finalizer
// that can wedge is worse than the leak it prevents: it makes the namespace
// undeletable, and the cure is an operator editing an object by hand in
// production, at which point the controller has cost more than it saved.
func (r *Reconciler) finalize(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cr, agentrunv1alpha1.FinalizerTerminateJob) {
		return ctrl.Result{}, nil
	}
	if cr.DeletionTimestamp == nil {
		return ctrl.Result{}, nil
	}

	deadline := cr.DeletionTimestamp.Add(r.finalizerBudget(cr))
	expired := r.now().After(deadline)

	if !expired {
		if err := r.deleteJobs(ctx, cr, r.cancelGrace(cr)); err != nil {
			return ctrl.Result{}, err
		}
		running, err := r.hasLivePods(ctx, cr)
		if err != nil {
			return ctrl.Result{}, err
		}
		if running {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	} else {
		r.log.Warn("releasing the terminate-job finalizer on a bounded wait",
			"agentRun", cr.Name, "runID", cr.Spec.RunID)
	}

	patch := client.MergeFrom(cr.DeepCopy())
	controllerutil.RemoveFinalizer(cr, agentrunv1alpha1.FinalizerTerminateJob)
	if err := r.Patch(ctx, cr, patch); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("release the finalizer on %s: %w", cr.Name, err)
	}
	return ctrl.Result{}, nil
}

// finalizerBudget is the grace period plus slack for the kubelet to act on it.
func (r *Reconciler) finalizerBudget(cr *agentrunv1alpha1.AgentRun) time.Duration {
	budget := r.cancelGrace(cr)
	if budget <= 0 {
		budget = time.Duration(launcher.DefaultGraceSeconds) * time.Second
	}
	return budget + time.Minute
}

// deleteJobs removes every Job belonging to the run, whatever attempt made it.
// Background propagation so the pods go with them: foreground would make this
// call wait on the very deletion the finalizer is already waiting on.
func (r *Reconciler) deleteJobs(ctx context.Context, cr *agentrunv1alpha1.AgentRun, grace time.Duration) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(cr.Namespace),
		client.MatchingLabels(runSelector(cr))); err != nil {
		return fmt.Errorf("list jobs of %s: %w", cr.Name, err)
	}
	background := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.DeletionTimestamp != nil {
			continue
		}
		opts := []client.DeleteOption{client.PropagationPolicy(background)}
		if grace > 0 {
			opts = append(opts, client.GracePeriodSeconds(int64(grace.Seconds())))
		}
		if err := r.Delete(ctx, job, opts...); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete job %s: %w", job.Name, err)
		}
	}
	return nil
}

func (r *Reconciler) hasLivePods(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(cr.Namespace),
		client.MatchingLabels(runSelector(cr))); err != nil {
		return false, fmt.Errorf("list pods of %s: %w", cr.Name, err)
	}
	for i := range pods.Items {
		switch pods.Items[i].Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		default:
			return true, nil
		}
	}
	return false, nil
}

// reap deletes a finished run's CR once the outcome has been delivered and the
// TTL has passed. Deleting it collects the Job, the pod, the Secret and the
// ConfigMap by owner reference — which is the only cleanup mechanism in the
// design, and the reason the Job has no TTL of its own.
func (r *Reconciler) reap(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (ctrl.Result, error) {
	finished := r.now()
	if cr.Status.FinishedAt != nil {
		finished = cr.Status.FinishedAt.Time
	}

	if hard := finished.Add(r.hardTTL()); r.now().After(hard) {
		// Past the ceiling with the outcome still undelivered. Deleting it
		// loses a report; keeping it accumulates objects on every run of a
		// cluster whose control plane has been gone for a week. The audit
		// record is what makes the choice visible afterwards.
		if !reportSettled(cr) {
			r.event(cr, corev1.EventTypeWarning, "ReportUndelivered",
				fmt.Sprintf("deleting after %s without the control plane accepting the outcome", r.hardTTL()))
			r.log.Error("deleting a run whose outcome was never accepted",
				"runID", cr.Spec.RunID, "phase", cr.Status.Phase, "attempt", cr.Status.Attempt)
		}
		return ctrl.Result{}, r.deleteRun(ctx, cr)
	}

	if !reportSettled(cr) {
		// Nothing to do but wait for the reporter. A minute is frequent enough
		// to catch the moment the backend comes back and rare enough not to
		// matter.
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	deadline := finished.Add(r.ttl(cr))
	if wait := deadline.Sub(r.now()); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, r.deleteRun(ctx, cr)
}

func (r *Reconciler) deleteRun(ctx context.Context, cr *agentrunv1alpha1.AgentRun) error {
	if cr.DeletionTimestamp != nil {
		return nil
	}
	if err := r.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s after its TTL: %w", cr.Name, err)
	}
	return nil
}

func (r *Reconciler) ttl(cr *agentrunv1alpha1.AgentRun) time.Duration {
	if cr.Spec.TTLSecondsAfterFinished != nil {
		return time.Duration(*cr.Spec.TTLSecondsAfterFinished) * time.Second
	}
	return DefaultTTL
}

func (r *Reconciler) hardTTL() time.Duration {
	if r.HardTTL > 0 {
		return r.HardTTL
	}
	return DefaultHardTTL
}

// handleAbandonError turns an abandon verdict from any Cluster API call into
// the recorded command. It reports whether the error was an abandon and has
// been dealt with.
func (r *Reconciler) handleAbandonError(ctx context.Context, cr *agentrunv1alpha1.AgentRun, err error) bool {
	if clusterapi.ActionOf(err) != clusterv1.ActionAbandon {
		return false
	}
	r.log.Info("the control plane says this run is no longer ours",
		"runID", cr.Spec.RunID, "error", err)
	if recErr := Record(ctx, r.Client, cr, clusterv1.Command{
		Type: clusterv1.CommandAbandon, RunID: cr.Spec.RunID,
	}, r.now()); recErr != nil {
		r.log.Error("recording an abandon", "runID", cr.Spec.RunID, "error", recErr)
	}
	return true
}

func runSelector(cr *agentrunv1alpha1.AgentRun) map[string]string {
	return map[string]string{agentrunv1alpha1.LabelRunID: lower(string(cr.Spec.RunID))}
}

// DefaultAckWaitBudget is several times the contract's 60 second ack deadline.
// Being generous is safe — nothing is running yet — and being too strict would
// discard work over one slow round trip to the control plane.
const DefaultAckWaitBudget = 5 * time.Minute

// awaitAck holds a materialised run until the control plane has confirmed the
// lease, and gives up if it never does.
//
// Giving up is the point. If the acknowledgement did not arrive, the backend's
// ack deadline expired, it raised the epoch and handed the work to somebody
// else — possibly to this very cluster, as a new lease that will replace this
// object anyway. Starting the pod regardless is how the same agent ends up
// running twice against the same branch.
func (r *Reconciler) awaitAck(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (ctrl.Result, error) {
	leasedAt, ok := LeasedAt(cr)
	if !ok {
		leasedAt = cr.CreationTimestamp.Time
	}
	if deadline := leasedAt.Add(r.AckWaitBudget); r.now().After(deadline) {
		r.log.Warn("no acknowledgement for this lease; discarding it",
			"runID", cr.Spec.RunID, "epoch", cr.Spec.LeaseEpoch, "since", leasedAt)
		r.event(cr, corev1.EventTypeWarning, "AckMissing",
			"the control plane never confirmed this lease; the run was not started")
		r.Notifier.Forget(cr.Spec.RunID)
		return ctrl.Result{}, r.deleteRun(ctx, cr)
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
