package backend

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Schedules: a run template admitted on a cron expression.
//
// Fires are driven by calling FireDueSchedules at a chosen instant rather than
// by waiting for a ticker, for the reason Sweep is: a test racing its own
// scanner reports failures that depend on scheduling. What is under test is
// the fence, the collapse of missed slots, the deadline and the policies —
// the rules that only show themselves when something went wrong.

// newSchedule creates a schedule the way the API does, from a valid default
// that opts adjust.
func (h *harness) newSchedule(opts ...func(*app.ScheduleInput)) app.Schedule {
	h.t.Helper()
	in := app.ScheduleInput{
		Name:    "nightly",
		Cron:    "0 3 * * *",
		Enabled: true,
		Run: run.SubmitRequest{
			Prompt:  "update the dependencies",
			RepoURL: "https://github.com/example/repo",
		},
	}
	for _, opt := range opts {
		opt(&in)
	}
	sch, err := h.App.CreateSchedule(context.Background(), in, "test", "api")
	if err != nil {
		h.t.Fatalf("create schedule: %v", err)
	}
	return sch
}

// Schedule reads a schedule's current state.
func (h *harness) Schedule(id runv1.ULID) app.Schedule {
	h.t.Helper()
	sch, err := h.App.Schedule(context.Background(), id)
	if err != nil {
		h.t.Fatalf("read schedule %s: %v", id, err)
	}
	return sch
}

// FireAt runs one scheduler pass standing at the given instant.
func (h *harness) FireAt(at time.Time) app.FireResult {
	h.t.Helper()
	result, err := h.App.FireDueSchedules(context.Background(), at)
	if err != nil {
		h.t.Fatalf("fire schedules: %v", err)
	}
	return result
}

// ScheduledRuns lists a schedule's runs, newest first.
func (h *harness) ScheduledRuns(id runv1.ULID) []store.Run {
	h.t.Helper()
	runs, err := h.Store.ListRuns(context.Background(), store.RunFilter{Schedule: id, Limit: 200})
	if err != nil {
		h.t.Fatalf("list runs of schedule %s: %v", id, err)
	}
	return runs
}

func TestADueScheduleAdmitsOneRunAttributedToTheSchedule(t *testing.T) {
	h := newHarness(t)
	sch := h.newSchedule()
	if sch.NextRunAt == nil {
		t.Fatal("an enabled schedule has no next fire")
	}
	due := *sch.NextRunAt
	if due.Hour() != 3 || due.Minute() != 0 {
		t.Errorf("next fire of 0 3 * * * in UTC is %s", due)
	}

	if got := h.FireAt(due.Add(-time.Second)); got != (app.FireResult{}) {
		t.Fatalf("a pass before the slot did %+v", got)
	}
	if got := h.FireAt(due.Add(time.Second)); got.Submitted != 1 {
		t.Fatalf("a pass at the slot did %+v, want one submitted", got)
	}

	runs := h.ScheduledRuns(sch.ID)
	if len(runs) != 1 {
		t.Fatalf("the schedule has %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.CreatedVia != "schedule" || r.CreatedBy != "schedule:"+string(sch.ID) || r.ScheduleID != sch.ID {
		t.Errorf("run attributed to %s via %s, schedule %s", r.CreatedBy, r.CreatedVia, r.ScheduleID)
	}
	if r.RepoURL != "https://github.com/example/repo" {
		t.Errorf("the run did not get the template: repo %q", r.RepoURL)
	}
	if !h.HasAudit(r.ID, store.AuditRunSubmitted) {
		t.Error("a scheduled run was admitted without an audit record")
	}

	after := h.Schedule(sch.ID)
	if after.LastRunID != r.ID || after.LastOutcome != store.ScheduleSubmitted {
		t.Errorf("schedule records last run %s, outcome %q", after.LastRunID, after.LastOutcome)
	}
	if after.NextRunAt == nil || !after.NextRunAt.Equal(due.Add(24*time.Hour)) {
		t.Errorf("next fire = %v, want %s", after.NextRunAt, due.Add(24*time.Hour))
	}

	if got := h.FireAt(due.Add(2 * time.Second)); got != (app.FireResult{}) {
		t.Errorf("a second pass in the same slot did %+v", got)
	}
}

// Every replica running the background loops runs the scheduler, and none is
// the leader. The fence in the run's own insert is the only thing standing
// between N replicas and N runs per slot.
func TestTwoReplicasFiringTheSameScheduleCreateOneRun(t *testing.T) {
	h := newHarness(t)
	replica := app.New(app.Options{
		Store:     h.Store,
		Artifacts: h.Artifacts,
		Logger:    slog.New(h.Logs),
		Timings:   testTimings(),
		Defaults:  testDefaults(),
	})

	const n = 12
	var due time.Time
	ids := make([]runv1.ULID, 0, n)
	for i := 0; i < n; i++ {
		sch := h.newSchedule(func(in *app.ScheduleInput) {
			in.Name = "s" + string(rune('a'+i))
			in.ConcurrencyPolicy = app.ConcurrencyAllow
		})
		ids = append(ids, sch.ID)
		due = *sch.NextRunAt
	}

	var (
		wg      sync.WaitGroup
		results [2]app.FireResult
		errs    [2]error
	)
	for i, svc := range []*app.Service{h.App, replica} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.FireDueSchedules(context.Background(), due.Add(time.Second))
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("fire: %v", err)
		}
	}

	for _, id := range ids {
		if runs := h.ScheduledRuns(id); len(runs) != 1 {
			t.Errorf("schedule %s fired %d runs from two replicas, want 1", id, len(runs))
		}
	}
	if total := results[0].Submitted + results[1].Submitted; total != n {
		t.Errorf("the replicas report %d submitted between them, want %d (%+v)", total, n, results)
	}
}

