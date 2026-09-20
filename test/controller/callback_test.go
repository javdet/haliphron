package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The pod's completion report: the only inbound HTTP in the whole system, and
// the only place where the least trusted component in it gets to talk.

// TestAForgedCompletionIsRefused. Without the per-run token, any pod in the
// agents namespace could post a completion for somebody else's run — including
// a successful one, for work that was never done.
func TestAForgedCompletionIsRefused(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	other := h.Backend.Enqueue(sampleSpec())
	h.poll()

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token at all", "", http.StatusUnauthorized},
		{"a token of its own invention", "not-the-token", http.StatusUnauthorized},
		{"another run's token", h.callbackToken(other), http.StatusUnauthorized},
		{"the run's own token", h.callbackToken(id), http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.postCompletion(sampleReport(id), tc.token)
			if rec.Code != tc.want {
				t.Fatalf("want %d, got %d", tc.want, rec.Code)
			}
		})
	}
}

// TestACompletionForAnUnknownRunIsPermanentlyRefused. The pod stops on a 409,
// which is right: whatever it belongs to, this cluster is not holding it, and
// five more attempts over sixty seconds would not change that.
func TestACompletionForAnUnknownRunIsPermanentlyRefused(t *testing.T) {
	h := newHarness(t)
	report := sampleReport("01J8X4K2ZQ7YB3M9F0R5W6T8CD")
	rec := h.postCompletion(report, "anything")
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 for a run this cluster does not hold, got %d", rec.Code)
	}
}

// TestTheReportIsRecordedAndForwardedUnchanged, and charged once however many
// times it arrives. The pod retries up to five times, the controller may
// restart in between, and the cost must still land on the bill exactly once.
func TestTheReportIsRecordedAndForwardedUnchanged(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)
	pod := h.startPod(id, 1)
	h.podExited(pod, 0, "Completed")
	h.reconcile(id)

	report := sampleReport(id)
	if rec := h.postCompletion(report, h.callbackToken(id)); rec.Code != http.StatusAccepted {
		t.Fatalf("the report was refused: %d", rec.Code)
	}

	cr := h.run(id)
	if !meta.IsStatusConditionTrue(cr.Status.Conditions, agentrunv1alpha1.ConditionResultReported) {
		t.Fatal("the callback was not recorded on the AgentRun")
	}
	if cr.Status.PRURL != report.Repo.PRURL {
		t.Fatalf("the pull request link was lost: %q", cr.Status.PRURL)
	}
	if cr.Status.Usage == nil || cr.Status.Usage.TotalCostUSD != report.Usage.TotalCostUSD {
		t.Fatalf("what the run cost was lost: %+v", cr.Status.Usage)
	}
	if cr.Status.Result == nil || cr.Status.Result.Completion == nil {
		t.Fatal("the pointer to completion.json was not recorded; the backend's fallback copy is unfindable")
	}

	h.flush()
	state, _ := h.Backend.RunState(id)
	if state.Completion == nil || state.Completion.Summary != report.Summary {
		t.Fatal("the report did not reach the control plane unchanged")
	}
	if len(state.Charges) != 1 {
		t.Fatalf("want exactly one charge, got %d", len(state.Charges))
	}

	// The pod retries; the controller forwards again; the bill does not move.
	if rec := h.postCompletion(report, h.callbackToken(id)); rec.Code != http.StatusAccepted {
		t.Fatalf("a repeat was refused: %d", rec.Code)
	}
	h.flush()
	state, _ = h.Backend.RunState(id)
	if len(state.Charges) != 1 {
		t.Fatalf("a repeated report was charged again: %d charges", len(state.Charges))
	}

	if cr := h.run(id); !cr.Status.Reported.CompletionDelivered {
		t.Fatal("delivery of the completion was not recorded, so the CR can never be reaped")
	}
}

// ---------------------------------------------------------------------------

func (h *harness) postCompletion(report runv1.CompletionReport, token string) *httptest.ResponseRecorder {
	h.t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		h.t.Fatalf("marshal report: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, runv1.CallbackPathCompletion, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Callback.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *harness) callbackToken(id runv1.ULID) string {
	h.t.Helper()
	secret := h.secret(id)
	if secret == nil {
		h.t.Fatalf("no Secret for %s", id)
	}
	return string(secret.Data[runv1.SecretKeyCallbackToken])
}

func sampleReport(id runv1.ULID) runv1.CompletionReport {
	return runv1.CompletionReport{
		RunID:     id,
		Attempt:   1,
		Status:    runv1.CompletionSuccess,
		ExitCode:  0,
		Agent:     runv1.AgentClaudeCode,
		Model:     "anthropic/claude-opus-5",
		Summary:   "renamed the widget, opened a pull request",
		ResultRef: &runv1.ObjectRef{Key: "runs/" + string(id) + "/result.md"},
		Repo: &runv1.RepoResult{
			Pushed: true, TargetBranch: "haliphron/run-x",
			PRURL: "https://github.com/acme/widgets/pull/42", PRAction: runv1.PRActionCreated,
		},
		Usage: &runv1.Usage{
			DurationMs: 120000, NumTurns: 7, TotalCostUSD: runv1.MoneyUSD("0.4231"),
		},
	}
}
