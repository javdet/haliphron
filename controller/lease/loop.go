// Package lease is the long poll: the only place in the system where work
// enters a cluster.
//
// Everything about it follows from the pull model. The backend never connects
// inwards, so the controller asks; the ask is a long poll rather than polling,
// so that latency does not cost request rate; the answer carries the whole run,
// including its secrets, so that a controller which loses the backend a second
// later can still execute it.
//
// The loop's own contract is short and unforgiving: a 204 is not an error and
// is re-polled at once, a dropped connection is expected and is re-polled no
// more than once a second, and every failure that came with a Problem is acted
// on by its action and never by its status code.
package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/agentrun"
	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/config"
	"github.com/automagicops/haliphron/controller/inputs"
	"github.com/automagicops/haliphron/controller/materialize"
)

// API is the part of the Cluster API this loop uses.
type API interface {
	Lease(ctx context.Context, req clusterv1.LeaseRequest) (*clusterv1.LeaseResponse, error)
	Ack(ctx context.Context, runID runv1.ULID, req clusterv1.AckRequest) (*clusterv1.AckResponse, error)
}

// Loop asks for work and makes it durable. It is a manager.Runnable: the
// manager starts it after the caches are warm, which matters because the first
// thing it does is read the AgentRuns that survived the last restart.
type Loop struct {
	API          API
	K8s          client.Client
	Materializer *materialize.Materializer
	Timings      *config.Timings
	// Inputs fetches a lease's input files into the spool before the lease is
	// acknowledged. Nil refuses every lease that has inputs, which is the
	// honest answer from a controller with nowhere to keep them.
	Inputs *inputs.Fetcher

	Namespace string
	ClusterID runv1.ULID
	Capacity  int32
	Runtimes  []runv1.AgentType

	Clock func() time.Time
	Log   *slog.Logger

	// pollSlack is added to the server's wait before the request's own deadline
	// fires, so that an expired long poll is the server's 204 and not our
	// timeout — the two are indistinguishable from the outside and only one of
	// them is normal.
	pollSlack time.Duration

	// lastSweep is when the inputs area was last swept for runs this
	// controller no longer holds. In memory on purpose: losing it costs one
	// early sweep, which is harmless.
	lastSweep time.Time
}

// inputSweepInterval and inputOrphanAge pace the sweep of fetched inputs whose
// run never materialised here. The age is far past any ack deadline, so a
// lease being handled right now is never swept from under itself.
const (
	inputSweepInterval = 10 * time.Minute
	inputOrphanAge     = time.Hour
)

// New builds the loop.
func New(l Loop) (*Loop, error) {
	if l.API == nil || l.K8s == nil || l.Materializer == nil {
		return nil, errors.New("lease: API, K8s and Materializer are required")
	}
	if l.Timings == nil {
		l.Timings = config.NewTimings()
	}
	if l.Clock == nil {
		l.Clock = time.Now
	}
	if l.Log == nil {
		l.Log = slog.Default()
	}
	if l.pollSlack == 0 {
		l.pollSlack = 10 * time.Second
	}
	return &l, nil
}

// NeedLeaderElection keeps one leader polling. Two controllers leasing for one
// cluster would each take half the work and each report a free-slot count the
// other contradicts.
func (l *Loop) NeedLeaderElection() bool { return true }

// Start runs until the context ends. It returns nil on a clean stop, because a
// manager that treats a graceful shutdown as a failure logs an error every
// time a pod is rescheduled.
func (l *Loop) Start(ctx context.Context) error {
	l.Resume(ctx)

	backoff := clusterapi.DefaultBackoff()
	// last is stamped when a poll *starts*, so a long poll that hung for its
	// full thirty seconds re-polls immediately while one that answered
	// instantly is held to once a second. One mechanism covers both the
	// contract's "204 re-polls at once" and its "a dropped connection no more
	// than once a second".
	pacer := &clusterapi.Pacer{MinInterval: time.Second}

	for {
		if !pacer.Wait(ctx, l.Clock) {
			return nil
		}
		l.maybeSweepInputs(ctx)

		_, err := l.PollOnce(ctx)

		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			stop, delay := l.classify(err, backoff)
			if stop {
				return nil
			}
			if delay > 0 && !clusterapi.Wait(ctx, delay) {
				return nil
			}
			continue
		}

		backoff.Reset()
	}
}