func TestAnOutageFiresAScheduleOnceAndNotOncePerMissedSlot(t *testing.T) {
	h := newHarness(t)
	sch := h.newSchedule(func(in *app.ScheduleInput) {
		in.Cron = "0 * * * *"
		in.ConcurrencyPolicy = app.ConcurrencyAllow
	})

	back := sch.NextRunAt.Add(72*time.Hour + 5*time.Minute)
	if got := h.FireAt(back); got.Submitted != 1 {
		t.Fatalf("three days of hourly slots fired %+v, want one run", got)
	}
	if got := h.FireAt(back.Add(time.Second)); got != (app.FireResult{}) {
		t.Errorf("the pass after the catch-up did %+v", got)
	}

	next := h.Schedule(sch.ID).NextRunAt
	want := back.Truncate(time.Hour).Add(time.Hour)
	if next == nil || !next.Equal(want) {
		t.Errorf("after the outage the next fire is %v, want the next slot from now, %s", next, want)
	}
	if runs := h.ScheduledRuns(sch.ID); len(runs) != 1 {
		t.Errorf("the schedule has %d runs, want 1", len(runs))
	}
}

func TestAFirePastItsStartingDeadlineIsDroppedAsMissed(t *testing.T) {
	h := newHarness(t)
	deadline := int32(60)
	sch := h.newSchedule(func(in *app.ScheduleInput) {
		in.Cron = "0 * * * *"
		in.StartingDeadlineSeconds = &deadline
	})
	due := *sch.NextRunAt

	if got := h.FireAt(due.Add(5 * time.Minute)); got.Missed != 1 || got.Submitted != 0 {
		t.Fatalf("a fire five minutes late with a one-minute deadline did %+v", got)
	}
	after := h.Schedule(sch.ID)
	if after.LastOutcome != store.ScheduleMissed || !strings.Contains(after.LastError, "deadline") {
		t.Errorf("outcome %q, error %q", after.LastOutcome, after.LastError)
	}
	if runs := h.ScheduledRuns(sch.ID); len(runs) != 0 {
		t.Errorf("a missed fire admitted %d runs", len(runs))
	}

	next := *after.NextRunAt
	if !next.Equal(due.Add(time.Hour)) {
		t.Errorf("next fire after the miss = %s, want %s", next, due.Add(time.Hour))
	}
	if got := h.FireAt(next.Add(30 * time.Second)); got.Submitted != 1 {
		t.Errorf("a fire inside the deadline did %+v", got)
	}
}

