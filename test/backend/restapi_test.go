package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The public API, over HTTP with a real token.
//
// What is being checked here is the transport's own obligations — the scopes, the
// idempotency header, the redirect to a presigned link, the snake_case shape a
// customer's script is written against — rather than the rules underneath,
// which the Cluster API tests already exercise from the other side.

// call sends an authenticated request to the public API.
func (h *harness) call(t *testing.T, token, method, path string, body any, into any) int {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.Public.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// Redirects are not followed: the result endpoint answering with a
	// Location is the behaviour under test.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(resp.Body)
	if into != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, into); err != nil {
			t.Fatalf("decode %s %s: %v (%s)", method, path, err, payload)
		}
	}
	h.lastResponse = resp
	return resp.StatusCode
}

func TestTheRunEndpointsSpeakSnakeCase(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	var created map[string]any
	status := h.call(t, token, http.MethodPost, "/api/v1/runs", map[string]any{
		"prompt":          "add a health endpoint",
		"repo":            "https://github.com/example/repo",
		"base_branch":     "main",
		"timeout_seconds": 1800,
		"max_cost_usd":    "5",
	}, &created)

	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for an asynchronous run: %+v", status, created)
	}
	for _, field := range []string{"run_id", "status", "agent", "model", "created_at"} {
		if _, ok := created[field]; !ok {
			t.Errorf("the response has no %s: %+v", field, created)
		}
	}
	if created["status"] != "Queued" {
		t.Errorf("status = %v, want Queued", created["status"])
	}

	id, _ := created["run_id"].(string)
	var fetched map[string]any
	if status := h.call(t, token, http.MethodGet, "/api/v1/runs/"+id, nil, &fetched); status != http.StatusOK {
		t.Fatalf("GET run: status %d", status)
	}
	if fetched["run_id"] != id {
		t.Errorf("fetched %v, want %s", fetched["run_id"], id)
	}

	var listed map[string]any
	if status := h.call(t, token, http.MethodGet, "/api/v1/runs?limit=10", nil, &listed); status != http.StatusOK {
		t.Fatalf("list runs: status %d", status)
	}
	runs, _ := listed["runs"].([]any)
	if len(runs) != 1 {
		t.Errorf("%d runs listed, want 1", len(runs))
	}
}

// A token carries scopes, and an endpoint that changes something requires the
// one that says so. The failure is a 403 with the scope named, so a caller can
// fix its token rather than guess.
func TestScopesAreEnforcedPerEndpoint(t *testing.T) {
	h := newHarness(t)
	readOnly := h.Token(store.ScopeRunsRead)

	var body map[string]any
	status := h.call(t, readOnly, http.MethodPost, "/api/v1/runs",
		map[string]any{"prompt": "do something"}, &body)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	detail, _ := body["error"].(map[string]any)
	if detail["code"] != "forbidden" {
		t.Errorf("code = %v, want forbidden", detail["code"])
	}

	if status := h.call(t, "", http.MethodGet, "/api/v1/runs", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("status = %d without a token, want 401", status)
	}
	if status := h.call(t, "hlt_nonsense", http.MethodGet, "/api/v1/runs", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("status = %d with an unknown token, want 401", status)
	}
}

