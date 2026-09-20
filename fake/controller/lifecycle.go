package controller

import (
	"context"
	"fmt"
	"net/http"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The reconcile loop, driven by hand. There is no informer and no Job
// controller: a test says what the pod did, and this code does everything
// around that — materialise, acknowledge, count attempts, obey commands and
// report.

// Sync performs one pass: take work, materialise it, acknowledge it, create the
// Jobs. Acknowledging is not skipped for work already held — a controller that
// restarted between creating the CR and acking must repeat the ack under the
// same epoch, and this is where that happens.
func (c *Controller) Sync(ctx context.Context) (int, error) {
	leases, err := c.Poll(ctx)
	if err != nil {
		return 0, err
	}
	for _, lease := range leases {
		c.take(lease)
	}
	if err := c.ackPending(ctx); err != nil {
		return len(leases), err
	}
	return len(leases), nil
}

// Poll is one long poll for work.
func (c *Controller) Poll(ctx context.Context) ([]clusterv1.Lease, error) {
	c.mu.Lock()
	free := c.capacity - int32(len(c.runs))
	if free < 0 {
		free = 0
	}
	req := clusterv1.LeaseRequest{
		FreeSlots: free, Runtimes: c.runtimes, WaitSeconds: 1, CapacitySlots: c.capacity,
	}
	auth := c.authed()
	cluster := c.clusterID
	c.mu.Unlock()

	var out clusterv1.LeaseResponse
	status, problem, err := c.call(ctx, "/clusters/"+string(cluster)+"/leases", auth, req, &out)
	if err != nil {
		return nil, err
	}
	if problem != nil {
		return nil, c.onProblem("", problem)
	}
	if status == http.StatusNoContent {
		// No work, and the poll expired normally. Re-poll at once and without
		// backoff: backoff here turns a long poll into polling.
		return nil, nil
	}
	if len(out.Leases) > 0 {
		// Identifiers and a count. The body holds a git token, a model key and
		// presigned URLs, and this is the line where a controller usually
		// leaks them.
		c.note("leases received: %d", len(out.Leases))
	}
	return out.Leases, nil
}

// take materialises one lease. A rejection is recorded on the state so that
// ackPending sends the negative ack — the work never starts, and nothing was
// spent.
func (c *Controller) take(lease clusterv1.Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state, rejection := c.materialize(lease)
	if rejection != nil {
		c.noteLocked("refusing run %s: %s", lease.RunID, rejection.Code)
		c.runs[lease.RunID] = &runState{
			runID: lease.RunID, epoch: lease.Epoch, attempt: lease.Attempt,
			lease: lease, rejection: rejection,
		}
		return
	}
	c.runs[lease.RunID] = state
}

// ackPending acknowledges everything materialised and not yet acknowledged.
func (c *Controller) ackPending(ctx context.Context) error {
	c.mu.Lock()
	var pending []*runState
	for _, r := range c.runs {
		if !r.acked && !c.abandoned[r.runID] {
			pending = append(pending, r)
		}
	}
	cluster := c.clusterID
	c.mu.Unlock()

	for _, r := range pending {
		req := clusterv1.AckRequest{
			ClusterID: cluster, Epoch: r.epoch,
			CRName: agentrunv1alpha1.ObjectName(r.runID), Namespace: c.namespace,
		}
		if r.rejection != nil {
			no := false
			req.Accepted, req.Rejection = &no, r.rejection
		}

		c.mu.Lock()
		auth := c.authed()
		c.mu.Unlock()

		var out clusterv1.AckResponse
		_, problem, err := c.call(ctx, "/leases/"+string(r.runID)+"/ack", auth, req, &out)
		if err != nil {
			return err
		}
		if problem != nil {
			if err := c.onProblem(r.runID, problem); err != nil {
				return err
			}
			continue
		}

		c.mu.Lock()
		c.record(ReportAck, r, "")
		if r.rejection != nil {
			// Refused work is not ours and never was. Drop it: the backend has
			// raised the epoch and will offer it elsewhere.
			delete(c.runs, r.runID)
			c.mu.Unlock()
			continue
		}
		r.acked = true
		c.mu.Unlock()

		// Commands that queued while the work was being materialised arrive on
		// the ack. Acting on them before the Job exists is the point: a cancel
		// that waited for the next heartbeat would first start a Job that must
		// immediately be killed.
		c.applyCommands(ctx, out.Commands)

		c.mu.Lock()
		if _, live := c.runs[r.runID]; live {
			c.objects.putJob(c.buildJob(r))
			c.updateStatus(r)
			c.noteLocked("job created run=%s attempt=%d", r.runID, r.attempt)
		}
		c.mu.Unlock()

		if err := c.reportPhase(ctx, r.runID, runv1.PhasePending, nil); err != nil {
			return err
		}
	}
	return nil
}

// Heartbeat sends every non-terminal run this cluster holds and acts on what
// comes back. ReportComplete is true because this fake always knows its full
// set: the partial case belongs to a controller whose informer cache is still
// warming, and is provoked with HeartbeatPartial.
func (c *Controller) Heartbeat(ctx context.Context) (clusterv1.HeartbeatResponse, error) {
	return c.heartbeat(ctx, true)
}

// HeartbeatPartial reports without claiming the list is complete — a controller
// that has just come up. The backend must draw no conclusion from a run being
// absent.
func (c *Controller) HeartbeatPartial(ctx context.Context) (clusterv1.HeartbeatResponse, error) {
	return c.heartbeat(ctx, false)
}

func (c *Controller) heartbeat(ctx context.Context, complete bool) (clusterv1.HeartbeatResponse, error) {
	c.mu.Lock()
	req := clusterv1.HeartbeatRequest{
		FreeSlots:      c.capacity - int32(len(c.runs)),
		CapacitySlots:  c.capacity,
		ReportComplete: complete,
		Controller:     &clusterv1.ControllerHealth{Version: c.version},
		Cluster: &clusterv1.ClusterFacts{
			K8sVersion: c.k8sVersion, CRDVersions: c.crdVersions,
			Runtimes: c.runtimes, QuotaExhausted: c.quotaExhausted,
		},
	}
	reported := map[runv1.ULID]runv1.Phase{}
	for _, r := range c.runs {
		if c.abandoned[r.runID] {
			continue
		}
		// A run drops out of the heartbeat when it is finished *and* the
		// backend has said so. Dropping it at the terminal phase instead
		// would lose every outcome produced while the control plane was
		// away — which is exactly when it matters.
		if r.terminal != "" && r.reportedPhase == r.phase {
			continue
		}
		req.Runs = append(req.Runs, c.observation(r, r.phase))
		reported[r.runID] = r.phase
	}
	auth := c.authed()
	cluster := c.clusterID
	c.mu.Unlock()

	var out clusterv1.HeartbeatResponse
	_, problem, err := c.call(ctx, "/clusters/"+string(cluster)+"/heartbeat", auth, req, &out)
	if err != nil {
		return out, err
	}
	if problem != nil {
		return out, c.onProblem("", problem)
	}

	rejected := map[runv1.ULID]bool{}
	for _, res := range out.Observations {
		rejected[res.RunID] = true
		c.applyResult(res)
	}
	c.mu.Lock()
	for id, phase := range reported {
		if r, ok := c.runs[id]; ok && !rejected[id] {
			r.reportedPhase = phase
		}
	}
	c.mu.Unlock()
	c.applyCommands(ctx, out.Commands)

	// A run the backend thinks is here and this controller did not mention.
	// Answering at once is what turns "the agent namespace was recreated" from
	// a batch of manual triage into one heartbeat.
	for _, id := range out.UnknownRuns {
		c.note("backend believes run %s is here; it is not", id)
	}
	return out, nil
}

// Start moves a run to Running, as observing the pod would.
func (c *Controller) Start(ctx context.Context, id runv1.ULID) error {
	if err := c.reportPhase(ctx, id, runv1.PhaseStarting, nil); err != nil {
		return err
	}
	return c.reportPhase(ctx, id, runv1.PhaseRunning, nil)
}

// Outcome is how the simulated pod ended. Only ExitCode is required: the phase,
// the failure class and the report status all follow from it through the shared
// table, which is the point of having the table in api/run/v1.
type Outcome struct {
	ExitCode int32
	// Reason and Message are what the controller observed — OOMKilled,
	// DeadlineExceeded — not what the pod said.
	Reason  string
	Message string

	Summary string
	CostUSD runv1.MoneyUSD
	PRURL   string

	// SkipCompletion models the pod's callback never arriving: the controller
	// died between the webhook and forwarding it, or the pod was killed after
	// uploading. The backend must reach CompletedWithoutResult and recover the
	// report from storage.
	SkipCompletion bool
}

// Finish ends an attempt. A retriable class inside the budget does not reach
// the backend as a terminal phase at all: the controller starts another attempt
// locally, which is what `attempt` counts and why the epoch does not move.
func (c *Controller) Finish(ctx context.Context, id runv1.ULID, out Outcome) error {
	c.mu.Lock()
	r, ok := c.runs[id]
	if !ok || c.abandoned[id] {
		c.mu.Unlock()
		return fmt.Errorf("finish %s: not held by this cluster", id)
	}
	class := runv1.FailureClassForExitCode(out.ExitCode)
	phase := runv1.PhaseForExitCode(out.ExitCode)
	if r.cancelled && r.terminal == "" && out.ExitCode != runv1.ExitSuccess {
		// A cancellation that arrived while the pod was still running. If it
		// had exited zero first, the run succeeded — the first terminal phase
		// wins on this side too.
		phase, class = runv1.PhaseCancelled, runv1.FailureNone
	}
	budget := int32(3)
	if r.lease.Spec.Retry != nil && r.lease.Spec.Retry.MaxInfraRetries != nil {
		budget = *r.lease.Spec.Retry.MaxInfraRetries
	}
	retriable := class.Retriable() && r.infraRetries < budget && !r.cancelled
	c.mu.Unlock()

	if retriable {
		return c.retryLocally(ctx, id, out, class)
	}

	exit := out.ExitCode
	extra := func(o *clusterv1.RunObservation) {
		o.ExitCode = &exit
		o.FailureClass = class
		o.Reason = out.Reason
		o.Message = out.Message
	}
	if err := c.reportPhase(ctx, id, phase, extra); err != nil {
		return err
	}
	if out.SkipCompletion {
		return nil
	}
	return c.reportCompletion(ctx, id, out, class, phase)
}

// retryLocally starts another attempt without asking the backend. The budget
// lives on the CR's status rather than in memory, so a controller restart does
// not hand a failing run a fresh set of attempts.
func (c *Controller) retryLocally(ctx context.Context, id runv1.ULID, out Outcome, class runv1.FailureClass) error {
	c.mu.Lock()
	r := c.runs[id]
	c.objects.deleteJob(agentrunv1alpha1.JobName(id, r.attempt))
	r.infraRetries++
	r.attempt++
	r.phase = runv1.PhasePending
	c.noteLocked("retrying run=%s locally: attempt=%d class=%s budget-used=%d",
		id, r.attempt, class, r.infraRetries)

	// The bundle's links must outlive the new attempt. An expired signature
	// surfaces as a lost result on work that actually succeeded, so the check
	// happens before the Job exists rather than after the upload fails.
	//
	// Object-store mode only: in relay mode there is nothing signed to expire —
	// the pod posts to this controller's Service — and asking for a bundle
	// would be a round trip that answers the same thing every time.
	needsBundle := !r.lease.Artifacts.Relay() && !r.lease.Artifacts.ExpiresAt.After(c.now())
	c.mu.Unlock()

	if needsBundle {
		if err := c.mintArtifacts(ctx, id); err != nil {
			return err
		}
	}

	c.mu.Lock()
	c.objects.putJob(c.buildJob(r))
	c.updateStatus(r)
	c.mu.Unlock()

	return c.reportPhase(ctx, id, runv1.PhasePending, nil)
}

// mintArtifacts asks for a fresh presigned bundle and updates the per-run
// Secret with it.
func (c *Controller) mintArtifacts(ctx context.Context, id runv1.ULID) error {
	c.mu.Lock()
	r, ok := c.runs[id]
	if !ok {
		c.mu.Unlock()
		return nil
	}
	req := clusterv1.ArtifactBundleRequest{Epoch: r.epoch, Attempt: r.attempt}
	auth := c.authed()
	c.mu.Unlock()

	var bundle clusterv1.ArtifactBundle
	_, problem, err := c.call(ctx, "/leases/"+string(id)+"/artifacts", auth, req, &bundle)
	if err != nil {
		return err
	}
	if problem != nil {
		return c.onProblem(id, problem)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	r.lease.Artifacts = bundle
	cr, ok := c.objects.getAgentRun(agentrunv1alpha1.ObjectName(id))
	if ok {
		c.objects.putSecret(c.buildSecret(r.lease, r, ownerRef(cr)))
	}
	c.noteLocked("artifact bundle refreshed run=%s", id)
	return nil
}

// reportPhase applies the phase locally and sends it on the low-latency path.
// Locally first: the CR is the durable record, and a report the backend never
// accepted must not leave the cluster's own view behind.
func (c *Controller) reportPhase(ctx context.Context, id runv1.ULID, phase runv1.Phase,
	extra func(*clusterv1.RunObservation)) error {

	c.mu.Lock()
	r, ok := c.runs[id]
	if !ok || c.abandoned[id] {
		c.mu.Unlock()
		return nil
	}
	// Monotonic within an attempt, and the first terminal phase stands — the
	// same rule the backend applies, applied here so the two never argue about
	// a run this controller already settled.
	if phase.Rank() < r.phase.Rank() || (r.terminal != "" && phase != r.terminal) {
		c.mu.Unlock()
		return nil
	}
	r.phase = phase
	if phase.IsTerminal() {
		r.terminal = phase
	}
	c.updateStatus(r)
	obs := c.observation(r, phase)
	if extra != nil {
		extra(&obs)
	}
	auth := c.authed()
	cluster := c.clusterID
	c.mu.Unlock()

	var out clusterv1.StatusIngestResponse
	_, problem, err := c.call(ctx, "/ingest/status", auth,
		clusterv1.StatusIngestRequest{ClusterID: cluster, Reports: []clusterv1.RunObservation{obs}}, &out)
	if err != nil {
		// Losing a report is survivable: the heartbeat carries the same state
		// an interval later and the result is already in storage.
		c.note("status ingest failed for run=%s: %v", id, err)
		return nil
	}
	if problem != nil {
		return c.onProblem(id, problem)
	}

	c.mu.Lock()
	c.record(ReportStatus, r, phase)
	c.mu.Unlock()

	for _, res := range out.Results {
		if res.Accepted {
			c.mu.Lock()
			if live, ok := c.runs[res.RunID]; ok {
				live.reportedPhase = phase
			}
			c.mu.Unlock()
		}
		c.applyResult(res)
	}
	return nil
}

func (c *Controller) reportCompletion(ctx context.Context, id runv1.ULID, out Outcome,
	class runv1.FailureClass, phase runv1.Phase) error {

	c.mu.Lock()
	r, ok := c.runs[id]
	if !ok || c.abandoned[id] {
		c.mu.Unlock()
		return nil
	}
	now := c.now()
	report := runv1.CompletionReport{
		RunID: id, Attempt: r.attempt,
		Status: completionStatus(phase), ExitCode: out.ExitCode,
		FailureClass: class,
		Reason:       out.Reason, Message: out.Message,
		Agent: r.lease.Spec.Agent, Model: r.lease.Spec.Model,
		Summary: out.Summary,
		Usage:   &runv1.Usage{TotalCostUSD: out.CostUSD, DurationMs: 42_000, NumTurns: 5},
	}
	if out.PRURL != "" {
		report.Repo = &runv1.RepoResult{
			Pushed: true, TargetBranch: r.lease.Spec.Repo.TargetBranch,
			PRURL: out.PRURL, PRAction: runv1.PRActionCreated,
		}
	}
	req := clusterv1.CompletionIngestRequest{
		ClusterID: c.clusterID, RunID: id, Epoch: r.epoch, Attempt: r.attempt,
		ReceivedAt: &now, Completion: report,
	}
	auth := c.authed()
	c.mu.Unlock()

	var resp clusterv1.CompletionIngestResponse
	_, problem, err := c.call(ctx, "/ingest/completion", auth, req, &resp)
	if err != nil {
		// The report is also in storage as completion.json, which is exactly
		// why losing it here is not losing the cost and the PR link.
		c.note("completion ingest failed for run=%s: %v", id, err)
		return nil
	}
	if problem != nil {
		return c.onProblem(id, problem)
	}

	c.mu.Lock()
	r.completionDelivered = true
	c.updateStatus(r)
	c.record(ReportCompletion, r, phase)
	c.mu.Unlock()

	c.applyCommands(ctx, resp.Commands)
	return nil
}

func completionStatus(phase runv1.Phase) runv1.CompletionStatus {
	switch phase {
	case runv1.PhaseSucceeded:
		return runv1.CompletionSuccess
	case runv1.PhaseTimedOut:
		return runv1.CompletionTimeout
	case runv1.PhaseCancelled:
		return runv1.CompletionCancelled
	default:
		return runv1.CompletionFailure
	}
}

func (c *Controller) observation(r *runState, phase runv1.Phase) clusterv1.RunObservation {
	now := c.now()
	return clusterv1.RunObservation{
		RunID: r.runID, Epoch: r.epoch, Attempt: r.attempt, Phase: phase,
		JobName:    agentrunv1alpha1.JobName(r.runID, r.attempt),
		ObservedAt: &now,
	}
}

// applyResult acts on one per-row verdict. Only abandon changes anything here:
// a rejection the controller can do nothing about is a log line.
func (c *Controller) applyResult(res clusterv1.StatusIngestResult) {
	if res.Accepted {
		return
	}
	c.note("report rejected run=%s code=%s action=%s", res.RunID, res.Code, res.Action)
	if res.Action == clusterv1.ActionAbandon {
		c.abandon(res.RunID, string(res.Code))
	}
}

// applyCommands obeys the top-down channel. An unknown type is a no-op and a
// log line, never a crash: the control plane may be newer, and a new command
// must not take down every older controller in the fleet during an upgrade.
func (c *Controller) applyCommands(ctx context.Context, cmds []clusterv1.Command) {
	for _, cmd := range cmds {
		switch cmd.Type {
		case clusterv1.CommandCancel:
			c.cancel(ctx, cmd)
		case clusterv1.CommandAbandon:
			c.abandon(cmd.RunID, "abandon command")
		default:
			c.note("ignoring unknown command type %q for run=%s", cmd.Type, cmd.RunID)
		}
	}
}

// cancel stops the Job. It does not overwrite a terminal phase: if the pod
// exited zero while the cancellation was in flight, the run succeeded.
func (c *Controller) cancel(ctx context.Context, cmd clusterv1.Command) {
	c.mu.Lock()
	r, ok := c.runs[cmd.RunID]
	if !ok || c.abandoned[cmd.RunID] {
		c.mu.Unlock()
		return
	}
	if r.terminal != "" {
		c.noteLocked("cancel for run=%s ignored: already %s", cmd.RunID, r.terminal)
		c.mu.Unlock()
		return
	}
	r.cancelled = true
	c.objects.deleteJob(agentrunv1alpha1.JobName(r.runID, r.attempt))
	c.noteLocked("cancelling run=%s grace=%ds", cmd.RunID, cmd.GracePeriodSeconds)
	c.mu.Unlock()

	// Cancellation is still reported: the run ends as Cancelled, and the result
	// goes too if the pod managed to upload one.
	_ = c.reportPhase(ctx, cmd.RunID, runv1.PhaseCancelled, nil)
}

// abandon drops the work and stops reporting about it. Everything goes: the
// Job, the CR, and with the CR the Secret and the ConfigMap by ownerReference.
func (c *Controller) abandon(id runv1.ULID, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.abandoned[id] {
		return
	}
	c.abandoned[id] = true
	delete(c.runs, id)
	c.objects.deleteAgentRun(agentrunv1alpha1.ObjectName(id))
	c.noteLocked("abandoned run=%s: %s", id, reason)
}

// onProblem turns a Problem into behaviour. The action decides, never the
// status code: inferring from the number is the divergence this field exists to
// prevent.
func (c *Controller) onProblem(id runv1.ULID, p *clusterv1.Problem) error {
	target := id
	if target == "" {
		target = p.RunID
	}
	switch p.Action {
	case clusterv1.ActionAbandon:
		if target != "" {
			c.abandon(target, string(p.Code))
			return nil
		}
		c.note("abandon with no run named: %s", p.Code)
		return nil
	case clusterv1.ActionRetry, clusterv1.ActionBackoff:
		// The work already taken is played out regardless; this is only about
		// when to try the call again.
		c.note("backing off after %s on run=%s", p.Code, target)
		return nil
	case clusterv1.ActionResync:
		c.note("resync requested: %s", p.Code)
		return nil
	case clusterv1.ActionReregister:
		c.note("credential unusable (%s): re-registration required", p.Code)
		return fmt.Errorf("reregister required: %w", p)
	default:
		// fatal: a defect or an incompatibility. A human is required, and a
		// retry is a busy loop.
		c.note("fatal: %s on run=%s", p.Code, target)
		return fmt.Errorf("fatal: %w", p)
	}
}
