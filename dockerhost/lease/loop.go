// Package lease is the long poll, as controller/lease runs it: a 204 is
// re-polled at once, a dropped connection no more than once a second, and
// every failure that came with a Problem is acted on by its action. What
// differs is only where the work becomes durable — a row instead of an
// AgentRun — and where a command is recorded.
package lease

import (
	"context"
	"errors"
	"log/slog"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/config"
	"github.com/automagicops/haliphron/controller/inputs"

	"github.com/automagicops/haliphron/dockerhost/materialize"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// API is the part of the Cluster API this loop uses.
type API interface {
	Lease(ctx context.Context, req clusterv1.LeaseRequest) (*clusterv1.LeaseResponse, error)
	Ack(ctx context.Context, runID runv1.ULID, req clusterv1.AckRequest) (*clusterv1.AckResponse, error)
}

// Kicker asks the reconciler to look at a run now.
type Kicker interface {
	Enqueue(runID runv1.ULID)
}

// Slots answers the lease's freeSlots.
type Slots interface {
	FreeSlots(ctx context.Context) (int32, error)
}

// Loop asks for work and makes it durable.
type Loop struct {
	API          API
	Store        *state.Store
	Materializer *materialize.Materializer
	Timings      *config.Timings
	Kicker       Kicker
	Slots        Slots
	// Inputs fetches a lease's input files into the spool before the lease is
	// acknowledged, exactly as on Kubernetes. Nil refuses every lease that has
	// inputs.
	Inputs *inputs.Fetcher

	Network   string
	ClusterID runv1.ULID
	Capacity  int32
	Runtimes  []runv1.AgentType

	Clock func() time.Time
	Log   *slog.Logger

	PollSlack time.Duration

	lastSweep time.Time
}

// inputSweepInterval and inputOrphanAge pace the sweep of fetched inputs whose
// run never became a row; see controller/lease.
const (
	inputSweepInterval = 10 * time.Minute
	inputOrphanAge     = time.Hour
)

func (l *Loop) now() time.Time {
	if l.Clock != nil {
		return l.Clock()
	}
	return time.Now()
}

func (l *Loop) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}

