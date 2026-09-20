package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/db"
)

// Applying a report: the single path behind /ingest/status, the runs[] of a
// heartbeat and /ingest/completion.
//
// All three start at lock_run.sql and then evaluate run.Apply in Go. The
// database contributes the row lock that makes read-decide-write atomic and
// the guard trigger that refuses what the rule should never produce; the rule
// itself stays in Go, because it is domain logic and expressing it as SQL
// would make every clarification of it a migration.

// lockedRun is the row lock_run.sql returns.
type lockedRun struct {
	RunID                runv1.ULID
	ClusterID            runv1.ULID
	Epoch                int64
	Attempt              int32
	Status               string
	Phase                runv1.Phase
	Rank                 int
	FailureClass         runv1.FailureClass
	CancelRequestedAt    *time.Time
	CompletionReceivedAt *time.Time
	AckDeadline          *time.Time
	LeaseDeadline        *time.Time
}

func (l lockedRun) state() run.State {
	return run.State{
		RunID: l.RunID, ClusterID: l.ClusterID, Epoch: l.Epoch, Attempt: l.Attempt,
		Status: l.Status, Phase: l.Phase, Rank: l.Rank, FailureClass: l.FailureClass,
		CancelRequestedAt: l.CancelRequestedAt, CompletionReceivedAt: l.CompletionReceivedAt,
		AckDeadline: l.AckDeadline, LeaseDeadline: l.LeaseDeadline,
	}
}

func lockRun(ctx context.Context, tx *sql.Tx, id runv1.ULID) (lockedRun, error) {
	var (
		l          lockedRun
		cluster    sql.NullString
		phase      sql.NullString
		cancelled  sql.NullTime
		completion sql.NullTime
		ackAt      sql.NullTime
		leaseAt    sql.NullTime
		class      string
	)
	err := tx.QueryRowContext(ctx, db.Query("lock_run"), id).Scan(
		&l.RunID, &cluster, &l.Epoch, &l.Attempt, &l.Status, &phase, &l.Rank,
		&class, &cancelled, &completion, &ackAt, &leaseAt)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedRun{}, ErrNotFound
	}
	if err != nil {
		return lockedRun{}, fmt.Errorf("store: lock run %s: %w", id, err)
	}

	l.ClusterID = runv1.ULID(cluster.String)
	l.Phase = runv1.Phase(phase.String)
	l.FailureClass = runv1.FailureClass(class)
	l.CancelRequestedAt = timePtr(cancelled)
	l.CompletionReceivedAt = timePtr(completion)
	l.AckDeadline = timePtr(ackAt)
	l.LeaseDeadline = timePtr(leaseAt)
	return l, nil
}

// Applied is the result of one observation, in the terms the controller is
// answered in.
type Applied struct {
	Decision run.Decision
	// Status is what the run is in after the write. It is the value that goes
	// into StatusIngestResult.appliedStatus, so it carries
	// CompletedWithoutResult where the column cannot.
	Status string
	Epoch  int64
	// LeaseDeadline is set when the observation renewed the lease, which is
	// what a heartbeat turns into a LeaseRenewal.
	LeaseDeadline time.Time
	// Recovered means a completion report for this attempt was already in the
	// ledger when the terminal status arrived, and its contents were promoted.
	Recovered bool
}

