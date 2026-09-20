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
	"github.com/automagicops/haliphron/db"
)

// Leasing, acknowledging and the two expiry scanners.
//
// Every statement in this file that matters is db.Query(...) — the text that
// ships beside the schema. The lease in particular: FOR UPDATE SKIP LOCKED is
// not an optimisation here but the reason two concurrent polls from one
// cluster hand out disjoint sets, and the ORDER BY is the one that matches the
// partial index. Retyping either of them in Go would produce a queue that
// works until two controllers ask at the same moment.

// Leased is one unit of work the queue handed out. Spec travels as raw JSON:
// it was rendered once at admission and is handed out byte-for-byte, so
// decoding and re-encoding it here would be a chance to change it.
type Leased struct {
	RunID    runv1.ULID
	Epoch    int64
	Attempt  int32
	Priority int32
	Spec     json.RawMessage
	// Prompt and PromptSHA256 travel with the work. The digest is also inside
	// Spec; it is scanned out separately so the lease handler can check the two
	// against each other without decoding the spec it is about to hand on
	// byte-for-byte.
	Prompt         string
	PromptSHA256   []byte
	AckDeadline    time.Time
	LeaseDeadline  time.Time
	TimeoutSeconds int32
	// CompletedPhases is what a previous attempt of this run got through under
	// the current epoch. Normally empty; not empty when the work is being
	// re-issued after its controller lost the CRs that held the checkpoint, and
	// in that case it is what stops the replacement paying for the model again.
	CompletedPhases []runv1.RuntimePhase
}

// EpochError is a message whose epoch is not the current one. Current and
// Status travel with it so the controller can tell "my work was reassigned"
// from "I am desynchronised" — the same rejection, different bugs.
type EpochError struct {
	Got     int64
	Current int64
	Status  string
}

func (e *EpochError) Error() string {
	return fmt.Sprintf("store: epoch %d is not the current %d", e.Got, e.Current)
}

// Stale reports whether the message is behind. Ahead is a different failure:
// the backend never issued that epoch, so no retry can help.
func (e *EpochError) Stale() bool { return e.Got < e.Current }

// LiveAttemptError is run_attempts_one_live refusing a second open attempt.
//
// It means somebody else's attempt for this run has not finished — a zombie
// controller, which the epoch answers, or a duplicate reconcile, which is
// answered by doing nothing. Either way it is not a condition the caller
// repairs by retrying the same statement, so it is a named error rather than a
// constraint violation surfacing as "store: open attempt ledger".
type LiveAttemptError struct {
	RunID runv1.ULID
	Epoch int64
}

func (e *LiveAttemptError) Error() string {
	return fmt.Sprintf("store: run %s already has a live attempt under epoch %d", e.RunID, e.Epoch)
}

// OwnershipError is a cluster acting on a run that belongs to another one.
type OwnershipError struct {
	Holder  runv1.ULID
	Caller  runv1.ULID
	Current int64
}

func (e *OwnershipError) Error() string {
	return fmt.Sprintf("store: run is held by %s, not %s", e.Holder, e.Caller)
}