// PollOnce performs one long poll and makes whatever came back durable. It
// returns how many leases were handled; zero with a nil error is the contract's
// 204, which is not a failure and is re-polled at once.
//
// It is separate from Start so that the protocol can be exercised one step at a
// time. A loop that can only be run forever can only be tested by waiting.
//
// The poll's own deadline covers the long poll only. The leases that come back
// are handled under ctx, because handling one can take a fetch of its input
// files, and that is bounded by the ack deadline rather than by whatever was
// left of thirty seconds of waiting.
func (l *Loop) PollOnce(ctx context.Context) (int, error) {
	timings := l.Timings.Get()
	free, err := l.freeSlots(ctx)
	if err != nil {
		l.Log.Error("counting free slots", "error", err)
		free = 0
	}

	wait := time.Duration(timings.MaxWaitSeconds) * time.Second
	pollCtx, cancel := context.WithTimeout(ctx, wait+l.pollSlack)
	resp, err := l.API.Lease(pollCtx, clusterv1.LeaseRequest{
		FreeSlots:     free,
		CapacitySlots: l.Capacity,
		Runtimes:      l.Runtimes,
		Features:      clusterv1.ControllerFeatures,
		WaitSeconds:   timings.MaxWaitSeconds,
	})
	cancel()
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, nil
	}
	for _, lease := range resp.Leases {
		l.handle(ctx, lease)
	}
	return len(resp.Leases), nil
}

// classify turns a failed poll into a decision. It reports whether to stop
// polling altogether and how long to wait first.
func (l *Loop) classify(err error, backoff *clusterapi.Backoff) (stop bool, delay time.Duration) {
	var transport *clusterapi.TransportError
	if errors.As(err, &transport) {
		// A proxy closing an idle long poll, a backend restarting mid-request.
		// Expected, and already paced by the loop's own guard.
		l.Log.Debug("the lease poll did not complete", "error", err)
		return false, 0
	}

	problem, ok := clusterapi.ProblemOf(err)
	if !ok {
		l.Log.Error("leasing", "error", err)
		return false, backoff.Next()
	}

	switch problem.Action {
	case clusterv1.ActionReregister:
		// The credential is unusable and no repeat will fix it. Stopping is the
		// honest response: continuing would be a cluster hammering a control
		// plane that has already said no.
		l.Log.Error("the control plane rejected this cluster's credential; polling stops until the chart is reinstalled",
			"code", problem.Code, "detail", problem.Detail)
		return true, 0
	case clusterv1.ActionFatal:
		// Usually a version window: the chart is older than the control plane
		// supports. A slow retry costs nothing and picks itself up the moment
		// somebody upgrades.
		l.Log.Error("the control plane refused the poll", "code", problem.Code, "title", problem.Title)
		return false, 60 * time.Second
	case clusterv1.ActionBackoff:
		if after, ok := clusterapi.RetryAfter(err); ok {
			return false, after
		}
		return false, backoff.Next()
	default:
		l.Log.Warn("the lease poll failed", "code", problem.Code, "action", problem.Action)
		return false, backoff.Next()
	}
}

