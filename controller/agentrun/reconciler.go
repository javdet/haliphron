// Package agentrun is the reconciler: the part of the controller that knows
// about Kubernetes and nothing about HTTP.
//
// Its contract is section 14 of the CRD document — reconciliation is a pure
// function of the spec and the observed cluster state, with nothing remembered
// between calls. Everything that must survive a restart is in the status, and
// everything that arrives from outside, including the backend's commands,
// arrives as an annotation on the object rather than as a field in memory. That
// is what makes a controller restart uneventful, and it is also what makes the
// whole phase table testable without a cluster.
package agentrun

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/launcher"
)

// Notifier receives the phase changes the reconciler observes. It is an
// interface so that the reconciler cannot wait on the network: the
// implementation queues, and a control plane that is down slows nothing down
// inside the cluster.
type Notifier interface {
	Observed(obs clusterv1.RunObservation)
	// Forget drops anything queued about a run that was taken away from this
	// cluster. Reporting after an abandon is the one thing the contract calls
	// out as forbidden.
	Forget(runID runv1.ULID)
}

// BundleRefresher mints a fresh presigned bundle for a lease this cluster still
// holds.
type BundleRefresher interface {
	Artifacts(ctx context.Context, runID runv1.ULID, req clusterv1.ArtifactBundleRequest) (*clusterv1.ArtifactBundle, error)
}

// Reconciler drives one AgentRun towards the state its spec describes.
type Reconciler struct {
	client.Client

	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	ClusterID runv1.ULID
	Builder   launcher.Builder
	Notifier  Notifier
	Bundles   BundleRefresher

	// StartupDeadline is how long a pod may fail to start before the attempt is
	// called a failure. Without it a nonexistent image waits forever, because
	// to Kubernetes ImagePullBackOff is a state and not an error.
	StartupDeadline time.Duration
	// BundleSlack is added to the run's own timeout when deciding whether the
	// presigned bundle will outlive the attempt.
	BundleSlack time.Duration
	// HardTTL is the ceiling on holding a CR whose outcome the backend never
	// accepted.
	HardTTL time.Duration
	// AckWaitBudget is how long a materialised run may wait for the backend to
	// confirm its lease before the controller concludes the acknowledgement
	// will never arrive and drops the object. The backend has reassigned the
	// work by then — that is what the ack deadline is for — so keeping it would
	// leave a run nobody will ever start.
	AckWaitBudget time.Duration

	Clock func() time.Time
	log   *slog.Logger
}

// Config is what Reconciler needs from outside.
type Config struct {
	Client          client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	ClusterID       runv1.ULID
	Builder         launcher.Builder
	Notifier        Notifier
	Bundles         BundleRefresher
	StartupDeadline time.Duration
	BundleSlack     time.Duration
	HardTTL         time.Duration
	AckWaitBudget   time.Duration
	Clock           func() time.Time
	Log             *slog.Logger
}

