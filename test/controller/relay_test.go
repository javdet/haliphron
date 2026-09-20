package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Relay mode: the controller holds the only copy of a run's result between the
// pod's upload and the backend's acknowledgement.
//
// That is the whole of principle P5 without a bucket, and every case below is
// one way the promise could be broken — acknowledged too early, forwarded too
// late, dropped before it was taken, or written somewhere other than the run
// that produced it.

// The acknowledgement the pod acts on has to mean the bytes are durable. It
// does not mean the backend has them; the pod is free to exit either way, and
// the controller owes the object onward for as long as that takes.
func TestAnAcknowledgedArtifactIsOnDiskBeforeThePodIsAnswered(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	// The control plane is down. The pod must still be able to finish.
	h.Backend.SetUnavailable(true)

	body := []byte("# the run's result\n")
	rec := h.postArtifact(id, runv1.StorageKeyResult, body, h.callbackToken(id))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	var ack runv1.ArtifactAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode the acknowledgement: %v", err)
	}
	want := fmt.Sprintf(runv1.StoragePrefixRun, id) + runv1.StorageKeyResult
	if ack.Ref.Key != want {
		t.Errorf("acknowledged key %q, want %q", ack.Ref.Key, want)
	}
	if !ack.Ref.Uploaded {
		t.Error("the acknowledgement does not mark the object uploaded, so the pod cannot report it")
	}
	if ack.Ref.SizeBytes != int64(len(body)) {
		t.Errorf("acknowledged %d bytes, want %d", ack.Ref.SizeBytes, len(body))
	}

	// On this controller's volume, by the time the pod heard back.
	pending, err := h.Spool.Pending()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Key != runv1.StorageKeyResult {
		t.Fatalf("the spool holds %+v", pending)
	}
	stored, err := h.Spool.Open(pending[0])
	if err != nil {
		t.Fatalf("open the spooled object: %v", err)
	}
	defer func() { _ = stored.Close() }()
	var got bytes.Buffer
	_, _ = got.ReadFrom(stored)
	if got.String() != string(body) {
		t.Errorf("spooled %q", got.String())
	}

	// And the CR says so, which is what keeps the TTL reaper off it.
	cr := h.run(id)
	c := meta.FindStatusCondition(cr.Status.Conditions, agentrunv1alpha1.ConditionArtifactsRelayed)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ConditionArtifactsRelayed = %+v, want False while the spool holds the object", c)
	}
}

// A backend outage costs forwarding latency and nothing else. The spooled copy
// stays put until it is acknowledged, because deleting first would throw away
// the only copy of a result a pod was told was safe.
func TestASpooledArtifactSurvivesABackendOutageAndIsForwardedAfter(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	h.Backend.SetUnavailable(true)
	if rec := h.postArtifact(id, runv1.StorageKeyResult, []byte("# done\n"), h.callbackToken(id)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d (%s)", rec.Code, rec.Body)
	}

	// A flush against a control plane that is not answering leaves it alone.
	_ = h.Reporter.Flush(h.ctx)
	if pending, _ := h.Spool.Pending(); len(pending) != 1 {
		t.Fatalf("the spool dropped an object the backend never took: %+v", pending)
	}
	if _, ok := h.Backend.Artifact(id, runv1.StorageKeyResult); ok {
		t.Fatal("the backend has an object it was never sent")
	}

	h.Backend.SetUnavailable(false)
	h.flush()

	stored, ok := h.Backend.Artifact(id, runv1.StorageKeyResult)
	if !ok {
		t.Fatal("the object was not forwarded once the control plane came back")
	}
	if string(stored) != "# done\n" {
		t.Errorf("the backend received %q", stored)
	}
	// Only now is the spooled copy released.
	if pending, _ := h.Spool.Pending(); len(pending) != 0 {
		t.Errorf("the spool kept %+v after the backend acknowledged", pending)
	}

	cr := h.run(id)
	c := meta.FindStatusCondition(cr.Status.Conditions, agentrunv1alpha1.ConditionArtifactsRelayed)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ConditionArtifactsRelayed = %+v, want True once the spool is empty", c)
	}
}

