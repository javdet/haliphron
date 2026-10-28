package backend

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Deleting runs, by an operator and by retention.
//
// The rules checked here are the ones that keep deletion from losing anything
// that is still in motion or still owed: only a run that has ended is deleted,
// the deletion is audited because nothing else will remember the run, its
// objects go with it, a child outlives its parent, and a controller that still
// holds the CR is told to abandon it rather than left retrying.

func deletePath(id runv1.ULID) string { return "/api/v1/runs/" + string(id) }

// aFinishedRun is a run cancelled before dispatch: terminal, with finished_at
// set, and no cluster needed to get it there.
func (h *harness) aFinishedRun(opts ...func(*run.SubmitRequest)) store.Run {
	h.t.Helper()
	admitted := h.Submit(opts...)
	if _, err := h.App.Cancel(context.Background(), admitted.ID, "test", "done with it"); err != nil {
		h.t.Fatalf("cancel %s: %v", admitted.ID, err)
	}
	return h.Run(admitted.ID)
}

func (h *harness) runExists(id runv1.ULID) bool {
	h.t.Helper()
	_, err := h.Store.RunByID(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		h.t.Fatalf("read run %s: %v", id, err)
	}
	return true
}

func (h *harness) stageObject(id runv1.ULID, name string) {
	h.t.Helper()
	if err := h.Artifacts.Put(context.Background(), artifacts.Key(id, name),
		[]byte("content of "+name), "text/plain"); err != nil {
		h.t.Fatalf("stage %s for %s: %v", name, id, err)
	}
}

func (h *harness) objectCount(id runv1.ULID) int {
	h.t.Helper()
	objects, err := h.Artifacts.List(context.Background(), artifacts.RunPrefix(id))
	if err != nil {
		h.t.Fatalf("list objects of %s: %v", id, err)
	}
	return len(objects)
}

func TestAFinishedRunCanBeDeletedWithItsObjectsAndTheDeletionIsAudited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.Token(store.ScopeAdmin)

	victim := h.aFinishedRun()
	bystander := h.aFinishedRun()
	for _, id := range []runv1.ULID{victim.ID, bystander.ID} {
		h.stageObject(id, runv1.StorageKeyResult)
		h.stageObject(id, runv1.StorageKeyAgentLog)
	}

	if status := h.call(t, admin, http.MethodDelete, deletePath(victim.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete a finished run: %d", status)
	}
	if h.runExists(victim.ID) {
		t.Fatal("a deleted run is still readable")
	}
	if n := h.objectCount(victim.ID); n != 0 {
		t.Errorf("%d objects of the deleted run are still stored", n)
	}
	if !h.runExists(bystander.ID) || h.objectCount(bystander.ID) != 2 {
		t.Error("deleting one run touched another")
	}

	var actor, actorKind, status, reason string
	if err := h.Store.DB().QueryRowContext(ctx, `
		SELECT actor, actor_kind, payload->>'status', payload->>'reason' FROM audit_log
		WHERE action = $1 AND run_id = $2`, store.AuditRunDeleted, string(victim.ID)).
		Scan(&actor, &actorKind, &status, &reason); err != nil {
		t.Fatalf("read the audit record of the deletion: %v", err)
	}
	if actor != "token:test" || actorKind != "user" || status != "Cancelled" || reason != "operator" {
		t.Errorf("audit record is by %q (%s) of a %s run for %q", actor, actorKind, status, reason)
	}
	// The run's earlier history survives it, which is why audit_log has no
	// foreign key to runs.
	if !h.HasAudit(victim.ID, store.AuditRunCancelled) {
		t.Error("the audit record of the cancellation went with the run")
	}

	if status := h.call(t, admin, http.MethodDelete, deletePath(victim.ID), nil, nil); status != http.StatusNotFound {
		t.Errorf("deleting it twice: %d, want 404", status)
	}
}

// Cancel first, then delete. A queued run deleted outright would be one
// nobody decided to stop, and a running one would leave a Job reporting
// against nothing.
func TestARunThatHasNotEndedCannotBeDeleted(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)
	queued := h.Submit()

	var body map[string]any
	if status := h.call(t, admin, http.MethodDelete, deletePath(queued.ID), nil, &body); status != http.StatusConflict {
		t.Fatalf("delete a queued run: %d, want 409", status)
	}
	if detail, _ := body["error"].(map[string]any); detail["code"] != "run_live" {
		t.Errorf("code = %v, want run_live", detail["code"])
	}
	if !h.runExists(queued.ID) {
		t.Fatal("a refused deletion deleted the run")
	}
	if h.HasAudit(queued.ID, store.AuditRunDeleted) {
		t.Error("a refused deletion was audited as a deletion")
	}
}

// runs:write is what every agent pod's per-run token carries, and the pod is
// untrusted. Deleting runs takes admin.
func TestDeletingARunTakesAdminAndNotRunsWrite(t *testing.T) {
	h := newHarness(t)
	finished := h.aFinishedRun()
	writer := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	if status := h.call(t, writer, http.MethodDelete, deletePath(finished.ID), nil, nil); status != http.StatusForbidden {
		t.Fatalf("delete with runs:write: %d, want 403", status)
	}
	if !h.runExists(finished.ID) {
		t.Fatal("a forbidden deletion deleted the run")
	}
}

