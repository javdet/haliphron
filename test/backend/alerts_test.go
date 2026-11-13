package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/store"
)

// Alert receivers: Alertmanager notifications that start runs, and the
// throttle that keeps a burst of one alert from starting a run per delivery.
//
// Deliveries go through the public listener, as Alertmanager's would, with the
// receiver's own credential. What is under test is the claim — one run per
// alert per window across concurrent deliveries — and the answers Alertmanager
// acts on: 401 and 404 without a valid credential or receiver, 200 for
// everything it must not retry, 503 for what it should.

const alertRole = "triage"

type alertReceiverBody struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	ThrottleSeconds int32  `json:"throttle_seconds"`
	WebhookPath     string `json:"webhook_path"`
	LastOutcome     string `json:"last_outcome"`
	LastRunID       string `json:"last_run_id"`
	LastError       string `json:"last_error"`
	Token           string `json:"token"`
	Run             struct {
		Role   string `json:"role"`
		Prompt string `json:"prompt"`
	} `json:"run"`
}

type alertAnswer struct {
	Outcome   string `json:"outcome"`
	RunID     string `json:"run_id"`
	Alerts    int    `json:"alerts"`
	Throttled int    `json:"throttled"`
	Error     string `json:"error"`
}

// alertReceiver creates a role and a receiver over REST with an admin token,
// and returns the receiver with its credential.
func (h *harness) alertReceiver(t *testing.T, body map[string]any) alertReceiverBody {
	t.Helper()
	if _, err := h.Store.UpsertRole(context.Background(), alertRole,
		json.RawMessage(`{"description":"looks into alerts"}`), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	req := map[string]any{
		"name": "on-call",
		"run": map[string]any{
			"prompt": "Investigate the alerts below.",
			"role":   alertRole,
			"repo":   "https://github.com/example/repo",
		},
	}
	for k, v := range body {
		req[k] = v
	}
	var out alertReceiverBody
	if status := h.call(t, h.Token(store.ScopeAdmin), http.MethodPost, "/api/v1/alert-receivers", req, &out); status != http.StatusCreated {
		t.Fatalf("create alert receiver: status %d", status)
	}
	if !strings.HasPrefix(out.Token, store.AlertCredentialPrefix) {
		t.Fatalf("the create answer carries no credential: %+v", out)
	}
	return out
}

// deliver posts a notification the way Alertmanager does.
func (h *harness) deliver(t *testing.T, receiverID, credential string, payload []byte) (int, alertAnswer) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.Public.URL+"/api/v1/alert-receivers/"+receiverID+"/alerts",
		bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var answer alertAnswer
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatalf("decode delivery answer: %v (%s)", err, raw)
		}
	}
	return resp.StatusCode, answer
}

// firing is a version 4 notification of firing alerts, one per fingerprint.
func firing(fingerprints ...string) []byte {
	return notificationOf("firing", fingerprints...)
}

func notificationOf(status string, fingerprints ...string) []byte {
	alerts := make([]map[string]any, 0, len(fingerprints))
	for _, fp := range fingerprints {
		alerts = append(alerts, map[string]any{
			"status":       status,
			"labels":       map[string]string{"alertname": "HighLatency", "pod": "pod-" + fp},
			"annotations":  map[string]string{"summary": "p99 over 2s on pod-" + fp},
			"startsAt":     "2026-11-13T10:00:00Z",
			"endsAt":       "0001-01-01T00:00:00Z",
			"generatorURL": "http://prometheus:9090/graph",
			"fingerprint":  fp,
		})
	}
	raw, _ := json.Marshal(map[string]any{
		"version":           "4",
		"groupKey":          `{}:{alertname="HighLatency"}`,
		"status":            status,
		"receiver":          "haliphron",
		"groupLabels":       map[string]string{"alertname": "HighLatency"},
		"commonLabels":      map[string]string{"alertname": "HighLatency"},
		"commonAnnotations": map[string]string{},
		"externalURL":       "http://alertmanager:9093",
		"alerts":            alerts,
	})
	return raw
}

