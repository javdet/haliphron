package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	fakebackend "github.com/automagicops/haliphron/fake/backend"
)

// Inputs: the files a person attached to a request. The controller fetches
// every one into its spool and verifies it before it acknowledges, so that
// nothing starts before the ack and a run it has taken still starts while the
// backend is down; the pod reads them back from the controller.

var (
	screenshot = fakebackend.InputFile{Name: "screenshot of the error.png", Body: []byte("\x89PNG\r\n\x1a\nnot really a png")}
	buildLog   = fakebackend.InputFile{Name: "build.log", Body: []byte("step 4/9 failed\n")}
)

func TestNoAckIsSentUntilEveryInputIsSpooledAndVerified(t *testing.T) {
	faults := newFaults()
	h := newHarness(t, withFaults(faults))
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot, buildLog))
	manifest := h.Backend.Inputs(id)

	faults.Fail("/inputs/"+manifest[1].File(), true)
	h.poll()

	if _, err := h.runErr(id); err == nil {
		t.Fatal("an AgentRun was created for a lease whose inputs were not all fetched")
	}
	if state, _ := h.Backend.RunState(id); state.Acked {
		t.Fatal("the lease was acknowledged with an input missing")
	}
	if !h.Spool.HasInput(id, manifest[0]) {
		t.Fatal("the input that did arrive was not kept for the next lease")
	}
	if h.Spool.HasInput(id, manifest[1]) {
		t.Fatal("an input that never arrived is in the spool")
	}

	// The backend reassigns; this time the transfer works.
	faults.Fail("/inputs/"+manifest[1].File(), false)
	h.Backend.Reassign(id)
	h.poll()

	state, _ := h.Backend.RunState(id)
	if !state.Acked || state.Epoch != 2 {
		t.Fatalf("want the second lease acknowledged, got acked=%t epoch=%d", state.Acked, state.Epoch)
	}
	for _, ref := range manifest {
		if ok, err := h.Spool.InputMatches(id, ref); err != nil || !ok {
			t.Fatalf("an input is not spooled and verified at the ack: %v", err)
		}
	}
	if n := h.Backend.InputFetches(id, manifest[0].Key); n != 1 {
		t.Errorf("an input already held was fetched again: %d fetches", n)
	}
}

func TestAnInputTheBackendNoLongerHasRefusesTheLease(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot, buildLog))
	manifest := h.Backend.Inputs(id)
	h.Backend.DropInput(id, manifest[1].Key)

	h.poll()
	assertRefused(t, h, id, manifest)
}

func TestAnInputWhoseBytesDoNotMatchTheManifestRefusesTheLease(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot, buildLog))
	manifest := h.Backend.Inputs(id)
	h.Backend.CorruptInput(id, manifest[1].Key, []byte("step 4/9 FAILED\n"))

	h.poll()
	assertRefused(t, h, id, manifest)
}

func assertRefused(t *testing.T, h *harness, id runv1.ULID, manifest []runv1.InputRef) {
	t.Helper()
	state, _ := h.Backend.RunState(id)
	if state.Acked {
		t.Fatal("the lease was acknowledged as taken")
	}
	excluded := false
	for _, c := range state.Excluded {
		excluded = excluded || c == h.ClusterID
	}
	if !excluded || !strings.Contains(state.Message, string(clusterv1.RejectMaterializationFailed)) {
		t.Fatalf("want a negative ack with MaterializationFailed, got excluded=%t message=%q", excluded, state.Message)
	}
	if strings.Contains(state.Message, "screenshot") || strings.Contains(state.Message, "build") {
		t.Fatalf("the refusal quotes an input's name: %q", state.Message)
	}
	if _, err := h.runErr(id); err == nil {
		t.Fatal("an AgentRun exists for a refused lease")
	}
	for _, ref := range manifest {
		if h.Spool.HasInput(id, ref) {
			t.Fatal("the inputs of a refused lease were kept")
		}
	}
}

func TestTheSecretCarriesTheInputManifestAndTheAgentRunDoesNot(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot, buildLog))
	h.poll()

	secret := h.secret(id)
	var manifest runv1.InputManifest
	if err := json.Unmarshal(secret.Data[runv1.SecretKeyInputs], &manifest); err != nil {
		t.Fatalf("the Secret's %s does not parse: %v", runv1.SecretKeyInputs, err)
	}
	want := h.Backend.Inputs(id)
	if len(manifest.Inputs) != len(want) {
		t.Fatalf("the Secret lists %d inputs, the lease %d", len(manifest.Inputs), len(want))
	}
	for i := range want {
		if manifest.Inputs[i] != want[i] {
			t.Fatalf("input %d differs: %+v vs %+v", i, manifest.Inputs[i], want[i])
		}
	}

	raw, _ := json.Marshal(h.run(id))
	for _, ref := range want {
		for _, leak := range []string{ref.Name, ref.Key, ref.SHA256} {
			if bytes.Contains(raw, []byte(leak)) {
				t.Fatalf("the AgentRun carries %q", leak)
			}
		}
	}

	// A run without inputs has no key at all, rather than an empty list.
	plain := h.Backend.Enqueue(sampleSpec())
	h.poll()
	if _, ok := h.secret(plain).Data[runv1.SecretKeyInputs]; ok {
		t.Fatal("a run without inputs has an input manifest")
	}
}

