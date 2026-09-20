package store

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/automagicops/haliphron/db"
)

func leaseSQL(t *testing.T) string { t.Helper(); return db.Query("lease") }

// Two polls arriving at once is not a rare race. A controller reconnects
// before its previous long poll has finished unwinding, and a rolling restart
// has two controller replicas asking at the same moment. Without SKIP LOCKED
// one of them blocks until the other commits and then hands out the same run.
func TestConcurrentLeasesAreDisjoint(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	cluster := newCluster(t, conn, "busy")
	const runs = 20
	for i := 0; i < runs; i++ {
		newQueuedRun(t, conn, cluster)
	}

	const pollers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
	)
	for i := 0; i < pollers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				id, _, ok := leaseOne(t, conn, cluster, 60, 120)
				if !ok {
					return
				}
				mu.Lock()
				seen[id]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != runs {
		t.Errorf("leased %d distinct runs, queued %d", len(seen), runs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("run %s was leased %d times", id, n)
		}
	}
}

// The first lease hands out epoch 1, as the table in section 3 of the Cluster
// API contract says it does. The epoch belongs to ownership, and handing work
// to a cluster that has not owned it before does not revoke anything.
func TestFirstLeaseHandsOutEpochOne(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	fx := seed(t, conn)

	id, epoch, ok := leaseOne(t, conn, fx.clusterID, 60, 120)
	if !ok {
		t.Fatal("nothing leased")
	}
	if id != fx.runID {
		t.Fatalf("leased %s, seeded %s", id, fx.runID)
	}
	if epoch != 1 {
		t.Errorf("first lease handed out epoch %d, want 1", epoch)
	}
}

// The property the whole fence rests on, and the reason expire_ack raises the
// epoch instead of the next lease doing it.
//
// Between requeue and re-lease a run sits in Queued, possibly for a while. If
// the epoch only rose when the work was handed out again, the controller that
// failed to ack would still be holding the current epoch during that window,
// and a status it managed to send would compare equal and be applied to a run
// it no longer owns. Raising it at the moment ownership is revoked leaves no
// window at all.
func TestAckExpiryFencesTheOldOwnerImmediately(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	ctx := context.Background()
	fx := seed(t, conn)

	_, held, ok := leaseOne(t, conn, fx.clusterID, 0, 120)
	if !ok {
		t.Fatal("nothing leased")
	}

	var (
		requeued string
		epoch    int64
		cluster  sql.NullString
	)
	err := conn.QueryRowContext(ctx, db.Query("expire_ack")).Scan(&requeued, &cluster, &epoch)
	if err != nil {
		t.Fatalf("expire ack: %v", err)
	}
	if requeued != fx.runID {
		t.Fatalf("expired %s, leased %s", requeued, fx.runID)
	}
	if epoch <= held {
		t.Errorf("run is back in the queue still at epoch %d; the old owner is not fenced", epoch)
	}

	var status string
	var ackDeadline, leaseDeadline sql.NullTime
	err = conn.QueryRowContext(ctx,
		`SELECT status, ack_deadline, lease_deadline FROM runs WHERE id = $1`, fx.runID).
		Scan(&status, &ackDeadline, &leaseDeadline)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "Queued" {
		t.Errorf("status after ack expiry is %s, want Queued", status)
	}
	if ackDeadline.Valid || leaseDeadline.Valid {
		t.Error("a queued run is still carrying lease deadlines")
	}
	// The assignment survives: reassignment is placement's decision, made
	// against the exclusion table, not a side effect of a controller restart.
	if !cluster.Valid {
		t.Error("ack expiry cleared the assigned cluster")
	}
}

// After ack the opposite rule applies. A Job may be executing this second, so
// the run goes to Unknown and is not handed to anybody else — and the epoch
// does not move, because the controller playing the work out offline is
// supposed to be able to report the result when the link comes back.
func TestLeaseExpiryGoesToUnknownAndKeepsTheEpoch(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	ctx := context.Background()
	fx := seed(t, conn)

	_, epoch, _ := leaseOne(t, conn, fx.clusterID, 60, 120)
	mustExec(t, conn, `UPDATE runs
		SET status = 'Running', observed_phase = 'Running', lease_deadline = now() - interval '1 second'
		WHERE id = $1`, fx.runID)

	var (
		id            string
		cluster       string
		expiredEpoch  int64
		attempt       int
		observedPhase sql.NullString
	)
	err := conn.QueryRowContext(ctx, db.Query("expire_lease")).
		Scan(&id, &cluster, &expiredEpoch, &attempt, &observedPhase)
	if err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if expiredEpoch != epoch {
		t.Errorf("lease expiry moved the epoch %d -> %d; the offline controller is now a zombie",
			epoch, expiredEpoch)
	}

	var status string
	var rank int
	err = conn.QueryRowContext(ctx,
		`SELECT status, observed_rank FROM runs WHERE id = $1`, fx.runID).Scan(&status, &rank)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "Unknown" {
		t.Errorf("status after lease expiry is %s, want Unknown", status)
	}
	// The rank survives Unknown, which is what lets the returning controller's
	// Running report be applied as the idempotent repeat it is instead of being
	// rejected as a regression.
	if rank != 30 {
		t.Errorf("observed rank after Unknown is %d, want the 30 it had while Running", rank)
	}
}

