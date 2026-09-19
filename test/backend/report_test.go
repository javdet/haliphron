package backend

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/store"
)

// A controller that was disconnected while its work was reassigned comes back
// with news about a run that is no longer its own. It is told to abandon, and
// nothing it said is applied: this is the zombie the fence exists to stop.
func TestAReportUnderAStaleEpochChangesNothing(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning); problem != nil {
		t.Fatalf("report running: %+v", problem)
	}

	// An operator retries the run, which is a revocation of ownership: the
	// epoch rises and the old holder's next message is stale.
	if _, err := h.App.Retry(context.Background(), admitted.ID, "operator"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	before := h.Run(admitted.ID)

	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded)
	fatalIfProblem(t, "stale report", problem)

	if result.Accepted {
		t.Fatal("a report under a stale epoch was accepted")
	}
	if result.Code != clusterv1.CodeEpochMismatch {
		t.Errorf("code = %s, want %s", result.Code, clusterv1.CodeEpochMismatch)
	}
	if result.Action != clusterv1.ActionAbandon {
		t.Errorf("action = %s, want abandon", result.Action)
	}
	if result.CurrentEpoch != before.Epoch {
		t.Errorf("currentEpoch = %d, want %d so the controller can tell reassignment from desync",
			result.CurrentEpoch, before.Epoch)
	}

	after := h.Run(admitted.ID)
	if after.Status != before.Status || after.ObservedPhase != before.ObservedPhase {
		t.Errorf("the run moved from %s/%s to %s/%s on a stale report",
			before.Status, before.ObservedPhase, after.Status, after.ObservedPhase)
	}
}

// An epoch the backend never issued is a defect rather than a race, and no
// retry improves it.
func TestAnEpochAheadOfTheCurrentOneIsFatal(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}

	result, problem := p.Phase(lease.RunID, lease.Epoch+5, 1, runv1.PhaseRunning)
	fatalIfProblem(t, "report from the future", problem)

	if result.Accepted {
		t.Fatal("an epoch that was never issued was accepted")
	}
	if result.Code != clusterv1.CodeInvalidRequest || result.Action != clusterv1.ActionFatal {
		t.Errorf("code/action = %s/%s, want InvalidRequest/fatal", result.Code, result.Action)
	}
}

// At-least-once delivery reorders reports. A Running that arrives after a
// Succeeded must not resurrect the run — and the controller is told to retry
// rather than abandon, because this is almost always reordering within a batch
// and not a loss of ownership.
func TestRunningAfterSucceededIsAPhaseRegression(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("report succeeded: %+v", problem)
	}

	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning)
	fatalIfProblem(t, "late running", problem)

	if result.Accepted {
		t.Fatal("a late Running report resurrected a finished run")
	}
	if result.Code != clusterv1.CodePhaseRegression {
		t.Errorf("code = %s, want %s", result.Code, clusterv1.CodePhaseRegression)
	}
	if result.Action != clusterv1.ActionRetry {
		t.Errorf("action = %s, want retry: reordering is not lost ownership", result.Action)
	}
	if phase := h.Run(admitted.ID).ObservedPhase; phase != runv1.PhaseSucceeded {
		t.Errorf("observed phase = %s, want Succeeded", phase)
	}
}

// Two different endings for one attempt. The first stands — reversing a
// terminal state on a late message is how a succeeded run becomes failed in
// the UI while its PR sits open — and the disagreement goes to the audit log
// rather than nowhere.
func TestTheFirstTerminalPhaseWinsAndTheSecondIsAudited(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("report succeeded: %+v", problem)
	}

	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseFailed)
	fatalIfProblem(t, "second ending", problem)

	if result.Accepted {
		t.Fatal("a second, different ending was accepted")
	}
	if result.Code != clusterv1.CodeRunTerminal || result.Action != clusterv1.ActionAbandon {
		t.Errorf("code/action = %s/%s, want RunTerminal/abandon", result.Code, result.Action)
	}
	if phase := h.Run(admitted.ID).ObservedPhase; phase != runv1.PhaseSucceeded {
		t.Errorf("observed phase = %s, want the first ending to stand", phase)
	}
	if !h.HasAudit(admitted.ID, store.AuditTerminalConflict) {
		t.Error("the disagreement was discarded without a record")
	}
}

