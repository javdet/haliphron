package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Deleting runs: one at a time by an operator, and in batches by retention.
//
// Only a run that has ended is deleted. A live run has a Job somewhere that
// keeps reporting against the row, and a Queued one is about to be handed out;
// the way to delete either is cancel first, then delete — the order tokens
// follow, for the same reason: the instruction to stop is recorded before the
// record goes.
//
// Everything that hangs off a run goes with it through ON DELETE CASCADE: its
// attempts, its exclusions, its per-run token and the idempotency key that
// admitted it. A child run is not deleted with its parent; its parent_run_id
// becomes NULL, which runs_guard permits because the parent link is not part
// of the admission group. The audit log has no foreign key and keeps the run's
// history — this is the retention on runs its schema comment anticipates.
//
// The objects under the run's prefix in the artifact store are not this
// package's to delete. The caller removes them once the row is gone, so that a
// failure there leaves orphaned bytes for the artifact reaper rather than a run
// whose result has vanished.

// ErrRunLive is a deletion asked of a run that has not ended.
var ErrRunLive = errors.New("store: run has not ended; cancel it before deleting it")

// terminalStatuses is the set a deletable run is in, spelled the way runs_guard
// spells it. CompletedWithoutResult is not among them because it is not a
// stored status: such a run is stored as the terminal status it observed.
const terminalStatuses = `('Succeeded', 'Failed', 'TimedOut', 'Cancelled')`

// DeleteRun removes one run that has ended and records the removal in the
// same transaction: once the row is gone, the audit log is the only place left
// that says the run existed and what it cost.
//
// The condition is in the DELETE itself rather than checked beforehand, as in
// DeleteToken. A concurrent retry that has already put the run back in the
// queue is seen by the statement, and the run is not deleted out from under
// the lease that is about to take it.
func (s *Store) DeleteRun(ctx context.Context, id runv1.ULID, actor string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var (
			status, createdBy string
			cost              runv1.MoneyUSD
			finished          sql.NullTime
		)
		err := tx.QueryRowContext(ctx, `
			DELETE FROM runs
			WHERE id = $1 AND status IN `+terminalStatuses+`
			RETURNING status, created_by, cost_usd, finished_at`, id).
			Scan(&status, &createdBy, &cost, &finished)
		if errors.Is(err, sql.ErrNoRows) {
			return whyRunNotDeleted(ctx, tx, id)
		}
		if err != nil {
			return fmt.Errorf("store: delete run %s: %w", id, err)
		}
		return appendAudit(ctx, tx, AuditEntry{
			Actor: actor, ActorKind: "user", Action: AuditRunDeleted,
			SubjectKind: "run", SubjectID: string(id), RunID: id,
			Payload: deletedRunPayload(status, createdBy, cost, finished, "operator"),
		})
	})
}

// whyRunNotDeleted tells a missing row from one the DELETE declined. Anything
// the DELETE declined had not ended when it looked.
func whyRunNotDeleted(ctx context.Context, tx *sql.Tx, id runv1.ULID) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("store: look up run %s: %w", id, err)
	default:
		return ErrRunLive
	}
}

// DeleteFinishedRuns removes up to limit runs that ended before cutoff, oldest
// first, and returns their identifiers so the caller can remove their objects.
//
// A run with no finished_at is never selected. Every path that makes a status
// terminal sets it, so this excludes nothing today, and if a future path forgets
// to, the run is kept rather than deleted — the direction a retention bug should
// fail in.
//
// SKIP LOCKED because the row lock is how the report and retry paths serialise
// on a run. A run somebody is acting on right now is not the one to delete, and
// the next pass will find it again if it is still eligible.
func (s *Store) DeleteFinishedRuns(ctx context.Context, cutoff time.Time, limit int) ([]runv1.ULID, error) {
	if limit <= 0 {
		limit = 500
	}
	type deleted struct {
		id                runv1.ULID
		status, createdBy string
		cost              runv1.MoneyUSD
		finished          sql.NullTime
	}

	var out []runv1.ULID
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			DELETE FROM runs
			WHERE id IN (
				SELECT id FROM runs
				WHERE status IN `+terminalStatuses+`
				  AND finished_at < $1
				ORDER BY finished_at
				LIMIT $2
				FOR UPDATE SKIP LOCKED)
			RETURNING id, status, created_by, cost_usd, finished_at`, cutoff, limit)
		if err != nil {
			return fmt.Errorf("store: delete finished runs: %w", err)
		}
		// Read out before auditing: one connection carries one statement at a
		// time, and the audit INSERTs go over the same transaction.
		var batch []deleted
		for rows.Next() {
			var d deleted
			if err := rows.Scan(&d.id, &d.status, &d.createdBy, &d.cost, &d.finished); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: scan deleted run: %w", err)
			}
			batch = append(batch, d)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: delete finished runs: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: delete finished runs: %w", err)
		}

		for _, d := range batch {
			if err := appendAudit(ctx, tx, AuditEntry{
				Actor: "retention", ActorKind: "system", Action: AuditRunDeleted,
				SubjectKind: "run", SubjectID: string(d.id), RunID: d.id,
				Payload: deletedRunPayload(d.status, d.createdBy, d.cost, d.finished, "retention"),
			}); err != nil {
				return err
			}
			out = append(out, d.id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// deletedRunPayload is what the audit log keeps of a run that is gone: enough
// to answer "who ran it, how did it end, and what did it cost" without the row.
func deletedRunPayload(status, createdBy string, cost runv1.MoneyUSD, finished sql.NullTime, reason string) map[string]any {
	payload := map[string]any{
		"status": status, "createdBy": createdBy, "costUsd": string(cost), "reason": reason,
	}
	if finished.Valid {
		payload["finishedAt"] = finished.Time.UTC().Format(time.RFC3339)
	}
	return payload
}