// Each row is one line of the epoch/attempt table in section 3 or of the
// ordering table in section 5, restated as a write the database either takes
// or refuses.
func TestRunsGuard(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		setup   string // applied first, expected to succeed
		write   string
		wantErr string
	}{
		{
			name:    "epoch never goes backwards",
			setup:   `UPDATE runs SET lease_epoch = 3 WHERE id = $1`,
			write:   `UPDATE runs SET lease_epoch = 2 WHERE id = $1`,
			wantErr: "epoch regression",
		},
		{
			name:    "attempt never goes backwards inside an epoch",
			setup:   `UPDATE runs SET attempt = 4 WHERE id = $1`,
			write:   `UPDATE runs SET attempt = 2 WHERE id = $1`,
			wantErr: "attempt regression",
		},
		{
			name:  "attempt resets when a new owner takes the run",
			setup: `UPDATE runs SET attempt = 4 WHERE id = $1`,
			write: `UPDATE runs SET lease_epoch = lease_epoch + 1, attempt = 1 WHERE id = $1`,
		},
		{
			name:    "the first terminal phase wins",
			setup:   `UPDATE runs SET status = 'Succeeded', observed_phase = 'Succeeded' WHERE id = $1`,
			write:   `UPDATE runs SET status = 'Failed', observed_phase = 'Failed' WHERE id = $1`,
			wantErr: "terminal status",
		},
		{
			name:  "a further attempt may reopen a terminal run",
			setup: `UPDATE runs SET status = 'Failed', observed_phase = 'Failed' WHERE id = $1`,
			write: `UPDATE runs SET attempt = attempt + 1, status = 'Dispatched', observed_phase = 'Pending' WHERE id = $1`,
		},
		{
			name:  "an operator retry may reopen a terminal run",
			setup: `UPDATE runs SET status = 'Failed', observed_phase = 'Failed' WHERE id = $1`,
			write: `UPDATE runs SET lease_epoch = lease_epoch + 1, attempt = 1, status = 'Queued', observed_phase = NULL WHERE id = $1`,
		},
		{
			name:    "the spec is frozen at admission",
			write:   `UPDATE runs SET spec = spec || '{"model":"other"}'::jsonb WHERE id = $1`,
			wantErr: "spec is immutable",
		},
		{
			name:    "so is the prompt digest",
			write:   `UPDATE runs SET prompt_sha256 = sha256('other'::bytea) WHERE id = $1`,
			wantErr: "admission fields are immutable",
		},
		{
			// The prompt is frozen with the rest of the admission group. An
			// edit between attempts would make attempt 2 a different run from
			// attempt 1 while every identifier said otherwise.
			name:    "so is the prompt itself",
			write:   `UPDATE runs SET prompt = 'delete production' WHERE id = $1`,
			wantErr: "admission fields are immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := newDB(t)
			fx := seed(t, conn)

			if tc.setup != "" {
				mustExec(t, conn, tc.setup, fx.runID)
			}
			_, err := conn.Exec(tc.write, fx.runID)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("write refused: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("write accepted; expected it to be refused with %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("refused with %v, expected something containing %q", err, tc.wantErr)
			}
		})
	}
}

// updated_at is maintained by the trigger rather than by every caller, which is
// the only way a column like this stays true.
func TestUpdatedAtIsMaintained(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	fx := seed(t, conn)

	var before time.Time
	mustQuery(t, conn, `SELECT updated_at FROM runs WHERE id = $1`, fx.runID).Scan(&before)
	mustExec(t, conn, `UPDATE runs SET priority = 5 WHERE id = $1`, fx.runID)

	var after time.Time
	mustQuery(t, conn, `SELECT updated_at FROM runs WHERE id = $1`, fx.runID).Scan(&after)
	if !after.After(before) {
		t.Errorf("updated_at did not move: %s -> %s", before, after)
	}
}

