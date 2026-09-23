package backend_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/backend"
)

// These are the "backend, against a fake controller" cases from section 13 of
// docs/contracts/cluster-api.md, run against the fake.
//
// They are not tests of an implementation detail. The controller team will
// write its side against this fake and will believe whatever it does, so every
// rule the fake gets wrong becomes a rule the controller gets wrong, discovered
// against the real backend months later. The checklist was written before
// either side existed for exactly this reason.

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

func TestRegisterIsIdempotentOnTheKeyPair(t *testing.T) {
	t.Parallel()
	b, c := start(t)

	// The case this exists for: a controller that received a 200 and died
	// before writing the clusterID into its Secret. Without idempotency the
	// token is spent, the identity is lost, and only a human with the UI can
	// repair it.
	again := c.register(b.BootstrapToken(), "cluster-a")
	if again.status != http.StatusOK {
		t.Fatalf("repeat register: status %d body %s", again.status, again.body)
	}
	var out clusterv1.RegisterResponse
	again.decode(t, &out)
	if out.ClusterID != c.cluster {
		t.Fatalf("repeat register handed out a new identity: %s then %s", c.cluster, out.ClusterID)
	}
}

func TestRegisterWithASpentTokenAndADifferentKeyIsRefused(t *testing.T) {
	t.Parallel()
	b, c := start(t)

	thief := newClient(t, c.baseURL, "kid-thief")
	resp := thief.register(b.BootstrapToken(), "cluster-thief")
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Code != clusterv1.CodeBootstrapTokenConsumed {
		t.Fatalf("want BootstrapTokenConsumed, got %s", p.Code)
	}
}

func TestRegisterRefusesADuplicateName(t *testing.T) {
	t.Parallel()
	b, c := start(t)

	other := newClient(t, c.baseURL, "kid-b")
	resp := other.register(b.IssueBootstrapToken(), "cluster-a")
	if resp.status != http.StatusConflict {
		t.Fatalf("want 409, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Code != clusterv1.CodeClusterNameTaken || p.Action != clusterv1.ActionFatal {
		t.Fatalf("want ClusterNameTaken/fatal, got %s/%s", p.Code, p.Action)
	}
}

func TestRegisterReturnsTheOperationalParameters(t *testing.T) {
	t.Parallel()
	b := backend.New()
	c := newClient(t, serve(t, b), "kid-a")
	out := c.mustRegister(b.BootstrapToken(), "cluster-a")

	// The intervals come from the control plane, not the cluster's values.yaml:
	// otherwise staleAfter drifts per installation and nobody can say what it
	// is. serverTime is how the controller notices clock skew at startup
	// instead of through random 401s under load.
	if out.Timings.AckTimeoutSeconds == 0 || out.Timings.LeaseTTLSeconds == 0 {
		t.Fatalf("timings not bootstrapped: %+v", out.Timings)
	}
	if out.TokenAudience != clusterv1.TokenAudience {
		t.Fatalf("audience %q", out.TokenAudience)
	}
	if out.ServerTime.IsZero() {
		t.Fatal("serverTime missing: clock skew becomes undiagnosable")
	}
}

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

func TestTokenChecks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*client)
		status int
		code   clusterv1.ProblemCode
		action clusterv1.Action
	}{
		{
			name:   "wrong audience",
			mutate: func(c *client) { c.audience = "haliphron-public-api" },
			status: http.StatusUnauthorized, code: clusterv1.CodeUnauthenticated,
		},
		{
			name:   "expired",
			mutate: func(c *client) { c.ttl = -time.Hour },
			status: http.StatusUnauthorized, code: clusterv1.CodeUnauthenticated,
		},
		{
			// Longer than the contract's ceiling. A cluster token that lives
			// for a day is the thing self-signed JWTs exist to avoid.
			name:   "lifetime beyond the ceiling",
			mutate: func(c *client) { c.ttl = 24 * time.Hour },
			status: http.StatusUnauthorized, code: clusterv1.CodeUnauthenticated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, c := start(t)
			tc.mutate(c)
			resp := c.lease(clusterv1.LeaseRequest{FreeSlots: 1, WaitSeconds: 1})
			if resp.status != tc.status {
				t.Fatalf("want %d, got %d body %s", tc.status, resp.status, resp.body)
			}
			if p := resp.problem(t); p.Code != tc.code {
				t.Fatalf("want %s, got %s", tc.code, p.Code)
			}
		})
	}
}

