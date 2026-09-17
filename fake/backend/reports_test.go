package backend_test

import (
	"net/http"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/backend"
)

// Report monotonicity, the result channel, commands and reconciliation: the
// rules that decide what a run's state is when messages arrive out of order,
// twice, or not at all. At-least-once delivery guarantees all three happen.

// ---------------------------------------------------------------------------
// monotonicity
// ---------------------------------------------------------------------------

func TestPhaseRegressionIsRejectedAndRetryable(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	c.ingest(observation(l, runv1.PhaseSucceeded))
	res := c.ingest(observation(l, runv1.PhaseRunning)).Results[0]

	if res.Accepted {
		t.Fatal("a Running that arrived after Succeeded resurrected the run")
	}
	if res.Code != clusterv1.CodePhaseRegression {
		t.Fatalf("want PhaseRegression, got %s", res.Code)
	}
	// Retry rather than abandon: this is almost always reordering within a
	// batch, not lost ownership, and the next heartbeat settles it. Telling the
	// controller to abandon here would throw away a live run over a race.
	if res.Action != clusterv1.ActionRetry {
		t.Fatalf("want retry, got %s", res.Action)
	}

	st, _ := b.RunState(id)
	if st.TerminalPhase != runv1.PhaseSucceeded {
		t.Fatalf("terminal phase became %q", st.TerminalPhase)
	}
}

func TestTheFirstTerminalPhaseWins(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	c.ingest(observation(l, runv1.PhaseSucceeded))
	res := c.ingest(observation(l, runv1.PhaseFailed)).Results[0]

	if res.Accepted || res.Code != clusterv1.CodeRunTerminal {
		t.Fatalf("want RunTerminal, got accepted=%t code=%s", res.Accepted, res.Code)
	}
	st, _ := b.RunState(id)
	if st.TerminalPhase != runv1.PhaseSucceeded {
		t.Fatalf("the second ending overwrote the first: %s", st.TerminalPhase)
	}

	// Dropping the disagreement silently would leave a succeeded run with an
	// open PR and a cluster convinced it failed, and no record that the two
	// ever disagreed.
	var found bool
	for _, e := range b.Audit() {
		if e.RunID == id && e.Kind == backend.AuditTerminalConflict {
			found = true
		}
	}
	if !found {
		t.Fatal("the conflicting terminal phase was not audited")
	}
}

func TestARepeatedObservationIsANoOp(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	c.ingest(observation(l, runv1.PhaseRunning))
	res := c.ingest(observation(l, runv1.PhaseRunning)).Results[0]
	// At-least-once delivery means this arrives twice. Accepting it is what
	// makes the controller's retry-on-network-error safe.
	if !res.Accepted {
		t.Fatalf("an idempotent repeat was rejected: %s", res.Code)
	}
}

func TestANewAttemptResetsThePhaseRank(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	c.ingest(observation(l, runv1.PhaseRunning))

	// The controller retried an infra failure locally, without asking: attempt
	// is its counter, epoch is the backend's.
	second := observation(l, runv1.PhasePending)
	second.Attempt = 2
	res := c.ingest(second).Results[0]

	if !res.Accepted {
		t.Fatalf("attempt 2 was rejected: %s", res.Code)
	}
	st, _ := b.RunState(id)
	if st.Attempt != 2 || st.Phase != runv1.PhasePending {
		t.Fatalf("want attempt 2 at Pending, got attempt %d at %s", st.Attempt, st.Phase)
	}
	if st.Epoch != l.Epoch {
		t.Fatalf("epoch moved to %d: a local retry is not a change of ownership", st.Epoch)
	}
	if len(st.Attempts) != 2 {
		t.Fatalf("want 2 attempt records, got %d", len(st.Attempts))
	}
}

func TestAnOlderAttemptIsRejected(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	second := observation(l, runv1.PhaseRunning)
	second.Attempt = 2
	c.ingest(second)

	res := c.ingest(observation(l, runv1.PhaseRunning)).Results[0]
	if res.Accepted || res.Code != clusterv1.CodeAttemptRegression {
		t.Fatalf("want AttemptRegression, got accepted=%t code=%s", res.Accepted, res.Code)
	}
}

func TestAStaleEpochReportIsAbandonedAndChangesNothing(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	c.ingest(observation(l, runv1.PhaseRunning))

	before, _ := b.RunState(id)
	b.Reassign(l.RunID)

	// The zombie: disconnected, its work reassigned, back with news about a run
	// that is no longer its own.
	res := c.ingest(observation(l, runv1.PhaseSucceeded)).Results[0]
	if res.Accepted {
		t.Fatal("a stale-epoch report was applied")
	}
	if res.Code != clusterv1.CodeEpochMismatch || res.Action != clusterv1.ActionAbandon {
		t.Fatalf("want EpochMismatch/abandon, got %s/%s", res.Code, res.Action)
	}
	if res.CurrentEpoch != before.Epoch+1 {
		t.Fatalf("the rejection did not carry the current epoch: %d", res.CurrentEpoch)
	}

	after, _ := b.RunState(id)
	if after.TerminalPhase != "" {
		t.Fatalf("the zombie drove the run terminal: %s", after.TerminalPhase)
	}
}

