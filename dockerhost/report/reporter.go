// Package report is everything the controller owes the backend: the phase
// changes, the agent's completion, the relayed artifacts, and the heartbeat.
// It is controller/report with the row in place of the AgentRun, and it shares
// that package's Queue: the two runtimes order, shed and retry observations by
// the same code.
//
// The two paths are deliberate, as on Kubernetes. The ingest path delivers a
// change within a second; the heartbeat repeats the state of every run an
// interval later. Losing an ingest costs latency and nothing else.
package report

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/config"
	ctrlreport "github.com/automagicops/haliphron/controller/report"
	"github.com/automagicops/haliphron/controller/spool"

	"github.com/automagicops/haliphron/dockerhost/lease"
	"github.com/automagicops/haliphron/dockerhost/reconcile"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// API is the part of the Cluster API the reporter uses.
type API interface {
	Heartbeat(ctx context.Context, req clusterv1.HeartbeatRequest) (*clusterv1.HeartbeatResponse, error)
	IngestStatus(ctx context.Context, req clusterv1.StatusIngestRequest) (*clusterv1.StatusIngestResponse, error)
	IngestCompletion(ctx context.Context, req clusterv1.CompletionIngestRequest) (*clusterv1.CompletionIngestResponse, error)
	IngestArtifact(ctx context.Context, req clusterv1.ArtifactIngestRequest, body io.Reader) (*clusterv1.ArtifactIngestResponse, error)
}

// Host is what the heartbeat says about the machine.
type Host interface {
	FreeSlots(ctx context.Context) (int32, error)
	// Facts are sent when they change. QuotaExhausted is true while the
	// network check fails or the disk is under its floor.
	Facts(ctx context.Context) clusterv1.ClusterFacts
}

// Reporter is the two loops and their shared state.
type Reporter struct {
	API       API
	Store     *state.Store
	Queue     *ctrlreport.Queue
	Timings   *config.Timings
	Spool     *spool.Spool
	Host      Host
	Kicker    lease.Kicker
	ClusterID runv1.ULID
	Capacity  int32
	Version   string
	Clock     func() time.Time
	Log       *slog.Logger

	mu               sync.Mutex
	completions      []pendingCompletion
	unreachableSince time.Time
	unreachableTotal time.Duration
	startedAt        time.Time
	reportComplete   bool
	lastFacts        *clusterv1.ClusterFacts
	wake             chan struct{}
	once             sync.Once
}

type pendingCompletion struct {
	runID      runv1.ULID
	epoch      int64
	attempt    int32
	receivedAt time.Time
	report     runv1.CompletionReport
}

func (r *Reporter) init() {
	r.once.Do(func() {
		if r.Queue == nil {
			r.Queue = ctrlreport.NewQueue(0)
		}
		if r.Timings == nil {
			r.Timings = config.NewTimings()
		}
		if r.Clock == nil {
			r.Clock = time.Now
		}
		if r.Log == nil {
			r.Log = slog.Default()
		}
		r.wake = make(chan struct{}, 1)
		r.startedAt = r.Clock()
	})
}

// Observed implements reconcile.Notifier.
func (r *Reporter) Observed(obs clusterv1.RunObservation) {
	r.init()
	r.Queue.Observed(obs)
	r.nudge()
}

// Forget implements reconcile.Notifier.
func (r *Reporter) Forget(runID runv1.ULID) {
	r.init()
	r.Queue.Forget(runID)
	r.mu.Lock()
	kept := r.completions[:0]
	for _, c := range r.completions {
		if c.runID != runID {
			kept = append(kept, c)
		}
	}
	r.completions = kept
	r.mu.Unlock()
}

// Completion implements callback.Sink. It never blocks on the network.
func (r *Reporter) Completion(runID runv1.ULID, epoch int64, attempt int32, report runv1.CompletionReport) {
	r.init()
	r.mu.Lock()
	r.completions = append(r.completions, pendingCompletion{
		runID: runID, epoch: epoch, attempt: attempt, receivedAt: r.Clock(), report: report,
	})
	r.mu.Unlock()
	r.nudge()
}

// Artifact implements callback.Sink.
func (r *Reporter) Artifact(entry spool.Entry, body io.Reader, budget int64) (spool.Entry, error) {
	if r.Spool == nil {
		return spool.Entry{}, errors.New("report: this controller has no artifact spool")
	}
	return r.Spool.Write(entry, body, budget)
}

// Forwarded implements callback.Sink.
func (r *Reporter) Forwarded() { r.init(); r.nudge() }

// Spent is what a run holds in the spool.
func (r *Reporter) Spent(id runv1.ULID) int64 {
	if r.Spool == nil {
		return 0
	}
	return r.Spool.Spent(id)
}