func (h *harness) alertRuns(t *testing.T, receiverID string) []store.Run {
	t.Helper()
	runs, err := h.Store.ListRuns(context.Background(),
		store.RunFilter{AlertReceiver: runv1.ULID(receiverID), Limit: 200})
	if err != nil {
		t.Fatalf("list runs of receiver %s: %v", receiverID, err)
	}
	return runs
}

func (h *harness) suppressions(t *testing.T, receiverID string) []store.AlertSuppression {
	t.Helper()
	out, err := h.Store.SuppressedAlerts(context.Background(), runv1.ULID(receiverID), nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// alertBlock is the part of a prompt between the markers.
func alertBlock(t *testing.T, prompt string) string {
	t.Helper()
	start := strings.Index(prompt, "<<<ALERTS-")
	if start < 0 {
		t.Fatalf("the prompt has no alert block:\n%s", prompt)
	}
	return prompt[start:]
}

func TestAFiringNotificationStartsOneRunUnderTheReceiversRole(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	if rc.ThrottleSeconds != 3600 || !rc.Enabled || rc.WebhookPath != "/api/v1/alert-receivers/"+rc.ID+"/alerts" {
		t.Errorf("receiver defaults: %+v", rc)
	}

	status, answer := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001", "aaaa000000000002"))
	if status != http.StatusOK || answer.Outcome != store.AlertSubmitted || answer.RunID == "" || answer.Alerts != 2 {
		t.Fatalf("delivery: status %d, answer %+v", status, answer)
	}

	runs := h.alertRuns(t, rc.ID)
	if len(runs) != 1 {
		t.Fatalf("the receiver has %d runs, want 1", len(runs))
	}
	r := runs[0]
	if string(r.ID) != answer.RunID || r.Role != alertRole || r.CreatedVia != "alertmanager" ||
		r.CreatedBy != "alert-receiver:"+rc.ID || string(r.AlertReceiverID) != rc.ID {
		t.Errorf("run %s: role %q, via %q, by %q, receiver %q", r.ID, r.Role, r.CreatedVia, r.CreatedBy, r.AlertReceiverID)
	}
	prompt := h.prompt(t, r.ID)
	if !strings.HasPrefix(prompt, "Investigate the alerts below.\n\n## Alerts") {
		t.Errorf("the prompt does not lead with the operator's instruction:\n%s", prompt)
	}
	block := alertBlock(t, prompt)
	for _, fp := range []string{"aaaa000000000001", "aaaa000000000002"} {
		if !strings.Contains(block, fp) {
			t.Errorf("the alert block does not carry %s", fp)
		}
	}
	if !h.HasAudit(r.ID, store.AuditRunSubmitted) || !h.HasAudit(r.ID, store.AuditAlertReceiverFired) {
		t.Error("the run was admitted without its audit records")
	}

	var after alertReceiverBody
	h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/alert-receivers/"+rc.ID, nil, &after)
	if after.LastOutcome != store.AlertSubmitted || after.LastRunID != answer.RunID {
		t.Errorf("receiver records outcome %q, run %q", after.LastOutcome, after.LastRunID)
	}
	if after.Token != "" {
		t.Error("reading a receiver shows its credential")
	}

	var listed struct {
		Runs []struct {
			RunID           string `json:"run_id"`
			AlertReceiverID string `json:"alert_receiver_id"`
		} `json:"runs"`
	}
	h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/runs?alert_receiver_id="+rc.ID, nil, &listed)
	if len(listed.Runs) != 1 || listed.Runs[0].AlertReceiverID != rc.ID {
		t.Errorf("the run list filtered by receiver: %+v", listed.Runs)
	}
}

func TestTheSameAlertInsideTheWindowIsThrottled(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)

	_, first := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001"))
	status, second := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001"))
	if status != http.StatusOK || second.Outcome != store.AlertThrottled || second.RunID != "" || second.Throttled != 1 {
		t.Fatalf("second delivery: status %d, answer %+v", status, second)
	}
	if runs := h.alertRuns(t, rc.ID); len(runs) != 1 {
		t.Fatalf("an identical delivery inside the window made %d runs, want 1", len(runs))
	}

	sup := h.suppressions(t, rc.ID)
	if len(sup) != 1 || sup[0].Fingerprint != "aaaa000000000001" || string(sup[0].RunID) != first.RunID ||
		sup[0].Suppressed != 1 || sup[0].Runs != 1 || sup[0].Alertname != "HighLatency" {
		t.Errorf("suppressions: %+v", sup)
	}
	if left := time.Until(sup[0].SuppressedUntil); left < 59*time.Minute || left > 61*time.Minute {
		t.Errorf("suppressed for %s, want the hour the receiver says", left)
	}

	var deliveries struct {
		Deliveries []struct {
			Outcome   string   `json:"outcome"`
			RunID     string   `json:"run_id"`
			Throttled []string `json:"throttled"`
		} `json:"deliveries"`
	}
	h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/alert-receivers/"+rc.ID+"/deliveries", nil, &deliveries)
	if len(deliveries.Deliveries) != 2 {
		t.Fatalf("%d deliveries logged, want 2", len(deliveries.Deliveries))
	}
	newest, oldest := deliveries.Deliveries[0], deliveries.Deliveries[1]
	if newest.Outcome != store.AlertThrottled || len(newest.Throttled) != 1 || oldest.Outcome != store.AlertSubmitted ||
		oldest.RunID != first.RunID {
		t.Errorf("deliveries, newest first: %+v", deliveries.Deliveries)
	}
}

func TestAGroupThatGainsAnAlertRunsAgainForTheNewAlertOnly(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)

	_, first := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001"))
	_, second := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001", "aaaa000000000002"))
	if second.Outcome != store.AlertSubmitted || second.Alerts != 1 || second.Throttled != 1 {
		t.Fatalf("second delivery: %+v", second)
	}
	prompt := h.prompt(t, runv1.ULID(second.RunID))
	block := alertBlock(t, prompt)
	if !strings.Contains(block, "aaaa000000000002") || strings.Contains(block, "aaaa000000000001") {
		t.Errorf("the second run's block should carry only the new alert:\n%s", block)
	}
	if !strings.Contains(prompt, "1 more firing alert of this group was left out") ||
		!strings.Contains(prompt, "run "+first.RunID) {
		t.Errorf("the prompt does not say which run handles the suppressed alert:\n%s", prompt)
	}
}