// ApplyObservation evaluates one report and writes what the decision says.
//
// leaseTTL renews the lease for an accepted observation of a live run. It is
// passed in rather than read from a table because the timings are the control
// plane's and are handed to the cluster at registration; a value read from the
// row would be a second copy of them.
func (s *Store) ApplyObservation(ctx context.Context, clusterID runv1.ULID,
	obs clusterv1.RunObservation, leaseTTL time.Duration) (Applied, error) {

	var out Applied
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := lockRun(ctx, tx, obs.RunID)
		if err != nil {
			return err
		}
		out.Epoch = locked.Epoch
		out.Status = locked.Status

		decision := run.Apply(locked.state(), obs, clusterID)
		out.Decision = decision

		if decision.Audit {
			if err := appendAudit(ctx, tx, AuditEntry{
				Actor: string(clusterID), ActorKind: "cluster",
				Action: AuditTerminalConflict, SubjectKind: "run", SubjectID: string(obs.RunID),
				RunID: obs.RunID, ClusterID: clusterID,
				Payload: map[string]any{
					"accepted": string(locked.Phase), "rejected": string(obs.Phase),
					"attempt": obs.Attempt, "epoch": obs.Epoch,
				},
			}); err != nil {
				return err
			}
		}
		if decision.Verdict == run.Reject {
			return nil
		}

		if decision.Verdict == run.Repeat {
			// One write, and only when it changes something: a run marked
			// Unknown while its cluster was unreachable comes back to the
			// status its phase implies. The rank does not move, which is what
			// makes this an idempotent repeat rather than a state change.
			status := run.StoredStatus(obs.Phase)
			if locked.Status == clusterv1.StatusUnknown && status != "" {
				if _, err := tx.ExecContext(ctx, `
					UPDATE runs SET status = $2, status_reason = NULL, status_message = NULL
					WHERE id = $1`, obs.RunID, status); err != nil {
					return fmt.Errorf("store: clear Unknown on %s: %w", obs.RunID, err)
				}
				out.Status = status
			}
		} else {
			if err := writeObservation(ctx, tx, locked, obs, clusterID, decision); err != nil {
				return err
			}
			out.Status = run.StoredStatus(obs.Phase)
		}

		if err := upsertAttempt(ctx, tx, obs, clusterID, locked.Epoch); err != nil {
			return err
		}

		if obs.Phase.IsTerminal() {
			// The report may have arrived before the status did: the two are
			// independent channels and the backend is obliged to work under
			// any ordering of them.
			recovered, err := promoteCompletion(ctx, tx, obs.RunID, locked.Epoch, obs.Attempt)
			if err != nil {
				return err
			}
			out.Recovered = recovered
			out.Status = run.StatusFor(obs.Phase, recovered)
			return nil
		}

		deadline, err := extendLeaseTx(ctx, tx, obs.RunID, leaseTTL)
		if err != nil {
			return err
		}
		out.LeaseDeadline = deadline
		return nil
	})
	return out, err
}

