package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// run_notes and runs.lineage: the claims of the schema itself. Scope, quotas
// and partition maintenance are properties of the statements in
// backend/store/notes.go and are tested against them in test/backend; what is
// here holds whichever statement writes.

// notePartitionFor creates the day partition a run's notes go into. The
// backend creates partitions standalone and attaches them; PARTITION OF is
// good enough for a fixture.
func notePartitionFor(t *testing.T, conn *sql.DB, runID string) string {
	t.Helper()
	var day time.Time
	if err := conn.QueryRow(`SELECT date_trunc('day', created_at, 'UTC') FROM runs WHERE id = $1`,
		runID).Scan(&day); err != nil {
		t.Fatal(err)
	}
	name := "run_notes_" + day.UTC().Format("20060102")
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS ` + name + ` PARTITION OF run_notes
		FOR VALUES FROM ('` + day.UTC().Format(time.RFC3339) + `') TO ('` +
		day.UTC().AddDate(0, 0, 1).Format(time.RFC3339) + `')`); err != nil {
		t.Fatalf("create note partition: %v", err)
	}
	return name
}

func putNote(t *testing.T, conn *sql.DB, runID, key, value string) error {
	t.Helper()
	_, err := conn.Exec(`
		INSERT INTO run_notes (run_id, owner_created_at, key, value)
		SELECT id, created_at, $2, $3 FROM runs WHERE id = $1`, runID, key, value)
	return err
}

func noteCount(t *testing.T, conn *sql.DB, runID string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM run_notes WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeletingARunDeletesItsNotesAndNoOtherRuns(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	other := newQueuedRun(t, conn, f.clusterID)
	notePartitionFor(t, conn, f.runID)

	for _, id := range []string{f.runID, other} {
		for _, key := range []string{"build/command", "layout"} {
			if err := putNote(t, conn, id, key, "something learned"); err != nil {
				t.Fatalf("write note: %v", err)
			}
		}
	}
	if _, err := conn.Exec(`DELETE FROM runs WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if n := noteCount(t, conn, f.runID); n != 0 {
		t.Errorf("%d notes outlived their run", n)
	}
	if n := noteCount(t, conn, other); n != 2 {
		t.Errorf("deleting one run took %d of another run's 2 notes", 2-n)
	}
}

func TestANoteIsBoundedByTheSchemaWhateverWritesIt(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	notePartitionFor(t, conn, f.runID)

	for name, write := range map[string][2]string{
		"empty key":        {"", "v"},
		"long key":         {strings.Repeat("k", 129), "v"},
		"empty value":      {"k", ""},
		"value over 16KiB": {"k", strings.Repeat("v", 16385)},
	} {
		if err := putNote(t, conn, f.runID, write[0], write[1]); err == nil {
			t.Errorf("%s: the schema accepted it", name)
		}
	}
	if err := putNote(t, conn, f.runID, "k", strings.Repeat("v", 16384)); err != nil {
		t.Errorf("a 16 KiB value was refused: %v", err)
	}
}

// Without a partition for its day a note has nowhere to go, and the insert
// says so rather than landing somewhere else: there is no default partition.
func TestANoteWithNoPartitionForItsDayIsRefused(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	if err := putNote(t, conn, f.runID, "k", "v"); err == nil {
		t.Fatal("a note was stored with no partition for its day")
	}
}

// The point of having no foreign key: partition DDL never waits for runs, the
// busiest table in the schema. Here runs is locked outright while a partition
// is created, attached and dropped.
func TestNotePartitionDDLNeverWaitsForRuns(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	ctx := context.Background()

	holder, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err := holder.Exec(`LOCK TABLE runs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`SET LOCAL lock_timeout = '1s'`,
		`CREATE TABLE run_notes_20300101 (LIKE run_notes INCLUDING DEFAULTS INCLUDING CONSTRAINTS,
			PRIMARY KEY (run_id, owner_created_at, key)) WITH (fillfactor = 90)`,
		`ALTER TABLE run_notes ATTACH PARTITION run_notes_20300101
			FOR VALUES FROM ('2030-01-01T00:00:00Z') TO ('2030-01-02T00:00:00Z')`,
		`DROP TABLE run_notes_20300101`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			t.Fatalf("%s\nwaited for the lock on runs: %v", strings.Fields(stmt)[0], err)
		}
	}
}

func TestARunsLineageIsImmutableAndNeverLongerThanItsDepth(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)

	if _, err := conn.Exec(`UPDATE runs SET lineage = ARRAY[$2]::ulid[] WHERE id = $1`,
		f.runID, newULID(t)); err == nil {
		t.Error("a run's lineage was rewritten after admission")
	}
	if _, err := conn.Exec(`UPDATE runs SET priority = priority + 1 WHERE id = $1`, f.runID); err != nil {
		t.Errorf("an update that leaves the lineage alone was refused: %v", err)
	}

	// A root run has no ancestors.
	if _, err := conn.Exec(`
		INSERT INTO runs (id, created_by, created_via, spec, prompt, prompt_sha256,
		                  agent, model, timeout_seconds, depth, lineage)
		VALUES ($1, 'test', 'api', $2::jsonb, 'p', sha256('p'::bytea),
		        'claude-code', 'anthropic/claude-opus-5', 3600, 0, ARRAY[$3]::ulid[])`,
		newULID(t), minimalSpec, f.runID); err == nil {
		t.Error("a run at depth 0 was given an ancestor")
	}
	// A run below one admitted before lineage existed knows fewer ancestors
	// than its depth, and that is allowed.
	if _, err := conn.Exec(`
		INSERT INTO runs (id, parent_run_id, created_by, created_via, spec, prompt, prompt_sha256,
		                  agent, model, timeout_seconds, depth, lineage)
		VALUES ($1, $3, 'test', 'agent', $2::jsonb, 'p', sha256('p'::bytea),
		        'claude-code', 'anthropic/claude-opus-5', 3600, 2, ARRAY[$3]::ulid[])`,
		newULID(t), minimalSpec, f.runID); err != nil {
		t.Errorf("a lineage shorter than the depth was refused: %v", err)
	}
}
