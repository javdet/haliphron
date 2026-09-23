package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The audit log.
//
// Phase 1 fills it from four places, and they have one thing in common: each
// is a case where something was silently discarded and the silence is the
// problem. A second, different terminal phase for the same attempt. A report
// refused under a stale epoch. An ack that refused a lease when nobody else
// could take it. A self-declared duration that does not match the window the
// controller observed.
//
// The table is append-only through a trigger, and there are no foreign keys
// from it: the record of what happened to a run has to outlive the run, and a
// cascade would delete the evidence along with the subject.

// Audit actions written in phase 1.
const (
	AuditTerminalConflict = "run.terminal_conflict"
	AuditEpochRejected    = "run.epoch_rejected"
	AuditNoClusterForRun  = "run.no_eligible_cluster"
	AuditAckExhausted     = "run.ack_timeout_exhausted"
	AuditUsageDivergence  = "run.usage_divergence"

	AuditRunSubmitted = "run.submitted"
	AuditRunCancelled = "run.cancelled"
	AuditRunRetried   = "run.retried"
	AuditRunUnknown   = "run.marked_unknown"

	AuditClusterRegistered = "cluster.registered"
	AuditClusterRevoked    = "cluster.revoked"
	AuditClusterStale      = "cluster.unreachable"
)

// AuditEntry is one record. Actor and ActorKind are who did it — a token, a
// cluster, the system's own scanner — because "the run was marked Unknown" is
// a different fact from "an operator marked the run Unknown".
type AuditEntry struct {
	Actor       string
	ActorKind   string
	Action      string
	SubjectKind string
	SubjectID   string
	RunID       runv1.ULID
	ClusterID   runv1.ULID
	Payload     map[string]any
}

// Audit appends a record outside any transaction the caller holds.
func (s *Store) Audit(ctx context.Context, entry AuditEntry) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return appendAudit(ctx, tx, entry) })
}

func appendAudit(ctx context.Context, tx *sql.Tx, entry AuditEntry) error {
	payload := entry.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: encode audit payload: %w", err)
	}
	if entry.ActorKind == "" {
		entry.ActorKind = "system"
	}
	if entry.SubjectKind == "" {
		entry.SubjectKind = "run"
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (id, actor, actor_kind, action, subject_kind, subject_id,
		                       run_id, cluster_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`,
		newID(), entry.Actor, entry.ActorKind, entry.Action, entry.SubjectKind,
		nullString(entry.SubjectID), nullString(string(entry.RunID)),
		nullString(string(entry.ClusterID)), encoded); err != nil {
		return fmt.Errorf("store: append audit %s: %w", entry.Action, err)
	}
	return nil
}

// AuditRecord is an entry as it is read back.
type AuditRecord struct {
	ID        runv1.ULID
	At        time.Time
	Actor     string
	ActorKind string
	Action    string
	RunID     runv1.ULID
	ClusterID runv1.ULID
	Payload   map[string]any
}

// AuditForRun returns a run's records, newest first. It is what the UI shows
// beside a run that ended in a way nobody expected.
func (s *Store) AuditForRun(ctx context.Context, id runv1.ULID, limit int) ([]AuditRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, at, actor, actor_kind, action, run_id, cluster_id, payload
		FROM audit_log WHERE run_id = $1 ORDER BY at DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read audit for %s: %w", id, err)
	}
	defer rows.Close()

	var out []AuditRecord
	for rows.Next() {
		var (
			r       AuditRecord
			runID   sql.NullString
			cluster sql.NullString
			payload []byte
		)
		if err := rows.Scan(&r.ID, &r.At, &r.Actor, &r.ActorKind, &r.Action,
			&runID, &cluster, &payload); err != nil {
			return nil, fmt.Errorf("store: scan audit record: %w", err)
		}
		r.RunID = runv1.ULID(runID.String)
		r.ClusterID = runv1.ULID(cluster.String)
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &r.Payload); err != nil {
				return nil, fmt.Errorf("store: decode audit payload: %w", err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