// writeObservation applies an advancing report in full.
func writeObservation(ctx context.Context, tx *sql.Tx, locked lockedRun,
	obs clusterv1.RunObservation, clusterID runv1.ULID, decision run.Decision) error {

	// A new attempt resets the rank by resetting the phase, and the guard
	// trigger permits leaving a terminal status only because the attempt grew.
	attempt := locked.Attempt
	if decision.NewAttempt {
		attempt = obs.Attempt
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE runs SET
			attempt        = $2,
			cluster_id     = COALESCE(cluster_id, $3),
			observed_phase = $4,
			status         = $5,
			status_reason  = COALESCE($6, status_reason),
			status_message = COALESCE($7, status_message),
			failure_class  = COALESCE($8, failure_class),
			exit_code      = COALESCE($9, exit_code),
			started_at     = CASE WHEN $10 THEN COALESCE(started_at, now()) ELSE started_at END,
			finished_at    = CASE WHEN $11 THEN now() ELSE NULL END
		WHERE id = $1`,
		obs.RunID, attempt, clusterID, string(obs.Phase), run.StoredStatus(obs.Phase),
		nullString(obs.Reason), nullString(truncate(obs.Message, 1024)),
		nullString(string(obs.FailureClass)), nullInt32(obs.ExitCode),
		obs.Phase.Rank() >= runv1.PhaseRunning.Rank(), obs.Phase.IsTerminal())
	if err != nil {
		if isGuardViolation(err) {
			// The guard refused a write the decision table said was legal,
			// which means the two disagree. That is a defect in this process,
			// and it is worth saying so rather than reporting a storage error.
			return fmt.Errorf("store: guard refused an accepted observation for %s: %w", obs.RunID, err)
		}
		return fmt.Errorf("store: write observation for %s: %w", obs.RunID, err)
	}
	return nil
}

// upsertAttempt records what the controller saw, on the row keyed by the
// ownership it happened under.
func upsertAttempt(ctx context.Context, tx *sql.Tx, obs clusterv1.RunObservation,
	clusterID runv1.ULID, epoch int64) error {

	// A terminal phase closes the row whether or not the controller sent a
	// finishedAt. It is the unique index that makes this matter: an attempt
	// left open because one optional timestamp was absent is a run nobody can
	// ever attempt again, and the failure would surface as a lease the backend
	// refuses to open a ledger for, minutes and one reassignment later.
	finished := obs.FinishedAt
	if finished == nil && obs.Phase.IsTerminal() {
		now := time.Now()
		finished = &now
	}

	// A higher attempt means the previous one is over, and the controller is the
	// only thing that raises the number — it does so only after an attempt
	// ended. Closing the earlier row here is the same rule the epoch trigger
	// applies one level up, and it is needed for the same reason: a local infra
	// retry never reports a terminal phase for the attempt it is replacing
	// (there is nothing to say about it that the next attempt will not say
	// better), so without this the row stays open and run_attempts_one_live
	// refuses the retry — turning a recoverable OOM into a run nobody can
	// attempt again.
	//
	// Strictly lower, because reports arrive reordered: a late observation of
	// attempt 1 must not close attempt 2.
	if err := closeSupersededAttempts(ctx, tx, obs.RunID, epoch, obs.Attempt); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO run_attempts (run_id, lease_epoch, attempt, cluster_id, phase,
		                          reason, message, job_name, pod_name, node_name,
		                          exit_code, failure_class, completed_phases,
		                          started_at, finished_at, cluster_observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		        COALESCE($12, 'none'), $13::text[]::runtime_phase[], $14, $15, $16)
		ON CONFLICT (run_id, lease_epoch, attempt) DO UPDATE SET
			phase               = COALESCE(EXCLUDED.phase, run_attempts.phase),
			reason              = COALESCE(EXCLUDED.reason, run_attempts.reason),
			message             = COALESCE(EXCLUDED.message, run_attempts.message),
			job_name            = COALESCE(EXCLUDED.job_name, run_attempts.job_name),
			pod_name            = COALESCE(EXCLUDED.pod_name, run_attempts.pod_name),
			node_name           = COALESCE(EXCLUDED.node_name, run_attempts.node_name),
			exit_code           = COALESCE(EXCLUDED.exit_code, run_attempts.exit_code),
			failure_class       = CASE WHEN EXCLUDED.failure_class = 'none'
			                           THEN run_attempts.failure_class ELSE EXCLUDED.failure_class END,
			-- Unioned, never replaced. Reports arrive reordered, and a
			-- heartbeat carrying an earlier snapshot must not shorten a list
			-- the low-latency path already grew: that would hand the next
			-- attempt a checkpoint saying the model had not run when it had.
			-- The order DISTINCT leaves behind does not matter; the reader
			-- sorts into the contract's execution order.
			completed_phases    = ARRAY(SELECT DISTINCT unnest(
			                        run_attempts.completed_phases || EXCLUDED.completed_phases)),
			started_at          = COALESCE(run_attempts.started_at, EXCLUDED.started_at),
			finished_at         = COALESCE(EXCLUDED.finished_at, run_attempts.finished_at),
			cluster_observed_at = COALESCE(EXCLUDED.cluster_observed_at, run_attempts.cluster_observed_at)`,
		obs.RunID, epoch, obs.Attempt, clusterID, nullString(string(obs.Phase)),
		nullString(obs.Reason), nullString(truncate(obs.Message, 1024)),
		nullString(obs.JobName), nullString(obs.PodName), nullString(obs.NodeName),
		nullInt32(obs.ExitCode), nullString(string(obs.FailureClass)),
		runtimePhaseArray(obs.CompletedPhases),
		nullTime(obs.StartedAt), nullTime(finished), nullTime(obs.ObservedAt))
	if err != nil {
		if isUniqueViolation(err, "run_attempts_one_live") {
			// Another attempt of this run is still open. Within one controller
			// that is a duplicate reconcile and doing nothing is right; across
			// two it is the zombie the epoch exists to fence, and the caller
			// turns this into an abandon rather than a retry.
			return &LiveAttemptError{RunID: obs.RunID, Epoch: epoch}
		}
		return fmt.Errorf("store: record attempt %d of %s: %w", obs.Attempt, obs.RunID, err)
	}
	return nil
}