// The retry that Slack and n8n perform on a timeout must return the first run.
func TestTheIdempotencyHeaderReturnsTheFirstRun(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	body := map[string]any{"prompt": "add a health endpoint"}

	post := func(key string, payload map[string]any) (int, map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		req, err := http.NewRequest(http.MethodPost, h.Public.URL+"/api/v1/runs", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", key)

		resp, err := h.Public.Client().Do(req)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		defer resp.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		return resp.StatusCode, decoded
	}

	firstStatus, first := post("n8n-42", body)
	if firstStatus != http.StatusAccepted {
		t.Fatalf("first: status %d", firstStatus)
	}
	secondStatus, second := post("n8n-42", body)
	if secondStatus != http.StatusOK {
		t.Errorf("repeat: status %d, want 200", secondStatus)
	}
	if second["run_id"] != first["run_id"] {
		t.Errorf("repeat produced %v, first produced %v", second["run_id"], first["run_id"])
	}

	// The same key with a different body is a client defect, not a retry.
	conflictStatus, conflict := post("n8n-42", map[string]any{"prompt": "delete production"})
	if conflictStatus != http.StatusUnprocessableEntity {
		t.Errorf("conflict: status %d, want 422", conflictStatus)
	}
	detail, _ := conflict["error"].(map[string]any)
	if detail["code"] != "idempotency_conflict" {
		t.Errorf("code = %v, want idempotency_conflict", detail["code"])
	}
}

// The result is handed out as a presigned link rather than proxied: the object
// store is already reachable from wherever the caller is, and putting every
// byte of every result through one process is how a control plane becomes a
// bottleneck for work it did not do.
func TestTheResultEndpointRedirectsToAPresignedLink(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	admitted := h.Submit()

	status := h.call(t, token, http.MethodGet, "/api/v1/runs/"+string(admitted.ID)+"/result", nil, nil)
	if status != http.StatusFound {
		t.Fatalf("status = %d, want 302", status)
	}
	location := h.lastResponse.Header.Get("Location")
	if location == "" {
		t.Fatal("no Location header")
	}
	if !contains(location, string(admitted.ID)) {
		t.Errorf("the link does not point at the run's own prefix: %s", location)
	}
	// The link is a bearer capability with a short life; it must not be cached
	// anywhere between here and the caller.
	if h.lastResponse.Header.Get("Cache-Control") != "no-store" {
		t.Error("the redirect is cacheable")
	}

	// And a key outside a run's own results cannot be signed at all: without
	// the check the endpoint is a way to mint a capability for any object.
	if status := h.call(t, token, http.MethodGet,
		"/api/v1/runs/"+string(admitted.ID)+"/result?key=../../etc/passwd", nil, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d for an arbitrary key, want 422", status)
	}
}

// The operator's two verbs, over HTTP.
func TestCancelAndRetryAreReachableFromTheAPI(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	admitted := h.Submit()

	var cancelled map[string]any
	if status := h.call(t, token, http.MethodPost,
		"/api/v1/runs/"+string(admitted.ID)+"/cancel",
		map[string]any{"reason": "wrong repo"}, &cancelled); status != http.StatusAccepted {
		t.Fatalf("cancel: status %d", status)
	}
	if cancelled["status"] != "Cancelled" {
		t.Errorf("status = %v, want Cancelled for a run that was never dispatched", cancelled["status"])
	}

	var retried map[string]any
	if status := h.call(t, token, http.MethodPost,
		"/api/v1/runs/"+string(admitted.ID)+"/retry", nil, &retried); status != http.StatusAccepted {
		t.Fatalf("retry: status %d", status)
	}
	if retried["status"] != "Queued" {
		t.Errorf("status = %v, want Queued after a retry", retried["status"])
	}
	if epoch, _ := retried["epoch"].(float64); epoch != 2 {
		t.Errorf("epoch = %v, want 2: a retry revokes ownership", retried["epoch"])
	}
}

// Roles, clusters and credentials are what make the platform usable without a
// UI. They are admin-scoped, and the secrets among them are shown once.
func TestTheConfigurationEndpointsAreAdminScoped(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)
	reader := h.Token(store.ScopeRunsRead)

	var role map[string]any
	if status := h.call(t, admin, http.MethodPut, "/api/v1/roles/coder",
		json.RawMessage(`{"model":"anthropic/claude-sonnet-5"}`), &role); status != http.StatusOK {
		t.Fatalf("put role: status %d", status)
	}
	if role["name"] != "coder" {
		t.Errorf("name = %v, want coder", role["name"])
	}
	if status := h.call(t, reader, http.MethodPut, "/api/v1/roles/coder",
		json.RawMessage(`{}`), nil); status != http.StatusForbidden {
		t.Errorf("a reader edited a role: status %d", status)
	}

	var token map[string]any
	if status := h.call(t, admin, http.MethodPost, "/api/v1/clusters/bootstrap-tokens",
		map[string]any{"name": "east", "ttl_seconds": 3600}, &token); status != http.StatusCreated {
		t.Fatalf("create bootstrap token: status %d", status)
	}
	secret, _ := token["token"].(string)
	if secret == "" {
		t.Fatal("the bootstrap token was not returned; it is unrecoverable afterwards")
	}
	if h.lastResponse.Header.Get("Cache-Control") != "no-store" {
		t.Error("a response carrying a credential is cacheable")
	}

	// And it registers a cluster, which is the only thing it is for.
	p := newProbe(t, h, "east")
	var clusters map[string]any
	if status := h.call(t, admin, http.MethodGet, "/api/v1/clusters", nil, &clusters); status != http.StatusOK {
		t.Fatalf("list clusters: status %d", status)
	}
	items, _ := clusters["clusters"].([]any)
	if len(items) == 0 {
		t.Fatal("the registered cluster is not listed")
	}
	for _, item := range items {
		fields, _ := item.(map[string]any)
		for _, forbidden := range []string{"public_key", "key_id", "token"} {
			if _, present := fields[forbidden]; present {
				t.Errorf("the cluster listing exposes %s", forbidden)
			}
		}
	}
	if status := h.call(t, admin, http.MethodPost,
		"/api/v1/clusters/"+string(p.clusterID)+"/revoke",
		map[string]any{"reason": "decommissioned"}, nil); status != http.StatusNoContent {
		t.Errorf("revoke: status %d", status)
	}
}