func TestAnEpochTheBackendNeverIssuedIsFatal(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()

	ahead := observation(l, runv1.PhaseRunning)
	ahead.Epoch = l.Epoch + 5
	res := c.ingest(ahead).Results[0]

	// Not a race: nothing can produce this but a defect, and a retry of a
	// defect is a busy loop.
	if res.Accepted || res.Action != clusterv1.ActionFatal {
		t.Fatalf("want fatal, got accepted=%t action=%s", res.Accepted, res.Action)
	}
}

func TestTheHeartbeatAndIngestApplyTheSameRules(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	c.ingest(observation(l, runv1.PhaseSucceeded))

	// The heartbeat is the periodic reconciliation and ingest is the
	// low-latency path. If they disagree, the heartbeat spends its life
	// rolling back what ingest just delivered.
	resp := c.heartbeat(clusterv1.HeartbeatRequest{
		FreeSlots: 1, ReportComplete: true,
		Runs: []clusterv1.RunObservation{observation(l, runv1.PhaseRunning)},
	})
	if len(resp.Observations) != 1 || resp.Observations[0].Code != clusterv1.CodePhaseRegression {
		t.Fatalf("the heartbeat did not apply the monotonicity rules: %+v", resp.Observations)
	}
	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusCompletedWithoutResult {
		t.Fatalf("the heartbeat rolled a terminal run back to %s", st.Status)
	}
}

// ---------------------------------------------------------------------------
// the result channel
// ---------------------------------------------------------------------------

func TestCompletionIsChargedOnce(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	c.ingest(observation(l, runv1.PhaseSucceeded))

	req := completionFor(l, "0.4231")
	if resp := c.completion(req); resp.status != http.StatusOK {
		t.Fatalf("completion: %d %s", resp.status, resp.body)
	}

	// The controller retries this on any network error. A sum that grows per
	// retry is a bill that grows per retry.
	resp := c.completion(req)
	var out clusterv1.CompletionIngestResponse
	resp.decode(t, &out)
	if !out.Duplicate {
		t.Fatal("the repeat was not reported as a duplicate")
	}

	st, _ := b.RunState(id)
	if len(st.Charges) != 1 {
		t.Fatalf("charged %d times", len(st.Charges))
	}
	if st.Status != clusterv1.StatusSucceeded {
		t.Fatalf("want Succeeded once the result arrived, got %s", st.Status)
	}
}

func TestATerminalStatusWithoutAResultIsNotAFailure(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	c.ingest(observation(l, runv1.PhaseSucceeded))

	st, _ := b.RunState(id)
	// The pod writes to storage before it calls back, so the contents exist
	// whatever happened to the callback. This is a read the backend owes
	// itself, not a lost run.
	if st.Status != clusterv1.StatusCompletedWithoutResult {
		t.Fatalf("want CompletedWithoutResult, got %s", st.Status)
	}
	if st.TerminalPhase != runv1.PhaseSucceeded {
		t.Fatalf("the terminal phase was lost: %q", st.TerminalPhase)
	}
}

func TestTheResultIsRecoveredFromStorage(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	// The controller died between the pod's webhook and forwarding it. The
	// copy in storage costs one PUT and is the only place the cost and the PR
	// link survive — result.md and output.json carry neither.
	report := completionFor(l, "1.2500").Completion
	report.Repo = &runv1.RepoResult{
		Pushed: true, PRURL: "https://github.com/acme/widgets/pull/42", PRNumber: 42,
	}
	b.SeedStoredCompletion(id, report)

	c.ingest(observation(l, runv1.PhaseSucceeded))

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusSucceeded {
		t.Fatalf("want Succeeded, got %s", st.Status)
	}
	if st.Completion == nil || st.Completion.Repo == nil || st.Completion.Repo.PRURL == "" {
		t.Fatal("the PR link was not recovered from storage")
	}
	if len(st.Charges) != 1 {
		t.Fatalf("want one charge, got %d", len(st.Charges))
	}
}

func TestCompletionWithAStaleEpochIsRefused(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	b.Reassign(l.RunID)

	resp := c.completion(completionFor(l, "0.1000"))
	if resp.status != http.StatusConflict {
		t.Fatalf("want 409, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Action != clusterv1.ActionAbandon {
		t.Fatalf("want abandon, got %s", p.Action)
	}
}

func TestACompletionThatNamesAnotherRunIsRefused(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	other := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	req := completionFor(l, "0.1000")
	req.Completion.RunID = other
	// The duplication exists so completion.json can be read from storage
	// without an envelope. A report that cannot name its own run is not a
	// backup copy, and a mismatch means the wrong object was read.
	if resp := c.completion(req); resp.status != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body %s", resp.status, resp.body)
	}
}

func TestSelfDeclaredDurationIsAudited(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	c.ingest(observation(l, runv1.PhaseSucceeded))

	req := completionFor(l, "0.4231")
	req.Completion.Usage.DurationMs = (2 * time.Hour).Milliseconds()
	c.completion(req)

	// Cost and tokens come from the least trusted component in the system. The
	// phase 1 mitigation is a record, not a block: a threshold that refuses
	// runs would refuse honest ones too.
	var found bool
	for _, e := range b.Audit() {
		if e.RunID == id && e.Kind == backend.AuditUsageDivergence {
			found = true
		}
	}
	if !found {
		t.Fatal("a two-hour duration on a run that just started was not audited")
	}
}