func TestRevokedClusterIsFatal(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Revoke(c.cluster)

	resp := c.lease(clusterv1.LeaseRequest{FreeSlots: 1, WaitSeconds: 1})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body %s", resp.status, resp.body)
	}
	p := resp.problem(t)
	// Fatal, not retry: the controller must stop polling and raise an event.
	// Retrying a revocation is a cluster hammering a control plane forever.
	if p.Code != clusterv1.CodeClusterRevoked || p.Action != clusterv1.ActionFatal {
		t.Fatalf("want ClusterRevoked/fatal, got %s/%s", p.Code, p.Action)
	}
}

func TestSubjectMustMatchTheKeysCluster(t *testing.T) {
	t.Parallel()
	b, a := start(t)
	other := join(t, b, a, "kid-b", "cluster-b")

	// Cluster A's key, claiming to be cluster B.
	forged := *a
	forged.cluster = other.cluster
	resp := a.post("/clusters/"+string(a.cluster)+"/leases", forged.token(),
		clusterv1.LeaseRequest{FreeSlots: 1, WaitSeconds: 1})

	if resp.status != http.StatusForbidden {
		t.Fatalf("want 403, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Code != clusterv1.CodeClusterMismatch {
		t.Fatalf("want ClusterMismatch, got %s", p.Code)
	}
}

// ---------------------------------------------------------------------------
// version compatibility
// ---------------------------------------------------------------------------

func TestControllerVersionWindow(t *testing.T) {
	t.Parallel()
	b := backend.New(backend.WithVersionRange("1.0.0", "1.4.0"))
	url := serve(t, b)

	c := newClient(t, url, "kid-a")
	c.mustRegister(b.BootstrapToken(), "cluster-a")

	c.version = "2.0.0"
	resp := c.lease(clusterv1.LeaseRequest{FreeSlots: 1, WaitSeconds: 1})
	if resp.status != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Code != clusterv1.CodeUnsupportedControllerVersion {
		t.Fatalf("want UnsupportedControllerVersion, got %s", p.Code)
	}

	// Missing entirely: in a multi-cluster installation there is otherwise no
	// telling which version sent what, and that is always asked after the fact.
	c.version = ""
	if resp := c.lease(clusterv1.LeaseRequest{FreeSlots: 1}); resp.status != http.StatusBadRequest {
		t.Fatalf("want 400 without the version header, got %d", resp.status)
	}
}

func TestUnknownFieldsAreIgnored(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())

	// Rule 2 of the compatibility section: both sides ignore what they do not
	// recognise. It costs a line and removes a whole class of upgrade failure,
	// and it is skipped just as regularly.
	resp := c.post("/clusters/"+string(c.cluster)+"/leases", c.token(), map[string]any{
		"freeSlots": 1, "waitSeconds": 1, "somethingFromTheFuture": []string{"a", "b"},
	})
	if resp.status != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", resp.status, resp.body)
	}
}

// ---------------------------------------------------------------------------
// leasing
// ---------------------------------------------------------------------------

func TestEmptyQueueLongPollsThenAnswers204(t *testing.T) {
	t.Parallel()
	_, c := start(t)

	started := time.Now()
	leases, resp := c.poll(1)
	elapsed := time.Since(started)

	if resp.status != http.StatusNoContent || len(leases) != 0 {
		t.Fatalf("want 204 with no leases, got %d with %d", resp.status, len(leases))
	}
	// It must hold the connection: answering immediately turns a long poll into
	// polling, which is the traffic pattern the design exists to avoid.
	if elapsed < 900*time.Millisecond {
		t.Fatalf("returned after %s, expected to hold for ~1s", elapsed)
	}
}