func TestTheCallbackServesARunsOwnInputsAndNothingElse(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot, buildLog))
	other := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(
		fakebackend.InputFile{Name: "someone else's.txt", Body: []byte("private\n")}))
	h.poll()
	mine, theirs := h.Backend.Inputs(id), h.Backend.Inputs(other)

	rec := h.getInput(id, mine[0].Key, h.callbackToken(id))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), screenshot.Body) {
		t.Fatalf("the run's own input: %d %q", rec.Code, rec.Body.Bytes())
	}
	if rec.Header().Get(runv1.HeaderSHA256) != mine[0].SHA256 ||
		rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("Content-Type") != "application/octet-stream" ||
		rec.Header().Get("Content-Length") == "" {
		t.Fatalf("headers: %v", rec.Header())
	}

	cases := []struct {
		name  string
		run   runv1.ULID
		key   string
		token string
		want  int
	}{
		{"another run's key under this run", id, theirs[0].Key, h.callbackToken(id), http.StatusNotFound},
		{"a key in the layout that the manifest does not name", id, "inputs/9-other.png", h.callbackToken(id), http.StatusNotFound},
		{"another run's token", id, mine[0].Key, h.callbackToken(other), http.StatusUnauthorized},
		{"no token", id, mine[0].Key, "", http.StatusUnauthorized},
		{"a key outside the layout", id, "inputs/../result.md", h.callbackToken(id), http.StatusBadRequest},
		{"an artifact key", id, runv1.StorageKeyResult, h.callbackToken(id), http.StatusBadRequest},
		{"no run named", "", mine[0].Key, h.callbackToken(id), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := h.getInput(tc.run, tc.key, tc.token); rec.Code != tc.want {
				t.Fatalf("want %d, got %d", tc.want, rec.Code)
			}
		})
	}

	// In the manifest and gone from the spool: the pod retries a 503.
	if err := h.Spool.ForgetInputs(id); err != nil {
		t.Fatal(err)
	}
	if rec := h.getInput(id, mine[0].Key, h.callbackToken(id)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a spooled input that went missing: want 503, got %d", rec.Code)
	}
}

func TestARestartBetweenFetchAndAckRepeatsTheAckWithoutFetchingAgain(t *testing.T) {
	faults := newFaults()
	h := newHarness(t, withFaults(faults))
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot, buildLog))
	manifest := h.Backend.Inputs(id)

	faults.Fail("/ack", true)
	h.poll()
	faults.Fail("/ack", false)

	next := h.restart()
	next.Lease.Resume(next.ctx)

	if state, _ := h.Backend.RunState(id); !state.Acked || state.Epoch != 1 {
		t.Fatalf("a restarted controller did not repeat the acknowledgement: acked=%t epoch=%d", state.Acked, state.Epoch)
	}
	for _, ref := range manifest {
		if n := h.Backend.InputFetches(id, ref.Key); n != 1 {
			t.Errorf("an input still in the spool was fetched %d times", n)
		}
		if !next.Spool.HasInput(id, ref) {
			t.Error("an input is missing after the restart")
		}
	}
}

func TestARestartThatLostTheSpooledInputsFetchesThemBeforeRepeatingTheAck(t *testing.T) {
	faults := newFaults()
	h := newHarness(t, withFaults(faults))
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot, buildLog))
	manifest := h.Backend.Inputs(id)

	faults.Fail("/ack", true)
	h.poll()
	faults.Fail("/ack", false)
	// The spool was an emptyDir.
	if err := h.Spool.ForgetInputs(id); err != nil {
		t.Fatal(err)
	}

	next := h.restart()
	next.Lease.Resume(next.ctx)

	if state, _ := h.Backend.RunState(id); !state.Acked {
		t.Fatal("the acknowledgement was not repeated once the inputs were back")
	}
	for _, ref := range manifest {
		if ok, _ := next.Spool.InputMatches(id, ref); !ok {
			t.Fatal("an input was not fetched again before the ack")
		}
		if n := h.Backend.InputFetches(id, ref.Key); n != 2 {
			t.Errorf("want two fetches of each input, got %d", n)
		}
	}
}

func TestFinalisingTheAgentRunDropsItsInputs(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec(), fakebackend.WithInputs(screenshot))
	h.poll()
	manifest := h.Backend.Inputs(id)
	if !h.Spool.HasInput(id, manifest[0]) {
		t.Fatal("the input is not spooled")
	}

	if err := h.K8s.Delete(h.ctx, h.run(id)); err != nil {
		t.Fatal(err)
	}
	h.reconcile(id)
	if h.Spool.HasInput(id, manifest[0]) {
		t.Fatal("the input of a collected run is still in the spool")
	}
}

func (h *harness) getInput(id runv1.ULID, key, token string) *httptest.ResponseRecorder {
	h.t.Helper()
	q := url.Values{}
	if id != "" {
		q.Set(runv1.QueryRunID, string(id))
	}
	q.Set(runv1.QueryKey, key)
	req := httptest.NewRequest(http.MethodGet, runv1.CallbackPathInputs+"?"+q.Encode(), nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Callback.Handler().ServeHTTP(rec, req)
	return rec
}