// handle makes one lease durable and acknowledges it. Nothing here starts the
// work: the Job waits for the acknowledgement, because the backend is entitled
// to reassign an unacknowledged lease on the grounds that it cannot have
// started.
//
// A lease with input files has them fetched first, into this controller's
// spool and verified, and only then is anything materialised. That order is
// what lets the acknowledgement keep both of its promises for such a run: the
// work has not started, and once it has, it no longer needs the backend.
func (l *Loop) handle(ctx context.Context, lease clusterv1.Lease) {
	var fetchCtx context.Context = ctx
	if len(lease.Inputs) > 0 {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithTimeout(ctx, inputs.Budget(l.Timings.Get().AckTimeoutSeconds))
		defer cancel()
		rejection, err := l.fetcher().Fetch(fetchCtx, lease)
		if err != nil {
			// No ack, as for a failed materialisation: the backend reassigns
			// after the deadline. What did arrive stays in the spool, and the
			// next lease of this run fetches only what is missing.
			l.Log.Error("fetching a lease's inputs", "runID", lease.RunID, "epoch", lease.Epoch, "error", err)
			return
		}
		if rejection != nil {
			l.forgetInputs(lease.RunID)
			l.ack(ctx, lease, materialize.Result{Rejection: rejection})
			return
		}
	}

	result, err := l.Materializer.Apply(ctx, lease)
	if err != nil {
		// No ack, positive or negative. The ack deadline expires, the backend
		// raises the epoch and reissues — which is the correct outcome for a
		// cluster that could not write to its own API server, and needs no code
		// of its own.
		l.Log.Error("materialising a lease", "runID", lease.RunID, "epoch", lease.Epoch, "error", err)
		return
	}
	if result.Superseded {
		l.Log.Info("a previous epoch of this run is still terminating; leaving the lease unacknowledged",
			"runID", lease.RunID, "epoch", lease.Epoch)
		return
	}
	if result.AgentRun != nil && len(lease.Inputs) > 0 {
		// The inputs were verified before the objects existed; this is the
		// check that nothing removed them in between. Collecting the previous
		// epoch of the same run is the one thing that can.
		rejection, err := l.fetcher().Confirm(fetchCtx, lease)
		if err != nil {
			l.Log.Error("re-fetching a lease's inputs", "runID", lease.RunID, "epoch", lease.Epoch, "error", err)
			return
		}
		if rejection != nil {
			if err := l.K8s.Delete(ctx, result.AgentRun); err != nil && !apierrors.IsNotFound(err) {
				l.Log.Error("deleting a run whose inputs could not be fetched", "runID", lease.RunID, "error", err)
				return
			}
			l.forgetInputs(lease.RunID)
			result = materialize.Result{Rejection: rejection}
		}
	}
	l.ack(ctx, lease, result)
}

// ack sends the acknowledgement for a handled lease: negative when the result
// carries a rejection, positive otherwise.
func (l *Loop) ack(ctx context.Context, lease clusterv1.Lease, result materialize.Result) {
	req := clusterv1.AckRequest{
		ClusterID: l.ClusterID,
		Epoch:     lease.Epoch,
		Namespace: l.Namespace,
	}
	if result.Rejection != nil {
		accepted := false
		req.Accepted = &accepted
		req.Rejection = result.Rejection
		l.Log.Warn("refusing a lease this cluster cannot materialise",
			"runID", lease.RunID, "code", result.Rejection.Code, "fields", result.Rejection.Fields)
	} else {
		req.CRName = result.AgentRun.Name
	}

	resp, err := l.API.Ack(ctx, lease.RunID, req)
	if err != nil {
		l.handleAckError(ctx, lease, result, err)
		return
	}

	if result.AgentRun == nil {
		return
	}
	if err := agentrun.MarkAcknowledged(ctx, l.K8s, result.AgentRun, l.Clock()); err != nil {
		l.Log.Error("recording the acknowledgement", "runID", lease.RunID, "error", err)
	}
	// A cancellation that arrived between the lease and the ack. Without this
	// it would wait a whole heartbeat interval, having first started a Job that
	// must immediately be killed.
	for _, cmd := range resp.Commands {
		if err := agentrun.Record(ctx, l.K8s, result.AgentRun, cmd, l.Clock()); err != nil {
			l.Log.Error("recording a command from the ack", "runID", lease.RunID, "error", err)
		}
	}
}

// handleAckError deals with the acknowledgement being refused. The only case
// that needs action is abandon: the work was reassigned while this cluster was
// materialising it, and the objects just created have to go.
func (l *Loop) handleAckError(ctx context.Context, lease clusterv1.Lease, result materialize.Result, err error) {
	if clusterapi.ActionOf(err) == clusterv1.ActionAbandon {
		// Whatever was fetched for this lease belongs to whoever holds the run
		// now. The CR, if there is one, takes the rest with it when the
		// reconciler collects it.
		l.forgetInputs(lease.RunID)
	}
	if clusterapi.ActionOf(err) == clusterv1.ActionAbandon && result.AgentRun != nil {
		l.Log.Info("the lease was reassigned before it could be acknowledged",
			"runID", lease.RunID, "epoch", lease.Epoch)
		if recErr := agentrun.Record(ctx, l.K8s, result.AgentRun, clusterv1.Command{
			Type: clusterv1.CommandAbandon, RunID: lease.RunID,
		}, l.Clock()); recErr != nil {
			l.Log.Error("recording an abandon after a refused ack", "runID", lease.RunID, "error", recErr)
		}
		return
	}
	l.Log.Error("acknowledging a lease", "runID", lease.RunID, "epoch", lease.Epoch, "error", err)
}