// attempt resets to 1 on every new epoch, so the attempt key has to carry the
// epoch. Under the (run_id, attempt) key sketched in architecture.md, the second
// owner's first attempt collides with the first owner's — and the row it
// collides with is the accounting record of an attempt that already spent money.
func TestAttemptKeyCarriesTheEpoch(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	fx := seed(t, conn)

	insert := `INSERT INTO run_attempts (run_id, lease_epoch, attempt, cluster_id, phase, cost_usd)
	           VALUES ($1, $2, 1, $3, 'Running', $4)`

	mustExec(t, conn, insert, fx.runID, 1, fx.clusterID, "0.4231")

	// Raising the epoch is what a reassignment does, and the trigger closes the
	// outstanding attempt in the same statement. Without that the partial
	// unique index would refuse the second row below — and the run would be
	// permanently unrunnable, which is the trap the trigger exists to avoid.
	mustExec(t, conn, `UPDATE runs SET lease_epoch = 2, attempt = 1 WHERE id = $1`, fx.runID)

	var open int
	mustQuery(t, conn, `SELECT count(*) FROM run_attempts WHERE run_id = $1 AND finished_at IS NULL`,
		fx.runID).Scan(&open)
	if open != 0 {
		t.Fatalf("%d attempts are still open after the epoch rose; the next owner cannot take the run", open)
	}
	var class, reason string
	mustQuery(t, conn,
		`SELECT failure_class::text, coalesce(reason, '') FROM run_attempts
		 WHERE run_id = $1 AND lease_epoch = 1`, fx.runID).Scan(&class, &reason)
	if class != "infra" {
		t.Errorf("the fenced attempt is class %q, want infra: it was ended by the platform", class)
	}
	if reason == "" {
		t.Error("the fenced attempt records no reason, so nobody can tell why it says Failed")
	}

	mustExec(t, conn, insert, fx.runID, 2, fx.clusterID, "0.1000")

	if _, err := conn.Exec(insert, fx.runID, 1, fx.clusterID, "9.9999"); err == nil {
		t.Error("the same attempt of the same ownership was inserted twice")
	}

	var total string
	mustQuery(t, conn,
		`SELECT sum(cost_usd)::text FROM run_attempts WHERE run_id = $1`, fx.runID).Scan(&total)
	if total != "0.523100" {
		t.Errorf("spend across attempts is %s, want 0.523100", total)
	}
}

// One live attempt per run, as a constraint rather than a convention.
//
// backoffLimit: 0 constrains one Job, not two controllers, and a rule that
// lives only in a controller's code is a rule the second controller does not
// know about. This is the place it cannot be forgotten.
func TestOnlyOneAttemptOfARunMayBeOpenAtATime(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	fx := seed(t, conn)

	insert := `INSERT INTO run_attempts (run_id, lease_epoch, attempt, cluster_id, phase)
	           VALUES ($1, 1, $2, $3, 'Running')`

	mustExec(t, conn, insert, fx.runID, 1, fx.clusterID)
	if _, err := conn.Exec(insert, fx.runID, 2, fx.clusterID); err == nil {
		t.Fatal("a second attempt was opened while the first was still running")
	}

	// Once the first has finished, the next may start. The index is about
	// concurrency, not about how many attempts a run is allowed.
	mustExec(t, conn,
		`UPDATE run_attempts SET finished_at = now() WHERE run_id = $1 AND attempt = 1`, fx.runID)
	mustExec(t, conn, insert, fx.runID, 2, fx.clusterID)

	// And a different run is unaffected: the constraint is per run.
	other := newQueuedRun(t, conn, fx.clusterID)
	mustExec(t, conn, insert, other, 1, fx.clusterID)
}