func TestLeaseWakesTheWaitingPoll(t *testing.T) {
	t.Parallel()
	b, c := start(t)

	done := make(chan []clusterv1.Lease, 1)
	go func() {
		leases, _ := c.poll(1)
		done <- leases
	}()

	time.Sleep(50 * time.Millisecond)
	b.Enqueue(sampleSpec())

	select {
	case leases := <-done:
		if len(leases) != 1 {
			t.Fatalf("want 1 lease, got %d", len(leases))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll did not wake when work arrived")
	}
}

func TestConcurrentPollsNeverHandOutTheSameRunTwice(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	// Room for all twenty: this is about disjointness, not the capacity
	// ceiling, which would otherwise stop issuance at the registered ten.
	c.heartbeat(clusterv1.HeartbeatRequest{FreeSlots: 20, CapacitySlots: 20, Runs: []clusterv1.RunObservation{}})
	for i := 0; i < 20; i++ {
		b.Enqueue(sampleSpec())
	}

	var mu sync.Mutex
	seen := map[runv1.ULID]int{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				leases, _ := c.poll(3)
				mu.Lock()
				for _, l := range leases {
					seen[l.RunID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	for id, n := range seen {
		if n > 1 {
			t.Fatalf("run %s was issued %d times: two controllers would run it twice", id, n)
		}
	}
	if len(seen) != 20 {
		t.Fatalf("issued %d of 20 runs", len(seen))
	}
}

func TestLeaseIsCappedByFreeSlotsAndByThePollCeiling(t *testing.T) {
	t.Parallel()
	b, c := start(t, backend.WithTimings(timingsWith(func(tm *clusterv1.Timings) {
		tm.MaxLeasesPerPoll = 2
	})))
	for i := 0; i < 5; i++ {
		b.Enqueue(sampleSpec())
	}

	leases, _ := c.poll(4)
	if len(leases) != 2 {
		t.Fatalf("want 2 leases (the ceiling), got %d", len(leases))
	}
	if leases, _ = c.poll(1); len(leases) != 1 {
		t.Fatalf("want 1 lease (free slots), got %d", len(leases))
	}
}

// The controller re-polls at once after any poll that returned work, so
// freeSlots and the per-poll ceiling bound one answer and not the total. The
// declared capacity less what the cluster already holds bounds the total.
func TestLeaseIsCappedByTheDeclaredCapacityAcrossPolls(t *testing.T) {
	t.Parallel()
	b, c := start(t) // registers with capacitySlots 10
	for i := 0; i < 15; i++ {
		b.Enqueue(sampleSpec())
	}

	total := 0
	for i := 0; i < 3; i++ {
		leases, _ := c.poll(10)
		total += len(leases)
	}
	if total != 10 {
		t.Fatalf("a cluster declaring 10 slots was handed %d runs over three polls", total)
	}
}

func TestACapacityAboveTheMaximumIsRefused(t *testing.T) {
	t.Parallel()
	_, c := start(t)
	resp := c.post("/clusters/"+string(c.cluster)+"/heartbeat", c.token(), clusterv1.HeartbeatRequest{
		CapacitySlots: clusterv1.MaxCapacitySlots + 1, Runs: []clusterv1.RunObservation{},
	})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: an unbounded declaration is an unbounded drain", resp.status)
	}
	if p := resp.problem(t); p.Action != clusterv1.ActionFatal {
		t.Fatalf("action = %s, want fatal", p.Action)
	}
}

func TestLeaseAndArtifactResponsesAreNoStore(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())

	leases, resp := c.poll(1)
	if got := resp.headers.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("lease Cache-Control = %q: a cache holding this body is a git token on disk", got)
	}
	l := leases[0]
	c.mustAck(l.RunID, l.Epoch)

	art := c.artifacts(l.RunID, l.Epoch, l.Attempt)
	if got := art.headers.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("artifacts Cache-Control = %q", got)
	}
}

func TestLeaseBodyNeverReachesTheLog(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	const secret = "ghs_a_very_secret_token"
	b.Enqueue(sampleSpec(), backend.WithSecrets(map[string]string{
		runv1.SecretKeyGitToken: secret,
	}))

	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	c.artifacts(l.RunID, l.Epoch, l.Attempt)

	// The lease is the one place secret material crosses the network in the
	// clear. The contract permits the runID, the epoch and a count, and nothing
	// else, at any level.
	for _, line := range b.Log() {
		if strings.Contains(line, secret) {
			t.Fatalf("the git token reached the log: %q", line)
		}
		if strings.Contains(line, "?sig=") {
			t.Fatalf("a presigned URL reached the log: %q", line)
		}
	}
}