// ForgetArtifacts implements reconcile.ArtifactForgetter.
func (r *Reporter) ForgetArtifacts(id runv1.ULID) {
	if r.Spool == nil {
		return
	}
	if err := r.Spool.Forget(id); err != nil {
		r.log().Error("could not discard a spool", "runID", id, "error", err)
	}
}

func (r *Reporter) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Reporter) nudge() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Start runs the ingest path until the context ends.
func (r *Reporter) Start(ctx context.Context) error {
	r.init()
	backoff := clusterapi.DefaultBackoff()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.wake:
		case <-time.After(time.Second):
		}
		if err := r.Flush(ctx); err != nil {
			if !clusterapi.Wait(ctx, backoff.Next()) {
				return nil
			}
			continue
		}
		backoff.Reset()
	}
}

// Flush delivers artifacts, then completions, then observations: a report
// never arrives ahead of the objects it names.
func (r *Reporter) Flush(ctx context.Context) error {
	r.init()
	if err := r.flushArtifacts(ctx); err != nil {
		return err
	}
	if err := r.flushCompletions(ctx); err != nil {
		return err
	}
	return r.flushObservations(ctx)
}

func (r *Reporter) flushObservations(ctx context.Context) error {
	batch := r.Queue.Take(clusterv1.MaxStatusReports)
	if len(batch) == 0 {
		return nil
	}
	resp, err := r.API.IngestStatus(ctx, clusterv1.StatusIngestRequest{ClusterID: r.ClusterID, Reports: batch})
	if err != nil {
		r.markUnreachable()
		r.Queue.Return(batch)
		r.log().Warn("ingesting observations", "count", len(batch), "error", err)
		return err
	}
	r.markReachable()
	r.applyResults(ctx, resp.Results, batch)
	return nil
}

