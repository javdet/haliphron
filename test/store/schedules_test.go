package store

import (
	"database/sql"
	"testing"
)

// schedules: a run template the backend fires, and the link from each run it
// started back to it.

func newSchedule(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	id := newULID(t)
	if _, err := conn.Exec(`
		INSERT INTO schedules (id, name, cron, run, next_run_at, created_by, updated_by)
		VALUES ($1, $2, '0 3 * * *', '{"prompt": "x"}', now() + interval '1 hour', 'test', 'test')`,
		id, name); err != nil {
		t.Fatalf("insert schedule %s: %v", name, err)
	}
	return id
}

func TestASchedulesNextRunIsNullExactlyWhenItIsDisabled(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	id := newSchedule(t, conn, "nightly")

	if _, err := conn.Exec(`UPDATE schedules SET enabled = false WHERE id = $1`, id); err == nil {
		t.Error("a disabled schedule kept a next fire")
	}
	if _, err := conn.Exec(`UPDATE schedules SET next_run_at = NULL WHERE id = $1`, id); err == nil {
		t.Error("an enabled schedule lost its next fire")
	}
	if _, err := conn.Exec(`UPDATE schedules SET enabled = false, next_run_at = NULL WHERE id = $1`, id); err != nil {
		t.Errorf("disabling a schedule: %v", err)
	}
}

func TestAScheduleNameIsTakenOnce(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	newSchedule(t, conn, "nightly")

	if _, err := conn.Exec(`
		INSERT INTO schedules (id, name, cron, run, next_run_at, created_by, updated_by)
		VALUES ($1, 'nightly', '@daily', '{}', now(), 'test', 'test')`, newULID(t)); err == nil {
		t.Error("two schedules share a name")
	}
}

func TestDeletingAScheduleKeepsItsRunsAndClearsTheirScheduleId(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	id := newSchedule(t, conn, "nightly")

	// Terminal first: runs_guard freezes admission fields, and the SET NULL
	// is an UPDATE it sees — this is what proves schedule_id is not among them.
	if _, err := conn.Exec(`UPDATE runs SET schedule_id = $2, status = 'Succeeded', observed_phase = 'Succeeded'
		WHERE id = $1`, f.runID, id); err != nil {
		t.Fatalf("link run: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM schedules WHERE id = $1`, id); err != nil {
		t.Fatalf("delete schedule: %v", err)
	}

	var schedule sql.NullString
	if err := conn.QueryRow(`SELECT schedule_id FROM runs WHERE id = $1`, f.runID).Scan(&schedule); err != nil {
		t.Fatalf("the run went with its schedule: %v", err)
	}
	if schedule.Valid {
		t.Errorf("the run still names deleted schedule %s", schedule.String)
	}
}

func TestDeletingTheLastRunClearsTheSchedulesLastRunId(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	id := newSchedule(t, conn, "nightly")
	if _, err := conn.Exec(`UPDATE schedules SET last_run_id = $2 WHERE id = $1`, id, f.runID); err != nil {
		t.Fatalf("record last run: %v", err)
	}

	if _, err := conn.Exec(`DELETE FROM run_attempts WHERE run_id = $1`, f.runID); err != nil {
		t.Fatalf("delete attempts: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM runs WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("run retention could not delete a schedule's last run: %v", err)
	}
	var last sql.NullString
	if err := conn.QueryRow(`SELECT last_run_id FROM schedules WHERE id = $1`, id).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last.Valid {
		t.Errorf("last_run_id still names deleted run %s", last.String)
	}
}
