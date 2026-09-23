package backend

import (
	"context"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// Acknowledging is the transition from "handed out" to "durable in a cluster",
// and the repeat is the case that matters: a controller that restarted between
// creating the CR and acking must repeat the call under the same epoch, and a
// second ack must not look like a second dispatch.
func TestARepeatedAckChangesNothing(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	first, problem := p.Ack(lease.RunID, lease.Epoch)
	fatalIfProblem(t, "ack", problem)
	if first.Status != clusterv1.StatusDispatched {
		t.Fatalf("status = %s, want Dispatched", first.Status)
	}

	second, problem := p.Ack(lease.RunID, lease.Epoch)
	fatalIfProblem(t, "repeat ack", problem)
	if second.Status != first.Status || second.Epoch != first.Epoch {
		t.Errorf("repeat ack answered %s/%d, first answered %s/%d",
			second.Status, second.Epoch, first.Status, first.Epoch)
	}
	if h.Run(admitted.ID).AckDeadline != nil {
		t.Error("an acknowledged run still carries an ack deadline, so the scanner would requeue it")
	}
}

// Without a negative ack the only way for a controller to say "I cannot run
// this" is to burn the run: create the Job, let it fail, report Failed. Every
// cause is detectable before the Job exists, so the work never starts and
// nothing is spent — the run goes to a cluster that can take it.
func TestARefusedLeaseExcludesTheClusterAndGoesElsewhere(t *testing.T) {
	h := newHarness(t)
	east := newProbe(t, h, "east")
	west := newProbe(t, h, "west")
	admitted := h.Submit()

	// Whichever cluster placement chose refuses; the other must end up with it.
	first, second := east, west
	lease, taken := tryLease(east)
	if !taken {
		lease, taken = tryLease(west)
		first, second = west, east
	}
	if !taken {
		t.Fatal("neither cluster was offered the run")
	}

	resp, problem := first.Reject(lease.RunID, lease.Epoch,
		clusterv1.RejectSpecFieldsPruned, "this cluster's CRD dropped runtime.mcpServers")
	fatalIfProblem(t, "reject", problem)

	if resp.Status != clusterv1.StatusQueued {
		t.Errorf("status = %s, want Queued: another cluster can take it", resp.Status)
	}
	if resp.Epoch != lease.Epoch+1 {
		t.Errorf("epoch = %d, want %d: a refusal revokes ownership", resp.Epoch, lease.Epoch+1)
	}

	// The refusing cluster is not offered it again; the other one is.
	if again, ok := tryLease(first); ok {
		t.Errorf("the refusing cluster was offered the run again: %+v", again)
	}
	reassigned, ok := tryLease(second)
	if !ok {
		t.Fatal("the run was not offered to the cluster that had not refused it")
	}
	if reassigned.RunID != admitted.ID || reassigned.Epoch != lease.Epoch+1 {
		t.Errorf("reassigned lease = %s/%d, want %s/%d",
			reassigned.RunID, reassigned.Epoch, admitted.ID, lease.Epoch+1)
	}
	if reassigned.Attempt != 1 {
		t.Errorf("attempt = %d, want 1: it resets when the epoch rises", reassigned.Attempt)
	}
}

// When there is nobody left to try, queueing forever is the silent failure:
// nobody can run this, and the operator needs to be told why rather than watch
// it sit. The reason the cluster gave is what a user reads.
func TestARefusalWithNoOtherClusterFailsTheRunWithItsReason(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	const reason = "the agent namespace has no quota left"
	resp, problem := p.Reject(lease.RunID, lease.Epoch, clusterv1.RejectQuotaExhausted, reason)
	fatalIfProblem(t, "reject", problem)

	if resp.Status != clusterv1.StatusFailed {
		t.Fatalf("status = %s, want Failed", resp.Status)
	}
	failed := h.Run(admitted.ID)
	if failed.FailureClass != runv1.FailureConfig {
		t.Errorf("failure class = %s, want config", failed.FailureClass)
	}
	if failed.StatusReason != "NoEligibleCluster" {
		t.Errorf("reason = %q, want NoEligibleCluster", failed.StatusReason)
	}
	if !contains(failed.StatusMessage, reason) {
		t.Errorf("message = %q, want the cluster's own reason in it", failed.StatusMessage)
	}
	if !h.HasAudit(admitted.ID, store.AuditNoClusterForRun) {
		t.Error("a run nobody could take was failed without a record")
	}
	if failed.FinishedAt == nil {
		t.Error("a failed run needs a finish time for the run list to sort by")
	}
}

// Ownership is checked on every run-scoped call: a cluster acting on somebody
// else's work is told to abandon it, not to retry.
func TestAnAckFromTheWrongClusterIsRefused(t *testing.T) {
	h := newHarness(t)
	east := newProbe(t, h, "east")
	west := newProbe(t, h, "west")
	h.Submit()

	lease, taken := tryLease(east)
	other := west
	if !taken {
		lease, taken = tryLease(west)
		other = east
	}
	if !taken {
		t.Fatal("neither cluster was offered the run")
	}

	_, problem := other.Ack(lease.RunID, lease.Epoch)
	if problem == nil {
		t.Fatal("a cluster acknowledged a lease it was never given")
	}
	if problem.Code != clusterv1.CodeRunLeasedByAnotherCluster {
		t.Errorf("code = %s, want %s", problem.Code, clusterv1.CodeRunLeasedByAnotherCluster)
	}
	if problem.Action != clusterv1.ActionAbandon {
		t.Errorf("action = %s, want abandon", problem.Action)
	}
}

// The bundle is reissued rather than refreshed in place, because the point of
// the call is an expiry later than the one the caller already holds: an expired
// signature surfaces as a lost result on work that actually succeeded.
func TestAnArtifactBundleCanBeReissuedForAnActiveLease(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}

	var bundle clusterv1.ArtifactBundle
	_, problem := p.post("/leases/"+string(lease.RunID)+"/artifacts", p.token(),
		clusterv1.ArtifactBundleRequest{Epoch: lease.Epoch, Attempt: 2}, &bundle)
	fatalIfProblem(t, "artifacts", problem)

	if !bundle.ExpiresAt.After(lease.Artifacts.ExpiresAt) {
		t.Errorf("reissued bundle expires at %s, not after the original's %s",
			bundle.ExpiresAt, lease.Artifacts.ExpiresAt)
	}
	if len(bundle.Put) != len(lease.Artifacts.Put) {
		t.Errorf("reissued bundle has %d PUT capabilities, want %d",
			len(bundle.Put), len(lease.Artifacts.Put))
	}

	// And a cluster that does not hold the run cannot mint one for it.
	west := newProbe(t, h, "west")
	_, problem = west.post("/leases/"+string(lease.RunID)+"/artifacts", west.token(),
		clusterv1.ArtifactBundleRequest{Epoch: lease.Epoch, Attempt: 2}, nil)
	if problem == nil {
		t.Fatal("a cluster minted capabilities for another cluster's run")
	}
	if problem.Action != clusterv1.ActionAbandon {
		t.Errorf("action = %s, want abandon", problem.Action)
	}
}