// The same observation delivered twice is what at-least-once delivery
// guarantees will happen, and it must be a no-op rather than an error.
func TestARepeatedObservationIsAccepted(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	for i := 0; i < 3; i++ {
		result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning)
		fatalIfProblem(t, "repeat", problem)
		if !result.Accepted {
			t.Fatalf("repeat %d was refused: %+v", i, result)
		}
	}
}

// A local infra retry does not go to the backend for a number: the controller
// raises attempt itself and the backend accepts it as monotonically
// increasing. The rank has to reset with it, or a Pending from attempt 2 would
// lose to the terminal rank of attempt 1 and the run would never move again.
func TestANewAttemptResetsTheRankAndOpensALedgerRow(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseFailed); problem != nil {
		t.Fatalf("report failed: %+v", problem)
	}

	result, problem := p.Phase(lease.RunID, lease.Epoch, 2, runv1.PhasePending)
	fatalIfProblem(t, "attempt 2", problem)
	if !result.Accepted {
		t.Fatalf("the second attempt was refused: %+v", result)
	}

	after := h.Run(admitted.ID)
	if after.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", after.Attempt)
	}
	if after.ObservedPhase != runv1.PhasePending {
		t.Errorf("observed phase = %s, want Pending: the rank resets with the attempt", after.ObservedPhase)
	}

	attempts, err := h.Store.Attempts(context.Background(), admitted.ID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("the ledger holds %d attempts, want 2", len(attempts))
	}
	for _, a := range attempts {
		if a.Epoch != lease.Epoch {
			t.Errorf("attempt %d is keyed under epoch %d, want %d", a.Attempt, a.Epoch, lease.Epoch)
		}
	}

	// And a report from the attempt that was superseded is refused.
	stale, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded)
	fatalIfProblem(t, "report from attempt 1", problem)
	if stale.Accepted {
		t.Error("a report from a superseded attempt was accepted")
	}
	if stale.Code != clusterv1.CodeAttemptRegression {
		t.Errorf("code = %s, want %s", stale.Code, clusterv1.CodeAttemptRegression)
	}
}

// The completion call is retried on any network error, so a sum that grows per
// retry is a bill that grows per retry. The row exists at most once per
// (run, epoch, attempt), and that is where "charged once" is enforced.
func TestARepeatedCompletionIsChargedOnce(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("report succeeded: %+v", problem)
	}

	report := completionFor(lease.RunID, 1, "1.250000", "https://github.com/example/repo/pull/7")
	first, problem := p.Complete(lease.RunID, lease.Epoch, 1, report)
	fatalIfProblem(t, "first completion", problem)
	if !first.Accepted || first.Duplicate {
		t.Fatalf("first completion: accepted=%t duplicate=%t", first.Accepted, first.Duplicate)
	}

	second, problem := p.Complete(lease.RunID, lease.Epoch, 1, report)
	fatalIfProblem(t, "repeat completion", problem)
	if !second.Accepted || !second.Duplicate {
		t.Fatalf("repeat completion: accepted=%t duplicate=%t, want accepted and duplicate",
			second.Accepted, second.Duplicate)
	}

	final := h.Run(admitted.ID)
	if final.CostUSD != "1.250000" {
		t.Errorf("cost = %s, want 1.250000 charged exactly once", final.CostUSD)
	}
	if final.Status != clusterv1.StatusSucceeded {
		t.Errorf("status = %s, want Succeeded", final.Status)
	}
	if final.PRURL != "https://github.com/example/repo/pull/7" {
		t.Errorf("pr url = %q, want the one in the report", final.PRURL)
	}
	if final.ResultSummary == "" {
		t.Error("the summary was not promoted, so the run list would hit storage per row")
	}
	if final.CompletionReceivedAt == nil {
		t.Error("completion_received_at is unset, so the run still looks uncollected")
	}
}

