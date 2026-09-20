package report

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/agentrun"
	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/config"
	"github.com/automagicops/haliphron/controller/spool"
)

// API is the part of the Cluster API the reporter uses.
type API interface {
	Heartbeat(ctx context.Context, req clusterv1.HeartbeatRequest) (*clusterv1.HeartbeatResponse, error)
	IngestStatus(ctx context.Context, req clusterv1.StatusIngestRequest) (*clusterv1.StatusIngestResponse, error)
	IngestCompletion(ctx context.Context, req clusterv1.CompletionIngestRequest) (*clusterv1.CompletionIngestResponse, error)
	// IngestArtifact streams one relayed object. It takes a reader rather than
	// bytes because the objects on this path can be hundreds of megabytes, and
	// a controller that buffered each one would be a controller an agent can
	// exhaust with one log.
	IngestArtifact(ctx context.Context, req clusterv1.ArtifactIngestRequest, body io.Reader) (*clusterv1.ArtifactIngestResponse, error)
}

// Reporter is everything the controller says. Two loops share one queue: the
// ingest path delivers a phase change within a second of observing it, and the
// heartbeat repeats the current state of every run an interval later.
//
// They are two paths to the same rules on the far side, and the duplication is
// deliberate: losing an ingest costs latency and nothing else, because the
// heartbeat will carry the same fact, ordered by (attempt, phase rank) rather
// than by arrival. That is what makes an unreliable network a performance
// problem instead of a correctness one.
type Reporter struct {
	API       API
	K8s       client.Client
	Queue     *Queue
	Timings   *config.Timings
	Namespace string
	ClusterID runv1.ULID
	Capacity  int32
	Version   string
	// Spool is the controller's copy of what pods handed it, in relay mode.
	// Nil in object-store mode, where the pod writes straight to the store and
	// nothing passes through here.
	Spool *spool.Spool

	Clock func() time.Time
	Log   *slog.Logger

	mu          sync.Mutex
	completions []pendingCompletion
	// unreachableSince is when the control plane last stopped answering. It is
	// reported from inside the cluster because the only side that knows about a
	// disconnect is the one that was unreachable for scraping at the time.
	unreachableSince time.Time
	unreachableTotal time.Duration

	startedAt      time.Time
	reportComplete bool
	wake           chan struct{}
}

type pendingCompletion struct {
	runID      runv1.ULID
	epoch      int64
	attempt    int32
	receivedAt time.Time
	report     runv1.CompletionReport
}

// Config is what a Reporter needs from outside.
type Config struct {
	API       API
	K8s       client.Client
	Queue     *Queue
	Timings   *config.Timings
	Namespace string
	ClusterID runv1.ULID
	Capacity  int32
	Version   string
	Spool     *spool.Spool
	Clock     func() time.Time
	Log       *slog.Logger
}