// closeSupersededAttempts finishes the open attempts a newer one replaced.
//
// The accounting record survives — that is the point of keeping the epoch and
// the attempt in the key — and only the claim is released. The class is infra
// and the reason names what happened, so that "why does attempt 1 say Failed
// when the run succeeded on attempt 2" has an answer in the row itself.
func closeSupersededAttempts(ctx context.Context, tx *sql.Tx, id runv1.ULID,
	epoch int64, attempt int32) error {

	if attempt <= 1 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE run_attempts
		SET finished_at   = now(),
		    failure_class = CASE WHEN failure_class = 'none' THEN 'infra'
		                         ELSE failure_class END,
		    reason        = COALESCE(reason, 'SupersededByRetry'),
		    message       = COALESCE(message, format('attempt %s superseded it', $3::int))
		WHERE run_id = $1 AND lease_epoch = $2 AND attempt < $3
		  AND finished_at IS NULL`,
		id, epoch, attempt)
	if err != nil {
		return fmt.Errorf("store: close the attempts superseded by %d of %s: %w", attempt, id, err)
	}
	return nil
}

// completedPhases reads the checkpoint accumulated under one epoch, across
// every attempt of it.
//
// Across attempts, because that is what makes a retry cheap: attempt 2 skips
// the model precisely because attempt 1 got through the run phase. Within one
// epoch, because a new epoch is new ownership and inherits nothing — the
// attempt counter resets with it, and so does what anyone is entitled to
// assume was already done.
func completedPhases(ctx context.Context, tx *sql.Tx, id runv1.ULID, epoch int64) ([]runv1.RuntimePhase, error) {
	var raw []string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(array_agg(DISTINCT p::text), '{}')
		FROM run_attempts a, LATERAL unnest(a.completed_phases) AS p
		WHERE a.run_id = $1 AND a.lease_epoch = $2`,
		id, epoch).Scan(pgArray(&raw))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read the checkpoint for %s: %w", id, err)
	}
	return runtimePhases(raw), nil
}

// Completion is one report from a pod, with the envelope the controller put
// around it.
type Completion struct {
	RunID      runv1.ULID
	ClusterID  runv1.ULID
	Epoch      int64
	Attempt    int32
	ReceivedAt *time.Time
	Report     runv1.CompletionReport
	Raw        json.RawMessage
}

// CompletionOutcome is what happened to it.
type CompletionOutcome struct {
	// Duplicate means a report for this (run, epoch, attempt) was already
	// applied and this one changed nothing. The cost is charged once: this
	// call is retried on any network error, and a sum that grows per retry is
	// a bill that grows per retry.
	Duplicate bool
	Status    string
	Epoch     int64

	// DeclaredMs and ObservedMs are the pod's self-declared duration and the
	// window the controller's lease actually covered. They are returned so the
	// caller can audit the gap: cost comes from the least trusted component in
	// the system, and the phase 1 mitigation is a record rather than a block.
	DeclaredMs int64
	ObservedMs int64
}