// Managed secrets go in and never come out: an endpoint that returns one turns
// a read scope into a credential.
func TestSecretsGoInAndOnlyTheirNamesComeBack(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	if status := h.call(t, admin, http.MethodPut, "/api/v1/secrets/github-app",
		map[string]any{"value": "ghs_a_real_looking_token"}, nil); status != http.StatusOK {
		t.Fatalf("put secret: status %d", status)
	}

	var listed map[string]any
	if status := h.call(t, admin, http.MethodGet, "/api/v1/secrets", nil, &listed); status != http.StatusOK {
		t.Fatalf("list secrets: status %d", status)
	}
	raw, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("encode listing: %v", err)
	}
	if contains(string(raw), "ghs_a_real_looking_token") {
		t.Fatal("a secret's value came back from the API")
	}
	if !contains(string(raw), "github-app") {
		t.Error("the secret's name is missing, so an operator cannot see what exists")
	}
}

// The list filters are what an operator and the UI actually use, and each one
// is a different SQL path: an array cast for the statuses, a substring match
// for the free-text box, a cursor built from the identifier.
func TestTheRunListFiltersAndPages(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	first := h.Submit()
	second := h.Submit(func(r *run.SubmitRequest) {
		r.RepoURL = "https://gitlab.com/example/other"
	})
	if _, err := h.App.Cancel(context.Background(), first.ID, "operator", "not needed"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var byStatus map[string]any
	if status := h.call(t, token, http.MethodGet,
		"/api/v1/runs?status=Cancelled", nil, &byStatus); status != http.StatusOK {
		t.Fatalf("filter by status: %d", status)
	}
	runs, _ := byStatus["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("%d runs match Cancelled, want 1", len(runs))
	}
	if got := runs[0].(map[string]any)["run_id"]; got != string(first.ID) {
		t.Errorf("matched %v, want %s", got, first.ID)
	}

	var bySearch map[string]any
	if status := h.call(t, token, http.MethodGet,
		"/api/v1/runs?q=gitlab.com", nil, &bySearch); status != http.StatusOK {
		t.Fatalf("free-text search: %d", status)
	}
	found, _ := bySearch["runs"].([]any)
	if len(found) != 1 {
		t.Fatalf("%d runs match gitlab.com, want 1", len(found))
	}
	if got := found[0].(map[string]any)["run_id"]; got != string(second.ID) {
		t.Errorf("matched %v, want %s", got, second.ID)
	}

	// One per page, and the cursor is the last identifier of the page.
	var page map[string]any
	if status := h.call(t, token, http.MethodGet, "/api/v1/runs?limit=1", nil, &page); status != http.StatusOK {
		t.Fatalf("first page: %d", status)
	}
	cursor, _ := page["next_before"].(string)
	if cursor == "" {
		t.Fatal("a full page carries no cursor, so a caller cannot page")
	}

	var next map[string]any
	if status := h.call(t, token, http.MethodGet,
		"/api/v1/runs?limit=1&before="+cursor, nil, &next); status != http.StatusOK {
		t.Fatalf("second page: %d", status)
	}
	items, _ := next["runs"].([]any)
	if len(items) != 1 {
		t.Fatalf("the second page holds %d runs, want 1", len(items))
	}
	if items[0].(map[string]any)["run_id"] == cursor {
		t.Error("the cursor returned the row it points at; pages overlap")
	}
}

// The attempt ledger is what an operator reads when a run cost more than it
// should have: one row per try, with the declared and the observed duration
// beside each other.
func TestTheAttemptLedgerIsReadableOverTheAPI(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}
	if _, problem := p.Complete(lease.RunID, lease.Epoch, 1,
		completionFor(lease.RunID, 1, "1.500000", "")); problem != nil {
		t.Fatalf("completion: %+v", problem)
	}

	var body map[string]any
	if status := h.call(t, token, http.MethodGet,
		"/api/v1/runs/"+string(admitted.ID)+"/attempts", nil, &body); status != http.StatusOK {
		t.Fatalf("attempts: status %d", status)
	}
	attempts, _ := body["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want 1", len(attempts))
	}
	row, _ := attempts[0].(map[string]any)
	for _, field := range []string{"attempt", "epoch", "cluster_id", "cost_usd", "declared_duration_ms"} {
		if _, ok := row[field]; !ok {
			t.Errorf("the ledger row has no %s: %+v", field, row)
		}
	}
	if row["cost_usd"] != "1.500000" {
		t.Errorf("cost = %v, want 1.500000", row["cost_usd"])
	}
}
