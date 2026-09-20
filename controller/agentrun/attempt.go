package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/launcher"
)

// DefaultMaxInfraRetries matches the schema's default. It is repeated here for
// the case where the field was pruned or the object predates the default.
const DefaultMaxInfraRetries int32 = 3

// retryBase is the first pause between attempts. It doubles with jitter: an
// ImagePullBackOff on a broken node would otherwise become a tight
// create-and-fail loop against the API server, which is how one bad node takes
// out the cluster's control plane rather than one run.
const retryBase = 15 * time.Second

// ensureJob creates the Job for the current attempt if it is not there yet.
//
// The order matters in one specific way: the Secret must exist first. The
// lease's materials and the CR are written by the lease loop, and a Job created
// before the Secret produces a pod stuck in CreateContainerConfigError — which
// the phase table then correctly, and uselessly, reports as an infrastructure
// failure of the controller's own making.
func (r *Reconciler) ensureJob(ctx context.Context, cr *agentrunv1alpha1.AgentRun, attempt int32) (ctrl.Result, error) {
	if at := cr.Status.Retry.NextAttemptAt; at != nil {
		if wait := at.Time.Sub(r.now()); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{
		Namespace: cr.Namespace, Name: cr.Spec.Materials.SecretName,
	}, &secret)
	if apierrors.IsNotFound(err) {
		// Materialisation has not finished, or somebody deleted the Secret.
		// Either way there is nothing to start yet.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get secret %s: %w", cr.Spec.Materials.SecretName, err)
	}

	if err := r.ensureFreshBundle(ctx, cr, &secret, attempt); err != nil {
		return ctrl.Result{}, err
	}

	// The checkpoint any earlier attempt of this run reported. The controller's
	// own copy, not the backend's: this is the path that has to work while the
	// control plane is unreachable (P4), and the backend's copy is the one that
	// survives losing the CR.
	job, err := r.Builder.Job(cr, attempt, cr.Status.CompletedPhases)
	if err != nil {
		// The spec was admitted and cannot be built: nothing a retry fixes.
		r.event(cr, corev1.EventTypeWarning, agentrunv1alpha1.ReasonJobCreateFailed, err.Error())
		return ctrl.Result{}, r.failValidation(ctx, cr, attempt, err)
	}

	if err := r.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			r.event(cr, corev1.EventTypeWarning, agentrunv1alpha1.ReasonJobCreateFailed, err.Error())
			return ctrl.Result{}, fmt.Errorf("create job %s: %w", job.Name, err)
		}
		// The name is derived from the run and the attempt, so a previous
		// epoch's Job of the same attempt can still be in the cluster, on its
		// way out. Claiming it would mean watching an object that is about to
		// disappear and reporting its removal as this attempt's failure.
		existing, getErr := r.jobFor(ctx, cr, attempt)
		if getErr != nil {
			return ctrl.Result{}, getErr
		}
		if existing == nil || existing.DeletionTimestamp != nil || !ownedBy(existing, cr) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if err := r.patchStatus(ctx, cr, func(cr *agentrunv1alpha1.AgentRun) {
		cr.Status.JobName = job.Name
		cr.Status.Attempt = attempt
		cr.Status.Retry.NextAttemptAt = nil
		setCondition(cr, agentrunv1alpha1.ConditionValidated, metav1.ConditionTrue,
			agentrunv1alpha1.ReasonMaterialsReady, "materials are in place and the spec builds a pod")
		setCondition(cr, agentrunv1alpha1.ConditionJobCreated, metav1.ConditionTrue,
			agentrunv1alpha1.ReasonJobCreated, job.Name)
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(cr, corev1.EventTypeNormal, agentrunv1alpha1.ReasonJobCreated,
		fmt.Sprintf("attempt %d started as %s", attempt, job.Name))
	return ctrl.Result{}, nil
}

// ensureFreshBundle reissues the presigned bundle when it would expire before
// the run could finish. Object-store mode only.
//
// This is the failure that looks like nothing else: every link is valid when
// the Job is created, the agent works for fifty minutes, and the upload of the
// result fails with 403 on a signature that expired ten minutes ago. The work
// succeeded and the evidence is gone. One comparison before the pod starts
// removes the whole class — and choosing relay mode removes it too, which is
// one of the quieter reasons the default is what it is.
func (r *Reconciler) ensureFreshBundle(ctx context.Context, cr *agentrunv1alpha1.AgentRun, secret *corev1.Secret, attempt int32) error {
	if r.Bundles == nil {
		return nil
	}
	// Relay mode has no signatures and nothing to expire: the pod posts to this
	// controller's Service, which does not stop working after two hours. The
	// whole of this function is object-store mode.
	raw, ok := secret.Data[runv1.SecretKeyPresigned]
	if !ok {
		return nil
	}
	var bundle clusterv1.ArtifactBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return fmt.Errorf("decode the presigned bundle of %s: %w", cr.Name, err)
	}

	need := r.now().Add(time.Duration(cr.Spec.Runtime.TimeoutSeconds) * time.Second).Add(r.bundleSlack())
	if bundle.ExpiresAt.After(need) {
		return nil
	}

	fresh, err := r.Bundles.Artifacts(ctx, cr.Spec.RunID, clusterv1.ArtifactBundleRequest{
		Epoch: cr.Spec.LeaseEpoch, Attempt: attempt,
	})
	if err != nil {
		if r.handleAbandonError(ctx, cr, err) {
			return nil
		}
		// Starting with links that expire too early is still better than not
		// starting: the entrypoint uploads the result before the git phases, so
		// most of the value is durable early, and the next attempt gets a fresh
		// bundle.
		r.log.Warn("could not reissue the presigned bundle; starting with the one on hand",
			"runID", cr.Spec.RunID, "expiresAt", bundle.ExpiresAt, "error", err)
		return nil
	}

	encoded, err := json.Marshal(fresh)
	if err != nil {
		return fmt.Errorf("encode the reissued bundle for %s: %w", cr.Name, err)
	}
	patch := client.MergeFrom(secret.DeepCopy())
	secret.Data[runv1.SecretKeyPresigned] = encoded
	if err := r.Patch(ctx, secret, patch); err != nil {
		return fmt.Errorf("store the reissued bundle for %s: %w", cr.Name, err)
	}
	return nil
}