func TestForbidSkipsWhileAPreviousRunIsLive(t *testing.T) {
	h := newHarness(t)
	sch := h.newSchedule(func(in *app.ScheduleInput) { in.Cron = "0 * * * *" })
	if sch.ConcurrencyPolicy != app.ConcurrencyForbid {
		t.Fatalf("default policy = %q, want Forbid", sch.ConcurrencyPolicy)
	}
	due := *sch.NextRunAt

	h.FireAt(due.Add(time.Second))
	first := h.Schedule(sch.ID).LastRunID

	// No cluster has leased it, so it is Queued — and Queued is live.
	if got := h.FireAt(due.Add(time.Hour + time.Second)); got.Skipped != 1 {
		t.Fatalf("a fire while run %s is queued did %+v, want skipped", first, got)
	}
	skipped := h.Schedule(sch.ID)
	if skipped.LastOutcome != store.ScheduleSkipped || !strings.Contains(skipped.LastError, string(first)) {
		t.Errorf("outcome %q, error %q: the reason should name the live run", skipped.LastOutcome, skipped.LastError)
	}
	if skipped.LastRunID != first {
		t.Errorf("a skip moved last_run_id to %s", skipped.LastRunID)
	}

	if _, err := h.App.Cancel(context.Background(), first, "test", "done"); err != nil {
		t.Fatal(err)
	}
	if got := h.FireAt(due.Add(2*time.Hour + time.Second)); got.Submitted != 1 {
		t.Errorf("a fire once the earlier run ended did %+v", got)
	}
}

func TestReplaceCancelsTheLiveRunOnceItsReplacementIsAdmitted(t *testing.T) {
	h := newHarness(t)
	sch := h.newSchedule(func(in *app.ScheduleInput) {
		in.Cron = "0 * * * *"
		in.ConcurrencyPolicy = app.ConcurrencyReplace
	})
	due := *sch.NextRunAt

	h.FireAt(due.Add(time.Second))
	first := h.Schedule(sch.ID).LastRunID
	if got := h.FireAt(due.Add(time.Hour + time.Second)); got.Submitted != 1 {
		t.Fatalf("a Replace fire did %+v", got)
	}
	second := h.Schedule(sch.ID).LastRunID

	if second == first {
		t.Fatal("the Replace fire admitted no new run")
	}
	if got := h.Run(first); got.Status != "Cancelled" || !strings.Contains(got.CancelReason, string(second)) {
		t.Errorf("the replaced run is %s (%q)", got.Status, got.CancelReason)
	}
	if got := h.Run(second); got.Status != "Queued" {
		t.Errorf("the replacement is %s, want Queued", got.Status)
	}
}

func TestAScheduleWhoseRoleWasDeletedRecordsTheRefusalAndAdvances(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.Store.UpsertRole(ctx, "coder", json.RawMessage(`{}`), "test"); err != nil {
		t.Fatal(err)
	}
	sch := h.newSchedule(func(in *app.ScheduleInput) { in.Run.Role = "coder" })
	if err := h.Store.DeleteRole(ctx, "coder"); err != nil {
		t.Fatal(err)
	}

	due := *sch.NextRunAt
	if got := h.FireAt(due.Add(time.Second)); got.Refused != 1 {
		t.Fatalf("a fire under a deleted role did %+v, want refused", got)
	}
	after := h.Schedule(sch.ID)
	if after.LastOutcome != store.ScheduleRefused || !strings.Contains(after.LastError, "coder") {
		t.Errorf("outcome %q, error %q", after.LastOutcome, after.LastError)
	}
	if !after.NextRunAt.Equal(due.Add(24 * time.Hour)) {
		t.Errorf("a refused fire left next_run_at at %s; it would be refused every tick", after.NextRunAt)
	}
	if runs := h.ScheduledRuns(sch.ID); len(runs) != 0 {
		t.Errorf("a refused fire admitted %d runs", len(runs))
	}
}