func TestArtifactBundleIsReissuedWithALaterExpiry(t *testing.T) {
	t.Parallel()
	// Object-store mode: this is the one call that only exists there. In relay
	// mode there is no signature to expire and the controller never makes it.
	b, c := start(t, backend.WithArtifactMode(runv1.ArtifactModeObjectStore))
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	b.AdvanceClock(30 * time.Minute)
	resp := c.artifacts(l.RunID, l.Epoch, 2)
	if resp.status != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", resp.status, resp.body)
	}
	var bundle clusterv1.ArtifactBundle
	resp.decode(t, &bundle)

	// Deliberately not idempotent: the whole point of the call is an expiry
	// later than the one the caller already holds. A bundle that outlived the
	// run looks like a lost result on work that succeeded.
	if !bundle.ExpiresAt.After(l.Artifacts.ExpiresAt) {
		t.Fatalf("reissued bundle expires at %s, no later than the original %s",
			bundle.ExpiresAt, l.Artifacts.ExpiresAt)
	}
	// The four keys the pod writes, and no reads at all: prompt.txt and
	// state.json are both gone from this store, which is what removed exit 21's
	// most common cause.
	for _, key := range []string{
		runv1.StorageKeyOutput, runv1.StorageKeyResult,
		runv1.StorageKeyCompletion, runv1.StorageKeyAgentLog,
	} {
		if _, ok := bundle.Put[key]; !ok {
			t.Errorf("the reissued bundle has no PUT for %s", key)
		}
	}
}

// The default is relay, because that is what an installation gets with nothing
// configured — and a controller has to work against it without being told
// anything. A fake whose default differed would let a controller pass its
// contract tests against a path most installations never take.
func TestALeaseDefaultsToRelayAndCarriesNoBucket(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec(), backend.WithPrompt("add a postgres database"))
	l := c.mustLease()

	switch {
	case !l.Artifacts.Relay():
		t.Errorf("mode = %q, want relay", l.Artifacts.Mode)
	case l.Artifacts.Bucket != "":
		t.Errorf("a relay lease names bucket %q", l.Artifacts.Bucket)
	case len(l.Artifacts.Put) != 0 || len(l.Artifacts.Post) != 0:
		t.Errorf("a relay lease carries capabilities: %+v", l.Artifacts)
	case l.Artifacts.MaxBytesPerRun <= 0:
		t.Error("a lease with no artifact budget lets a run fill the control plane's volume")
	}
}

// The prompt travels in the lease, and the two digests beside it agree with it.
// A controller checks both before writing a Secret, so a fake that set only one
// would let a controller bug through.
func TestALeaseCarriesThePromptAndAMatchingDigest(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec(), backend.WithPrompt("add a postgres database"))
	l := c.mustLease()

	if l.Prompt != "add a postgres database" {
		t.Fatalf("lease prompt = %q", l.Prompt)
	}
	sum := sha256.Sum256([]byte(l.Prompt))
	want := hex.EncodeToString(sum[:])
	if l.PromptSHA256 != want {
		t.Errorf("lease digest = %q, want %q", l.PromptSHA256, want)
	}
	if l.Spec.PromptSHA256 != want {
		t.Errorf("spec digest = %q, want %q", l.Spec.PromptSHA256, want)
	}
	// And the spec carries no prompt of its own: a field for one would put
	// customer text into every `kubectl get agentrun -o yaml`.
	if strings.Contains(string(mustJSON(t, l.Spec)), "add a postgres database") {
		t.Error("the rendered spec carries the prompt text")
	}
}