// Resume repeats the acknowledgement for every run that was materialised but
// never confirmed.
//
// This is the restart case, and it is the reason the ack is idempotent on
// (runID, epoch). A controller that created the objects and died before the
// call comes back, finds them, and says the same thing again; the backend
// answers 200 and the run proceeds. Without it the work would sit in the
// cluster, started by nobody, until the ack deadline reassigned it.
func (l *Loop) Resume(ctx context.Context) {
	var runs agentrunv1alpha1.AgentRunList
	if err := l.K8s.List(ctx, &runs, client.InNamespace(l.Namespace)); err != nil {
		l.Log.Error("listing AgentRuns at startup", "error", err)
		return
	}
	for i := range runs.Items {
		cr := &runs.Items[i]
		if agentrun.IsAcknowledged(cr) || agentrun.IsAbandoned(cr) ||
			cr.DeletionTimestamp != nil || cr.Status.Phase.IsTerminal() {
			continue
		}
		if !l.resumeInputs(ctx, cr) {
			continue
		}
		resp, err := l.API.Ack(ctx, cr.Spec.RunID, clusterv1.AckRequest{
			ClusterID: l.ClusterID,
			Epoch:     cr.Spec.LeaseEpoch,
			CRName:    cr.Name,
			Namespace: cr.Namespace,
		})
		if err != nil {
			if clusterapi.ActionOf(err) == clusterv1.ActionAbandon {
				if recErr := agentrun.Record(ctx, l.K8s, cr, clusterv1.Command{
					Type: clusterv1.CommandAbandon, RunID: cr.Spec.RunID,
				}, l.Clock()); recErr != nil {
					l.Log.Error("recording an abandon at startup", "runID", cr.Spec.RunID, "error", recErr)
				}
				continue
			}
			l.Log.Error("repeating an acknowledgement at startup", "runID", cr.Spec.RunID, "error", err)
			continue
		}
		l.Log.Info("repeated the acknowledgement after a restart",
			"runID", cr.Spec.RunID, "epoch", cr.Spec.LeaseEpoch, "status", resp.Status)
		if err := agentrun.MarkAcknowledged(ctx, l.K8s, cr, l.Clock()); err != nil {
			l.Log.Error("recording the acknowledgement", "runID", cr.Spec.RunID, "error", err)
		}
		for _, cmd := range resp.Commands {
			if err := agentrun.Record(ctx, l.K8s, cr, cmd, l.Clock()); err != nil {
				l.Log.Error("recording a command at startup", "runID", cr.Spec.RunID, "error", err)
			}
		}
	}
}

// freeSlots is the capacity minus what is already running. Zero is a legitimate
// answer and still worth polling with: the channel stays open, and the backend
// learns the cluster is alive.
func (l *Loop) freeSlots(ctx context.Context) (int32, error) {
	var runs agentrunv1alpha1.AgentRunList
	if err := l.K8s.List(ctx, &runs, client.InNamespace(l.Namespace)); err != nil {
		return 0, fmt.Errorf("list AgentRuns: %w", err)
	}
	var active int32
	for i := range runs.Items {
		cr := &runs.Items[i]
		if cr.DeletionTimestamp != nil || cr.Status.Phase.IsTerminal() {
			continue
		}
		active++
	}
	if free := l.Capacity - active; free > 0 {
		return free, nil
	}
	return 0, nil
}

// fetcher is the configured Fetcher, or one with nothing wired, which refuses
// every lease that has inputs.
func (l *Loop) fetcher() *inputs.Fetcher {
	if l.Inputs != nil {
		return l.Inputs
	}
	return &inputs.Fetcher{Log: l.Log}
}