// The completion and the terminal status are independent channels, and the
// backend is obliged to work under any ordering of them. Here the result
// arrives first.
func TestACompletionBeforeTheTerminalStatusIsResolvedWhenItArrives(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning); problem != nil {
		t.Fatalf("report running: %+v", problem)
	}

	report := completionFor(lease.RunID, 1, "0.400000", "https://github.com/example/repo/pull/9")
	if _, problem := p.Complete(lease.RunID, lease.Epoch, 1, report); problem != nil {
		t.Fatalf("completion: %+v", problem)
	}

	// The run is not finished yet: the controller has not said so.
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusRunning {
		t.Errorf("status = %s, want Running until the terminal observation arrives", status)
	}

	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded)
	fatalIfProblem(t, "terminal status", problem)
	if result.AppliedStatus != clusterv1.StatusSucceeded {
		t.Errorf("appliedStatus = %s, want Succeeded: the result was already here",
			result.AppliedStatus)
	}

	final := h.Run(admitted.ID)
	if final.CostUSD != "0.400000" || final.PRURL == "" {
		t.Errorf("the report's contents were not promoted: cost=%s pr=%q", final.CostUSD, final.PRURL)
	}
}

// A terminal status with no report is not a failure. The pod writes its result
// to storage before it calls back, so the contents exist and this is a read the
// backend owes itself — which is what CompletedWithoutResult names.
func TestATerminalStatusWithoutACompletionRecoversItFromStorage(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}

	// The pod uploaded completion.json and then the controller died between
	// the webhook and forwarding it. Both copies exist for this reason.
	report := completionFor(lease.RunID, 1, "2.500000", "https://github.com/example/repo/pull/11")
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	if err := h.Artifacts.Put(context.Background(),
		artifacts.Key(admitted.ID, runv1.StorageKeyCompletion), raw, "application/json"); err != nil {
		t.Fatalf("stage completion.json: %v", err)
	}

	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}

	final := h.Run(admitted.ID)
	if final.CostUSD != "2.500000" {
		t.Errorf("cost = %s, want the value lifted from storage", final.CostUSD)
	}
	if final.PRURL != "https://github.com/example/repo/pull/11" {
		t.Errorf("pr url = %q, want the one lifted from storage", final.PRURL)
	}
	if final.ReportedStatus() != clusterv1.StatusSucceeded {
		t.Errorf("reported status = %s, want Succeeded once the contents are here",
			final.ReportedStatus())
	}
}

// With nothing in storage the run stays visibly uncollected rather than
// quietly succeeding with no cost and no PR, and the sweeper keeps looking.
func TestATerminalStatusWithNoResultAnywhereIsReportedAsSuch(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}

	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded)
	fatalIfProblem(t, "terminal status", problem)
	if result.AppliedStatus != clusterv1.StatusCompletedWithoutResult {
		t.Errorf("appliedStatus = %s, want %s",
			result.AppliedStatus, clusterv1.StatusCompletedWithoutResult)
	}

	final := h.Run(admitted.ID)
	if final.Status != clusterv1.StatusSucceeded {
		t.Errorf("stored status = %s, want the terminal status the run_status domain can hold",
			final.Status)
	}
	if final.ReportedStatus() != clusterv1.StatusCompletedWithoutResult {
		t.Errorf("reported status = %s, want %s",
			final.ReportedStatus(), clusterv1.StatusCompletedWithoutResult)
	}

	// The sweeper is the second chance: the pod may still be uploading.
	report := completionFor(lease.RunID, 1, "0.750000", "")
	raw, _ := json.Marshal(report)
	if err := h.Artifacts.Put(context.Background(),
		artifacts.Key(admitted.ID, runv1.StorageKeyCompletion), raw, "application/json"); err != nil {
		t.Fatalf("stage completion.json: %v", err)
	}
	if swept := h.Sweep(); swept.Recovered != 1 {
		t.Fatalf("the sweep recovered %d results, want 1", swept.Recovered)
	}
	if cost := h.Run(admitted.ID).CostUSD; cost != "0.750000" {
		t.Errorf("cost = %s, want the recovered value", cost)
	}
}