// A controller relays an artifact, and the fake keeps it under the run's prefix
// — stamped from the authenticated envelope, not from the query.
func TestARelayedArtifactLandsUnderTheRunsPrefix(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()

	body := []byte("# done\n")
	resp := c.relayArtifact(l.RunID, l.Epoch, l.Attempt, runv1.StorageKeyResult, body)
	if resp.status != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", resp.status, resp.body)
	}
	var ack clusterv1.ArtifactIngestResponse
	resp.decode(t, &ack)

	want := "runs/" + string(l.RunID) + "/" + runv1.StorageKeyResult
	if ack.Ref.Key != want {
		t.Errorf("stored under %q, want %q", ack.Ref.Key, want)
	}
	if !ack.Ref.Uploaded {
		t.Error("the acknowledgement does not mark the object uploaded")
	}
	stored, ok := b.Artifact(l.RunID, runv1.StorageKeyResult)
	if !ok || string(stored) != string(body) {
		t.Errorf("the fake stored %q (found: %v)", stored, ok)
	}

	// A repeat is a success and says so: the controller retries on any network
	// error and does not drop its spooled copy until it hears a 200.
	again := c.relayArtifact(l.RunID, l.Epoch, l.Attempt, runv1.StorageKeyResult, body)
	if again.status != http.StatusOK {
		t.Fatalf("a repeat gave %d", again.status)
	}
	again.decode(t, &ack)
	if !ack.Duplicate {
		t.Error("a repeated relay was not reported as a duplicate")
	}
}

// A cluster that lost a run must not overwrite the result of the one that holds
// it now. The refusal carries abandon, which is what makes the controller drop
// its spool rather than retry forever.
func TestARelayedArtifactUnderAStaleEpochIsRefused(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	b.Reassign(l.RunID)

	resp := c.relayArtifact(l.RunID, l.Epoch, l.Attempt, runv1.StorageKeyResult, []byte("stale"))
	if resp.status != http.StatusConflict {
		t.Fatalf("want 409, got %d body %s", resp.status, resp.body)
	}
	if _, ok := b.Artifact(l.RunID, runv1.StorageKeyResult); ok {
		t.Error("the stale cluster's bytes were stored anyway")
	}
}

// A transfer that was cut is refused rather than stored: a half-written
// result.md under the right key is worse than none, because the recovery path
// would read it and believe it.
func TestARelayedArtifactWithAWrongDigestIsRefused(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()

	resp := c.relayArtifactWithDigest(l.RunID, l.Epoch, l.Attempt,
		runv1.StorageKeyResult, []byte("truncated"), strings.Repeat("a", 64))
	if resp.status != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body %s", resp.status, resp.body)
	}
	if _, ok := b.Artifact(l.RunID, runv1.StorageKeyResult); ok {
		t.Error("a body that did not match its digest was stored")
	}
}

func TestArtifactsWithAStaleEpochAreRefused(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()
	b.Reassign(l.RunID)

	resp := c.artifacts(l.RunID, l.Epoch, l.Attempt)
	if resp.status != http.StatusConflict {
		t.Fatalf("want 409, got %d body %s", resp.status, resp.body)
	}
	if p := resp.problem(t); p.Action != clusterv1.ActionAbandon {
		t.Fatalf("want abandon, got %s", p.Action)
	}
}

// ---------------------------------------------------------------------------
// the two deadlines
// ---------------------------------------------------------------------------

func TestAckDeadlineExpiryRequeuesWithANewEpoch(t *testing.T) {
	t.Parallel()
	b, a := start(t)
	other := join(t, b, a, "kid-b", "cluster-b")
	id := b.Enqueue(sampleSpec())

	l := a.mustLease()
	if l.Epoch != 1 {
		t.Fatalf("first issuance must be epoch 1, got %d", l.Epoch)
	}

	// Nothing was created in the cluster, so nothing started: this is the one
	// expiry where reassigning immediately is safe.
	b.AdvanceClock(time.Duration(clusterv1.DefaultTimings().AckTimeoutSeconds+1) * time.Second)

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusQueued {
		t.Fatalf("want Queued after the ack deadline, got %s", st.Status)
	}
	if st.Epoch != 2 {
		t.Fatalf("want epoch 2, got %d: without the bump the dead controller's late ack is accepted", st.Epoch)
	}

	moved := other.mustLease()
	if moved.RunID != id || moved.Epoch != 2 {
		t.Fatalf("want run %s at epoch 2 on the other cluster, got %s at %d", id, moved.RunID, moved.Epoch)
	}

	// The first controller comes back and acks the work it was given. Its
	// epoch is stale, and it must be told to abandon rather than allowed to
	// dispatch a run someone else is now holding.
	late := a.ack(id, l.Epoch)
	if late.status != http.StatusConflict {
		t.Fatalf("want 409 for the late ack, got %d body %s", late.status, late.body)
	}
	if p := late.problem(t); p.Code != clusterv1.CodeEpochMismatch || p.Action != clusterv1.ActionAbandon {
		t.Fatalf("want EpochMismatch/abandon, got %s/%s", p.Code, p.Action)
	}
}