func (l *Loop) forgetInputs(id runv1.ULID) {
	if l.Inputs == nil || l.Inputs.Spool == nil {
		return
	}
	if err := l.Inputs.Spool.ForgetInputs(id); err != nil {
		l.Log.Error("discarding a run's inputs", "runID", id, "error", err)
	}
}

// resumeInputs is the inputs half of repeating an acknowledgement after a
// restart: the run's manifest, read back from its Secret, must be in the spool
// before the ack goes out again. Inputs that are missing — a spool on an
// emptyDir, a crash mid-fetch that the Secret nonetheless outlived — are
// fetched again. It reports whether the ack may be repeated.
//
// A run whose inputs cannot be fetched is not acknowledged: the reconciler
// discards it when its ack budget runs out, and the backend reassigns. One
// whose inputs are refused is deleted and refused, as a fresh lease would be.
func (l *Loop) resumeInputs(ctx context.Context, cr *agentrunv1alpha1.AgentRun) bool {
	var secret corev1.Secret
	err := l.K8s.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.Materials.SecretName}, &secret)
	if apierrors.IsNotFound(err) {
		// The crash came between the CR and the Secret. The ack is repeated
		// as before; the reconciler cannot start a Job without the Secret, and
		// the next lease of this run re-ensures it.
		return true
	}
	if err != nil {
		l.Log.Error("reading a run's Secret at startup", "runID", cr.Spec.RunID, "error", err)
		return false
	}
	raw := secret.Data[runv1.SecretKeyInputs]
	if len(raw) == 0 {
		return true
	}
	var manifest runv1.InputManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		l.Log.Error("the input manifest in a run's Secret does not parse", "runID", cr.Spec.RunID)
		return false
	}
	lease := clusterv1.Lease{RunID: cr.Spec.RunID, Epoch: cr.Spec.LeaseEpoch, Inputs: manifest.Inputs}
	fetchCtx, cancel := context.WithTimeout(ctx, inputs.Budget(l.Timings.Get().AckTimeoutSeconds))
	defer cancel()
	rejection, err := l.fetcher().Confirm(fetchCtx, lease)
	if err != nil {
		l.Log.Error("fetching a run's inputs at startup", "runID", cr.Spec.RunID, "epoch", cr.Spec.LeaseEpoch, "error", err)
		return false
	}
	if rejection != nil {
		if err := l.K8s.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
			l.Log.Error("deleting a run whose inputs could not be fetched", "runID", cr.Spec.RunID, "error", err)
			return false
		}
		l.forgetInputs(cr.Spec.RunID)
		l.ack(ctx, lease, materialize.Result{Rejection: rejection})
		return false
	}
	return true
}

// maybeSweepInputs drops, at most once per interval, the inputs of runs that
// have no AgentRun here: fetched for a lease that was then never
// materialised. The explicit paths — a refusal, an abandon, the CR's
// finalizer — handle every run that got as far as an object.
func (l *Loop) maybeSweepInputs(ctx context.Context) {
	if l.Inputs == nil || l.Inputs.Spool == nil {
		return
	}
	now := l.Clock()
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < inputSweepInterval {
		return
	}
	l.lastSweep = now
	l.SweepInputs(ctx)
}

// SweepInputs is one pass of the sweep, exported so that it can be driven a
// step at a time.
func (l *Loop) SweepInputs(ctx context.Context) {
	if l.Inputs == nil || l.Inputs.Spool == nil {
		return
	}
	var runs agentrunv1alpha1.AgentRunList
	if err := l.K8s.List(ctx, &runs, client.InNamespace(l.Namespace)); err != nil {
		l.Log.Error("listing AgentRuns to sweep inputs", "error", err)
		return
	}
	held := make(map[runv1.ULID]bool, len(runs.Items))
	for i := range runs.Items {
		held[runs.Items[i].Spec.RunID] = true
	}
	dropped, err := l.Inputs.Spool.SweepInputs(func(id runv1.ULID) bool { return held[id] },
		l.Clock().Add(-inputOrphanAge))
	if err != nil {
		l.Log.Error("sweeping inputs", "error", err)
		return
	}
	if dropped > 0 {
		l.Log.Info("dropped the inputs of runs this cluster does not hold", "runs", dropped)
	}
}
