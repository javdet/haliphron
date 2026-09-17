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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/agentrun"
	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/config"
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
}

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
		timings := l.Timings.Get()
		wait := time.Duration(timings.MaxWaitSeconds) * time.Second

		pollCtx, cancel := context.WithTimeout(ctx, wait+l.pollSlack)
		_, err := l.PollOnce(pollCtx)
		cancel()

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
func (l *Loop) PollOnce(ctx context.Context) (int, error) {
	timings := l.Timings.Get()
	free, err := l.freeSlots(ctx)
	if err != nil {
		l.Log.Error("counting free slots", "error", err)
		free = 0
	}

	resp, err := l.API.Lease(ctx, clusterv1.LeaseRequest{
		FreeSlots:     free,
		CapacitySlots: l.Capacity,
		Runtimes:      l.Runtimes,
		WaitSeconds:   timings.MaxWaitSeconds,
	})
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
func (l *Loop) handle(ctx context.Context, lease clusterv1.Lease) {
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