func TestOnlyTheReceiversOwnCredentialDelivers(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	other := h.alertReceiver(t, map[string]any{"name": "other"})
	payload := firing("aaaa000000000001")

	for name, c := range map[string]struct{ id, credential string }{
		"no credential":           {rc.ID, ""},
		"a wrong credential":      {rc.ID, store.AlertCredentialPrefix + "nope"},
		"another receiver's":      {rc.ID, other.Token},
		"a platform admin token":  {rc.ID, h.Token(store.ScopeAdmin)},
		"an unknown receiver":     {"01JZZZZZZZZZZZZZZZZZZZZZZZ", rc.Token},
		"an unknown receiver too": {"not-an-id", rc.Token},
	} {
		if status, _ := h.deliver(t, c.id, c.credential, payload); status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, status)
		}
	}

	var rotated alertReceiverBody
	if status := h.call(t, h.Token(store.ScopeAdmin), http.MethodPost,
		"/api/v1/alert-receivers/"+rc.ID+"/rotate-token", nil, &rotated); status != http.StatusOK {
		t.Fatalf("rotate: status %d", status)
	}
	if h.lastResponse.Header.Get("Cache-Control") != "no-store" {
		t.Error("a credential was answered without no-store")
	}
	if status, _ := h.deliver(t, rc.ID, rc.Token, payload); status != http.StatusUnauthorized {
		t.Errorf("the rotated-away credential: status %d, want 401", status)
	}
	if status, answer := h.deliver(t, rc.ID, rotated.Token, payload); status != http.StatusOK || answer.Outcome != store.AlertSubmitted {
		t.Errorf("the new credential: status %d, %+v", status, answer)
	}

	if status := h.call(t, h.Token(store.ScopeAdmin), http.MethodPatch, "/api/v1/alert-receivers/"+rc.ID,
		map[string]any{"enabled": false}, nil); status != http.StatusOK {
		t.Fatalf("disable: status %d", status)
	}
	if status, _ := h.deliver(t, rc.ID, rotated.Token, firing("aaaa000000000009")); status != http.StatusNotFound {
		t.Errorf("a disabled receiver: status %d, want 404", status)
	}
}

