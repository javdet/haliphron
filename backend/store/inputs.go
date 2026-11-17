package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Run inputs: the files a person sent with a request. The semantics are in
// docs/contracts/run-store.md and at the top of db/migrations/0020_run_inputs.sql.
//
// run_inputs is written by insertRunInputs inside InsertRun's transaction and
// by nothing else; it goes with its run by cascade. run_input_uploads is the
// ledger of admissions that stored objects before their insert: a row is
// written by BeginInputUpload, removed by InsertRun in the run's own
// transaction, and otherwise found by OrphanInputUploads once it is old.

// RunInput is one row of a run's manifest.
type RunInput struct {
	Ordinal         int16
	Key             string
	Name            string
	MediaType       string
	SizeBytes       int64
	SHA256          []byte
	SourceMessageID string
}

// Ref is the row as the lease and the pod see it.
func (in RunInput) Ref() runv1.InputRef {
	return runv1.InputRef{
		Key:       in.Key,
		Name:      in.Name,
		MediaType: in.MediaType,
		SizeBytes: in.SizeBytes,
		SHA256:    hex.EncodeToString(in.SHA256),
	}
}

// insertRunInputs writes a run's manifest and closes its upload ledger row.
// Called from InsertRun only, in the run's own transaction.
func insertRunInputs(ctx context.Context, tx *sql.Tx, id runv1.ULID, inputs []RunInput) error {
	for _, in := range inputs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_inputs (run_id, ordinal, key, name, media_type, size_bytes,
			                        sha256, source_message_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, in.Ordinal, in.Key, in.Name, in.MediaType, in.SizeBytes, in.SHA256,
			nullString(in.SourceMessageID)); err != nil {
			return fmt.Errorf("store: insert input %d of %s: %w", in.Ordinal, id, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_input_uploads WHERE run_id = $1`, id); err != nil {
		return fmt.Errorf("store: close the upload ledger of %s: %w", id, err)
	}
	return nil
}

// RunInputs is a run's manifest, in ordinal order. A run without inputs has
// an empty one, which is not an error.
func (s *Store) RunInputs(ctx context.Context, id runv1.ULID) ([]RunInput, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ordinal, key, name, media_type, size_bytes, sha256,
		       coalesce(source_message_id, '')
		  FROM run_inputs WHERE run_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("store: read the inputs of %s: %w", id, err)
	}
	defer rows.Close()

	var out []RunInput
	for rows.Next() {
		var in RunInput
		if err := rows.Scan(&in.Ordinal, &in.Key, &in.Name, &in.MediaType, &in.SizeBytes,
			&in.SHA256, &in.SourceMessageID); err != nil {
			return nil, fmt.Errorf("store: scan an input of %s: %w", id, err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RunInput is one entry of a run's manifest, by key. ErrNotFound when the
// manifest does not name it.
func (s *Store) RunInput(ctx context.Context, id runv1.ULID, key string) (RunInput, error) {
	var in RunInput
	err := s.db.QueryRowContext(ctx, `
		SELECT ordinal, key, name, media_type, size_bytes, sha256,
		       coalesce(source_message_id, '')
		  FROM run_inputs WHERE run_id = $1 AND key = $2`, id, key).Scan(
		&in.Ordinal, &in.Key, &in.Name, &in.MediaType, &in.SizeBytes, &in.SHA256,
		&in.SourceMessageID)
	if err == sql.ErrNoRows {
		return RunInput{}, ErrNotFound
	}
	if err != nil {
		return RunInput{}, fmt.Errorf("store: read input %s of %s: %w", key, id, err)
	}
	return in, nil
}

// BeginInputUpload records that an admission is about to store objects under
// the prefix of a run that does not exist yet. Written before the first
// object, so that a process that dies at any point after it leaves a row the
// sweeper finds.
func (s *Store) BeginInputUpload(ctx context.Context, id runv1.ULID) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO run_input_uploads (run_id) VALUES ($1)
		ON CONFLICT (run_id) DO NOTHING`, id); err != nil {
		return fmt.Errorf("store: record the input upload of %s: %w", id, err)
	}
	return nil
}

// OrphanInputUploads names the ledger rows older than olderThan. A row whose
// run exists is closed here and not returned — its insert committed, so the
// row should not be there at all — and the rest name prefixes no run owns.
func (s *Store) OrphanInputUploads(ctx context.Context, olderThan time.Duration, limit int) ([]runv1.ULID, error) {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM run_input_uploads u
		 WHERE u.started_at < now() - make_interval(secs => $1)
		   AND EXISTS (SELECT 1 FROM runs r WHERE r.id = u.run_id)`,
		olderThan.Seconds()); err != nil {
		return nil, fmt.Errorf("store: close settled input uploads: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id FROM run_input_uploads
		 WHERE started_at < now() - make_interval(secs => $1)
		 ORDER BY started_at
		 LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list orphaned input uploads: %w", err)
	}
	defer rows.Close()
	var out []runv1.ULID
	for rows.Next() {
		var id runv1.ULID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan an orphaned input upload: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ForgetInputUpload drops a ledger row once its objects are gone, or once an
// admission that failed has deleted them itself. It refuses nothing: a row
// whose run exists is already gone, closed by InsertRun.
func (s *Store) ForgetInputUpload(ctx context.Context, id runv1.ULID) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM run_input_uploads u
		 WHERE u.run_id = $1
		   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = u.run_id)`, id); err != nil {
		return fmt.Errorf("store: forget the input upload of %s: %w", id, err)
	}
	return nil
}