// An ack timeout keeps the assignment and excludes nothing, so unlike a
// negative ack it cannot run out of clusters to try. A controller that takes
// the lease and whose ack never arrives would be handed the run forever; the
// ceiling ends it, and an operator's retry starts the count again.
func TestAckDeadlineExpiriesFailTheRunAtTheCeiling(t *testing.T) {
	t.Parallel()
	b, a := start(t, backend.WithMaxAckExpiries(3))
	id := b.Enqueue(sampleSpec())
	window := time.Duration(clusterv1.DefaultTimings().AckTimeoutSeconds+1) * time.Second

	var last clusterv1.Lease
	for i := int64(1); i <= 3; i++ {
		last = a.mustLease()
		if last.Epoch != i {
			t.Fatalf("lease %d carries epoch %d", i, last.Epoch)
		}
		b.AdvanceClock(window)
	}

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusFailed {
		t.Fatalf("want Failed after three unacknowledged leases, got %s", st.Status)
	}
	if st.Reason != "AckTimeoutExhausted" || st.FailureClass != runv1.FailureInfra {
		t.Errorf("want AckTimeoutExhausted/infra, got %s/%s", st.Reason, st.FailureClass)
	}
	if st.Epoch != last.Epoch+1 {
		t.Errorf("want epoch %d, got %d: the last holder must be fenced", last.Epoch+1, st.Epoch)
	}
	if leases, _ := a.poll(1); len(leases) != 0 {
		t.Fatalf("a failed run was handed out again: %+v", leases)
	}
	if p := a.ack(id, last.Epoch).problem(t); p.Action != clusterv1.ActionAbandon {
		t.Errorf("the last holder's late ack got %s, want abandon", p.Action)
	}
	audited := false
	for _, e := range b.Audit() {
		audited = audited || (e.RunID == id && e.Kind == backend.AuditAckTimeoutExhausted)
	}
	if !audited {
		t.Error("the run was failed without an audit record")
	}

	b.Retry(id)
	a.mustLease()
	b.AdvanceClock(window)
	if st, _ := b.RunState(id); st.Status != clusterv1.StatusQueued {
		t.Errorf("after a retry one expiry gave %s, want Queued: the count starts again", st.Status)
	}
}

func TestLeaseDeadlineExpiryGivesUnknownNotQueued(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())

	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)
	c.ingest(observation(l, runv1.PhaseRunning))

	b.AdvanceClock(time.Duration(clusterv1.DefaultTimings().LeaseTTLSeconds+1) * time.Second)

	st, _ := b.RunState(id)
	// The Job may be running this second. Requeueing would be the expensive
	// mistake: two agents on one repository, two PRs and two bills, over what
	// may have been a ten-second network glitch.
	if st.Status != clusterv1.StatusUnknown {
		t.Fatalf("want Unknown after the lease deadline, got %s", st.Status)
	}
	if st.Epoch != l.Epoch {
		t.Fatalf("epoch changed to %d: a lost heartbeat is not a reassignment", st.Epoch)
	}
}

func TestHeartbeatExtendsTheLease(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())
	l := c.mustLease()
	c.mustAck(l.RunID, l.Epoch)

	b.AdvanceClock(60 * time.Second)
	resp := c.heartbeat(clusterv1.HeartbeatRequest{
		FreeSlots: 1, ReportComplete: true,
		Runs: []clusterv1.RunObservation{observation(l, runv1.PhaseRunning)},
	})
	if len(resp.Leases) != 1 {
		t.Fatalf("want 1 renewal, got %d", len(resp.Leases))
	}

	b.AdvanceClock(90 * time.Second)
	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusRunning {
		t.Fatalf("want Running after a renewal, got %s: the heartbeat did not extend the deadline", st.Status)
	}
}