func TestAChildRunOutlivesItsDeletedParent(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)
	parent := h.aFinishedRun()
	child := h.Submit(func(r *run.SubmitRequest) {
		r.ParentRunID = parent.ID
		r.Depth = parent.Depth + 1
	})

	if status := h.call(t, admin, http.MethodDelete, deletePath(parent.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete the parent: %d", status)
	}
	remaining := h.Run(child.ID)
	if remaining.ParentRunID != "" {
		t.Errorf("parent_run_id = %q, want it cleared", remaining.ParentRunID)
	}
	if remaining.Status != clusterv1.StatusQueued {
		t.Errorf("child status = %s, want it untouched", remaining.Status)
	}
}

// A controller keeps the CR for a day after the run ends, and may report on
// it after an operator deleted it. The contract's answer to an unknown run is
// abandon, and that is what it has to get — not an error it retries.
func TestAReportAboutADeletedRunIsAnsweredWithAbandon(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("terminal status: %+v", problem)
	}
	if err := h.App.Delete(context.Background(), admitted.ID, "operator"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Status ingest is a batch, so the answer is per report rather than a
	// Problem for the whole request: one deleted run must not fail the others.
	result, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded)
	fatalIfProblem(t, "report about a deleted run", problem)
	if result.Accepted {
		t.Fatal("a report about a deleted run was accepted")
	}
	if result.Code != clusterv1.CodeRunNotFound || result.Action != clusterv1.ActionAbandon {
		t.Errorf("result = %s/%s, want %s/abandon", result.Code, result.Action, clusterv1.CodeRunNotFound)
	}
}

func TestRetentionDeletesOnlyRunsThatFinishedLongerAgoThanItKeeps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	old := h.aFinishedRun()
	recent := h.aFinishedRun()
	live := h.Submit()
	h.stageObject(old.ID, runv1.StorageKeyResult)

	// Forty days ago. finished_at is an observation column, which runs_guard
	// leaves writable.
	if _, err := h.Store.DB().ExecContext(ctx,
		`UPDATE runs SET finished_at = now() - interval '40 days' WHERE id = $1`, string(old.ID)); err != nil {
		t.Fatalf("backdate the old run: %v", err)
	}

	if n, err := h.App.ExpireRuns(ctx, 0); err != nil || n != 0 {
		t.Fatalf("a zero retention deleted %d runs (err %v); zero means keep forever", n, err)
	}

	n, err := h.App.ExpireRuns(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("expire runs: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d runs, want 1", n)
	}
	if h.runExists(old.ID) {
		t.Error("a run past retention is still there")
	}
	if h.objectCount(old.ID) != 0 {
		t.Error("the objects of a run past retention are still stored")
	}
	if !h.runExists(recent.ID) {
		t.Error("a run inside retention was deleted")
	}
	if !h.runExists(live.ID) {
		t.Error("a run that has not ended was deleted")
	}

	var actor, reason string
	if err := h.Store.DB().QueryRowContext(ctx, `
		SELECT actor, payload->>'reason' FROM audit_log
		WHERE action = $1 AND run_id = $2`, store.AuditRunDeleted, string(old.ID)).
		Scan(&actor, &reason); err != nil {
		t.Fatalf("read the audit record of the expiry: %v", err)
	}
	if actor != "retention" || reason != "retention" {
		t.Errorf("audit record is by %q for %q, want retention", actor, reason)
	}

	if n, err := h.App.ExpireRuns(ctx, 30*24*time.Hour); err != nil || n != 0 {
		t.Errorf("a second pass deleted %d runs (err %v), want none", n, err)
	}
}

// aFailedRun is a run refused by the only cluster there is: Failed, with
// finished_at set, and nothing left running anywhere.
func (h *harness) aFailedRun(p *probe) store.Run {
	h.t.Helper()
	admitted := h.Submit()
	lease := p.LeaseOne()
	if lease.RunID != admitted.ID {
		h.t.Fatalf("leased %s, want %s", lease.RunID, admitted.ID)
	}
	_, problem := p.Reject(lease.RunID, lease.Epoch, clusterv1.RejectQuotaExhausted, "no quota")
	fatalIfProblem(h.t, "reject", problem)
	failed := h.Run(admitted.ID)
	if failed.Status != clusterv1.StatusFailed {
		h.t.Fatalf("status = %s, want Failed", failed.Status)
	}
	return failed
}

func (h *harness) finishedAgo(id runv1.ULID, ago time.Duration) {
	h.t.Helper()
	if _, err := h.Store.DB().ExecContext(context.Background(),
		`UPDATE runs SET finished_at = now() - make_interval(secs => $2) WHERE id = $1`,
		string(id), ago.Seconds()); err != nil {
		h.t.Fatalf("backdate run %s: %v", id, err)
	}
}

type runRetentionBody struct {
	FailedRetentionSeconds int64  `json:"failed_retention_seconds"`
	RetentionSeconds       int64  `json:"retention_seconds"`
	Source                 string `json:"source"`
	UpdatedBy              string `json:"updated_by"`
}