// Start runs until the context ends.
func (l *Loop) Start(ctx context.Context) error {
	if l.Timings == nil {
		l.Timings = config.NewTimings()
	}
	if l.PollSlack == 0 {
		l.PollSlack = 10 * time.Second
	}
	l.Resume(ctx)

	backoff := clusterapi.DefaultBackoff()
	pacer := &clusterapi.Pacer{MinInterval: time.Second}
	for {
		if !pacer.Wait(ctx, l.now) {
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

// PollOnce performs one long poll and makes whatever came back durable. The
// poll's deadline covers the long poll only; the leases are handled under ctx,
// because fetching a lease's inputs is bounded by the ack deadline instead.
func (l *Loop) PollOnce(ctx context.Context) (int, error) {
	if l.Timings == nil {
		l.Timings = config.NewTimings()
	}
	free := int32(0)
	if l.Slots != nil {
		n, err := l.Slots.FreeSlots(ctx)
		if err != nil {
			l.log().Error("counting free slots", "error", err)
		} else {
			free = n
		}
	}
	pollCtx := ctx
	if l.PollSlack > 0 {
		wait := time.Duration(l.Timings.Get().MaxWaitSeconds) * time.Second
		var cancel context.CancelFunc
		pollCtx, cancel = context.WithTimeout(ctx, wait+l.PollSlack)
		defer cancel()
	}
	resp, err := l.API.Lease(pollCtx, clusterv1.LeaseRequest{
		FreeSlots:     free,
		CapacitySlots: l.Capacity,
		Runtimes:      l.Runtimes,
		Features:      clusterv1.ControllerFeatures,
		WaitSeconds:   l.Timings.Get().MaxWaitSeconds,
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

func (l *Loop) classify(err error, backoff *clusterapi.Backoff) (bool, time.Duration) {
	var transport *clusterapi.TransportError
	if errors.As(err, &transport) {
		l.log().Debug("the lease poll did not complete", "error", err)
		return false, 0
	}
	problem, ok := clusterapi.ProblemOf(err)
	if !ok {
		l.log().Error("leasing", "error", err)
		return false, backoff.Next()
	}
	switch problem.Action {
	case clusterv1.ActionReregister:
		l.log().Error("the control plane rejected this host's credential; polling stops until it is re-registered",
			"code", problem.Code, "detail", problem.Detail)
		return true, 0
	case clusterv1.ActionFatal:
		l.log().Error("the control plane refused the poll", "code", problem.Code, "title", problem.Title)
		return false, 60 * time.Second
	case clusterv1.ActionBackoff:
		if after, ok := clusterapi.RetryAfter(err); ok {
			return false, after
		}
		return false, backoff.Next()
	default:
		l.log().Warn("the lease poll failed", "code", problem.Code, "action", problem.Action)
		return false, backoff.Next()
	}
}

// handle makes one lease durable and acknowledges it. Nothing here starts the
// work.
//
// A lease with input files has them fetched into the spool and verified first,
// for controller/lease's reason: once acknowledged, the run must not need the
// backend to start.
func (l *Loop) handle(ctx context.Context, lease clusterv1.Lease) {
	var fetchCtx context.Context = ctx
	if len(lease.Inputs) > 0 {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithTimeout(ctx, inputs.Budget(l.timings().Get().AckTimeoutSeconds))
		defer cancel()
		rejection, err := l.fetcher().Fetch(fetchCtx, lease)
		if err != nil {
			l.log().Error("fetching a lease's inputs", "runID", lease.RunID, "epoch", lease.Epoch, "error", err)
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
		l.log().Error("materialising a lease", "runID", lease.RunID, "epoch", lease.Epoch, "error", err)
		return
	}
	if result.Superseded {
		l.log().Info("a previous epoch of this run is still being removed; leaving the lease unacknowledged",
			"runID", lease.RunID, "epoch", lease.Epoch)
		if l.Kicker != nil {
			l.Kicker.Enqueue(lease.RunID)
		}
		return
	}
	if result.Run != nil && len(lease.Inputs) > 0 {
		// Removing the previous epoch's row takes the run's inputs with it,
		// and that can land between the fetch and the insert.
		rejection, err := l.fetcher().Confirm(fetchCtx, lease)
		if err != nil {
			l.log().Error("re-fetching a lease's inputs", "runID", lease.RunID, "epoch", lease.Epoch, "error", err)
			return
		}
		if rejection != nil {
			if err := l.Store.Delete(ctx, lease.RunID); err != nil && !errors.Is(err, state.ErrNotFound) {
				l.log().Error("deleting a run whose inputs could not be fetched", "runID", lease.RunID, "error", err)
				return
			}
			l.forgetInputs(lease.RunID)
			result = materialize.Result{Rejection: rejection}
		}
	}
	l.ack(ctx, lease, result)
}

// ack sends the acknowledgement for a handled lease.
func (l *Loop) ack(ctx context.Context, lease clusterv1.Lease, result materialize.Result) {
	req := clusterv1.AckRequest{ClusterID: l.ClusterID, Epoch: lease.Epoch, Namespace: l.Network}
	if result.Rejection != nil {
		accepted := false
		req.Accepted = &accepted
		req.Rejection = result.Rejection
		l.log().Warn("refusing a lease this host cannot run",
			"runID", lease.RunID, "code", result.Rejection.Code, "fields", result.Rejection.Fields)
	} else {
		req.CRName = string(lease.RunID)
	}

	resp, err := l.API.Ack(ctx, lease.RunID, req)
	if err != nil {
		l.handleAckError(ctx, lease.RunID, result.Run != nil, err)
		return
	}
	if result.Run == nil {
		return
	}
	l.acknowledged(ctx, lease.RunID, lease.Epoch, resp.Commands)
}

// acknowledged records the ack and any commands that came with it. A cancel
// that arrived between the lease and the ack is recorded before the container
// exists, so it is never started.
func (l *Loop) acknowledged(ctx context.Context, runID runv1.ULID, epoch int64, commands []clusterv1.Command) {
	now := l.now()
	if _, err := l.Store.Mutate(ctx, runID, func(r *state.Run) bool {
		changed := false
		if r.Epoch == epoch && r.AckedAt == nil {
			r.AckedAt = &now
			changed = true
		}
		for _, cmd := range commands {
			if Record(r, cmd, now) {
				changed = true
			}
		}
		return changed
	}); err != nil && !errors.Is(err, state.ErrNotFound) {
		l.log().Error("recording the acknowledgement", "runID", runID, "error", err)
	}
	if l.Kicker != nil {
		l.Kicker.Enqueue(runID)
	}
}

func (l *Loop) handleAckError(ctx context.Context, runID runv1.ULID, materialised bool, err error) {
	if clusterapi.ActionOf(err) == clusterv1.ActionAbandon && !materialised {
		// Nothing durable to remove, only what was fetched for the lease.
		l.forgetInputs(runID)
	}
	if clusterapi.ActionOf(err) == clusterv1.ActionAbandon && materialised {
		l.log().Info("the lease was reassigned before it could be acknowledged", "runID", runID)
		now := l.now()
		if _, recErr := l.Store.Mutate(ctx, runID, func(r *state.Run) bool {
			return Record(r, clusterv1.Command{Type: clusterv1.CommandAbandon, RunID: runID}, now)
		}); recErr != nil && !errors.Is(recErr, state.ErrNotFound) {
			l.log().Error("recording an abandon after a refused ack", "runID", runID, "error", recErr)
		}
		if l.Kicker != nil {
			l.Kicker.Enqueue(runID)
		}
		return
	}
	l.log().Error("acknowledging a lease", "runID", runID, "error", err)
}

// Resume repeats the acknowledgement for every row that was written but never
// confirmed: the restart case, and the reason the ack is idempotent.
func (l *Loop) Resume(ctx context.Context) {
	rows, err := l.Store.List(ctx)
	if err != nil {
		l.log().Error("listing runs at startup", "error", err)
		return
	}
	for i := range rows {
		r := &rows[i]
		if r.IsAcknowledged() || r.IsAbandoned() || r.Status.Phase.IsTerminal() {
			continue
		}
		if !l.resumeInputs(ctx, r) {
			continue
		}
		resp, err := l.API.Ack(ctx, r.RunID, clusterv1.AckRequest{
			ClusterID: l.ClusterID, Epoch: r.Epoch, CRName: string(r.RunID), Namespace: l.Network,
		})
		if err != nil {
			l.handleAckError(ctx, r.RunID, true, err)
			continue
		}
		l.log().Info("repeated the acknowledgement after a restart", "runID", r.RunID, "epoch", r.Epoch, "status", resp.Status)
		l.acknowledged(ctx, r.RunID, r.Epoch, resp.Commands)
	}
}

// Record writes a command onto a row. Commands repeat until the observed
// state reflects them, so recording one twice is a no-op: the first write
// wins. An unknown command type is a no-op too — the control plane may be
// newer than this controller. It reports whether anything changed.
func Record(r *state.Run, cmd clusterv1.Command, now time.Time) bool {
	switch cmd.Type {
	case clusterv1.CommandCancel:
		if r.IsCancelRequested() || r.IsAbandoned() {
			return false
		}
		t := now.UTC()
		r.CancelRequestedAt = &t
		if cmd.GracePeriodSeconds > 0 {
			r.CancelGraceSeconds = cmd.GracePeriodSeconds
		}
		r.CancelReason = cmd.Reason
		return true
	case clusterv1.CommandAbandon:
		if r.IsAbandoned() {
			return false
		}
		t := now.UTC()
		r.AbandonedAt = &t
		return true
	default:
		return false
	}
}

func (l *Loop) timings() *config.Timings {
	if l.Timings == nil {
		l.Timings = config.NewTimings()
	}
	return l.Timings
}

// fetcher is the configured Fetcher, or one with nothing wired, which refuses
// every lease that has inputs.
func (l *Loop) fetcher() *inputs.Fetcher {
	if l.Inputs != nil {
		return l.Inputs
	}
	return &inputs.Fetcher{Log: l.log()}
}

func (l *Loop) forgetInputs(id runv1.ULID) {
	if l.Inputs == nil || l.Inputs.Spool == nil {
		return
	}
	if err := l.Inputs.Spool.ForgetInputs(id); err != nil {
		l.log().Error("discarding a run's inputs", "runID", id, "error", err)
	}
}

// resumeInputs makes sure a row's inputs are in the spool before its
// acknowledgement is repeated after a restart, fetching what is missing. It
// reports whether the ack may be repeated; a row whose inputs are refused is
// deleted and the lease refused, as a fresh lease would be.
func (l *Loop) resumeInputs(ctx context.Context, r *state.Run) bool {
	if len(r.Materials.Inputs) == 0 {
		return true
	}
	lease := clusterv1.Lease{RunID: r.RunID, Epoch: r.Epoch, Inputs: r.Materials.Inputs}
	fetchCtx, cancel := context.WithTimeout(ctx, inputs.Budget(l.timings().Get().AckTimeoutSeconds))
	defer cancel()
	rejection, err := l.fetcher().Confirm(fetchCtx, lease)
	if err != nil {
		l.log().Error("fetching a run's inputs at startup", "runID", r.RunID, "epoch", r.Epoch, "error", err)
		return false
	}
	if rejection != nil {
		if err := l.Store.Delete(ctx, r.RunID); err != nil && !errors.Is(err, state.ErrNotFound) {
			l.log().Error("deleting a run whose inputs could not be fetched", "runID", r.RunID, "error", err)
			return false
		}
		l.forgetInputs(r.RunID)
		l.ack(ctx, lease, materialize.Result{Rejection: rejection})
		return false
	}
	return true
}

// maybeSweepInputs drops, at most once per interval, the inputs of runs that
// have no row: fetched for a lease that never became one. Every row that does
// exist takes its inputs with it when the reconciler removes it.
func (l *Loop) maybeSweepInputs(ctx context.Context) {
	if l.Inputs == nil || l.Inputs.Spool == nil {
		return
	}
	now := l.now()
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < inputSweepInterval {
		return
	}
	l.lastSweep = now
	l.SweepInputs(ctx)
}

// SweepInputs is one pass of the sweep.
func (l *Loop) SweepInputs(ctx context.Context) {
	if l.Inputs == nil || l.Inputs.Spool == nil {
		return
	}
	rows, err := l.Store.List(ctx)
	if err != nil {
		l.log().Error("listing runs to sweep inputs", "error", err)
		return
	}
	held := make(map[runv1.ULID]bool, len(rows))
	for i := range rows {
		held[rows[i].RunID] = true
	}
	dropped, err := l.Inputs.Spool.SweepInputs(func(id runv1.ULID) bool { return held[id] },
		l.now().Add(-inputOrphanAge))
	if err != nil {
		l.log().Error("sweeping inputs", "error", err)
		return
	}
	if dropped > 0 {
		l.log().Info("dropped the inputs of runs this host does not hold", "runs", dropped)
	}
}
