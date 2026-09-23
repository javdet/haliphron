package app

import (
	"context"
	"time"

	"github.com/automagicops/haliphron/backend/store"
)

// The sweepers: the passage of time, applied.
//
// There are two deadlines and therefore two scanners, and their consequences
// are opposite — an ack timeout returns the work to the queue, a lease timeout
// deliberately does not — so they are never one query. The rest of the pass is
// everything else that is true only because time went by: a cluster that
// stopped reporting, a run nobody has placed yet, a result sitting in storage
// that nothing has collected.
//
// One pass does all of it in a fixed order, so that a run returned to the queue
// by the ack scanner is placed in the same tick rather than in the next one.

// SweepResult is what one pass did. Returned rather than only logged because
// the tests assert on it and because it is the shape of the metrics.
type SweepResult struct {
	// AckExpired counts every expired ack; AckExhausted is the subset that hit
	// the ceiling and failed the run instead of requeueing it.
	AckExpired    int
	AckExhausted  int
	LeaseExpired  int
	Placed        int
	Recovered     int
	StaleClusters int
}

// Sweep performs one pass.
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var out SweepResult

	expired, err := s.store.ExpireAcks(ctx, s.limits.MaxAckExpiries)
	if err != nil {
		return out, err
	}
	out.AckExpired = len(expired)
	for _, e := range expired {
		// The per-run MCP token went out in the lease that just expired, and
		// that lease is dead either way: the next one mints its own. Revoking
		// here is safe because nothing starts before the ack, so no pod ever
		// held it, and it is what keeps a run cycling through ack timeouts from
		// leaving a live credential behind for every cycle. A failure is logged
		// rather than returned: the expiry has already committed, the token
		// still dies at its TTL, and aborting would skip placement for
		// everything else in the pass.
		if err := s.store.RevokeRunTokens(ctx, e.RunID); err != nil {
			s.log.Error("could not revoke the tokens of an expired lease", "run", e.RunID, "error", err)
		}

		if !e.Exhausted() {
			// The epoch rose inside the statement that requeued it, which is
			// the fence closing: from here the controller that was holding the
			// run reports under a strictly smaller epoch and is told to
			// abandon.
			s.log.Warn("ack deadline expired; work returned to the queue",
				"run", e.RunID, "cluster", e.ClusterID, "epoch", e.Epoch,
				"expiries", e.Expiries, "ceiling", s.limits.MaxAckExpiries)
			continue
		}

		// Past anything a restart explains. The controller keeps taking the
		// lease and its ack never arrives, and requeueing again would only
		// repeat that every ackTimeout for as long as nobody looked.
		out.AckExhausted++
		s.log.Error("ack deadline expired too many times; run failed",
			"run", e.RunID, "cluster", e.ClusterID, "epoch", e.Epoch, "expiries", e.Expiries)
		if err := s.store.Audit(ctx, store.AuditEntry{
			Actor: "expiry", ActorKind: "system", Action: store.AuditAckExhausted,
			SubjectKind: "run", SubjectID: string(e.RunID),
			RunID: e.RunID, ClusterID: e.ClusterID,
			Payload: map[string]any{"reason": "AckTimeoutExhausted", "expiries": e.Expiries},
		}); err != nil {
			return out, err
		}
	}

	lost, err := s.store.ExpireLeases(ctx)
	if err != nil {
		return out, err
	}
	out.LeaseExpired = len(lost)
	for _, l := range lost {
		// Unknown, never Queued. After the ack a Job may be executing this
		// second, and handing the same run to a second cluster would run the
		// agent twice, pay for the model twice and push two branches.
		s.log.Warn("lease deadline expired; run is Unknown",
			"run", l.RunID, "cluster", l.ClusterID, "epoch", l.Epoch, "phase", l.Phase)
		if err := s.store.Audit(ctx, store.AuditEntry{
			Actor: "expiry", ActorKind: "system", Action: store.AuditRunUnknown,
			SubjectKind: "run", SubjectID: string(l.RunID),
			RunID: l.RunID, ClusterID: l.ClusterID,
			Payload: map[string]any{"reason": "LeaseDeadlineExceeded", "phase": string(l.Phase)},
		}); err != nil {
			return out, err
		}
	}

	stale, err := s.store.MarkStaleClusters(ctx, s.staleAfter())
	if err != nil {
		return out, err
	}
	out.StaleClusters = len(stale)
	for _, id := range stale {
		s.log.Warn("cluster stopped reporting", "cluster", id, "staleAfter", s.staleAfter())
		if err := s.store.Audit(ctx, store.AuditEntry{
			Actor: "expiry", ActorKind: "system", Action: store.AuditClusterStale,
			SubjectKind: "cluster", SubjectID: string(id), ClusterID: id,
		}); err != nil {
			return out, err
		}
	}

	placed, err := s.store.PlaceQueued(ctx, 100)
	if err != nil {
		return out, err
	}
	out.Placed = placed

	pending, err := s.store.PendingResults(ctx, 50)
	if err != nil {
		return out, err
	}
	for _, p := range pending {
		recovered, err := s.recoverResult(ctx, p)
		if err != nil {
			// One unreadable object must not stop the pass: the others are
			// independent, and this one will be tried again next time.
			s.log.Error("could not recover a result", "run", p.RunID, "error", err)
			continue
		}
		if recovered {
			out.Recovered++
		}
	}

	if out.AckExpired > out.AckExhausted || out.Placed > 0 {
		// Work became available to somebody. Releasing the long polls now is
		// the difference between a requeued run starting immediately and one
		// waiting out a poll interval it has no reason to.
		s.work.broadcast()
	}
	return out, nil
}

// RunSweeper sweeps on an interval until the context ends.
//
// The interval is a fraction of the shortest deadline it enforces. The ack
// timeout is 60 seconds by default, so a pass every few seconds detects an
// expiry within a few seconds of it happening; sweeping once a minute would
// make the effective ack timeout somewhere between one and two minutes, which
// is the sort of imprecision that gets diagnosed as a slow controller.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && !ctxDone(ctx) {
				s.log.Error("sweep failed", "error", err)
			}
		}
	}
}

// RunReaper purges what has expired but is not state: spent idempotency keys.
// It is separate from the sweep because it is housekeeping on a table nothing
// waits for, and running it every few seconds would be a delete storm for no
// reason.
func (s *Service) RunReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.store.PurgeExpired(ctx)
			if err != nil && !ctxDone(ctx) {
				s.log.Error("purge failed", "error", err)
				continue
			}
			if n > 0 {
				s.log.Info("expired idempotency keys purged", "rows", n)
			}
		}
	}
}