// The checkpoint is unioned on write, never replaced. Reports arrive reordered,
// and a heartbeat carrying an earlier snapshot must not shorten a list the
// low-latency path already grew — which would hand the next attempt a
// checkpoint saying the model had not run when it had.
func TestCompletedPhasesAreUnionedRatherThanReplaced(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	fx := seed(t, conn)

	upsert := `INSERT INTO run_attempts (run_id, lease_epoch, attempt, cluster_id, completed_phases)
	           VALUES ($1, 1, 1, $2, $3::text[]::runtime_phase[])
	           ON CONFLICT (run_id, lease_epoch, attempt) DO UPDATE SET
	             completed_phases = ARRAY(SELECT DISTINCT unnest(
	               run_attempts.completed_phases || EXCLUDED.completed_phases))`

	mustExec(t, conn, upsert, fx.runID, fx.clusterID, "{init,validate,fetch,run}")
	// A later report that saw less. It must not shorten what is stored.
	mustExec(t, conn, upsert, fx.runID, fx.clusterID, "{init,validate}")

	var stored string
	mustQuery(t, conn,
		`SELECT array_to_string(ARRAY(SELECT DISTINCT unnest(completed_phases) ORDER BY 1), ',')
		 FROM run_attempts WHERE run_id = $1`, fx.runID).Scan(&stored)
	for _, want := range []string{"init", "validate", "fetch", "run"} {
		if !strings.Contains(stored, want) {
			t.Errorf("phase %s was lost: stored %q", want, stored)
		}
	}

	// A phase name the contract does not know is refused by the domain, which
	// is what keeps a retry from skipping a step nothing ever did.
	if _, err := conn.Exec(upsert, fx.runID, fx.clusterID, "{invoke-model}"); err == nil {
		t.Error("a phase name outside the contract was accepted")
	}
}

// A prompt over the ceiling is refused by the check constraint. Admission
// refuses it first with a 413 naming the limit; this is the backstop, and the
// reason the limit is 512 KiB rather than a round number is that the value has
// to fit in a Secret beside three credentials.
func TestThePromptCeilingIsEnforcedBySchema(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	cluster := newCluster(t, conn, "east")

	tooLong := strings.Repeat("x", 512*1024+1)
	_, err := conn.Exec(`
		INSERT INTO runs (id, created_by, created_via, spec, prompt, prompt_sha256,
		                  agent, model, timeout_seconds, cluster_id)
		VALUES ($1, 'test', 'api', '{}'::jsonb, $2::text, sha256($3::bytea),
		        'claude-code', 'anthropic/claude-opus-5', 3600, $4)`,
		newULID(t), tooLong, []byte(tooLong), cluster)
	if err == nil {
		t.Fatal("a prompt over 512 KiB was stored; the per-run Secret would exceed 1 MiB")
	}
}

// An audit record that can be edited in place is not an audit record. Deletion
// stays open: retention is a real operation whose boundary the chart sets.
func TestAuditLogIsAppendOnly(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	id := newULID(t)
	mustExec(t, conn, `
		INSERT INTO audit_log (id, actor, actor_kind, action, subject_kind, subject_id)
		VALUES ($1, 'system', 'system', 'run.terminal.conflict', 'run', $2)`, id, newULID(t))

	if _, err := conn.Exec(`UPDATE audit_log SET action = 'nothing.happened' WHERE id = $1`, id); err == nil {
		t.Error("an audit record was edited")
	}
	if _, err := conn.Exec(`DELETE FROM audit_log WHERE id = $1`, id); err != nil {
		t.Errorf("retention cannot delete an audit record: %v", err)
	}
}

// The same Idempotency-Key with a different body is a client bug, and handing
// back the first run's id would answer with work that was never requested.
func TestIdempotencyKeyIsScopedAndBodyChecked(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	fx := seed(t, conn)

	insert := `INSERT INTO idempotency_keys (scope, key, request_sha256, run_id, expires_at)
	           VALUES ($1, 'k1', sha256($2::bytea), $3, now() + interval '24 hours')`

	mustExec(t, conn, insert, "runs.create", []byte("body-a"), fx.runID)
	// The same key under a different scope is a different request.
	mustExec(t, conn, insert, "runs.retry", []byte("body-a"), fx.runID)

	if _, err := conn.Exec(insert, "runs.create", []byte("body-b"), fx.runID); err == nil {
		t.Error("the same key was stored twice within a scope")
	}

	var same bool
	mustQuery(t, conn, `
		SELECT request_sha256 = sha256($1::bytea)
		FROM idempotency_keys WHERE scope = 'runs.create' AND key = 'k1'`,
		[]byte("body-b")).Scan(&same)
	if same {
		t.Error("a different body hashed to the stored digest")
	}
}

// ---------------------------------------------------------------------------

func mustExec(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", firstLine(query), err)
	}
}

func mustQuery(t *testing.T, conn *sql.DB, query string, args ...any) *sql.Row {
	t.Helper()
	return conn.QueryRow(query, args...)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "..."
	}
	return s
}