// The brake on ack timeouts. A negative ack writes an exclusion and so runs out
// of clusters to try; an ack timeout keeps the assignment and excludes nothing,
// and a controller that materialises the lease and cannot deliver its ack
// would be handed the run every ackTimeout forever, with a per-run token minted
// for each lease. At the ceiling the run fails, the last holder is fenced, no
// token those leases carried is left live, and an operator's retry starts the
// count again.
func TestAckTimeoutsStopAtTheCeilingAndFailTheRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	expire := func() app.SweepResult {
		t.Helper()
		// Aged rather than waited out: five ack windows is five seconds of a
		// test doing nothing.
		if _, err := h.Store.DB().ExecContext(ctx,
			`UPDATE runs SET ack_deadline = now() - interval '1 second'
			 WHERE id = $1 AND status = 'Leased'`, admitted.ID); err != nil {
			t.Fatalf("age the ack deadline: %v", err)
		}
		return h.Sweep()
	}

	ceiling := clusterv1.DefaultMaxAckExpiries
	var last clusterv1.Lease
	for i := 1; i <= ceiling; i++ {
		last = p.LeaseOne()
		swept := expire()
		if swept.AckExpired != 1 {
			t.Fatalf("expiry %d: the scanner expired %d acks, want 1", i, swept.AckExpired)
		}
		want := 0
		if i == ceiling {
			want = 1
		}
		if swept.AckExhausted != want {
			t.Fatalf("expiry %d of %d: %d runs exhausted, want %d", i, ceiling, swept.AckExhausted, want)
		}
	}

	failed := h.Run(admitted.ID)
	if failed.Status != clusterv1.StatusFailed {
		t.Fatalf("status = %s, want Failed", failed.Status)
	}
	if failed.StatusReason != "AckTimeoutExhausted" {
		t.Errorf("reason = %q, want AckTimeoutExhausted", failed.StatusReason)
	}
	if failed.FailureClass != runv1.FailureInfra {
		t.Errorf("failure class = %s, want infra", failed.FailureClass)
	}
	if failed.Epoch != last.Epoch+1 {
		t.Errorf("epoch = %d, want %d: the last holder must be fenced", failed.Epoch, last.Epoch+1)
	}
	if failed.FinishedAt == nil {
		t.Error("a failed run needs a finish time for the run list to sort by")
	}
	if !h.HasAudit(admitted.ID, store.AuditAckExhausted) {
		t.Error("the run was failed without a record")
	}
	if again, ok := tryLease(p); ok {
		t.Errorf("a run failed for ack exhaustion was handed out again: %+v", again)
	}
	if _, problem := p.Ack(admitted.ID, last.Epoch); problem == nil || problem.Action != clusterv1.ActionAbandon {
		t.Errorf("the last holder's late ack got %+v, want abandon", problem)
	}

	tokens, err := h.Store.ListTokens(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	minted := 0
	for _, token := range tokens {
		if token.RunID != admitted.ID {
			continue
		}
		minted++
		if token.RevokedAt == nil {
			t.Errorf("the per-run token %s outlived the lease that carried it", token.ID)
		}
	}
	if minted != ceiling {
		t.Errorf("%d per-run tokens minted, want one per lease (%d)", minted, ceiling)
	}

	if _, err := h.App.Retry(ctx, admitted.ID, "operator"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	p.LeaseOne()
	if swept := expire(); swept.AckExhausted != 0 {
		t.Error("the first expiry after a retry failed the run: the count did not start again")
	}
	if requeued := h.Run(admitted.ID); requeued.Status != clusterv1.StatusQueued {
		t.Errorf("status after one expiry following a retry = %s, want Queued", requeued.Status)
	}
}

// tryLease takes work if this cluster is offered any. Two clusters are
// registered in several of these tests and placement chooses one of them; which
// one is not the property under test.
func tryLease(p *probe) (clusterv1.Lease, bool) {
	p.t.Helper()
	leases, _ := p.Lease(1, 1)
	if len(leases) == 0 {
		return clusterv1.Lease{}, false
	}
	return leases[0], true
}