func TestAFailedRunIsDeletedAfterThreeDaysByDefaultAndNoOtherRunIs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := newProbe(t, h, "east")

	old := h.aFailedRun(p)
	recent := h.aFailedRun(p)
	cancelled := h.aFinishedRun()
	h.stageObject(old.ID, runv1.StorageKeyResult)
	h.finishedAgo(old.ID, 3*24*time.Hour+time.Minute)
	h.finishedAgo(recent.ID, 3*24*time.Hour-time.Hour)
	h.finishedAgo(cancelled.ID, 40*24*time.Hour)

	keep, err := h.App.RunRetention(ctx)
	if err != nil {
		t.Fatalf("read run retention: %v", err)
	}
	if keep.Failed != 3*24*time.Hour || keep.Source != app.RunRetentionDefault {
		t.Fatalf("retention = %s from %s, want 72h from the default", keep.Failed, keep.Source)
	}

	n, err := h.App.ExpireFailedRuns(ctx, keep.Failed)
	if err != nil {
		t.Fatalf("expire failed runs: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d runs, want 1", n)
	}
	if h.runExists(old.ID) {
		t.Error("a failed run older than three days is still there")
	}
	if h.objectCount(old.ID) != 0 {
		t.Error("the objects of an expired failed run are still stored")
	}
	if !h.runExists(recent.ID) {
		t.Error("a failed run younger than three days was deleted")
	}
	if !h.runExists(cancelled.ID) {
		t.Error("a cancelled run was deleted by the failed-run retention")
	}

	var actor, reason string
	if err := h.Store.DB().QueryRowContext(ctx, `
		SELECT actor, payload->>'reason' FROM audit_log
		WHERE action = $1 AND run_id = $2`, store.AuditRunDeleted, string(old.ID)).
		Scan(&actor, &reason); err != nil {
		t.Fatalf("read the audit record of the expiry: %v", err)
	}
	if actor != "retention" || reason != "failed_retention" {
		t.Errorf("audit record is by %q for %q, want retention for failed_retention", actor, reason)
	}
}

func TestAnOperatorChangesTheFailedRunRetentionFromTheAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := newProbe(t, h, "east")
	admin := h.Token(store.ScopeAdmin)
	writer := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	var got runRetentionBody
	if status := h.call(t, writer, http.MethodGet, "/api/v1/run-retention", nil, &got); status != http.StatusOK {
		t.Fatalf("read retention with runs:read: %d", status)
	}
	if got.FailedRetentionSeconds != 259200 || got.Source != "default" || got.RetentionSeconds != 0 {
		t.Errorf("retention = %+v, want 259200 s by default and no general retention", got)
	}

	if status := h.call(t, writer, http.MethodPut, "/api/v1/run-retention",
		map[string]any{"failed_retention_seconds": 0}, nil); status != http.StatusForbidden {
		t.Errorf("runs:write changed the retention: %d, want 403", status)
	}
	for _, body := range []map[string]any{
		{},
		{"failed_retention_seconds": -1},
		{"failed_retention_seconds": 60},
	} {
		if status := h.call(t, admin, http.MethodPut, "/api/v1/run-retention", body, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("PUT %v = %d, want 422", body, status)
		}
	}

	if status := h.call(t, admin, http.MethodPut, "/api/v1/run-retention",
		map[string]any{"failed_retention_seconds": 7 * 86400}, &got); status != http.StatusOK {
		t.Fatalf("save retention: %d", status)
	}
	if got.FailedRetentionSeconds != 7*86400 || got.Source != "database" || got.UpdatedBy == "" {
		t.Errorf("saved retention = %+v, want a week from the database with who saved it", got)
	}
	var saved int
	if err := h.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1`, store.AuditRunRetentionSaved).Scan(&saved); err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	if saved != 1 {
		t.Errorf("saving the retention left %d audit records, want 1", saved)
	}

	failed := h.aFailedRun(p)
	h.finishedAgo(failed.ID, 5*24*time.Hour)
	keep, err := h.App.RunRetention(ctx)
	if err != nil {
		t.Fatalf("read run retention: %v", err)
	}
	if n, err := h.App.ExpireFailedRuns(ctx, keep.Failed); err != nil || n != 0 {
		t.Errorf("a week's retention deleted %d runs five days old (err %v)", n, err)
	}

	// Zero keeps failed runs for ever.
	if status := h.call(t, admin, http.MethodPut, "/api/v1/run-retention",
		map[string]any{"failed_retention_seconds": 0}, &got); status != http.StatusOK || got.FailedRetentionSeconds != 0 {
		t.Fatalf("save a zero retention: %d, %+v", status, got)
	}
	h.finishedAgo(failed.ID, 400*24*time.Hour)
	if keep, err = h.App.RunRetention(ctx); err != nil {
		t.Fatalf("read run retention: %v", err)
	}
	if n, err := h.App.ExpireFailedRuns(ctx, keep.Failed); err != nil || n != 0 || !h.runExists(failed.ID) {
		t.Errorf("a zero retention deleted %d runs (err %v); zero means keep forever", n, err)
	}
}
