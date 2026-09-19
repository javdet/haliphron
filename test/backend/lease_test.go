package backend

import (
	"net/http"
	"sync"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// A poll of an empty queue waits and then answers 204 — not an empty list, so
// the controller can re-poll at once without parsing a body, and not an error,
// so it does not back off. Backoff here would turn a long poll into polling.
func TestAnEmptyQueueAnswers204AfterTheWait(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")

	start := time.Now()
	leases, status := p.Lease(1, 2)
	elapsed := time.Since(start)

	if status != http.StatusNoContent || len(leases) != 0 {
		t.Fatalf("status = %d with %d leases, want 204 and none", status, len(leases))
	}
	if elapsed < 2*time.Second {
		t.Errorf("the poll returned after %s, before its %s wait elapsed", elapsed, 2*time.Second)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the poll overran its wait by %s", elapsed-2*time.Second)
	}
}

// Work admitted while a controller is already waiting must reach it then, not
// when its poll expires. The notifier is what turns a long poll into a long
// poll rather than a delay.
func TestAWaitingPollIsWokenByAdmission(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")

	var (
		wg     sync.WaitGroup
		leases []clusterv1.Lease
		waited time.Duration
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		leases, _ = p.Lease(1, 2)
		waited = time.Since(start)
	}()

	time.Sleep(200 * time.Millisecond)
	run := h.Submit()
	wg.Wait()

	if len(leases) != 1 || leases[0].RunID != run.ID {
		t.Fatalf("the waiting poll did not receive the admitted run: %+v", leases)
	}
	if waited > 1500*time.Millisecond {
		t.Errorf("the poll waited %s for work that was admitted after 200ms", waited)
	}
}

// Two polls from one cluster happen: a controller reconnects before the
// previous long poll has unwound, and a rollout gives two replicas asking at
// the same moment. FOR UPDATE SKIP LOCKED is what makes their answers
// disjoint; without it one blocks and then receives the same run.
func TestConcurrentPollsNeverHandOutTheSameRunTwice(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")

	const runs = 8
	for i := 0; i < runs; i++ {
		h.Submit()
	}

	var (
		mu   sync.Mutex
		seen = map[runv1.ULID]int{}
		wg   sync.WaitGroup
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leases, _ := p.Lease(runs, 1)
			mu.Lock()
			defer mu.Unlock()
			for _, l := range leases {
				seen[l.RunID]++
			}
		}()
	}
	wg.Wait()

	if len(seen) != runs {
		t.Errorf("%d distinct runs were handed out, want %d", len(seen), runs)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("run %s was handed out %d times", id, count)
		}
	}
}

// The first lease hands out epoch 1. It follows from the epoch rising on the
// revocation of ownership rather than on issuance: a run that was never
// offered to anyone has no ownership to fence.
func TestTheFirstLeaseIssuesEpochOneAndACompleteBundle(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()

	if lease.Epoch != 1 {
		t.Errorf("epoch = %d, want 1", lease.Epoch)
	}
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", lease.Attempt)
	}
	if lease.RunID != admitted.ID {
		t.Errorf("run = %s, want %s", lease.RunID, admitted.ID)
	}

	// Everything needed to execute without calling back (principle P4).
	if lease.Spec.Prompt.Key == "" || lease.Spec.Prompt.SHA256 == "" {
		t.Errorf("the spec does not point at a verifiable prompt: %+v", lease.Spec)
	}
	if lease.Secrets[runv1.SecretKeyLLMAPIKey] == "" {
		t.Error("the lease carries no model credential")
	}
	if lease.Secrets[runv1.SecretKeyGitToken] == "" {
		t.Error("the lease carries no git token for a run with a repository")
	}
	if lease.Secrets[runv1.SecretKeyMCPConfig] == "" {
		t.Error("the lease carries no MCP configuration, so a child run cannot be attributed")
	}
	for _, key := range []string{
		runv1.StorageKeyOutput, runv1.StorageKeyResult, runv1.StorageKeyState,
		runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog,
	} {
		if _, ok := lease.Artifacts.Put[key]; !ok {
			t.Errorf("the bundle has no PUT capability for %s", key)
		}
	}
	for _, key := range []string{runv1.StorageKeyPrompt, runv1.StorageKeyState} {
		if _, ok := lease.Artifacts.Get[key]; !ok {
			t.Errorf("the bundle has no GET capability for %s", key)
		}
	}
	if !lease.AckDeadline.Before(lease.LeaseDeadline) {
		t.Errorf("ackDeadline %s should fall before leaseDeadline %s",
			lease.AckDeadline, lease.LeaseDeadline)
	}

	// The deadlines are the backend's, so the run must carry them too.
	stored := h.Run(admitted.ID)
	if stored.Status != clusterv1.StatusLeased {
		t.Errorf("status = %s, want %s", stored.Status, clusterv1.StatusLeased)
	}
	if stored.AckDeadline == nil {
		t.Error("a leased run must have an ack deadline for the scanner to find")
	}
}

