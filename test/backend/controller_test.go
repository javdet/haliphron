package backend

import (
	"context"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controller"
)

// The other half of the contract: a controller that behaves correctly, driven
// against this backend over HTTP.
//
// The probe answers what the backend does with messages a correct controller
// never sends. These answer whether a correct controller works at all — and
// they are the tests that would catch a backend that is internally consistent
// and wrong about the protocol, because FakeController was written from the
// same contract by the other side and agrees with nothing here by
// construction.

func newController(t *testing.T, h *harness, opts ...controller.Option) *controller.Controller {
	t.Helper()
	opts = append([]controller.Option{
		controller.WithName("east"),
		controller.WithNamespace("haliphron-agents"),
		controller.WithRuntimes(runv1.AgentClaudeCode),
		controller.WithCapacity(4),
	}, opts...)

	c, err := controller.New(h.Server.URL, opts...)
	if err != nil {
		t.Fatalf("build controller: %v", err)
	}
	if err := c.Register(context.Background(), h.BootstrapToken()); err != nil {
		t.Fatalf("register controller: %v", err)
	}
	return c
}

// One run, start to finish, through the real protocol.
func TestARunGoesFromAdmissionToResult(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := newController(t, h)
	admitted := h.Submit()

	if _, err := h.App.Sweep(ctx); err != nil {
		t.Fatalf("place the run: %v", err)
	}
	taken, err := c.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if taken != 1 {
		t.Fatalf("the controller took %d leases, want 1", taken)
	}

	// Materialised and acknowledged: the work now survives a controller
	// restart, which is what Dispatched means.
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusDispatched {
		t.Fatalf("status = %s, want Dispatched", status)
	}

	if err := c.Start(ctx, admitted.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusRunning {
		t.Errorf("status = %s, want Running", status)
	}

	if err := c.Finish(ctx, admitted.ID, controller.Outcome{
		ExitCode: runv1.ExitSuccess,
		Summary:  "added the endpoint",
		CostUSD:  "1.750000",
		PRURL:    "https://github.com/example/repo/pull/3",
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	final := h.Run(admitted.ID)
	if final.Status != clusterv1.StatusSucceeded {
		t.Errorf("status = %s, want Succeeded", final.Status)
	}
	if final.CostUSD != "1.750000" {
		t.Errorf("cost = %s, want 1.750000", final.CostUSD)
	}
	if final.PRURL != "https://github.com/example/repo/pull/3" {
		t.Errorf("pr url = %q, want the report's", final.PRURL)
	}
	if final.ResultSummary != "added the endpoint" {
		t.Errorf("summary = %q, want the report's", final.ResultSummary)
	}
	if final.CompletionReceivedAt == nil {
		t.Error("the run still looks uncollected after its completion arrived")
	}
	if final.FinishedAt == nil {
		t.Error("a finished run needs a finish time")
	}

	// The per-run MCP token dies with the run. A token that outlives it is a
	// way to start work charged to a budget nobody is watching.
	tokens, err := h.Store.ListTokens(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	for _, token := range tokens {
		if token.RunID == admitted.ID && token.RevokedAt == nil {
			t.Errorf("the per-run token %s outlived its run", token.ID)
		}
	}
}

// A retriable failure inside the budget does not reach the backend as a
// terminal phase at all: the controller starts another attempt locally, which
// is what attempt counts and why the epoch does not move.
func TestAControllersLocalRetryIsANewAttemptAndNotANewEpoch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := newController(t, h)
	admitted := h.Submit()

	h.Sweep()
	if _, err := c.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := c.Start(ctx, admitted.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 137 is SIGKILL: an OOM or an eviction. The platform stopped the process,
	// so it is infra and the controller retries it on its own.
	if err := c.Finish(ctx, admitted.ID, controller.Outcome{ExitCode: 137, Reason: "OOMKilled"}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	after := h.Run(admitted.ID)
	if after.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", after.Attempt)
	}
	if after.Epoch != 1 {
		t.Errorf("epoch = %d, want 1: a local retry asks nobody for a number", after.Epoch)
	}
	if after.ObservedPhase != runv1.PhasePending {
		t.Errorf("observed phase = %s, want Pending: the rank reset with the attempt", after.ObservedPhase)
	}

	// The second attempt succeeds, and the ledger keeps both.
	if err := c.Start(ctx, admitted.ID); err != nil {
		t.Fatalf("start the second attempt: %v", err)
	}
	if err := c.Finish(ctx, admitted.ID, controller.Outcome{
		ExitCode: runv1.ExitSuccess, CostUSD: "0.500000",
	}); err != nil {
		t.Fatalf("finish the second attempt: %v", err)
	}

	attempts, err := h.Store.Attempts(ctx, admitted.ID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("the ledger holds %d attempts, want 2", len(attempts))
	}
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusSucceeded {
		t.Errorf("status = %s, want Succeeded", status)
	}
}

// A controller whose work was reassigned while it was disconnected is told to
// abandon it: cancel the Job, delete the CR, report nothing further. Anything
// it said afterwards would be appended to a run that now belongs to somebody
// else.
func TestAReassignedRunIsAbandonedByItsFormerHolder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := newController(t, h)
	admitted := h.Submit()

	h.Sweep()
	if _, err := c.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := c.Start(ctx, admitted.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// An operator retries the run: ownership is revoked and the epoch rises.
	if _, err := h.App.Retry(ctx, admitted.ID, "operator"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	before := len(c.SentFor(admitted.ID))

	// The old holder finishes its work and tries to report it.
	if err := c.Finish(ctx, admitted.ID, controller.Outcome{
		ExitCode: runv1.ExitSuccess, CostUSD: "3.000000",
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if !c.Abandoned(admitted.ID) {
		t.Error("the controller was not told to abandon work it no longer owns")
	}
	after := h.Run(admitted.ID)
	if after.Status != clusterv1.StatusQueued {
		t.Errorf("status = %s, want Queued: the retry put it back", after.Status)
	}
	if after.CostUSD != "0.000000" {
		t.Errorf("cost = %s, want nothing charged from an abandoned attempt", after.CostUSD)
	}

	// Nothing further is reported about it. What was sent before the abandon
	// is the status reports that were still current at the time.
	for _, sent := range c.SentFor(admitted.ID)[before:] {
		if sent.Kind == controller.ReportCompletion {
			t.Error("the abandoned controller still delivered a completion")
		}
	}
}

// The pod's callback never arrived: the controller died between the webhook
// and forwarding it, or the pod was killed after uploading. The result is in
// storage regardless, so the backend reaches CompletedWithoutResult and
// recovers the contents itself.
func TestATerminalPhaseWithNoCallbackIsRecoverable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := newController(t, h)
	admitted := h.Submit()

	h.Sweep()
	if _, err := c.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := c.Start(ctx, admitted.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Finish(ctx, admitted.ID, controller.Outcome{
		ExitCode: runv1.ExitSuccess, SkipCompletion: true,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	stuck := h.Run(admitted.ID)
	if stuck.Status != clusterv1.StatusSucceeded {
		t.Errorf("stored status = %s, want the terminal status", stuck.Status)
	}
	if stuck.ReportedStatus() != clusterv1.StatusCompletedWithoutResult {
		t.Errorf("reported status = %s, want %s",
			stuck.ReportedStatus(), clusterv1.StatusCompletedWithoutResult)
	}
	if stuck.CompletionReceivedAt != nil {
		t.Error("a run whose report never arrived must not look collected")
	}
}

// A cluster whose CRD is older than the spec drops fields on write and answers
// 201, which is the CRD contract's main trap. Refusing the lease is how the
// controller says so before anything is spent, and with only one cluster the
// run fails with the reason a user can read.
func TestASpecTheClusterWouldSilentlyPruneIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := newController(t, h, controller.WithPrunedFields("runtime.timeoutSeconds"))
	admitted := h.Submit()

	h.Sweep()
	if _, err := c.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	failed := h.Run(admitted.ID)
	if failed.Status != clusterv1.StatusFailed {
		t.Fatalf("status = %s, want Failed: nobody can run this spec", failed.Status)
	}
	if failed.FailureClass != runv1.FailureConfig {
		t.Errorf("failure class = %s, want config", failed.FailureClass)
	}
	if !contains(failed.StatusMessage, "CRD") && !contains(failed.StatusMessage, "prune") {
		t.Errorf("message = %q, want the cluster's own explanation", failed.StatusMessage)
	}
	// Nothing was spent, and nothing ran.
	if failed.CostUSD != "0.000000" {
		t.Errorf("cost = %s, want nothing: the work never started", failed.CostUSD)
	}
}

// The heartbeat is the reconciliation path, and it must apply observations by
// the same rules as the low-latency one. A controller that has been working
// through an outage delivers everything it accumulated on the next one.
func TestTheHeartbeatDeliversWhatAnOutageHeldBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := newController(t, h)
	admitted := h.Submit()

	h.Sweep()
	if _, err := c.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The backend goes away while the run progresses. The controller plays the
	// work out and accumulates reports — the availability decoupling principle.
	h.SetDown(true)
	_ = c.Start(ctx, admitted.ID)
	_ = c.Finish(ctx, admitted.ID, controller.Outcome{
		ExitCode: runv1.ExitSuccess, CostUSD: "0.900000", Summary: "done while blind",
	})

	h.SetDown(false)
	resp, err := c.Heartbeat(ctx)
	if err != nil {
		t.Fatalf("heartbeat after the outage: %v", err)
	}
	if len(resp.Observations) != 0 {
		t.Errorf("the accumulated report was rejected: %+v", resp.Observations)
	}

	recovered := h.Run(admitted.ID)
	if recovered.Status != clusterv1.StatusSucceeded {
		t.Errorf("status = %s, want Succeeded: the heartbeat carried the outcome", recovered.Status)
	}
}
