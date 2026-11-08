package store

import (
	"database/sql"
	"testing"
)

// resource_locks and deferred_runs: the claims of the schema itself. The
// ordering rule — every set sees every earlier lock — is a property of the
// statements in backend/store/locks.go and is tested against them in
// test/backend; what is here is what holds whichever statement writes.

func newLock(t *testing.T, conn *sql.DB, runID, name string) string {
	t.Helper()
	id := newULID(t)
	if _, err := conn.Exec(`
		INSERT INTO resource_locks (id, run_id, lease_epoch, name, expires_at)
		VALUES ($1, $2, 1, $3, now() + interval '1 hour')`, id, runID, name); err != nil {
		t.Fatalf("insert lock %s: %v", name, err)
	}
	return id
}

func lockCount(t *testing.T, conn *sql.DB, runID string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM resource_locks WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestARunsLocksGoInTheTransactionThatMakesItTerminal(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"Succeeded", "Failed", "TimedOut", "Cancelled"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			conn := newDB(t)
			f := seed(t, conn)
			newLock(t, conn, f.runID, "prod/db")
			newLock(t, conn, f.runID, "prod/lb")

			tx, err := conn.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`UPDATE runs SET status = $2, observed_phase = $3 WHERE id = $1`,
				f.runID, status, status); err != nil {
				t.Fatalf("end run: %v", err)
			}
			var inside int
			if err := tx.QueryRow(`SELECT count(*) FROM resource_locks WHERE run_id = $1`, f.runID).Scan(&inside); err != nil {
				t.Fatal(err)
			}
			if inside != 0 {
				t.Errorf("%d locks survive the statement that made the run %s", inside, status)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if n := lockCount(t, conn, f.runID); n != 2 {
				t.Errorf("a rolled-back status change released %d of 2 locks", 2-n)
			}
		})
	}
}

func TestAnyStatusThatIsNotTerminalKeepsTheLocks(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	newLock(t, conn, f.runID, "prod/db")

	// A requested cancellation is not an ending: the pod is alive until the
	// next heartbeat delivers the cancel.
	if _, err := conn.Exec(`UPDATE runs SET cancel_requested_at = now(), cancel_requested_by = 'op'
		WHERE id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE runs SET status = 'Unknown' WHERE id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if n := lockCount(t, conn, f.runID); n != 1 {
		t.Errorf("a cancel request and Unknown released the run's lock")
	}
}

// A run can leave a terminal status again under a higher attempt. Its locks
// went at the first terminal status and are not resurrected; the new attempt
// sets them again, and nothing in the schema stands in its way.
func TestALockReleasedAtATerminalAttemptCanBeSetAgainByTheNextAttempt(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	newLock(t, conn, f.runID, "prod/db")

	if _, err := conn.Exec(`UPDATE runs SET status = 'Failed', observed_phase = 'Failed' WHERE id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE runs SET status = 'Running', observed_phase = 'Running', attempt = 2
		WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("a second attempt could not leave the terminal status: %v", err)
	}
	if n := lockCount(t, conn, f.runID); n != 0 {
		t.Errorf("%d locks came back with the second attempt", n)
	}
	newLock(t, conn, f.runID, "prod/db")
}

func TestDeletingARunDeletesItsLocksAndItsWaitingDeferredRuns(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	newLock(t, conn, f.runID, "prod/db")
	if _, err := conn.Exec(`
		INSERT INTO deferred_runs (id, parent_run_id, template, not_before, give_up_at, created_by)
		VALUES ($1, $2, '{"prompt":"x"}', now(), now() + interval '1 day', 'test')`, newULID(t), f.runID); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(`DELETE FROM runs WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	var n int
	if err := conn.QueryRow(`SELECT (SELECT count(*) FROM resource_locks) + (SELECT count(*) FROM deferred_runs)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d lock or deferred rows outlived their run", n)
	}
}

func TestALockNameIsHeldOncePerRunAndIsBounded(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	other := newQueuedRun(t, conn, f.clusterID)
	newLock(t, conn, f.runID, "prod/db")

	// Names are free-form and not unique across runs: two runs may both say
	// they are working on prod/db, and deciding what that means is theirs.
	newLock(t, conn, other, "prod/db")

	if _, err := conn.Exec(`INSERT INTO resource_locks (id, run_id, lease_epoch, name, expires_at)
		VALUES ($1, $2, 1, 'prod/db', now() + interval '1 hour')`, newULID(t), f.runID); err == nil {
		t.Error("one run holds two locks of one name")
	}
	for _, name := range []string{"", string(make([]byte, 129))} {
		if _, err := conn.Exec(`INSERT INTO resource_locks (id, run_id, lease_epoch, name, expires_at)
			VALUES ($1, $2, 1, $3, now() + interval '1 hour')`, newULID(t), f.runID, name); err == nil {
			t.Errorf("a %d-byte name was stored", len(name))
		}
	}
	if _, err := conn.Exec(`INSERT INTO resource_locks (id, run_id, lease_epoch, name, expires_at)
		VALUES ($1, $2, 1, 'x', now() - interval '1 second')`, newULID(t), f.runID); err == nil {
		t.Error("a lock was stored already expired")
	}
}

func TestADeferredRunWaitsForSomethingAndIsFinishedExactlyWhenItIsNotWaiting(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)

	if _, err := conn.Exec(`
		INSERT INTO deferred_runs (id, parent_run_id, template, give_up_at, created_by)
		VALUES ($1, $2, '{}', now() + interval '1 day', 'test')`, newULID(t), f.runID); err == nil {
		t.Error("a deferred run waits for neither a lock nor a time")
	}

	lock := newLock(t, conn, f.runID, "prod/db")
	id := newULID(t)
	if _, err := conn.Exec(`
		INSERT INTO deferred_runs (id, parent_run_id, template, wait_for_locks, give_up_at, created_by)
		VALUES ($1, $2, '{}', ARRAY[$3]::ulid[], now() + interval '1 day', 'test')`, id, f.runID, lock); err != nil {
		t.Fatalf("insert deferred run: %v", err)
	}
	if _, err := conn.Exec(`UPDATE deferred_runs SET state = 'expired' WHERE id = $1`, id); err == nil {
		t.Error("a deferred run ended without a finished_at")
	}
	if _, err := conn.Exec(`UPDATE deferred_runs SET state = 'expired', finished_at = now() WHERE id = $1`, id); err != nil {
		t.Errorf("expire a deferred run: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO deferred_runs (id, parent_run_id, template, wait_for_locks, give_up_at, created_by)
		VALUES ($1, $2, '{}', ARRAY['not-a-ulid']::ulid[], now() + interval '1 day', 'test')`, newULID(t), f.runID); err == nil {
		t.Error("a deferred run waits on something that is not a lock id")
	}
}
