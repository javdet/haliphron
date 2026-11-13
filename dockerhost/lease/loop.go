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

	Network   string
	ClusterID runv1.ULID
	Capacity  int32
	Runtimes  []runv1.AgentType

	Clock func() time.Time
	Log   *slog.Logger

	PollSlack time.Duration
}

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
		wait := time.Duration(l.Timings.Get().MaxWaitSeconds) * time.Second
		pollCtx, cancel := context.WithTimeout(ctx, wait+l.PollSlack)
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

// PollOnce performs one long poll and makes whatever came back durable.
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
	resp, err := l.API.Lease(ctx, clusterv1.LeaseRequest{
		FreeSlots:     free,
		CapacitySlots: l.Capacity,
		Runtimes:      l.Runtimes,
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
func (l *Loop) handle(ctx context.Context, lease clusterv1.Lease) {
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