// A restart between the acknowledgement and the forward is the case the spool
// is a volume for. Clearing it on startup would break the promise the
// acknowledgement made, and the pod that trusted it is long gone.
func TestARestartDoesNotLoseAnAcknowledgedArtifact(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	h.Backend.SetUnavailable(true)
	if rec := h.postArtifact(id, runv1.StorageKeyResult, []byte("# done\n"), h.callbackToken(id)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}

	second := h.restart()
	second.Backend.SetUnavailable(false)
	second.flush()

	if _, ok := second.Backend.Artifact(id, runv1.StorageKeyResult); !ok {
		t.Fatal("an acknowledged artifact was lost across a restart")
	}
}

// The run prefix is stamped from the CR the controller authenticated against. A
// pod that could name an absolute key could name another run's.
func TestAPodCannotNameAnotherRunsPrefix(t *testing.T) {
	h := newHarness(t)
	victim := h.Backend.Enqueue(sampleSpec())
	h.poll()
	attacker := h.Backend.Enqueue(sampleSpec())
	h.poll()

	for _, key := range []string{
		"../" + string(victim) + "/result.md",
		"/etc/passwd",
		"runs/" + string(victim) + "/result.md",
	} {
		rec := h.postArtifact(attacker, key, []byte("theirs now"), h.callbackToken(attacker))
		if rec.Code == http.StatusOK {
			t.Errorf("key %q was accepted", key)
		}
	}
	if h.Spool.Spent(victim) != 0 {
		t.Error("another run's spool was written into")
	}
}

// The token binds to a run, not to a namespace. Without that, any pod in the
// agents namespace could overwrite somebody else's result.
func TestARelayedArtifactNeedsTheRunsOwnToken(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	other := h.Backend.Enqueue(sampleSpec())
	h.poll()

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"no token at all", "", http.StatusUnauthorized},
		{"a token of its own invention", "not-the-token", http.StatusUnauthorized},
		{"another run's token", h.callbackToken(other), http.StatusUnauthorized},
		{"the run's own token", h.callbackToken(id), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.postArtifact(id, runv1.StorageKeyResult, []byte("x"), tc.token)
			if rec.Code != tc.want {
				t.Fatalf("want %d, got %d (%s)", tc.want, rec.Code, rec.Body)
			}
		})
	}
}

// The budget is enforced one hop from the pod, so the transfer is not paid for
// twice. The refusal is a 413 because it is the one the entrypoint can act on:
// it drops the object and carries on rather than failing a run over an
// attachment.
func TestARunThatSpendsItsArtifactBudgetIsRefusedWith413(t *testing.T) {
	h := newHarness(t, withArtifactBudget(64))
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	if rec := h.postArtifact(id, runv1.StorageKeyResult,
		bytes.Repeat([]byte("x"), 40), h.callbackToken(id)); rec.Code != http.StatusOK {
		t.Fatalf("a write inside the budget gave %d (%s)", rec.Code, rec.Body)
	}
	rec := h.postArtifact(id, "logs/chunks/000001.log",
		bytes.Repeat([]byte("x"), 40), h.callbackToken(id))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a write past the budget gave %d, want 413 (%s)", rec.Code, rec.Body)
	}
	// And the result that was already accepted is still there: the cap drops
	// the object that crossed it, not the run.
	if pending, _ := h.Spool.Pending(); len(pending) != 1 {
		t.Errorf("the spool holds %+v after a refused write", pending)
	}
}

// A transfer that was cut is refused rather than spooled: a half-written
// result.md under the right key is worse than none, because the recovery path
// would read it and believe it.
func TestARelayedArtifactWithAWrongDigestIsRefused(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	rec := h.postArtifactWithDigest(id, runv1.StorageKeyResult, []byte("truncated"),
		h.callbackToken(id), strings.Repeat("a", 64))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body)
	}
	if pending, _ := h.Spool.Pending(); len(pending) != 0 {
		t.Errorf("a body that did not match its digest was spooled: %+v", pending)
	}
}