// ApplyCompletion charges the run once and resolves it to its terminal status.
func (s *Store) ApplyCompletion(ctx context.Context, c Completion) (CompletionOutcome, error) {
	var out CompletionOutcome
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := lockRun(ctx, tx, c.RunID)
		if err != nil {
			return err
		}
		if c.Epoch != locked.Epoch {
			return &EpochError{Got: c.Epoch, Current: locked.Epoch, Status: locked.Status}
		}
		out.Epoch = locked.Epoch
		out.Status = locked.Status

		var (
			already sql.NullTime
			started sql.NullTime
		)
		err = tx.QueryRowContext(ctx, `
			SELECT completion_received_at, started_at FROM run_attempts
			WHERE run_id = $1 AND lease_epoch = $2 AND attempt = $3`,
			c.RunID, locked.Epoch, c.Attempt).Scan(&already, &started)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The ledger row is opened at issuance, so its absence means the
			// controller started an attempt the backend never saw a report
			// for. The row is created here rather than the report refused:
			// losing the cost of work that ran is the worse outcome.
		case err != nil:
			return fmt.Errorf("store: read attempt ledger for %s: %w", c.RunID, err)
		case already.Valid:
			out.Duplicate = true
			out.Status = run.StatusFor(locked.Phase, true)
			return nil
		}

		usage := c.Report.Usage
		if usage == nil {
			usage = &runv1.Usage{}
		}
		if started.Valid {
			out.ObservedMs = time.Since(started.Time).Milliseconds()
		}
		out.DeclaredMs = usage.DurationMs

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_attempts (run_id, lease_epoch, attempt, cluster_id,
			                          exit_code, failure_class, completion,
			                          completion_received_at, cost_usd,
			                          input_tokens, output_tokens,
			                          cache_read_tokens, cache_write_tokens,
			                          declared_duration_ms, observed_duration_ms,
			                          completed_phases, finished_at)
			VALUES ($1, $2, $3, $4, $5, COALESCE($6, 'none'), $7::jsonb, now(),
			        COALESCE($8::numeric, 0), $9, $10, $11, $12, $13, $14,
			        $15::text[]::runtime_phase[], now())
			ON CONFLICT (run_id, lease_epoch, attempt) DO UPDATE SET
				completed_phases      = ARRAY(SELECT DISTINCT unnest(
				                          run_attempts.completed_phases || EXCLUDED.completed_phases)),
				exit_code             = EXCLUDED.exit_code,
				failure_class         = CASE WHEN EXCLUDED.failure_class = 'none'
				                             THEN run_attempts.failure_class ELSE EXCLUDED.failure_class END,
				completion            = EXCLUDED.completion,
				completion_received_at= EXCLUDED.completion_received_at,
				cost_usd              = EXCLUDED.cost_usd,
				input_tokens          = EXCLUDED.input_tokens,
				output_tokens         = EXCLUDED.output_tokens,
				cache_read_tokens     = EXCLUDED.cache_read_tokens,
				cache_write_tokens    = EXCLUDED.cache_write_tokens,
				declared_duration_ms  = EXCLUDED.declared_duration_ms,
				observed_duration_ms  = COALESCE(run_attempts.observed_duration_ms, EXCLUDED.observed_duration_ms),
				finished_at           = COALESCE(run_attempts.finished_at, EXCLUDED.finished_at)`,
			c.RunID, locked.Epoch, c.Attempt, clusterOrHolder(c.ClusterID, locked.ClusterID),
			nullInt32(&c.Report.ExitCode), nullString(string(c.Report.FailureClass)),
			[]byte(c.Raw), nullString(string(usage.TotalCostUSD)),
			usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens,
			nullInt64(usage.DurationMs), nullInt64(out.ObservedMs),
			runtimePhaseArray(c.Report.CompletedPhases)); err != nil {
			return fmt.Errorf("store: record completion for %s: %w", c.RunID, err)
		}

		if err := promoteAttempt(ctx, tx, c.RunID, locked.Epoch, c.Attempt, c.Report); err != nil {
			return err
		}

		out.Status = run.StatusFor(locked.Phase, true)
		if !locked.Phase.IsTerminal() {
			// The result arrived before the status. Nothing to resolve yet:
			// the terminal observation is still owed, and until it comes the
			// run is what the controller last said it was.
			out.Status = locked.Status
		}
		return nil
	})
	return out, err
}

// promoteCompletion lifts an already-recorded report onto the run when the
// terminal status arrives after it. It reports whether there was one.
func promoteCompletion(ctx context.Context, tx *sql.Tx, id runv1.ULID, epoch int64, attempt int32) (bool, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `
		SELECT completion FROM run_attempts
		WHERE run_id = $1 AND lease_epoch = $2 AND attempt = $3 AND completion IS NOT NULL`,
		id, epoch, attempt).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read recorded completion for %s: %w", id, err)
	}

	var report runv1.CompletionReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return false, fmt.Errorf("store: decode recorded completion for %s: %w", id, err)
	}
	if err := promoteAttempt(ctx, tx, id, epoch, attempt, report); err != nil {
		return false, err
	}
	return true, nil
}

// promoteAttempt copies the fields the UI and the run list read out of the
// attempt's report, and re-sums the cost.
//
// runs.cost_usd is a maintained sum over run_attempts rather than a truth of
// its own: the unit of accounting is the attempt, and "charged once" holds
// because the row exists at most once per (run, epoch, attempt).
func promoteAttempt(ctx context.Context, tx *sql.Tx, id runv1.ULID, epoch int64,
	attempt int32, report runv1.CompletionReport) error {

	repo := report.Repo
	if repo == nil {
		repo = &runv1.RepoResult{}
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE runs r SET
			result_summary         = COALESCE($2, r.result_summary),
			pr_url                 = COALESCE($3, r.pr_url),
			pr_number              = COALESCE($4, r.pr_number),
			pr_action              = COALESCE($5, r.pr_action),
			commit_sha             = COALESCE($6, r.commit_sha),
			pushed                 = COALESCE($7, r.pushed),
			exit_code              = COALESCE($8, r.exit_code),
			failure_class          = COALESCE($9, r.failure_class),
			status_reason          = COALESCE($10, r.status_reason),
			status_message         = COALESCE($11, r.status_message),
			completion_received_at = now(),
			cost_usd           = totals.cost,
			input_tokens       = totals.input_tokens,
			output_tokens      = totals.output_tokens,
			cache_read_tokens  = totals.cache_read,
			cache_write_tokens = totals.cache_write,
			num_turns          = $12
		FROM (
			SELECT COALESCE(SUM(cost_usd), 0)           AS cost,
			       COALESCE(SUM(input_tokens), 0)       AS input_tokens,
			       COALESCE(SUM(output_tokens), 0)      AS output_tokens,
			       COALESCE(SUM(cache_read_tokens), 0)  AS cache_read,
			       COALESCE(SUM(cache_write_tokens), 0) AS cache_write
			FROM run_attempts WHERE run_id = $1
		) AS totals
		WHERE r.id = $1`,
		id, nullString(truncate(report.Summary, clusterv1.MaxSummaryBytes)),
		nullString(repo.PRURL), nullInt32(nonZero(repo.PRNumber)),
		nullString(string(repo.PRAction)), nullString(repo.CommitSHA),
		nullBool(repo.Pushed), nullInt32(&report.ExitCode),
		nullString(string(report.FailureClass)),
		nullString(truncate(report.Reason, 256)), nullString(truncate(report.Message, 1024)),
		turns(report))
	if err != nil {
		return fmt.Errorf("store: promote attempt %d of %s: %w", attempt, id, err)
	}
	return nil
}

func extendLeaseTx(ctx context.Context, tx *sql.Tx, id runv1.ULID, ttl time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, nil
	}
	var deadline sql.NullTime
	err := tx.QueryRowContext(ctx, `
		UPDATE runs SET lease_deadline = now() + make_interval(secs => $2)
		WHERE id = $1
		RETURNING lease_deadline`, id, ttl.Seconds()).Scan(&deadline)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: renew lease %s: %w", id, err)
	}
	return deadline.Time, nil
}

func clusterOrHolder(caller, holder runv1.ULID) runv1.ULID {
	if caller != "" {
		return caller
	}
	return holder
}

func turns(report runv1.CompletionReport) int32 {
	if report.Usage == nil {
		return 0
	}
	return report.Usage.NumTurns
}

func nonZero(v int32) *int32 {
	if v == 0 {
		return nil
	}
	return &v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullBool(v bool) any {
	if !v {
		return nil
	}
	return v
}

// truncate bounds a value at the limit the column carries, on a rune boundary.
// The alternative is a write that fails a CHECK on a field nobody reads
// closely, losing a report whose cost and PR link are the parts that matter.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