// A fire computed from a schedule that has since been edited must not commit:
// the operator changed what the schedule is, and the fire is of what it was.
// Driven at the store, where the fence is, because the window between the read
// and the insert is not one a test can stand in from outside.
func TestAFireFencedOnAStaleRevisionAdmitsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sch := h.newSchedule()
	read := *sch.NextRunAt

	prompt := "a different task"
	edited, err := h.App.UpdateSchedule(ctx, sch.ID, app.SchedulePatch{
		Run: &run.SubmitRequest{Prompt: prompt}}, "test", "api")
	if err != nil {
		t.Fatal(err)
	}
	if !edited.NextRunAt.Equal(read) {
		t.Fatalf("an edit to the template moved the next fire from %s to %s", read, edited.NextRunAt)
	}
	if edited.Revision == sch.Revision {
		t.Fatal("an edit did not raise the revision")
	}

	stale := store.ScheduleFire{Expected: read, Revision: sch.Revision, Next: read.Add(24 * time.Hour), At: read}
	id := run.MustULID(time.Now())
	err = h.Store.InsertRun(ctx, store.NewRun{
		ID: id, CreatedBy: "test", CreatedVia: "schedule", Spec: json.RawMessage(`{}`),
		Prompt: "x", PromptSHA256: make([]byte, 32), Agent: runv1.AgentClaudeCode, Model: "m",
		TimeoutSeconds: 3600, ScheduleID: sch.ID, ScheduleFire: &stale,
	})
	if !errors.Is(err, store.ErrScheduleSettled) {
		t.Fatalf("insert under a stale fence: err = %v, want ErrScheduleSettled", err)
	}
	if _, err := h.Store.RunByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the run of a lost fire was committed: %v", err)
	}
	if err := h.Store.AdvanceSchedule(ctx, sch.ID, stale, store.ScheduleSkipped, ""); !errors.Is(err, store.ErrScheduleSettled) {
		t.Errorf("advance under a stale fence: err = %v, want ErrScheduleSettled", err)
	}

	if got := h.FireAt(read.Add(time.Second)); got.Submitted != 1 {
		t.Fatalf("the fire of the edited schedule did %+v", got)
	}
	r := h.ScheduledRuns(sch.ID)[0]
	if got, _ := h.Store.Prompt(ctx, r.ID); got != prompt {
		t.Errorf("the fire ran %q, want the edited template", got)
	}
}

func TestRunNowIgnoresThePolicyAndLeavesNextRunAtAlone(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	sch := h.newSchedule()
	other := h.newSchedule(func(in *app.ScheduleInput) { in.Name = "other" })
	due := *sch.NextRunAt
	h.FireAt(due.Add(time.Second))
	scheduled := h.Schedule(sch.ID)

	// Forbid, and a fire's run is queued: run now admits regardless.
	var created map[string]any
	if status := h.call(t, token, http.MethodPost, "/api/v1/schedules/"+string(sch.ID)+"/run", nil, &created); status != http.StatusAccepted {
		t.Fatalf("run now: status = %d: %+v", status, created)
	}
	id := runv1.ULID(created["run_id"].(string))
	r := h.Run(id)
	if r.ScheduleID != sch.ID || r.CreatedVia != "api" || r.CreatedBy != "token:test" {
		t.Errorf("run now: schedule %s, via %s, by %s", r.ScheduleID, r.CreatedVia, r.CreatedBy)
	}
	if created["schedule_id"] != string(sch.ID) {
		t.Errorf("the run's view does not name its schedule: %v", created["schedule_id"])
	}

	after := h.Schedule(sch.ID)
	if !after.NextRunAt.Equal(*scheduled.NextRunAt) || after.LastRunID != scheduled.LastRunID {
		t.Errorf("run now moved the schedule: next %s → %s, last run %s → %s",
			scheduled.NextRunAt, after.NextRunAt, scheduled.LastRunID, after.LastRunID)
	}
	if runs := h.ScheduledRuns(sch.ID); len(runs) != 2 {
		t.Errorf("the schedule lists %d runs, want the fire's and run now's", len(runs))
	}

	// The idempotency key is bound to the schedule as well as the body.
	key := "run-now-1"
	post := func(target runv1.ULID) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, h.Public.URL+"/api/v1/schedules/"+string(target)+"/run", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	status, first := post(sch.ID)
	if status != http.StatusAccepted {
		t.Fatalf("keyed run now: %d %v", status, first)
	}
	if status, again := post(sch.ID); status != http.StatusOK || again["run_id"] != first["run_id"] {
		t.Errorf("a repeat under the key: %d, run %v, want 200 and %v", status, again["run_id"], first["run_id"])
	}
	if status, _ := post(other.ID); status != http.StatusUnprocessableEntity {
		t.Errorf("the same key on another schedule: %d, want 422", status)
	}
}