// Lease hands work to a cluster.
//
// runtimes must not be empty: the statement filters with agent = ANY($2), so
// an empty array hands out nothing. The caller resolves "no restriction" into
// the full set, because what the full set is belongs to the contract and not
// to a SQL default.
func (s *Store) Lease(ctx context.Context, clusterID runv1.ULID, runtimes []runv1.AgentType,
	limit int, ackTimeout, leaseTTL time.Duration) ([]Leased, error) {

	if limit <= 0 || len(runtimes) == 0 {
		return nil, nil
	}

	var out []Leased
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Query("lease"),
			clusterID, agentTypeArray(runtimes), limit,
			ackTimeout.Seconds(), leaseTTL.Seconds())
		if err != nil {
			return fmt.Errorf("store: lease for %s: %w", clusterID, err)
		}
		defer rows.Close()

		for rows.Next() {
			var l Leased
			if err := rows.Scan(&l.RunID, &l.Epoch, &l.Attempt, &l.Priority, &l.Spec,
				&l.Prompt, &l.PromptSHA256,
				&l.AckDeadline, &l.LeaseDeadline, &l.TimeoutSeconds); err != nil {
				return fmt.Errorf("store: scan lease: %w", err)
			}
			out = append(out, l)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: lease rows: %w", err)
		}

		// The attempt ledger is opened at issuance rather than at the first
		// report, because its started_at is what the usage audit compares the
		// pod's self-declared duration against. A row created when the report
		// arrives would make that comparison trivially true.
		//
		// It is also where the duplicate guard bites first: run_attempts_one_live
		// refuses a second open row for the same run, so a lease handed out
		// while somebody's attempt is still running cannot open a second
		// ledger. That cannot happen through this statement — a run in Queued
		// has had its epoch raised, and raising it closed the old attempt — and
		// the constraint is what makes "cannot happen" true across two backends
		// rather than within one.
		for i := range out {
			l := &out[i]
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO run_attempts (run_id, lease_epoch, attempt, cluster_id, started_at)
				VALUES ($1, $2, $3, $4, now())
				ON CONFLICT (run_id, lease_epoch, attempt) DO NOTHING`,
				l.RunID, l.Epoch, l.Attempt, clusterID); err != nil {
				if isUniqueViolation(err, "run_attempts_one_live") {
					return &LiveAttemptError{RunID: l.RunID, Epoch: l.Epoch}
				}
				return fmt.Errorf("store: open attempt ledger for %s: %w", l.RunID, err)
			}

			// The checkpoint an earlier owner accumulated under this epoch. It
			// is read back rather than carried in memory because the point of
			// the column is to survive the controller that wrote it.
			phases, err := completedPhases(ctx, tx, l.RunID, l.Epoch)
			if err != nil {
				return err
			}
			l.CompletedPhases = phases
		}
		return nil
	})
	return out, err
}

// AckOutcome is the state after an acknowledgement.
type AckOutcome struct {
	Status        string
	Epoch         int64
	LeaseDeadline time.Time
	// Repeat means the run was already acknowledged under this epoch. The
	// controller retries this call after a restart, and a second ack must not
	// look like a second dispatch.
	Repeat bool
}

// AckLease records that a lease became durable in the cluster: the Secret, the
// ConfigMap and the AgentRun exist, and the work now survives a controller
// restart.
func (s *Store) AckLease(ctx context.Context, runID, clusterID runv1.ULID, epoch int64,
	leaseTTL time.Duration) (AckOutcome, error) {

	var out AckOutcome
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		state, err := lockRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := checkOwnership(state, clusterID, epoch); err != nil {
			return err
		}

		out.Epoch = state.Epoch
		if state.Status != clusterv1.StatusLeased {
			// Already acknowledged, or moved on. Either way the transition has
			// happened and repeating it would reset a deadline the cluster has
			// been extending by heartbeat.
			out.Repeat = true
			out.Status = state.Status
			if state.LeaseDeadline != nil {
				out.LeaseDeadline = *state.LeaseDeadline
			}
			return nil
		}

		err = tx.QueryRowContext(ctx, `
			UPDATE runs SET
				status         = 'Dispatched',
				ack_deadline   = NULL,
				lease_deadline = now() + make_interval(secs => $2),
				dispatched_at  = now(),
				status_reason  = NULL,
				status_message = NULL
			WHERE id = $1
			RETURNING lease_deadline`, runID, leaseTTL.Seconds()).Scan(&out.LeaseDeadline)
		if err != nil {
			return fmt.Errorf("store: ack %s: %w", runID, err)
		}
		out.Status = clusterv1.StatusDispatched
		return nil
	})
	return out, err
}

// Rejection is a controller saying it could not materialise a lease, before
// anything started and before anything was spent.
type Rejection struct {
	Code    clusterv1.RejectionCode
	Message string
	Fields  []string
}

// RejectOutcome is what became of the run.
type RejectOutcome struct {
	Status string
	Epoch  int64
	// Reassigned is the cluster the work went to instead, if there was one.
	Reassigned runv1.ULID
}

// RejectLease excludes the cluster and finds the run another one.
//
// Without a negative ack the only way for a controller to say "I cannot run
// this" is to burn the run: create the Job, let it fail, report Failed — and
// every cause of a rejection is detectable before the Job exists. The
// exclusion is kept rather than just the fact of it, because when this was the
// last eligible cluster the message is what a user reads in the UI.
func (s *Store) RejectLease(ctx context.Context, runID, clusterID runv1.ULID, epoch int64,
	rejection Rejection) (RejectOutcome, error) {

	var out RejectOutcome
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		state, err := lockRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := checkOwnership(state, clusterID, epoch); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_cluster_exclusions (run_id, cluster_id, lease_epoch, code, message, fields)
			VALUES ($1, $2, $3, $4, $5, $6::text[])
			ON CONFLICT (run_id, cluster_id) DO UPDATE
			SET lease_epoch = EXCLUDED.lease_epoch, code = EXCLUDED.code,
			    message = EXCLUDED.message, fields = EXCLUDED.fields, at = now()`,
			runID, clusterID, state.Epoch, string(rejection.Code),
			nullString(rejection.Message), textArray(rejection.Fields)); err != nil {
			return fmt.Errorf("store: exclude %s from %s: %w", clusterID, runID, err)
		}

		agent, err := runAgent(ctx, tx, runID)
		if err != nil {
			return err
		}
		next, err := pickCluster(ctx, tx, runID, agent)
		if err != nil {
			return err
		}

		// The epoch rises here because ownership is being revoked, which is
		// what closes the fence: from this moment the refusing controller's
		// messages compare as stale rather than as equal.
		if next == "" {
			out.Status = clusterv1.StatusFailed
			out.Epoch = state.Epoch + 1
			detail := string(rejection.Code)
			if rejection.Message != "" {
				detail += ": " + rejection.Message
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE runs SET
					status         = 'Failed',
					lease_epoch    = lease_epoch + 1,
					attempt        = 1,
					ack_deadline   = NULL,
					lease_deadline = NULL,
					failure_class  = 'config',
					status_reason  = 'NoEligibleCluster',
					status_message = $2,
					finished_at    = now()
				WHERE id = $1`, runID, detail); err != nil {
				return fmt.Errorf("store: fail %s after rejection: %w", runID, err)
			}
			return appendAudit(ctx, tx, AuditEntry{
				Actor: string(clusterID), ActorKind: "cluster",
				Action: AuditNoClusterForRun, SubjectKind: "run", SubjectID: string(runID),
				RunID: runID, ClusterID: clusterID,
				Payload: map[string]any{"code": rejection.Code, "message": rejection.Message},
			})
		}

		out.Status = clusterv1.StatusQueued
		out.Epoch = state.Epoch + 1
		out.Reassigned = next
		if _, err := tx.ExecContext(ctx, `
			UPDATE runs SET
				status         = 'Queued',
				cluster_id     = $2,
				lease_epoch    = lease_epoch + 1,
				attempt        = 1,
				ack_deadline   = NULL,
				lease_deadline = NULL,
				queued_at      = now(),
				status_reason  = 'MaterializationRefused',
				status_message = $3
			WHERE id = $1`, runID, next, nullString(rejection.Message)); err != nil {
			return fmt.Errorf("store: requeue %s after rejection: %w", runID, err)
		}
		return nil
	})
	return out, err
}

// ExpiredAck is a lease that was handed out and never acknowledged.
type ExpiredAck struct {
	RunID     runv1.ULID
	ClusterID runv1.ULID
	Epoch     int64
}

// ExpireAcks returns work to the queue. Before the ack the work is guaranteed
// not to have started — that is the whole reason this deadline is separate and
// short — so it can be reassigned immediately and safely.
func (s *Store) ExpireAcks(ctx context.Context) ([]ExpiredAck, error) {
	rows, err := s.db.QueryContext(ctx, db.Query("expire_ack"))
	if err != nil {
		return nil, fmt.Errorf("store: expire acks: %w", err)
	}
	defer rows.Close()

	var out []ExpiredAck
	for rows.Next() {
		var (
			e       ExpiredAck
			cluster sql.NullString
		)
		if err := rows.Scan(&e.RunID, &cluster, &e.Epoch); err != nil {
			return nil, fmt.Errorf("store: scan expired ack: %w", err)
		}
		e.ClusterID = runv1.ULID(cluster.String)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExpiredLease is a cluster that stopped reporting on work it had started.
type ExpiredLease struct {
	RunID     runv1.ULID
	ClusterID runv1.ULID
	Epoch     int64
	Attempt   int32
	Phase     runv1.Phase
}

// ExpireLeases marks runs Unknown, never Queued.
//
// After the ack a Job may be executing this second, and handing the same run to
// a second cluster would run the agent twice, pay for the model twice and push
// two branches. The epoch is deliberately not raised: the controller that lost
// the network plays the work out and reports under the epoch it holds, and the
// run recovers by itself.
func (s *Store) ExpireLeases(ctx context.Context) ([]ExpiredLease, error) {
	rows, err := s.db.QueryContext(ctx, db.Query("expire_lease"))
	if err != nil {
		return nil, fmt.Errorf("store: expire leases: %w", err)
	}
	defer rows.Close()

	var out []ExpiredLease
	for rows.Next() {
		var (
			e       ExpiredLease
			cluster sql.NullString
			phase   sql.NullString
		)
		if err := rows.Scan(&e.RunID, &cluster, &e.Epoch, &e.Attempt, &phase); err != nil {
			return nil, fmt.Errorf("store: scan expired lease: %w", err)
		}
		e.ClusterID = runv1.ULID(cluster.String)
		e.Phase = runv1.Phase(phase.String)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExtendLease pushes a run's lease deadline out by the TTL. Called for every
// observation a heartbeat carries and accepts.
func (s *Store) ExtendLease(ctx context.Context, runID runv1.ULID, ttl time.Duration) (time.Time, error) {
	var deadline time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE runs SET lease_deadline = now() + make_interval(secs => $2)
		WHERE id = $1
		RETURNING lease_deadline`, runID, ttl.Seconds()).Scan(&deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: extend lease %s: %w", runID, err)
	}
	return deadline, nil
}

// checkOwnership is the pair of checks every run-scoped call makes before it
// writes anything: the epoch is current, and the caller is the holder.
func checkOwnership(state lockedRun, caller runv1.ULID, epoch int64) error {
	if epoch != state.Epoch {
		return &EpochError{Got: epoch, Current: state.Epoch, Status: state.Status}
	}
	if state.ClusterID != "" && state.ClusterID != caller {
		return &OwnershipError{Holder: state.ClusterID, Caller: caller, Current: state.Epoch}
	}
	return nil
}