// ---------------------------------------------------------------------------
// negative acknowledgement
// ---------------------------------------------------------------------------

func TestNegativeAckRequeuesAndExcludesTheCluster(t *testing.T) {
	t.Parallel()
	b, a := start(t)
	other := join(t, b, a, "kid-b", "cluster-b")
	id := b.Enqueue(sampleSpec())

	l := a.mustLease()
	resp := a.rejectAck(l.RunID, l.Epoch, clusterv1.RejectSpecFieldsPruned,
		"the CRD in this cluster pruned runtime.mcpServers")
	if resp.status != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", resp.status, resp.body)
	}

	st, _ := b.RunState(id)
	if st.Status != clusterv1.StatusQueued || st.Epoch != 2 {
		t.Fatalf("want Queued at epoch 2, got %s at %d", st.Status, st.Epoch)
	}

	// Without the exclusion the same lease returns to the same cluster
	// forever, and a spec one cluster cannot materialise becomes an infinite
	// loop rather than a failure someone can read.
	if leases, _ := a.poll(1); len(leases) != 0 {
		t.Fatalf("the refusing cluster was offered the work again")
	}
	if moved := other.mustLease(); moved.RunID != id {
		t.Fatalf("want the work on the other cluster, got %s", moved.RunID)
	}
}

func TestNegativeAckWithNowhereElseToGoFails(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	id := b.Enqueue(sampleSpec())

	l := c.mustLease()
	c.rejectAck(l.RunID, l.Epoch, clusterv1.RejectQuotaExhausted, "exceeded quota in haliphron-agents")

	st, _ := b.RunState(id)
	// An endless queue is the silent failure. Nobody can run this, and the
	// operator needs to be told why rather than watch it sit at Queued.
	if st.Status != clusterv1.StatusFailed {
		t.Fatalf("want Failed, got %s", st.Status)
	}
	if st.FailureClass != runv1.FailureConfig {
		t.Fatalf("want class config, got %q", st.FailureClass)
	}
	if !strings.Contains(st.Message, "exceeded quota") {
		t.Fatalf("the reason did not survive to the UI: %q", st.Message)
	}
}

// ---------------------------------------------------------------------------
// injected faults
// ---------------------------------------------------------------------------

func TestUnavailableTellsTheControllerToBackOff(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.SetUnavailable(true)

	resp := c.lease(clusterv1.LeaseRequest{FreeSlots: 1, WaitSeconds: 1})
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.status)
	}
	p := resp.problem(t)
	if p.Action != clusterv1.ActionBackoff {
		t.Fatalf("want backoff, got %s", p.Action)
	}
	if resp.headers.Get("Retry-After") == "" {
		t.Fatal("no Retry-After: backoff without a floor is a thundering herd on a recovering backend")
	}

	// Work taken before the outage is still the cluster's when it returns:
	// availability decoupling means an unreachable control plane does not kill
	// an hour of agent work.
	b.SetUnavailable(false)
	b.Enqueue(sampleSpec())
	if l := c.mustLease(); l.RunID == "" {
		t.Fatal("no lease after recovery")
	}
}

func TestOversizedBodyIsRefusedBeforeItIsParsed(t *testing.T) {
	t.Parallel()
	_, c := start(t)

	huge := strings.Repeat("x", clusterv1.MaxRequestBytes+1024)
	resp := c.post("/ingest/status", c.token(), map[string]any{
		"clusterID": string(c.cluster),
		"reports":   []any{map[string]any{"runID": huge}},
	})
	if resp.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", resp.status)
	}
}

func TestBatchSizeIsBounded(t *testing.T) {
	t.Parallel()
	b, c := start(t)
	b.Enqueue(sampleSpec())
	l := c.mustLease()

	reports := make([]clusterv1.RunObservation, clusterv1.MaxStatusReports+1)
	for i := range reports {
		reports[i] = observation(l, runv1.PhaseRunning)
	}
	resp := c.post("/ingest/status", c.token(), clusterv1.StatusIngestRequest{
		ClusterID: c.cluster, Reports: reports,
	})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("want 400 for an oversized batch, got %d body %s", resp.status, resp.body)
	}
}