func TestAScheduleIsRefusedAtSaveTimeByTheSameRulesAsARun(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	valid := func() map[string]any {
		return map[string]any{
			"name": "nightly", "cron": "0 3 * * *",
			"run": map[string]any{"prompt": "update the dependencies", "repo": "https://github.com/example/repo"},
		}
	}
	cases := []struct {
		name  string
		edit  func(map[string]any)
		field string
	}{
		{"an unknown role", func(b map[string]any) { b["run"].(map[string]any)["role"] = "nobody" }, "run.role"},
		{"no prompt", func(b map[string]any) { b["run"].(map[string]any)["prompt"] = " " }, "run.prompt"},
		{"a branch without a repository", func(b map[string]any) {
			b["run"] = map[string]any{"prompt": "x", "target_branch": "fix"}
		}, "run.target_branch"},
		{"a timeout out of range", func(b map[string]any) { b["run"].(map[string]any)["timeout_seconds"] = 5 }, "run.timeout_seconds"},
		{"a bad cron", func(b map[string]any) { b["cron"] = "every night" }, "cron"},
		{"a timezone inside the cron", func(b map[string]any) { b["cron"] = "CRON_TZ=Europe/Berlin 0 3 * * *" }, "cron"},
		{"an unknown timezone", func(b map[string]any) { b["timezone"] = "Europe/Atlantis" }, "timezone"},
		{"an unknown policy", func(b map[string]any) { b["concurrency_policy"] = "Queue" }, "concurrency_policy"},
		{"a negative deadline", func(b map[string]any) { b["starting_deadline_seconds"] = -1 }, "starting_deadline_seconds"},
		{"a name with capitals", func(b map[string]any) { b["name"] = "Nightly Job" }, "name"},
	}
	for _, tc := range cases {
		body := valid()
		tc.edit(body)
		var refused map[string]any
		status := h.call(t, token, http.MethodPost, "/api/v1/schedules", body, &refused)
		detail, _ := refused["error"].(map[string]any)
		if status != http.StatusUnprocessableEntity || detail["field"] != tc.field {
			t.Errorf("%s: status %d, field %v, want 422 naming %s (%v)", tc.name, status, detail["field"], tc.field, detail["message"])
		}
	}

	if status := h.call(t, token, http.MethodPost, "/api/v1/schedules", valid(), nil); status != http.StatusCreated {
		t.Fatalf("a valid schedule: status %d", status)
	}
	if status := h.call(t, token, http.MethodPost, "/api/v1/schedules", valid(), nil); status != http.StatusConflict {
		t.Errorf("a second schedule with the same name: status %d, want 409", status)
	}
}