// New builds a reconciler, refusing the two dependencies whose absence would
// show up as silence rather than as a failure: without a client there is
// nothing to reconcile, and without a notifier the runs would execute and
// nobody would ever be told.
func New(cfg Config) (*Reconciler, error) {
	if cfg.Client == nil {
		return nil, errors.New("agentrun: a client is required")
	}
	if cfg.Notifier == nil {
		return nil, errors.New("agentrun: a notifier is required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	if cfg.StartupDeadline <= 0 {
		cfg.StartupDeadline = DefaultStartupDeadline
	}
	if cfg.AckWaitBudget <= 0 {
		cfg.AckWaitBudget = DefaultAckWaitBudget
	}
	return &Reconciler{
		Client:          cfg.Client,
		Scheme:          cfg.Scheme,
		Recorder:        cfg.Recorder,
		ClusterID:       cfg.ClusterID,
		Builder:         cfg.Builder,
		Notifier:        cfg.Notifier,
		Bundles:         cfg.Bundles,
		StartupDeadline: cfg.StartupDeadline,
		BundleSlack:     cfg.BundleSlack,
		HardTTL:         cfg.HardTTL,
		AckWaitBudget:   cfg.AckWaitBudget,
		Clock:           clock,
		log:             log,
	}, nil
}

// +kubebuilder:rbac:groups=haliphron.io,resources=agentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=haliphron.io,resources=agentruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=haliphron.io,resources=agentruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile observes one run and does the one thing the observation calls for.
//
// The order is deliberate. Commands come first, because an abandoned run must
// not be reported on even once more. Deletion comes second, because a deleted
// object has no future to plan. Only then is the cluster observed, and the
// observation is folded into the status before anything acts on it — so that
// whatever happens next, the backend has already been told what was seen.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr agentrunv1alpha1.AgentRun
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if IsAbandoned(&cr) {
		return r.abandon(ctx, &cr)
	}
	if cr.DeletionTimestamp != nil {
		return r.finalize(ctx, &cr)
	}

	attempt := currentAttempt(&cr)
	job, err := r.jobFor(ctx, &cr, attempt)
	if err != nil {
		return ctrl.Result{}, err
	}
	pod, err := r.podFor(ctx, &cr, attempt)
	if err != nil {
		return ctrl.Result{}, err
	}

	obs := Derive(&cr, job, pod, r.now(), r.StartupDeadline)
	if IsCancelRequested(&cr) {
		if obs, err = r.applyCancel(ctx, &cr, job, pod, obs); err != nil {
			return ctrl.Result{}, err
		}
	}
	obs = Monotonic(&cr, attempt, obs)

	changed, err := r.applyObservation(ctx, &cr, attempt, obs)
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	if changed {
		r.notify(&cr)
	}

	if cr.Status.Phase.IsTerminal() {
		return r.handleTerminal(ctx, &cr)
	}
	if job == nil {
		if !IsAcknowledged(&cr) {
			return r.awaitAck(ctx, &cr)
		}
		return r.ensureJob(ctx, &cr, attempt)
	}
	return ctrl.Result{RequeueAfter: r.startupCheck(&cr, job)}, nil
}

// startupCheck is when to look again at a run that has not started. Everything
// else is driven by watches; the startup deadline is the one fact no event will
// ever announce, because nothing happens when a pod fails to start — that is
// precisely the problem with it.
func (r *Reconciler) startupCheck(cr *agentrunv1alpha1.AgentRun, job *batchv1.Job) time.Duration {
	if cr.Status.Phase == runv1.PhaseRunning {
		return 0
	}
	if job == nil || job.CreationTimestamp.IsZero() {
		return 0
	}
	remaining := job.CreationTimestamp.Add(r.StartupDeadline).Sub(r.now())
	if remaining < time.Second {
		remaining = time.Second
	}
	return remaining
}

// SetupWithManager wires the watches. Pods are watched rather than owned: they
// belong to the Job, not to the AgentRun, and the link back is the run-id
// label — which is exactly why the label is on the pod template and not only on
// the Job.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentrun").
		For(&agentrunv1alpha1.AgentRun{}).
		Owns(&batchv1.Job{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podToAgentRun)).
		Complete(r)
}

func (r *Reconciler) podToAgentRun(_ context.Context, obj client.Object) []reconcile.Request {
	runID := obj.GetLabels()[agentrunv1alpha1.LabelRunID]
	if runID == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      agentrunv1alpha1.ObjectName(runv1.ULID(strings.ToUpper(runID))),
	}}}
}

// notify hands the current state to the reporter. Nothing is said about an
// abandoned run, ever.
func (r *Reconciler) notify(cr *agentrunv1alpha1.AgentRun) {
	if IsAbandoned(cr) {
		return
	}
	r.Notifier.Observed(observationFor(cr, r.now()))
}

func (r *Reconciler) event(cr *agentrunv1alpha1.AgentRun, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(cr, eventType, reason, message)
}

func (r *Reconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// currentAttempt is the attempt the controller is working on. It comes from the
// status rather than the lease, because the lease's attempt is only ever the
// first one: every retry after that is the controller's own decision.
func currentAttempt(cr *agentrunv1alpha1.AgentRun) int32 {
	if cr.Status.Attempt >= 1 {
		return cr.Status.Attempt
	}
	return 1
}

func lower(s string) string { return strings.ToLower(s) }