// ---------------------------------------------------------------------------
// commands and reconciliation
// ---------------------------------------------------------------------------

func TestCancelIsRedeliveredUntilThePhaseIsTerminal(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	c.ingest(observation(l, runv1.PhaseRunning))

	b.Cancel(l.RunID)

	beat := func() []clusterv1.Command {
		return c.heartbeat(clusterv1.HeartbeatRequest{
			FreeSlots: 1, ReportComplete: true,
			Runs: []clusterv1.RunObservation{observation(l, runv1.PhaseRunning)},
		}).Commands
	}

	// Commands carry no acknowledgement on purpose: an acknowledgement would
	// have to be stored and expired, while repeating cancel is harmless. The
	// observed state is the acknowledgement.
	for i := 0; i < 3; i++ {
		cmds := beat()
		if len(cmds) != 1 || cmds[0].Type != clusterv1.CommandCancel {
			t.Fatalf("heartbeat %d carried %+v, want a cancel", i, cmds)
		}
	}

	c.ingest(observation(l, runv1.PhaseCancelled))
	if cmds := beat(); len(cmds) != 0 {
		t.Fatalf("cancel was redelivered after the run ended: %+v", cmds)
	}
}

func TestCancelArrivingDuringMaterialisationRidesOnTheAck(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()

	b.Cancel(l.RunID)
	ack := c.mustAck(l.RunID, l.Epoch)

	// Otherwise it waits a full heartbeat interval, having started a Job that
	// must immediately be killed — a minute of an agent's budget for nothing.
	if len(ack.Commands) != 1 || ack.Commands[0].Type != clusterv1.CommandCancel {
		t.Fatalf("the ack did not carry the pending cancel: %+v", ack.Commands)
	}
}

func TestAnUnknownCommandTypeCanBeDelivered(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	// The controller must ignore this rather than crash: a newer control plane
	// will send one, and a panic would take down every older controller in the
	// fleet during an upgrade. The fake can produce it so that behaviour is
	// testable at all.
	b.InjectCommand(l.RunID, clusterv1.Command{Type: "quarantine"})

	cmds := c.heartbeat(clusterv1.HeartbeatRequest{
		FreeSlots: 1, ReportComplete: true,
		Runs: []clusterv1.RunObservation{observation(l, runv1.PhaseRunning)},
	}).Commands
	if len(cmds) != 1 || cmds[0].Type != "quarantine" {
		t.Fatalf("want the injected command, got %+v", cmds)
	}
}

func TestReportCompleteRevealsALostRun(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	// The controller was moved and lost its PV, or the agent namespace was
	// recreated: the CRs are gone. Without reconciliation the backend waits out
	// staleAfter for every run and gets a batch of Unknowns to triage by hand.
	resp := c.heartbeat(clusterv1.HeartbeatRequest{FreeSlots: 1, ReportComplete: true})
	if len(resp.UnknownRuns) != 1 || resp.UnknownRuns[0] != id {
		t.Fatalf("want %s in unknownRuns, got %v", id, resp.UnknownRuns)
	}
}

func TestAPartialReportSaysNothingAboutWhatIsMissing(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	// The controller has just come up and its informer cache is not warm.
	// Reacting to silence here takes work away from a controller that is about
	// to report it.
	resp := c.heartbeat(clusterv1.HeartbeatRequest{FreeSlots: 1, ReportComplete: false})
	if len(resp.UnknownRuns) != 0 {
		t.Fatalf("a partial report produced unknownRuns: %v", resp.UnknownRuns)
	}
}

func TestQuotaExhaustedStopsAssignment(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())

	c.heartbeat(clusterv1.HeartbeatRequest{
		FreeSlots: 5, ReportComplete: true,
		Cluster: &clusterv1.ClusterFacts{QuotaExhausted: true},
	})

	// Otherwise an exhausted ResourceQuota is visible only as a series of
	// Failed runs with "exceeded quota" in the message.
	if leases, _ := c.poll(5); len(leases) != 0 {
		t.Fatalf("work was assigned to a cluster with no quota: %d leases", len(leases))
	}
}

func completionFor(l clusterv1.Lease, cost runv1.MoneyUSD) clusterv1.CompletionIngestRequest {
	return clusterv1.CompletionIngestRequest{
		RunID: l.RunID, Epoch: l.Epoch, Attempt: l.Attempt,
		Completion: runv1.CompletionReport{
			RunID: l.RunID, Attempt: l.Attempt,
			Status: runv1.CompletionSuccess, ExitCode: runv1.ExitSuccess,
			Agent: runv1.AgentClaudeCode, Model: "anthropic/claude-opus-5",
			Summary: "added the rds module",
			Usage:   &runv1.Usage{TotalCostUSD: cost, DurationMs: 42_000, NumTurns: 7},
		},
	}
}