func TestTheScheduleEndpointsSpeakSnakeCase(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)
	readOnly := h.Token(store.ScopeRunsRead)

	var created map[string]any
	status := h.call(t, token, http.MethodPost, "/api/v1/schedules", map[string]any{
		"name": "weekday-review", "cron": "30 9 * * 1-5", "timezone": "Europe/Berlin",
		"concurrency_policy": "Replace", "starting_deadline_seconds": 600,
		"run": map[string]any{
			"prompt": "review open PRs", "repo": "https://github.com/example/repo",
			"base_branch": "main", "create_pr": false, "max_cost_usd": "5",
		},
	}, &created)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d: %+v", status, created)
	}
	for _, key := range []string{"id", "name", "cron", "timezone", "enabled", "concurrency_policy",
		"starting_deadline_seconds", "run", "next_run_at", "created_by", "created_at", "updated_at"} {
		if _, ok := created[key]; !ok {
			t.Errorf("the schedule has no %q: %v", key, created)
		}
	}
	tmpl := created["run"].(map[string]any)
	if tmpl["base_branch"] != "main" || tmpl["max_cost_usd"] != "5" || tmpl["create_pr"] != false {
		t.Errorf("the template did not round-trip: %v", tmpl)
	}
	if created["enabled"] != true {
		t.Errorf("a schedule created without enabled is %v, want true", created["enabled"])
	}
	id := created["id"].(string)
	path := "/api/v1/schedules/" + id

	if status := h.call(t, readOnly, http.MethodPost, "/api/v1/schedules", created, nil); status != http.StatusForbidden {
		t.Errorf("create with runs:read: status %d, want 403", status)
	}
	var listed struct {
		Schedules []map[string]any `json:"schedules"`
	}
	if status := h.call(t, readOnly, http.MethodGet, "/api/v1/schedules", nil, &listed); status != http.StatusOK || len(listed.Schedules) != 1 {
		t.Errorf("list: status %d, %d schedules", status, len(listed.Schedules))
	}

	var patched map[string]any
	if status := h.call(t, token, http.MethodPatch, path, map[string]any{"enabled": false}, &patched); status != http.StatusOK {
		t.Fatalf("disable: status %d: %v", status, patched)
	}
	if _, ok := patched["next_run_at"]; ok || patched["cron"] != "30 9 * * 1-5" {
		t.Errorf("a disabled schedule: next_run_at %v, cron %v", patched["next_run_at"], patched["cron"])
	}
	if got := h.FireAt(time.Now().Add(30 * 24 * time.Hour)); got != (app.FireResult{}) {
		t.Errorf("a disabled schedule fired: %+v", got)
	}
	if status := h.call(t, token, http.MethodPatch, path, map[string]any{"enabled": true}, &patched); status != http.StatusOK {
		t.Fatalf("enable: status %d", status)
	}
	if _, ok := patched["next_run_at"]; !ok {
		t.Error("an enabled schedule has no next_run_at")
	}

	var ran map[string]any
	if status := h.call(t, token, http.MethodPost, path+"/run", nil, &ran); status != http.StatusAccepted {
		t.Fatalf("run now: status %d", status)
	}
	h.Submit()
	var runs struct {
		Runs []map[string]any `json:"runs"`
	}
	h.call(t, readOnly, http.MethodGet, "/api/v1/runs?schedule_id="+id, nil, &runs)
	if len(runs.Runs) != 1 || runs.Runs[0]["run_id"] != ran["run_id"] {
		t.Errorf("runs filtered by schedule: %v", runs.Runs)
	}

	if status := h.call(t, token, http.MethodDelete, path, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: status %d", status)
	}
	if status := h.call(t, readOnly, http.MethodGet, path, nil, nil); status != http.StatusNotFound {
		t.Errorf("a deleted schedule: status %d, want 404", status)
	}
	if kept := h.Run(runv1.ULID(ran["run_id"].(string))); kept.ScheduleID != "" {
		t.Errorf("a deleted schedule's run still names it: %s", kept.ScheduleID)
	}
}

// An agent pod holds runs:write, and does whatever the repository's prompt
// injection says. A schedule spends on its own long after the run is gone.
func TestARunsOwnTokenCannotManageSchedules(t *testing.T) {
	h := newHarness(t)
	sch := h.newSchedule()
	parent := h.Submit()
	perRun, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  parent.ID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"name": "persist", "cron": "* * * * *", "run": map[string]any{"prompt": "x"}}
	if status := h.call(t, perRun.Secret, http.MethodPost, "/api/v1/schedules", body, nil); status != http.StatusForbidden {
		t.Errorf("create with a run's token: status %d, want 403", status)
	}
	if status := h.call(t, perRun.Secret, http.MethodPost, "/api/v1/schedules/"+string(sch.ID)+"/run", nil, nil); status != http.StatusForbidden {
		t.Errorf("run now with a run's token: status %d, want 403", status)
	}
	if status := h.call(t, perRun.Secret, http.MethodPatch, "/api/v1/schedules/"+string(sch.ID),
		map[string]any{"enabled": false}, nil); status != http.StatusForbidden {
		t.Errorf("patch with a run's token: status %d, want 403", status)
	}
	if status := h.call(t, perRun.Secret, http.MethodGet, "/api/v1/schedules", nil, nil); status != http.StatusOK {
		t.Errorf("list with a run's token: status %d, want 200", status)
	}
}