// Artifacts are forwarded before the completion that names them. A report that
// arrived first would leave the control plane holding a result pointing at
// objects it does not have, and the CompletedWithoutResult recovery would read
// that window as a lost result.
func TestArtifactsAreForwardedBeforeTheCompletionThatNamesThem(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	h.Backend.SetUnavailable(true)
	if rec := h.postArtifact(id, runv1.StorageKeyResult, []byte("# done\n"), h.callbackToken(id)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}
	if rec := h.postCompletion(sampleReport(id), h.callbackToken(id)); rec.Code != http.StatusAccepted {
		t.Fatalf("completion: %d", rec.Code)
	}

	h.Backend.SetUnavailable(false)
	h.flush()

	if _, ok := h.Backend.Artifact(id, runv1.StorageKeyResult); !ok {
		t.Fatal("the artifact was not forwarded")
	}
	if state, ok := h.Backend.RunState(id); !ok || state.Completion == nil {
		t.Fatal("the completion was not forwarded")
	}
	// Both arrived; the order is what the flush guarantees, and the property a
	// test can see is that the artifact never arrives second.
	if !h.Backend.ArtifactPrecededCompletion(id) {
		t.Error("the completion reached the control plane before the result it names")
	}
}

// Abandoning a run means its work belongs to another cluster now. Forwarding
// our copy would overwrite theirs under the same key.
func TestAnAbandonedRunsSpoolIsDiscarded(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	h.Backend.SetUnavailable(true)
	if rec := h.postArtifact(id, runv1.StorageKeyResult, []byte("ours"), h.callbackToken(id)); rec.Code != http.StatusOK {
		t.Fatalf("upload: %d", rec.Code)
	}

	// The backend reassigns the run, so every report from here carries a stale
	// epoch and comes back as abandon.
	h.Backend.SetUnavailable(false)
	h.Backend.Reassign(id)
	h.flush()

	if h.Spool.Spent(id) != 0 {
		t.Error("the spool of an abandoned run was kept")
	}
	if _, ok := h.Backend.Artifact(id, runv1.StorageKeyResult); ok {
		t.Error("the artifacts of an abandoned run were forwarded over the new owner's")
	}
}

// ---------------------------------------------------------------------------

// postArtifact uploads the way the entrypoint does: metadata in the query, the
// bytes as the body, the digest in a header.
func (h *harness) postArtifact(id runv1.ULID, key string, body []byte, token string) *httptest.ResponseRecorder {
	h.t.Helper()
	sum := sha256.Sum256(body)
	return h.postArtifactWithDigest(id, key, body, token, hex.EncodeToString(sum[:]))
}

func (h *harness) postArtifactWithDigest(id runv1.ULID, key string, body []byte,
	token, digest string) *httptest.ResponseRecorder {

	h.t.Helper()
	q := url.Values{
		"runID":            {string(id)},
		runv1.QueryKey:     {key},
		runv1.QueryAttempt: {strconv.Itoa(1)},
	}
	req := httptest.NewRequest(http.MethodPost,
		runv1.CallbackPathArtifacts+"?"+q.Encode(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(runv1.HeaderSHA256, digest)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Callback.Handler().ServeHTTP(rec, req)
	return rec
}

// postPhase reports one entrypoint phase the way the pod does.
func (h *harness) postPhase(id runv1.ULID, phase runv1.RuntimePhase,
	outcome runv1.PhaseOutcome, token string) *httptest.ResponseRecorder {

	h.t.Helper()
	body, err := json.Marshal(runv1.PhaseReport{
		RunID: id, Attempt: 1, Phase: phase, Outcome: outcome,
	})
	if err != nil {
		h.t.Fatalf("marshal the phase report: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, runv1.CallbackPathPhase, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Callback.Handler().ServeHTTP(rec, req)
	return rec
}
