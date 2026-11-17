package store

import (
	"bytes"
	"testing"
)

// run_inputs is a run's manifest of the files a person sent with the request,
// and run_input_uploads the ledger that finds objects stored for a run that was
// never inserted (0020).

func TestAnInputKeyOutsideTheLayoutIsRefusedByTheSchema(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	sum := bytes.Repeat([]byte{1}, 32)

	if _, err := conn.Exec(`
		INSERT INTO run_inputs (run_id, ordinal, key, name, media_type, size_bytes, sha256)
		VALUES ($1, 1, 'inputs/1-screenshot.png', 'screenshot.png', 'image/png', 10, $2)`,
		f.runID, sum); err != nil {
		t.Fatalf("a well-formed input was refused: %v", err)
	}

	for _, key := range []string{
		"inputs/../result.md", "result.md", "inputs/1-.hidden", "inputs/1-a/b",
		"inputs/x.png", "/inputs/2-x.png", "inputs/2-a..b",
	} {
		if _, err := conn.Exec(`
			INSERT INTO run_inputs (run_id, ordinal, key, name, media_type, size_bytes, sha256)
			VALUES ($1, 2, $2, 'x', 'image/png', 10, $3)`, f.runID, key, sum); err == nil {
			t.Errorf("the schema accepted the input key %q", key)
		}
	}
	if _, err := conn.Exec(`
		INSERT INTO run_inputs (run_id, ordinal, key, name, media_type, size_bytes, sha256)
		VALUES ($1, 3, 'inputs/3-x.png', 'x', 'image/png', 10, $2)`, f.runID, []byte{1, 2, 3}); err == nil {
		t.Error("the schema accepted a digest that is not 32 bytes")
	}
	if _, err := conn.Exec(`
		INSERT INTO run_inputs (run_id, ordinal, key, name, media_type, size_bytes, sha256)
		VALUES ($1, 4, 'inputs/1-screenshot.png', 'x', 'image/png', 10, $2)`, f.runID, sum); err == nil {
		t.Error("one run holds two inputs under one key")
	}
}

func TestARunsInputsGoWithTheRun(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)

	if _, err := conn.Exec(`
		INSERT INTO run_inputs (run_id, ordinal, key, name, media_type, size_bytes, sha256)
		VALUES ($1, 1, 'inputs/1-a.png', 'a.png', 'image/png', 1, $2)`,
		f.runID, bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatalf("insert an input: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM runs WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("delete the run: %v", err)
	}
	var left int
	if err := conn.QueryRow(`SELECT count(*) FROM run_inputs WHERE run_id = $1`, f.runID).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("%d inputs outlived their run", left)
	}
}

func TestAnInputCannotBeRecordedForARunThatDoesNotExist(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	if _, err := conn.Exec(`
		INSERT INTO run_inputs (run_id, ordinal, key, name, media_type, size_bytes, sha256)
		VALUES ($1, 1, 'inputs/1-a.png', 'a.png', 'image/png', 1, $2)`,
		newULID(t), bytes.Repeat([]byte{7}, 32)); err == nil {
		t.Error("a manifest row was written for no run")
	}
	// The upload ledger is the opposite: its point is to outlive a run that
	// never came to be.
	if _, err := conn.Exec(`INSERT INTO run_input_uploads (run_id) VALUES ($1)`, newULID(t)); err != nil {
		t.Errorf("the upload ledger refused a run that does not exist yet: %v", err)
	}
}

func TestTheInputsPhaseIsACheckpointTheSchemaAccepts(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	var ok bool
	if err := conn.QueryRow(`SELECT 'inputs'::runtime_phase = 'inputs'`).Scan(&ok); err != nil || !ok {
		t.Errorf("runtime_phase refuses 'inputs': %v", err)
	}
}