func (r *Reporter) flushCompletions(ctx context.Context) error {
	r.mu.Lock()
	pending := r.completions
	r.completions = nil
	r.mu.Unlock()

	var failed []pendingCompletion
	var firstErr error
	for _, c := range pending {
		resp, err := r.API.IngestCompletion(ctx, clusterv1.CompletionIngestRequest{
			ClusterID: r.ClusterID, RunID: c.runID, Epoch: c.epoch, Attempt: c.attempt,
			ReceivedAt: &c.receivedAt, Completion: c.report,
		})
		if err != nil {
			if clusterapi.ActionOf(err) == clusterv1.ActionAbandon {
				r.abandon(ctx, c.runID)
				continue
			}
			r.markUnreachable()
			failed = append(failed, c)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		r.markReachable()
		if resp.Accepted || resp.Duplicate {
			r.mutate(ctx, c.runID, func(row *state.Run) bool {
				if row.Epoch != c.epoch || row.Status.Reported.CompletionDelivered {
					return false
				}
				row.Status.Reported.CompletionDelivered = true
				row.Status.Reported.LastDeliveredAt = ptr(r.Clock())
				return true
			})
		}
		r.recordCommands(ctx, resp.Commands)
	}
	if len(failed) > 0 {
		r.mu.Lock()
		r.completions = append(failed, r.completions...)
		r.mu.Unlock()
	}
	return firstErr
}

// flushArtifacts forwards what is spooled, oldest first, and deletes each
// spooled copy only after the backend has acknowledged it.
func (r *Reporter) flushArtifacts(ctx context.Context) error {
	if r.Spool == nil {
		return nil
	}
	pending, err := r.Spool.Pending()
	if err != nil {
		return err
	}
	var firstErr error
	for _, entry := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := r.forwardOne(ctx, entry)
		switch {
		case err == nil:
			if err := r.Spool.Done(entry); err != nil {
				r.log().Error("could not drop a forwarded artifact", "runID", entry.RunID, "key", entry.Key, "error", err)
			}
		case clusterapi.ActionOf(err) == clusterv1.ActionAbandon:
			r.log().Warn("discarding the spool of a run this host no longer holds", "runID", entry.RunID)
			if err := r.Spool.Forget(entry.RunID); err != nil {
				r.log().Error("could not discard a spool", "runID", entry.RunID, "error", err)
			}
		case clusterapi.ActionOf(err) == clusterv1.ActionFatal:
			r.log().Error("the control plane refused an artifact outright; dropping it",
				"runID", entry.RunID, "key", entry.Key, "error", err)
			if err := r.Spool.Done(entry); err != nil {
				r.log().Error("could not drop a refused artifact", "runID", entry.RunID, "error", err)
			}
		default:
			r.markUnreachable()
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr == nil {
		if len(pending) > 0 {
			r.markReachable()
		}
		r.markRelayed(ctx)
	}
	return firstErr
}

func (r *Reporter) forwardOne(ctx context.Context, entry spool.Entry) error {
	body, err := r.Spool.Open(entry)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	_, err = r.API.IngestArtifact(ctx, clusterv1.ArtifactIngestRequest{
		ClusterID: r.ClusterID, RunID: entry.RunID, Epoch: entry.Epoch, Attempt: entry.Attempt,
		Key: entry.Key, ContentType: entry.ContentType, SHA256: entry.SHA256, SizeBytes: entry.SizeBytes,
	}, body)
	return err
}

// markRelayed flips ArtifactsRelayed to true on every row whose spool is now
// empty. It is what releases the reaper.
func (r *Reporter) markRelayed(ctx context.Context) {
	rows, err := r.Store.List(ctx)
	if err != nil {
		return
	}
	for i := range rows {
		row := &rows[i]
		if row.Status.ArtifactsRelayed == nil || *row.Status.ArtifactsRelayed || r.Spool.Spent(row.RunID) > 0 {
			continue
		}
		r.mutate(ctx, row.RunID, func(cur *state.Run) bool {
			if cur.Status.ArtifactsRelayed == nil || *cur.Status.ArtifactsRelayed || r.Spool.Spent(cur.RunID) > 0 {
				return false
			}
			t := true
			cur.Status.ArtifactsRelayed = &t
			return true
		})
		if r.Kicker != nil {
			r.Kicker.Enqueue(row.RunID)
		}
	}
}

// Heartbeat runs the periodic half until the context ends.
func (r *Reporter) Heartbeat(ctx context.Context) error {
	r.init()
	backoff := clusterapi.DefaultBackoff()
	for {
		interval := time.Duration(r.Timings.Get().HeartbeatIntervalSeconds) * time.Second
		if interval <= 0 {
			interval = 10 * time.Second
		}
		if err := r.Beat(ctx); err != nil {
			if !clusterapi.Wait(ctx, backoff.Next()) {
				return nil
			}
			continue
		}
		backoff.Reset()
		if !clusterapi.Wait(ctx, interval) {
			return nil
		}
	}
}

// Beat sends one heartbeat and applies everything that came back.
func (r *Reporter) Beat(ctx context.Context) error {
	r.init()
	runs, err := r.observeAll(ctx)
	if err != nil {
		return err
	}
	free := int32(0)
	var facts *clusterv1.ClusterFacts
	if r.Host != nil {
		if n, err := r.Host.FreeSlots(ctx); err == nil {
			free = n
		} else {
			r.log().Warn("counting free slots for the heartbeat", "error", err)
		}
		f := r.Host.Facts(ctx)
		r.mu.Lock()
		if r.lastFacts == nil || !reflect.DeepEqual(*r.lastFacts, f) {
			facts = &f
		}
		r.mu.Unlock()
	}

	r.mu.Lock()
	complete := r.reportComplete
	r.mu.Unlock()
	req := clusterv1.HeartbeatRequest{
		FreeSlots:      free,
		CapacitySlots:  r.Capacity,
		ReportComplete: complete,
		Runs:           runs,
		Controller: &clusterv1.ControllerHealth{
			Version:                   r.Version,
			UptimeSeconds:             int64(r.Clock().Sub(r.startedAt).Seconds()),
			BackendUnreachableSeconds: int64(r.unreachableSeconds()),
			LeaseQueueDepth:           int32(r.Queue.Depth()),
		},
		Cluster: facts,
	}
	resp, err := r.API.Heartbeat(ctx, req)
	if err != nil {
		r.markUnreachable()
		r.log().Warn("heartbeat", "error", err)
		return err
	}
	r.markReachable()
	r.mu.Lock()
	r.reportComplete = true
	if facts != nil {
		r.lastFacts = facts
	}
	r.mu.Unlock()

	r.Timings.Set(resp.Timings)
	r.applyResults(ctx, resp.Observations, runs)
	r.recordCommands(ctx, resp.Commands)
	r.checkUnknown(ctx, resp.UnknownRuns)
	return nil
}

// observeAll is the current state of every run this host holds, read from the
// store — which is what makes the answer survive a restart.
func (r *Reporter) observeAll(ctx context.Context) ([]clusterv1.RunObservation, error) {
	rows, err := r.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	now := r.Clock()
	out := make([]clusterv1.RunObservation, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if row.IsAbandoned() || row.Status.Phase == "" {
			continue
		}
		if row.Status.Phase.IsTerminal() && settled(row) {
			continue
		}
		out = append(out, reconcile.ObservationFor(row, now))
		if len(out) >= clusterv1.MaxHeartbeatRuns {
			break
		}
	}
	return out, nil
}

func settled(row *state.Run) bool {
	rep := row.Status.Reported
	return rep.Attempt == max(row.Status.Attempt, 1) && rep.Phase == row.Status.Phase
}

// applyResults records what the backend accepted and acts on what it refused.
func (r *Reporter) applyResults(ctx context.Context, results []clusterv1.StatusIngestResult, sent []clusterv1.RunObservation) {
	byRun := make(map[runv1.ULID]clusterv1.RunObservation, len(sent))
	for _, obs := range sent {
		if prev, ok := byRun[obs.RunID]; !ok || obs.Attempt > prev.Attempt ||
			(obs.Attempt == prev.Attempt && obs.Phase.Rank() >= prev.Phase.Rank()) {
			byRun[obs.RunID] = obs
		}
	}
	rejected := map[runv1.ULID]bool{}
	for _, res := range results {
		rejected[res.RunID] = !res.Accepted
		if res.Accepted {
			continue
		}
		switch res.Action {
		case clusterv1.ActionAbandon:
			r.abandon(ctx, res.RunID)
		case clusterv1.ActionRetry:
			r.log().Debug("an observation was refused and will be repeated", "runID", res.RunID, "code", res.Code)
		default:
			r.log().Warn("an observation was refused", "runID", res.RunID, "code", res.Code, "action", res.Action)
		}
	}
	for runID, obs := range byRun {
		if rejected[runID] {
			continue
		}
		r.mutate(ctx, runID, func(row *state.Run) bool {
			rep := row.Status.Reported
			if row.Epoch != obs.Epoch || rep.Attempt > obs.Attempt ||
				(rep.Attempt == obs.Attempt && rep.Phase.Rank() >= obs.Phase.Rank()) {
				return false
			}
			row.Status.Reported.Attempt = obs.Attempt
			row.Status.Reported.Phase = obs.Phase
			row.Status.Reported.LastDeliveredAt = ptr(r.Clock())
			return true
		})
		if obs.Phase.IsTerminal() && r.Kicker != nil {
			r.Kicker.Enqueue(runID)
		}
	}
}

// checkUnknown answers the backend's reconciliation question.
func (r *Reporter) checkUnknown(ctx context.Context, unknown []runv1.ULID) {
	for _, runID := range unknown {
		row, err := r.Store.Get(ctx, runID)
		switch {
		case errors.Is(err, state.ErrNotFound):
			// Saying nothing is the answer: the backend sets Unknown at once.
			r.log().Warn("the control plane believes this run is here; it is not", "runID", runID)
		case err != nil:
			r.log().Error("checking a run the control plane asked about", "runID", runID, "error", err)
		default:
			if !row.IsAbandoned() && row.Status.Phase != "" {
				r.Observed(reconcile.ObservationFor(&row, r.Clock()))
			}
		}
	}
}

func (r *Reporter) recordCommands(ctx context.Context, commands []clusterv1.Command) {
	now := r.Clock()
	for _, cmd := range commands {
		_, err := r.Store.Mutate(ctx, cmd.RunID, func(row *state.Run) bool { return lease.Record(row, cmd, now) })
		if errors.Is(err, state.ErrNotFound) {
			continue
		}
		if err != nil {
			r.log().Error("recording a command", "runID", cmd.RunID, "type", cmd.Type, "error", err)
			continue
		}
		if cmd.Type == clusterv1.CommandAbandon {
			r.Forget(cmd.RunID)
		}
		if r.Kicker != nil {
			r.Kicker.Enqueue(cmd.RunID)
		}
	}
}

func (r *Reporter) abandon(ctx context.Context, runID runv1.ULID) {
	r.Forget(runID)
	r.ForgetArtifacts(runID)
	r.recordCommands(ctx, []clusterv1.Command{{Type: clusterv1.CommandAbandon, RunID: runID}})
}

func (r *Reporter) mutate(ctx context.Context, runID runv1.ULID, fn func(*state.Run) bool) {
	if _, err := r.Store.Mutate(ctx, runID, fn); err != nil && !errors.Is(err, state.ErrNotFound) {
		r.log().Error("recording delivery", "runID", runID, "error", err)
	}
}

func (r *Reporter) markUnreachable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unreachableSince.IsZero() {
		r.unreachableSince = r.Clock()
	}
}

func (r *Reporter) markReachable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.unreachableSince.IsZero() {
		r.unreachableTotal += r.Clock().Sub(r.unreachableSince)
		r.unreachableSince = time.Time{}
	}
}

func (r *Reporter) unreachableSeconds() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := r.unreachableTotal
	if !r.unreachableSince.IsZero() {
		total += r.Clock().Sub(r.unreachableSince)
	}
	return total.Seconds()
}

func ptr[T any](v T) *T { return &v }