// The lease is the only message in the system carrying secret material in the
// clear. Section 9 of the contract requires its body to be absent from the
// logs at any level, and a debug line added during an incident is how that
// breaks — so it is asserted rather than reviewed.
func TestTheLeaseBodyNeverReachesTheLog(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	h.Submit()

	lease := p.LeaseOne()

	secrets := []string{
		lease.Secrets[runv1.SecretKeyGitToken],
		lease.Secrets[runv1.SecretKeyLLMAPIKey],
		lease.Secrets[runv1.SecretKeyMCPConfig],
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if h.Logs.Contains(secret) {
			t.Errorf("a secret from the lease body appears in the log")
		}
	}
	for _, url := range lease.Artifacts.Put {
		if h.Logs.Contains(url.URL) {
			t.Error("a presigned URL appears in the log; it is a bearer capability")
		}
	}
	// What is logged instead: identifiers and a count.
	var mentioned bool
	for _, line := range h.Logs.Lines() {
		if len(line) > 0 && (contains(line, "leases issued") || contains(line, "leases handed out")) {
			mentioned = true
		}
	}
	if !mentioned {
		t.Error("issuing leases should be logged as a count")
	}
}

// Before the ack the work is guaranteed not to have started, so it returns to
// the queue and can be reassigned at once. The epoch rises in the same
// statement, which is the fence closing: the controller that was holding it
// now reports under a strictly smaller epoch.
func TestAnAckTimeoutRequeuesTheWorkAndRaisesTheEpoch(t *testing.T) {
	h := newHarness(t)
	east := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := east.LeaseOne()
	if lease.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", lease.Epoch)
	}

	// The ack never comes. The deadline is a second in these timings.
	time.Sleep(1200 * time.Millisecond)
	result := h.Sweep()
	if result.AckExpired != 1 {
		t.Fatalf("the scanner expired %d acks, want 1", result.AckExpired)
	}

	requeued := h.Run(admitted.ID)
	if requeued.Status != clusterv1.StatusQueued {
		t.Errorf("status = %s, want %s", requeued.Status, clusterv1.StatusQueued)
	}
	if requeued.Epoch != 2 {
		t.Errorf("epoch = %d, want 2: ownership was revoked", requeued.Epoch)
	}
	if requeued.Attempt != 1 {
		t.Errorf("attempt = %d, want 1: it resets when the epoch rises", requeued.Attempt)
	}
	if requeued.AckDeadline != nil || requeued.LeaseDeadline != nil {
		t.Error("a queued run holds neither deadline")
	}

	// And the work is available again — to this cluster or another one.
	second := east.LeaseOne()
	if second.Epoch != 2 {
		t.Errorf("the re-issued lease carries epoch %d, want 2", second.Epoch)
	}
}

// After the ack a Job may be executing this second, so the run becomes Unknown
// and is not handed to anybody else: two agents on one repository is two PRs
// and two bills. The epoch deliberately does not rise, so the controller that
// lost the network can still report when it comes back.
func TestALeaseTimeoutMakesTheRunUnknownRatherThanQueued(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning)
	fatalIfProblem(t, "report running", problem)
	if !result.Accepted {
		t.Fatalf("the Running report was refused: %+v", result)
	}

	// The lease TTL is two seconds in these timings, and no heartbeat extends it.
	time.Sleep(2200 * time.Millisecond)
	if swept := h.Sweep(); swept.LeaseExpired != 1 {
		t.Fatalf("the scanner expired %d leases, want 1", swept.LeaseExpired)
	}

	lost := h.Run(admitted.ID)
	if lost.Status != clusterv1.StatusUnknown {
		t.Errorf("status = %s, want %s", lost.Status, clusterv1.StatusUnknown)
	}
	if lost.Epoch != lease.Epoch {
		t.Errorf("epoch = %d, want %d: a lease timeout revokes nothing", lost.Epoch, lease.Epoch)
	}
	if lost.ObservedPhase != runv1.PhaseRunning {
		t.Errorf("observed phase = %s, want Running: Unknown is about visibility, not progress",
			lost.ObservedPhase)
	}

	// It is not handed to anybody, including the cluster that lost it.
	if leases, status := p.Lease(1, 1); len(leases) != 0 {
		t.Errorf("an Unknown run was re-issued (%d leases, status %d)", len(leases), status)
	}
}

// A controller that comes back with the same epoch recovers the run by itself.
// The phase rank never moved, so the report it sends compares equal and is
// applied as the idempotent repeat it is — which clears Unknown. That is the
// entire reason Unknown lives in status and not in observed_phase.
func TestAReturningControllerClearsUnknownByItself(t *testing.T) {
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

	time.Sleep(2200 * time.Millisecond)
	h.Sweep()
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusUnknown {
		t.Fatalf("status = %s, want Unknown before the controller returns", status)
	}

	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseRunning)
	fatalIfProblem(t, "report running after the outage", problem)
	if !result.Accepted {
		t.Fatalf("the returning report was refused: %+v", result)
	}

	recovered := h.Run(admitted.ID)
	if recovered.Status != clusterv1.StatusRunning {
		t.Errorf("status = %s, want Running: the run should recover by itself", recovered.Status)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