// Cost and duration come from the pod, the least trusted component in the
// system. The phase 1 mitigation is not a block — a wrong threshold refuses
// honest runs — but a record an operator can go and read.
func TestAnImpossibleSelfDeclaredDurationIsAudited(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}

	report := completionFor(lease.RunID, 1, "9.000000", "")
	report.Usage.DurationMs = int64(4 * time.Hour / time.Millisecond)
	if _, problem := p.Complete(lease.RunID, lease.Epoch, 1, report); problem != nil {
		t.Fatalf("completion: %+v", problem)
	}

	if !h.HasAudit(admitted.ID, store.AuditUsageDivergence) {
		t.Error("a four-hour run inside a lease seconds old was not recorded anywhere")
	}
	// Recorded, not refused: the cost still lands, because a threshold that
	// throws away accounting is worse than one that flags it.
	if cost := h.Run(admitted.ID).CostUSD; cost != "9.000000" {
		t.Errorf("cost = %s, want the declared value recorded", cost)
	}
}

// The run-scoped calls answer with a Problem rather than a per-row result, and
// the answer has to say the same thing: this work is not yours any more.
func TestACompletionUnderAStaleEpochIsRefusedWithAbandon(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, err := h.App.Retry(context.Background(), admitted.ID, "operator"); err != nil {
		t.Fatalf("retry: %v", err)
	}

	_, problem := p.Complete(lease.RunID, lease.Epoch, 1,
		completionFor(lease.RunID, 1, "5.000000", "https://github.com/example/repo/pull/13"))
	if problem == nil {
		t.Fatal("a completion under a stale epoch was accepted")
	}
	if problem.Code != clusterv1.CodeEpochMismatch {
		t.Errorf("code = %s, want %s", problem.Code, clusterv1.CodeEpochMismatch)
	}
	if problem.Action != clusterv1.ActionAbandon {
		t.Errorf("action = %s, want abandon", problem.Action)
	}
	if problem.CurrentEpoch != lease.Epoch+1 {
		t.Errorf("currentEpoch = %d, want %d", problem.CurrentEpoch, lease.Epoch+1)
	}
	if cost := h.Run(admitted.ID).CostUSD; cost != "0.000000" {
		t.Errorf("cost = %s, want nothing charged from an abandoned attempt", cost)
	}
}

// An ack timeout keeps the assignment, and that is a deliberate correction to
// the sketch in section 13 of the Cluster API contract, which says the work
// goes to another cluster.
//
// The reason is in expire_ack.sql: the ordinary cause of a missed ack is a
// controller that was restarting, and sending every one of those through
// re-placement would move work away from a healthy cluster on the strength of a
// rolling update. Reassignment stays available — it is what a negative ack and
// an operator's retry do, and both consult the exclusion table — but it is not
// what a timeout means.
func TestAnAckTimeoutKeepsTheAssignmentForTheSameCluster(t *testing.T) {
	h := newHarness(t)
	east := newProbe(t, h, "east")
	west := newProbe(t, h, "west")
	admitted := h.Submit()

	holder, other := east, west
	lease, taken := tryLease(east)
	if !taken {
		lease, taken = tryLease(west)
		holder, other = west, east
	}
	if !taken {
		t.Fatal("neither cluster was offered the run")
	}
	_ = lease

	time.Sleep(1200 * time.Millisecond)
	if swept := h.Sweep(); swept.AckExpired != 1 {
		t.Fatalf("the scanner expired %d acks, want 1", swept.AckExpired)
	}

	requeued := h.Run(admitted.ID)
	if requeued.ClusterID != runv1.ULID(holderID(holder)) {
		t.Errorf("assignment = %s, want it kept on %s", requeued.ClusterID, holderID(holder))
	}
	if again, ok := tryLease(other); ok {
		t.Errorf("the run moved to a cluster it was never assigned to: %+v", again)
	}
	if again, ok := tryLease(holder); !ok {
		t.Error("the cluster that timed out was not offered the work again")
	} else if again.Epoch != lease.Epoch+1 {
		t.Errorf("re-issued epoch = %d, want %d", again.Epoch, lease.Epoch+1)
	}
}

func holderID(p *probe) string { return string(p.clusterID) }