// New builds a reporter.
func New(cfg Config) (*Reporter, error) {
	if cfg.API == nil || cfg.K8s == nil {
		return nil, errors.New("report: API and K8s are required")
	}
	if cfg.Queue == nil {
		cfg.Queue = NewQueue(0)
	}
	if cfg.Timings == nil {
		cfg.Timings = config.NewTimings()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Reporter{
		API:       cfg.API,
		K8s:       cfg.K8s,
		Queue:     cfg.Queue,
		Timings:   cfg.Timings,
		Namespace: cfg.Namespace,
		ClusterID: cfg.ClusterID,
		Capacity:  cfg.Capacity,
		Version:   cfg.Version,
		Spool:     cfg.Spool,
		Clock:     cfg.Clock,
		Log:       cfg.Log,
		wake:      make(chan struct{}, 1),
	}, nil
}

// NeedLeaderElection is false for the ingest path. Forwarding an observation or
// a completion is idempotent on the far side, and the pod's callback lands on
// whichever replica the Service picked — which must therefore be able to send
// it on. The heartbeat is the half that needs a single voice, and it is a
// runnable of its own.
func (r *Reporter) NeedLeaderElection() bool { return false }

// Observed implements agentrun.Notifier.
func (r *Reporter) Observed(obs clusterv1.RunObservation) {
	r.Queue.Observed(obs)
	r.nudge()
}

// Forget implements agentrun.Notifier.
func (r *Reporter) Forget(runID runv1.ULID) {
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

// Completion accepts the pod's report for forwarding. It never blocks on the
// network: the pod is waiting on this call, and a control plane outage must not
// turn into an agent container that cannot exit.
func (r *Reporter) Completion(runID runv1.ULID, epoch int64, attempt int32, report runv1.CompletionReport) {
	r.mu.Lock()
	r.completions = append(r.completions, pendingCompletion{
		runID: runID, epoch: epoch, attempt: attempt,
		receivedAt: r.Clock(), report: report,
	})
	r.mu.Unlock()
	r.nudge()
}

func (r *Reporter) nudge() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Start runs the ingest path until the context ends.
func (r *Reporter) Start(ctx context.Context) error {
	if r.startedAt.IsZero() {
		r.startedAt = r.Clock()
	}
	r.ingestLoop(ctx)
	return nil
}

// Heartbeat is the periodic half, as a separate runnable because it is the half
// that must not be duplicated: two controllers beating for one cluster would
// each renew the other's leases and each answer the other's reconciliation
// questions, and the backend would believe both.
func (r *Reporter) Heartbeat() manager.Runnable {
	return &heartbeatRunnable{reporter: r}
}

type heartbeatRunnable struct{ reporter *Reporter }

func (h *heartbeatRunnable) NeedLeaderElection() bool { return true }

func (h *heartbeatRunnable) Start(ctx context.Context) error {
	if h.reporter.startedAt.IsZero() {
		h.reporter.startedAt = h.reporter.Clock()
	}
	h.reporter.heartbeatLoop(ctx)
	return nil
}

// ingestLoop is the low-latency path. It waits for something to say, says it,
// and on failure puts it back: the heartbeat is the safety net, so nothing here
// needs to be more than best effort.
func (r *Reporter) ingestLoop(ctx context.Context) {
	backoff := clusterapi.DefaultBackoff()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(time.Second):
		}

		if err := r.Flush(ctx); err != nil {
			if !clusterapi.Wait(ctx, backoff.Next()) {
				return
			}
			continue
		}
		backoff.Reset()
	}
}

// Flush delivers everything waiting, in the one order that is safe.
//
// Artifacts first, because a completion names references the backend resolves:
// forwarding the report ahead of the objects it describes leaves a window in
// which the control plane holds a result pointing at nothing, and the
// CompletedWithoutResult recovery path reads that window as a lost result.
//
// Then completions, because a report is owed to a run that has already
// finished. Then the phase observations, which the heartbeat would carry anyway.
//
// A failure at any step stops the rest. That is deliberate rather than
// pessimistic: every one of these failures is the same failure — the control
// plane is unreachable — and pressing on would turn one error into three and
// three retries into nine.
//
// It is exported so that the delivery path can be driven a step at a time.
func (r *Reporter) Flush(ctx context.Context) error {
	if err := r.flushArtifacts(ctx); err != nil {
		return err
	}
	if err := r.flushCompletions(ctx); err != nil {
		return err
	}
	_, err := r.flushObservations(ctx)
	return err
}

// flushObservations sends one batch. It reports whether anything went out.
func (r *Reporter) flushObservations(ctx context.Context) (bool, error) {
	batch := r.Queue.Take(clusterv1.MaxStatusReports)
	if len(batch) == 0 {
		return false, nil
	}
	resp, err := r.API.IngestStatus(ctx, clusterv1.StatusIngestRequest{
		ClusterID: r.ClusterID, Reports: batch,
	})
	if err != nil {
		r.markUnreachable()
		// Back onto the queue. The heartbeat would carry the current state
		// anyway, but a terminal phase of a superseded attempt exists nowhere
		// else once the run has moved on.
		r.Queue.Return(batch)
		r.Log.Warn("ingesting observations", "count", len(batch), "error", err)
		return false, err
	}
	r.markReachable()
	r.applyResults(ctx, resp.Results, batch)
	return true, nil
}