func TestAResolvedOnlyNotificationStartsNothing(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)

	status, answer := h.deliver(t, rc.ID, rc.Token, notificationOf("resolved", "aaaa000000000001"))
	if status != http.StatusOK || answer.Outcome != store.AlertResolved {
		t.Fatalf("status %d, answer %+v", status, answer)
	}
	if runs := h.alertRuns(t, rc.ID); len(runs) != 0 {
		t.Errorf("a resolved notification started %d runs", len(runs))
	}
	if sup := h.suppressions(t, rc.ID); len(sup) != 0 {
		t.Errorf("a resolved alert was suppressed: %+v", sup)
	}
}

func TestAnUnreadablePayloadIsA400(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	for name, body := range map[string][]byte{
		"not json":      []byte("{"),
		"not version 4": []byte(`{"version":"3","status":"firing","alerts":[]}`),
		"over a MiB":    bytes.Repeat([]byte(" "), 1<<20+1),
	} {
		if status, _ := h.deliver(t, rc.ID, rc.Token, body); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, status)
		}
	}
}

func TestANotificationAdmissionRefusesIsRecordedAndAnswered200(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	if err := h.Store.DeleteRole(context.Background(), alertRole); err != nil {
		t.Fatal(err)
	}

	status, answer := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001"))
	if status != http.StatusOK || answer.Outcome != store.AlertRefused || !strings.Contains(answer.Error, alertRole) {
		t.Fatalf("status %d, answer %+v", status, answer)
	}
	var after alertReceiverBody
	h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/alert-receivers/"+rc.ID, nil, &after)
	if after.LastOutcome != store.AlertRefused || !strings.Contains(after.LastError, alertRole) {
		t.Errorf("receiver records outcome %q, error %q", after.LastOutcome, after.LastError)
	}
	if sup := h.suppressions(t, rc.ID); len(sup) != 0 {
		t.Errorf("a refused notification claimed its alerts: %+v", sup)
	}
}

// Alertmanager's HA peers each deliver the same group, and every API replica
// may take one of them. The claim inside InsertRun is what makes them one run.
func TestConcurrentDeliveriesOfOneGroupStartOneRun(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	payload := firing("aaaa000000000001", "aaaa000000000002", "aaaa000000000003")

	const peers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses []int
		outcomes = map[string]int{}
	)
	for i := 0; i < peers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, answer := h.deliver(t, rc.ID, rc.Token, payload)
			mu.Lock()
			defer mu.Unlock()
			statuses = append(statuses, status)
			outcomes[answer.Outcome]++
		}()
	}
	wg.Wait()

	for _, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("a concurrent delivery was answered %d", s)
		}
	}
	if outcomes[store.AlertSubmitted] != 1 || outcomes[store.AlertThrottled] != peers-1 {
		t.Errorf("outcomes %v, want one submitted and the rest throttled", outcomes)
	}
	if runs := h.alertRuns(t, rc.ID); len(runs) != 1 {
		t.Errorf("%d concurrent deliveries of one group made %d runs, want 1", peers, len(runs))
	}
}

