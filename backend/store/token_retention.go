package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Retention for per-run tokens, kept as data so an operator can change it from
// the UI. The semantics are at the top of db/migrations/0014_token_retention.sql.
// The default it falls back to before the first save is the app layer's.

// TokenRetention is the row.
type TokenRetention struct {
	// RunTokens is how long after it was created an ended run-mcp token is
	// kept; zero keeps them for ever.
	RunTokens time.Duration
	UpdatedBy string
	UpdatedAt time.Time
}

// TokenRetention reads the stored retention, or ErrNotFound when none was ever
// saved.
func (s *Store) TokenRetention(ctx context.Context) (TokenRetention, error) {
	var (
		r       TokenRetention
		seconds int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT run_token_seconds, updated_by, updated_at FROM token_retention`).
		Scan(&seconds, &r.UpdatedBy, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenRetention{}, ErrNotFound
	}
	if err != nil {
		return TokenRetention{}, fmt.Errorf("store: read token retention: %w", err)
	}
	r.RunTokens = time.Duration(seconds) * time.Second
	return r, nil
}

// PutTokenRetention saves the retention and records the change in the same
// transaction, as PutRunRetention does.
func (s *Store) PutTokenRetention(ctx context.Context, runTokens time.Duration, by string) (TokenRetention, error) {
	seconds := int64(runTokens / time.Second)
	out := TokenRetention{RunTokens: time.Duration(seconds) * time.Second}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO token_retention (run_token_seconds, updated_by, updated_at)
			VALUES ($1, $2, clock_timestamp())
			ON CONFLICT (tenant_id) DO UPDATE SET
			       run_token_seconds = EXCLUDED.run_token_seconds, updated_by = EXCLUDED.updated_by,
			       updated_at = EXCLUDED.updated_at
			RETURNING updated_by, updated_at`, seconds, by).
			Scan(&out.UpdatedBy, &out.UpdatedAt)
		if err != nil {
			return fmt.Errorf("store: save token retention: %w", err)
		}
		return appendAudit(ctx, tx, AuditEntry{
			Actor: by, ActorKind: "user", Action: AuditTokenRetentionSaved,
			SubjectKind: "token_retention", SubjectID: "run_tokens",
			Payload: map[string]any{"runTokenSeconds": seconds},
		})
	})
	if err != nil {
		return TokenRetention{}, err
	}
	return out, nil
}

// DeleteEndedRunTokens removes up to limit run-mcp tokens created before
// cutoff that have already ended, oldest first, and returns how many went.
//
// The token must have ended as well as be old: a run-mcp token always carries
// an expiry and is revoked when its run ends, so this excludes nothing today,
// and if a run ever outlives the age its token keeps working rather than
// vanishing under it. Nothing is audited per token: the token was minted by
// the system for one run, and that run's own history is what records it.
func (s *Store) DeleteEndedRunTokens(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM api_tokens
		WHERE id IN (
			SELECT id FROM api_tokens
			WHERE kind = $1
			  AND created_at < $2
			  AND (revoked_at IS NOT NULL OR expires_at <= now())
			ORDER BY created_at
			LIMIT $3)`, TokenKindRunMCP, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("store: delete ended run tokens: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