func (r *Reconciler) bundleSlack() time.Duration {
	if r.BundleSlack > 0 {
		return r.BundleSlack
	}
	// The same slack the Job's deadline uses: whatever the agent's own budget
	// is, the upload, the push and the report happen after it.
	return time.Duration(launcher.DefaultDeadlineSlackSeconds) * time.Second
}

// handleTerminal decides what happens after an attempt ends: another attempt,
// or the wait for delivery and the TTL.
func (r *Reconciler) handleTerminal(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (ctrl.Result, error) {
	if r.shouldRetry(cr) {
		return r.scheduleRetry(ctx, cr)
	}
	if at := cr.Status.Retry.NextAttemptAt; at != nil {
		if wait := at.Time.Sub(r.now()); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		return r.startNextAttempt(ctx, cr)
	}
	return r.reap(ctx, cr)
}

// shouldRetry applies the one rule that decides whether the cluster repairs a
// failure itself: only infra and git are retried. Replaying an agent that
// already pushed a branch and opened a pull request spends the budget twice and
// can produce conflicting commits, and no amount of retrying fixes a prompt.
func (r *Reconciler) shouldRetry(cr *agentrunv1alpha1.AgentRun) bool {
	if cr.Status.Retry.NextAttemptAt != nil {
		return false
	}
	if !cr.Status.Phase.IsTerminal() || cr.Status.Phase == runv1.PhaseSucceeded {
		return false
	}
	if cr.Status.Phase == runv1.PhaseCancelled || IsCancelRequested(cr) {
		return false
	}
	if !cr.Status.FailureClass.Retriable() {
		return false
	}
	return cr.Status.Retry.InfraRetries < maxInfraRetries(cr)
}

func maxInfraRetries(cr *agentrunv1alpha1.AgentRun) int32 {
	if cr.Spec.Retry != nil && cr.Spec.Retry.MaxInfraRetries != nil {
		return *cr.Spec.Retry.MaxInfraRetries
	}
	return DefaultMaxInfraRetries
}

// scheduleRetry books the next attempt without starting it. The terminal phase
// of the attempt that just ended stays in the status until then, which is what
// guarantees the backend hears about it: the observation is queued from the
// status, and an attempt bumped in the same pass would overwrite it before it
// was ever sent.
func (r *Reconciler) scheduleRetry(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (ctrl.Result, error) {
	retries := cr.Status.Retry.InfraRetries + 1
	delay := retryDelay(retries)
	at := metav1.NewTime(r.now().Add(delay))

	if err := r.patchStatus(ctx, cr, func(cr *agentrunv1alpha1.AgentRun) {
		cr.Status.Retry.InfraRetries = retries
		cr.Status.Retry.NextAttemptAt = &at
		setCondition(cr, agentrunv1alpha1.ConditionJobCreated, metav1.ConditionFalse,
			agentrunv1alpha1.ReasonBackoff,
			fmt.Sprintf("attempt %d failed with class %s; retrying at %s",
				cr.Status.Attempt, cr.Status.FailureClass, at.Format(time.RFC3339)))
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(cr, corev1.EventTypeNormal, agentrunv1alpha1.ReasonBackoff,
		fmt.Sprintf("attempt %d failed (%s); retry %d of %d in %s",
			cr.Status.Attempt, cr.Status.FailureClass, retries, maxInfraRetries(cr), delay.Round(time.Second)))
	return ctrl.Result{RequeueAfter: delay}, nil
}

// startNextAttempt raises the attempt and clears the observations of the one
// that failed. The attempt is what resets the phase rank on the backend's side,
// so a Pending after a Failed is accepted rather than rejected as a regression.
func (r *Reconciler) startNextAttempt(ctx context.Context, cr *agentrunv1alpha1.AgentRun) (ctrl.Result, error) {
	next := cr.Status.Attempt + 1
	if err := r.patchStatus(ctx, cr, func(cr *agentrunv1alpha1.AgentRun) {
		cr.Status.Attempt = next
		cr.Status.Phase = runv1.PhasePending
		cr.Status.Reason = ReasonAwaitingJob
		cr.Status.Message = ""
		cr.Status.ExitCode = nil
		cr.Status.FailureClass = ""
		cr.Status.FinishedAt = nil
		cr.Status.StartedAt = nil
		cr.Status.JobName = ""
		cr.Status.PodName = ""
		cr.Status.NodeName = ""
		cr.Status.Retry.NextAttemptAt = nil
		setCondition(cr, agentrunv1alpha1.ConditionCompleted, metav1.ConditionFalse,
			agentrunv1alpha1.ReasonPending, fmt.Sprintf("attempt %d starting", next))
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.notify(cr)
	return ctrl.Result{Requeue: true}, nil
}

// retryDelay is exponential with half-range jitter, capped. The cap matters as
// much as the growth: a run whose retries spread over ten minutes has a lease
// the heartbeat is still renewing, and one whose retries spread over an hour
// does not.
func retryDelay(attempt int32) time.Duration {
	d := retryBase << min(attempt-1, 5)
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// failValidation records a spec the controller admitted and then could not
// build a pod from. It is terminal and of class config: nothing about the
// cluster will make it work.
func (r *Reconciler) failValidation(ctx context.Context, cr *agentrunv1alpha1.AgentRun, attempt int32, cause error) error {
	err := r.patchStatus(ctx, cr, func(cr *agentrunv1alpha1.AgentRun) {
		cr.Status.Attempt = attempt
		cr.Status.Phase = runv1.PhaseFailed
		cr.Status.Reason = agentrunv1alpha1.ReasonInvalidSpec
		cr.Status.Message = truncate(cause.Error(), 1024)
		cr.Status.FailureClass = runv1.FailureConfig
		cr.Status.FinishedAt = ptr(metav1.NewTime(r.now()))
		setCondition(cr, agentrunv1alpha1.ConditionValidated, metav1.ConditionFalse,
			agentrunv1alpha1.ReasonInvalidSpec, cause.Error())
		setCondition(cr, agentrunv1alpha1.ConditionCompleted, metav1.ConditionTrue,
			string(runv1.PhaseFailed), agentrunv1alpha1.ReasonInvalidSpec)
	})
	if err != nil {
		return err
	}
	r.notify(cr)
	return nil
}

// ownedBy reports whether a Job belongs to this AgentRun, by UID rather than by
// name: the names repeat across epochs, the UIDs do not.
func ownedBy(job *batchv1.Job, cr *agentrunv1alpha1.AgentRun) bool {
	for _, ref := range job.OwnerReferences {
		if ref.UID == cr.UID {
			return true
		}
	}
	return false
}

// jobFor fetches the Job of one attempt. The name is derived, so this is a get
// rather than a list: one run has exactly one Job per attempt, and a list would
// make that a hope.
func (r *Reconciler) jobFor(ctx context.Context, cr *agentrunv1alpha1.AgentRun, attempt int32) (*batchv1.Job, error) {
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{
		Namespace: cr.Namespace,
		Name:      agentrunv1alpha1.JobName(cr.Spec.RunID, attempt),
	}, &job)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("get job for %s attempt %d: %w", cr.Name, attempt, err)
	}
	return &job, nil
}

// podFor finds the pod of one attempt by the labels the Job's template carries.
// The attempt label is what keeps the pod of a previous attempt from being read
// as the current one — the Jobs differ by name, but their pods differ only by
// what the template says about them.
func (r *Reconciler) podFor(ctx context.Context, cr *agentrunv1alpha1.AgentRun, attempt int32) (*corev1.Pod, error) {
	var pods corev1.PodList
	selector := client.MatchingLabels{
		agentrunv1alpha1.LabelRunID:   lower(string(cr.Spec.RunID)),
		agentrunv1alpha1.LabelAttempt: fmt.Sprint(attempt),
	}
	if err := r.List(ctx, &pods, client.InNamespace(cr.Namespace), selector); err != nil {
		return nil, fmt.Errorf("list pods for %s attempt %d: %w", cr.Name, attempt, err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	// backoffLimit is zero, so there is one pod per attempt. If a cluster
	// somehow produced two, the newest is the one that matters.
	newest := &pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &pods.Items[i]
		}
	}
	return newest, nil
}