// flushCompletions forwards the pod's reports, unedited. A duplicate is a
// success: the backend charges the cost once, and the controller's job is only
// to make sure the report arrives at least that once.
func (r *Reporter) flushCompletions(ctx context.Context) error {
	r.mu.Lock()
	pending := r.completions
	r.completions = nil
	r.mu.Unlock()

	var failed []pendingCompletion
	var firstErr error
	for _, c := range pending {
		resp, err := r.API.IngestCompletion(ctx, clusterv1.CompletionIngestRequest{
			ClusterID:  r.ClusterID,
			RunID:      c.runID,
			Epoch:      c.epoch,
			Attempt:    c.attempt,
			ReceivedAt: &c.receivedAt,
			Completion: c.report,
		})
		if err != nil {
			if clusterapi.ActionOf(err) == clusterv1.ActionAbandon {
				// The run was reassigned. Its report belongs to whoever holds
				// it now, and the copy in storage is what they will read.
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
			r.markCompletionDelivered(ctx, c)
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

// heartbeatLoop proves the cluster is alive, renews the leases, reconciles what
// the two sides believe and collects commands — one call because it is one
// interval, and four calls would multiply the request rate without adding a
// fact.
func (r *Reporter) heartbeatLoop(ctx context.Context) {
	backoff := clusterapi.DefaultBackoff()
	for {
		interval := time.Duration(r.Timings.Get().HeartbeatIntervalSeconds) * time.Second
		if interval <= 0 {
			interval = 10 * time.Second
		}
		if err := r.Beat(ctx); err != nil {
			if !clusterapi.Wait(ctx, backoff.Next()) {
				return
			}
			continue
		}
		backoff.Reset()
		if !clusterapi.Wait(ctx, interval) {
			return
		}
	}
}

// Beat sends one heartbeat and applies everything that came back.
func (r *Reporter) Beat(ctx context.Context) error {
	runs, err := r.observeAll(ctx)
	if err != nil {
		return err
	}

	// The first beat claims nothing about completeness. Under reportComplete
	// the backend is entitled to treat an unmentioned run as lost, and a
	// controller that has just come up is the one case where silence means
	// "not yet" rather than "gone".
	complete := r.reportComplete
	req := clusterv1.HeartbeatRequest{
		FreeSlots:      r.freeSlots(runs),
		CapacitySlots:  r.Capacity,
		ReportComplete: complete,
		Runs:           runs,
		Controller: &clusterv1.ControllerHealth{
			Version:                   r.Version,
			UptimeSeconds:             int64(r.Clock().Sub(r.startedAt).Seconds()),
			BackendUnreachableSeconds: int64(r.unreachableSeconds()),
			LeaseQueueDepth:           int32(r.Queue.Depth()),
		},
	}

	resp, err := r.API.Heartbeat(ctx, req)
	if err != nil {
		r.markUnreachable()
		r.Log.Warn("heartbeat", "error", err)
		return err
	}
	r.markReachable()
	r.reportComplete = true

	r.Timings.Set(resp.Timings)
	r.applyResults(ctx, resp.Observations, runs)
	r.recordCommands(ctx, resp.Commands)
	r.checkUnknown(ctx, resp.UnknownRuns)

	// A run the backend did not renew is a run it no longer regards as ours.
	// That is information, not an instruction: the abandon command is how it
	// says so, and acting on the absence would race a heartbeat that crossed
	// with a lease.
	return nil
}

// observeAll is the current state of every run this cluster holds, read from
// the CRs rather than from memory — which is what makes the answer survive a
// restart and a cache rebuild.
func (r *Reporter) observeAll(ctx context.Context) ([]clusterv1.RunObservation, error) {
	var list agentrunv1alpha1.AgentRunList
	if err := r.K8s.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
		return nil, fmt.Errorf("list AgentRuns: %w", err)
	}
	now := r.Clock()
	out := make([]clusterv1.RunObservation, 0, len(list.Items))
	for i := range list.Items {
		cr := &list.Items[i]
		if agentrun.IsAbandoned(cr) || cr.Status.Phase == "" {
			continue
		}
		if cr.Status.Phase.IsTerminal() && settled(cr) {
			// Already acknowledged. Repeating it every interval would make the
			// heartbeat grow with the day's history.
			continue
		}
		obs := observationOf(cr, now)
		out = append(out, obs)
		if len(out) >= clusterv1.MaxHeartbeatRuns {
			break
		}
	}
	return out, nil
}

func (r *Reporter) freeSlots(runs []clusterv1.RunObservation) int32 {
	var active int32
	for _, obs := range runs {
		if !obs.Phase.IsTerminal() {
			active++
		}
	}
	if free := r.Capacity - active; free > 0 {
		return free
	}
	return 0
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
	rejected := make(map[runv1.ULID]bool, len(results))

	for _, res := range results {
		rejected[res.RunID] = !res.Accepted
		if res.Accepted {
			continue
		}
		switch res.Action {
		case clusterv1.ActionAbandon:
			r.abandon(ctx, res.RunID)
		case clusterv1.ActionRetry:
			// Almost always a reordering inside a batch rather than a loss of
			// ownership. The next heartbeat carries the current state and
			// settles it.
			r.Log.Debug("an observation was refused and will be repeated",
				"runID", res.RunID, "code", res.Code)
		default:
			r.Log.Warn("an observation was refused",
				"runID", res.RunID, "code", res.Code, "action", res.Action)
		}
	}

	// Anything the backend did not single out was accepted: the contract makes
	// the response carry rejected rows only, because a response listing every
	// accepted row would be the heartbeat's largest field and say nothing.
	for runID, obs := range byRun {
		if rejected[runID] {
			continue
		}
		r.markDelivered(ctx, runID, obs)
	}
}

// checkUnknown answers the backend's reconciliation question. A run it believes
// is here and this controller did not mention is either one whose CR really is
// gone — the agent namespace was recreated, the controller was moved without
// its state — or one this controller is about to report anyway.
func (r *Reporter) checkUnknown(ctx context.Context, unknown []runv1.ULID) {
	for _, runID := range unknown {
		var cr agentrunv1alpha1.AgentRun
		err := r.K8s.Get(ctx, types.NamespacedName{
			Namespace: r.Namespace, Name: agentrunv1alpha1.ObjectName(runID),
		}, &cr)
		switch {
		case apierrors.IsNotFound(err):
			// Nothing to say about it, and saying nothing is the answer: the
			// backend sets Unknown at once instead of waiting out staleAfter,
			// which is the whole point of having asked.
			r.Log.Warn("the control plane believes this run is here; it is not",
				"runID", runID)
		case err != nil:
			r.Log.Error("checking a run the control plane asked about", "runID", runID, "error", err)
		default:
			r.Observed(observationOf(&cr, r.Clock()))
		}
	}
}

func (r *Reporter) recordCommands(ctx context.Context, commands []clusterv1.Command) {
	for _, cmd := range commands {
		var cr agentrunv1alpha1.AgentRun
		err := r.K8s.Get(ctx, types.NamespacedName{
			Namespace: r.Namespace, Name: agentrunv1alpha1.ObjectName(cmd.RunID),
		}, &cr)
		if apierrors.IsNotFound(err) {
			// Commands repeat until the observed state reflects them, so a
			// command for a run that is already gone is ordinary.
			continue
		}
		if err != nil {
			r.Log.Error("reading a run a command names", "runID", cmd.RunID, "error", err)
			continue
		}
		if err := agentrun.Record(ctx, r.K8s, &cr, cmd, r.Clock()); err != nil {
			r.Log.Error("recording a command", "runID", cmd.RunID, "type", cmd.Type, "error", err)
		}
	}
}

func (r *Reporter) abandon(ctx context.Context, runID runv1.ULID) {
	r.Forget(runID)
	// The artifacts go with the claim. They belong to whoever holds the run
	// now, and forwarding ours would overwrite theirs under the same key.
	r.ForgetArtifacts(runID)
	r.recordCommands(ctx, []clusterv1.Command{{Type: clusterv1.CommandAbandon, RunID: runID}})
}

// markDelivered records what the backend has accepted. This is the bookkeeping
// the TTL depends on: without it a controller restart either re-sends
// everything forever or drops the rule that a CR outlives its run until the
// outcome has been heard.
func (r *Reporter) markDelivered(ctx context.Context, runID runv1.ULID, obs clusterv1.RunObservation) {
	r.patchRun(ctx, runID, func(cr *agentrunv1alpha1.AgentRun) bool {
		reported := cr.Status.Reported
		if reported.Attempt > obs.Attempt ||
			(reported.Attempt == obs.Attempt && reported.Phase.Rank() >= obs.Phase.Rank()) {
			return false
		}
		cr.Status.Reported.Attempt = obs.Attempt
		cr.Status.Reported.Phase = obs.Phase
		cr.Status.Reported.LastDeliveredAt = ptrTime(metav1.NewTime(r.Clock()))
		return true
	})
}

func (r *Reporter) markCompletionDelivered(ctx context.Context, c pendingCompletion) {
	r.patchRun(ctx, c.runID, func(cr *agentrunv1alpha1.AgentRun) bool {
		if cr.Status.Reported.CompletionDelivered {
			return false
		}
		cr.Status.Reported.CompletionDelivered = true
		cr.Status.Reported.LastDeliveredAt = ptrTime(metav1.NewTime(r.Clock()))
		return true
	})
}

func (r *Reporter) patchRun(ctx context.Context, runID runv1.ULID, mutate func(*agentrunv1alpha1.AgentRun) bool) {
	var cr agentrunv1alpha1.AgentRun
	key := types.NamespacedName{Namespace: r.Namespace, Name: agentrunv1alpha1.ObjectName(runID)}
	if err := r.K8s.Get(ctx, key, &cr); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.Error("reading a run to record delivery", "runID", runID, "error", err)
		}
		return
	}
	patch := client.MergeFrom(cr.DeepCopy())
	if !mutate(&cr) {
		return
	}
	if err := r.K8s.Status().Patch(ctx, &cr, patch); err != nil && !apierrors.IsNotFound(err) {
		r.Log.Error("recording delivery", "runID", runID, "error", err)
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

// observationOf is the heartbeat's view of one run. It duplicates what the
// reconciler queued, on purpose: the queue is a courier and the CR is the
// record, and only the record survives a restart.
func observationOf(cr *agentrunv1alpha1.AgentRun, now time.Time) clusterv1.RunObservation {
	obs := clusterv1.RunObservation{
		RunID:        cr.Spec.RunID,
		Epoch:        cr.Spec.LeaseEpoch,
		Attempt:      max(cr.Status.Attempt, 1),
		Phase:        cr.Status.Phase,
		Reason:       cr.Status.Reason,
		Message:      cr.Status.Message,
		JobName:      cr.Status.JobName,
		PodName:      cr.Status.PodName,
		NodeName:     cr.Status.NodeName,
		ExitCode:     cr.Status.ExitCode,
		FailureClass: cr.Status.FailureClass,
		// The checkpoint rides on the observation rather than on a channel of
		// its own: it is the same fact about the same attempt, and a second
		// channel would need the same epoch check, the same ordering rule and
		// the same batching. The backend unions it, so sending the whole list
		// on every observation is idempotent rather than wasteful.
		CompletedPhases: cr.Status.CompletedPhases,
		ObservedAt:      ptrUTC(now),
	}
	if cr.Status.StartedAt != nil {
		obs.StartedAt = ptrUTC(cr.Status.StartedAt.Time)
	}
	if cr.Status.FinishedAt != nil {
		obs.FinishedAt = ptrUTC(cr.Status.FinishedAt.Time)
	}
	return obs
}

func settled(cr *agentrunv1alpha1.AgentRun) bool {
	reported := cr.Status.Reported
	return reported.Attempt == max(cr.Status.Attempt, 1) && reported.Phase == cr.Status.Phase
}

func ptrUTC(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }
