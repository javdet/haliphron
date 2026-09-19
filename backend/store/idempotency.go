package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Idempotency-Key on the mutating REST and MCP calls.
//
// The retry that Slack and n8n perform on a timeout must return the first run
// rather than start a second one. The digest of the body is what makes that
// safe: the same key with a different body is a client defect, and answering it
// with the first run's identifier would hand back the result of work nobody
// ordered. That case is a 422, and this column is how it is told apart.

// ErrIdempotencyConflict is the same key with a different body.
var ErrIdempotencyConflict = errors.New("store: idempotency key was used with a different request")

// IdempotentResult is a replayed answer.
type IdempotentResult struct {
	RunID    runv1.ULID
	Status   int
	Response json.RawMessage
}

// ClaimIdempotencyKey reserves a key for a request, or returns what the first
// request answered.
//
// The claim and the replay are one statement: two requests arriving at the
// same moment with the same key must not both see "no row" and both proceed.
// ON CONFLICT DO NOTHING plus a read of the loser's row is what makes the
// second one wait for nothing and answer with the first one's result.
func (s *Store) ClaimIdempotencyKey(ctx context.Context, scope, key string,
	body []byte, ttl time.Duration) (*IdempotentResult, error) {

	if key == "" {
		return nil, nil
	}
	digest := sha256.Sum256(body)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO idempotency_keys (scope, key, request_sha256, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		ON CONFLICT (tenant_id, scope, key) DO NOTHING`,
		scope, key, digest[:], ttl.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: claim idempotency key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, nil
	}

	var (
		stored   []byte
		runID    sql.NullString
		status   sql.NullInt32
		response []byte
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT request_sha256, run_id, response_status, response_body
		FROM idempotency_keys WHERE scope = $1 AND key = $2`, scope, key).
		Scan(&stored, &runID, &status, &response)
	if errors.Is(err, sql.ErrNoRows) {
		// The row expired between the insert and the read. Treating it as a
		// fresh claim is right: there is nothing to replay.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read idempotency key: %w", err)
	}
	if string(stored) != string(digest[:]) {
		return nil, ErrIdempotencyConflict
	}

	return &IdempotentResult{
		RunID:    runv1.ULID(runID.String),
		Status:   int(status.Int32),
		Response: response,
	}, nil
}

// CompleteIdempotencyKey records what the first request answered, so the retry
// can be given the same thing.
func (s *Store) CompleteIdempotencyKey(ctx context.Context, scope, key string,
	runID runv1.ULID, status int, response json.RawMessage) error {

	if key == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE idempotency_keys
		SET run_id = $3, response_status = $4, response_body = $5::jsonb
		WHERE scope = $1 AND key = $2`,
		scope, key, nullString(string(runID)), status, []byte(response)); err != nil {
		return fmt.Errorf("store: complete idempotency key: %w", err)
	}
	return nil
}

// ReleaseIdempotencyKey drops a claim whose request failed. Without it a
// request that failed on a transient error would be unrepeatable under the same
// key, and the caller's retry — the thing the key exists to make safe — would
// return an empty replay forever.
func (s *Store) ReleaseIdempotencyKey(ctx context.Context, scope, key string) error {
	if key == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE scope = $1 AND key = $2 AND run_id IS NULL`,
		scope, key); err != nil {
		return fmt.Errorf("store: release idempotency key: %w", err)
	}
	return nil
}

// PurgeExpired removes spent idempotency keys. Retention elsewhere is a
// deliberate operation with a boundary the chart sets; this table is a cache
// with a column that says when each row stopped mattering.
func (s *Store) PurgeExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("store: purge idempotency keys: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