// Overlapping sets claim rows in fingerprint order, so they cannot deadlock,
// and every alert is carried by exactly one run.
func TestOverlappingConcurrentDeliveriesCarryEachAlertOnce(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)

	fp := func(i int) string { return fmt.Sprintf("bbbb%012x", i%6) }
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if status, answer := h.deliver(t, rc.ID, rc.Token, firing(fp(i), fp(i+1), fp(i+2))); status != http.StatusOK {
				t.Errorf("delivery %d: status %d %+v", i, status, answer)
			}
		}(i)
	}
	wg.Wait()
	if t.Failed() {
		for _, line := range h.Logs.Lines() {
			if strings.Contains(line, "alert") {
				t.Log(line)
			}
		}
	}

	sup := h.suppressions(t, rc.ID)
	if len(sup) != 6 {
		t.Fatalf("%d alerts suppressed, want all 6", len(sup))
	}
	carried := map[string]string{}
	for _, r := range h.alertRuns(t, rc.ID) {
		block := alertBlock(t, h.prompt(t, r.ID))
		for i := 0; i < 6; i++ {
			if strings.Contains(block, `"fingerprint": "`+fp(i)+`"`) {
				if prev, ok := carried[fp(i)]; ok {
					t.Errorf("alert %s is carried by runs %s and %s", fp(i), prev, r.ID)
				}
				carried[fp(i)] = string(r.ID)
			}
		}
	}
	for _, s := range sup {
		if s.Runs != 1 || carried[s.Fingerprint] != string(s.RunID) {
			t.Errorf("alert %s: claimed %d times, by %s; carried by %s", s.Fingerprint, s.Runs, s.RunID, carried[s.Fingerprint])
		}
	}
}

func TestAZeroWindowSuppressesNothing(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, map[string]any{"throttle_seconds": 0})
	if rc.ThrottleSeconds != 0 {
		t.Fatalf("throttle_seconds 0 was stored as %d", rc.ThrottleSeconds)
	}
	for i := 0; i < 2; i++ {
		if _, answer := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001")); answer.Outcome != store.AlertSubmitted {
			t.Errorf("delivery %d: %+v", i, answer)
		}
	}
	if runs := h.alertRuns(t, rc.ID); len(runs) != 2 {
		t.Errorf("%d runs, want one per delivery", len(runs))
	}
}

func TestAnExpiredWindowLetsTheNextDeliveryRun(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, map[string]any{"throttle_seconds": 1})
	h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001"))
	time.Sleep(1100 * time.Millisecond)
	if _, answer := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001")); answer.Outcome != store.AlertSubmitted {
		t.Fatalf("a delivery after the window: %+v", answer)
	}
	if runs := h.alertRuns(t, rc.ID); len(runs) != 2 {
		t.Errorf("%d runs, want 2", len(runs))
	}
}

func TestClearingASuppressionLetsTheNextDeliveryRun(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	admin := h.Token(store.ScopeAdmin)
	h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001", "aaaa000000000002"))

	var cleared struct {
		Cleared int64 `json:"cleared"`
	}
	if status := h.call(t, admin, http.MethodDelete,
		"/api/v1/alert-receivers/"+rc.ID+"/suppressions/aaaa000000000001", nil, &cleared); status != http.StatusOK || cleared.Cleared != 1 {
		t.Fatalf("clear one: status %d, %+v", status, cleared)
	}
	if status := h.call(t, admin, http.MethodDelete,
		"/api/v1/alert-receivers/"+rc.ID+"/suppressions/aaaa000000000001", nil, nil); status != http.StatusNotFound {
		t.Errorf("clearing a cleared suppression: status %d, want 404", status)
	}
	if status := h.call(t, h.Token(store.ScopeRunsWrite), http.MethodDelete,
		"/api/v1/alert-receivers/"+rc.ID+"/suppressions", nil, nil); status != http.StatusForbidden {
		t.Errorf("clearing with runs:write: status %d, want 403", status)
	}

	_, answer := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001", "aaaa000000000002"))
	if answer.Outcome != store.AlertSubmitted || answer.Alerts != 1 || answer.Throttled != 1 {
		t.Fatalf("a delivery after clearing one of two: %+v", answer)
	}

	if status := h.call(t, admin, http.MethodDelete,
		"/api/v1/alert-receivers/"+rc.ID+"/suppressions", nil, &cleared); status != http.StatusOK || cleared.Cleared != 2 {
		t.Fatalf("clear all: status %d, %+v", status, cleared)
	}
	if sup := h.suppressions(t, rc.ID); len(sup) != 0 {
		t.Errorf("suppressions left after clearing all: %+v", sup)
	}
}

func TestAlertReceiversAreManagedWithAdminAndReadWithRunsRead(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	body := map[string]any{"name": "x", "run": map[string]any{"prompt": "x", "role": alertRole}}

	if status := h.call(t, h.Token(store.ScopeRunsWrite), http.MethodPost, "/api/v1/alert-receivers", body, nil); status != http.StatusForbidden {
		t.Errorf("create with runs:write: status %d, want 403", status)
	}
	if status := h.call(t, h.Token(store.ScopeRunsWrite), http.MethodPost,
		"/api/v1/alert-receivers/"+rc.ID+"/rotate-token", nil, nil); status != http.StatusForbidden {
		t.Errorf("rotate with runs:write: status %d, want 403", status)
	}

	parent := h.Submit()
	perRun, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if status := h.call(t, perRun.Secret, http.MethodPost, "/api/v1/alert-receivers", body, nil); status != http.StatusForbidden {
		t.Errorf("create with a run's token: status %d, want 403", status)
	}

	var list struct {
		Receivers []alertReceiverBody `json:"alert_receivers"`
	}
	if status := h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/alert-receivers", nil, &list); status != http.StatusOK {
		t.Fatalf("list with runs:read: status %d", status)
	}
	if len(list.Receivers) != 1 || list.Receivers[0].Token != "" || list.Receivers[0].Run.Role != alertRole {
		t.Errorf("list: %+v", list.Receivers)
	}
}

func TestAnAlertReceiverIsCheckedAgainstAdmissionWhenSaved(t *testing.T) {
	h := newHarness(t)
	h.alertReceiver(t, nil)
	admin := h.Token(store.ScopeAdmin)

	for name, c := range map[string]struct {
		body  map[string]any
		field string
	}{
		"no role": {map[string]any{"name": "a", "run": map[string]any{"prompt": "x"}}, "run.role"},
		"unknown role": {map[string]any{"name": "a",
			"run": map[string]any{"prompt": "x", "role": "nobody"}}, "run.role"},
		"no prompt": {map[string]any{"name": "a", "run": map[string]any{"role": alertRole}}, "run.prompt"},
		"window over a week": {map[string]any{"name": "a", "throttle_seconds": 604801,
			"run": map[string]any{"prompt": "x", "role": alertRole}}, "throttle_seconds"},
		"bad name": {map[string]any{"name": "A B",
			"run": map[string]any{"prompt": "x", "role": alertRole}}, "name"},
	} {
		var out errorEnvelope
		if status := h.call(t, admin, http.MethodPost, "/api/v1/alert-receivers", c.body, &out); status != http.StatusUnprocessableEntity ||
			out.Error.Field != c.field {
			t.Errorf("%s: status %d, field %q, want 422 on %s", name, status, out.Error.Field, c.field)
		}
	}

	dup := map[string]any{"name": "on-call", "run": map[string]any{"prompt": "x", "role": alertRole}}
	if status := h.call(t, admin, http.MethodPost, "/api/v1/alert-receivers", dup, nil); status != http.StatusConflict {
		t.Errorf("a taken name: status %d, want 409", status)
	}
}

func TestDeletingAnAlertReceiverKeepsItsRuns(t *testing.T) {
	h := newHarness(t)
	rc := h.alertReceiver(t, nil)
	_, answer := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000001"))

	if status := h.call(t, h.Token(store.ScopeAdmin), http.MethodDelete, "/api/v1/alert-receivers/"+rc.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: status %d", status)
	}
	r := h.Run(runv1.ULID(answer.RunID))
	if r.AlertReceiverID != "" {
		t.Errorf("the run still names deleted receiver %s", r.AlertReceiverID)
	}
	if status, _ := h.deliver(t, rc.ID, rc.Token, firing("aaaa000000000002")); status != http.StatusUnauthorized {
		t.Errorf("a delivery to a deleted receiver: status %d, want 401", status)
	}
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"error"`
}
