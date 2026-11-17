package store

import (
	"database/sql"
	"testing"
)

// alert_receivers, alert_fingerprints and alert_deliveries: Alertmanager
// notifications that start runs, the throttle, and the delivery log. The claim
// itself is a statement in backend/store and is exercised by test/backend;
// what is checked here is what the schema guarantees on its own.

func newAlertReceiver(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	id := newULID(t)
	if _, err := conn.Exec(`
		INSERT INTO alert_receivers (id, name, run, token_sha256, created_by, updated_by)
		VALUES ($1, $2::text, '{"prompt": "x", "role": "triage"}', sha256(convert_to($2::text, 'UTF8')), 'test', 'test')`,
		id, name); err != nil {
		t.Fatalf("insert alert receiver %s: %v", name, err)
	}
	return id
}

func TestAnAlertReceiverDefaultsToAnHourAndBoundsItsWindow(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	id := newAlertReceiver(t, conn, "on-call")

	var throttle int
	if err := conn.QueryRow(`SELECT throttle_seconds FROM alert_receivers WHERE id = $1`, id).Scan(&throttle); err != nil {
		t.Fatal(err)
	}
	if throttle != 3600 {
		t.Errorf("default throttle_seconds = %d, want 3600", throttle)
	}
	for _, bad := range []int{-1, 604801} {
		if _, err := conn.Exec(`UPDATE alert_receivers SET throttle_seconds = $2 WHERE id = $1`, id, bad); err == nil {
			t.Errorf("throttle_seconds %d was accepted", bad)
		}
	}
	if _, err := conn.Exec(`
		INSERT INTO alert_receivers (id, name, run, token_sha256, created_by, updated_by)
		VALUES ($1, 'on-call', '{}', sha256('other'::bytea), 'test', 'test')`, newULID(t)); err == nil {
		t.Error("two alert receivers share a name")
	}
}

func TestAFingerprintIsLowerCaseHex(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	id := newAlertReceiver(t, conn, "on-call")
	for fp, ok := range map[string]bool{"0123abcd": true, "ABCD": false, "": false, "zz": false} {
		_, err := conn.Exec(`
			INSERT INTO alert_fingerprints (receiver_id, fingerprint, suppressed_until, first_run_at, last_run_at)
			VALUES ($1, $2, now(), now(), now())`, id, fp)
		if (err == nil) != ok {
			t.Errorf("fingerprint %q: accepted %v, want %v (%v)", fp, err == nil, ok, err)
		}
	}
}

func TestOnlyASubmittedDeliveryCarriesARun(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	id := newAlertReceiver(t, conn, "on-call")

	insert := func(outcome string, run any) error {
		_, err := conn.Exec(`
			INSERT INTO alert_deliveries (id, receiver_id, status, firing, resolved, outcome, run_id)
			VALUES ($1, $2, 'firing', 1, 0, $3, $4)`, newULID(t), id, outcome, run)
		return err
	}
	if err := insert("submitted", f.runID); err != nil {
		t.Errorf("a submitted delivery with its run: %v", err)
	}
	if err := insert("throttled", f.runID); err == nil {
		t.Error("a throttled delivery carries a run")
	}
	if err := insert("throttled", nil); err != nil {
		t.Errorf("a throttled delivery: %v", err)
	}
}

func TestDeletingAnAlertReceiverKeepsItsRunsAndTakesItsHistory(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	id := newAlertReceiver(t, conn, "on-call")

	// Terminal first: runs_guard freezes admission fields, and the SET NULL is
	// an UPDATE it sees — this proves alert_receiver_id is not among them.
	if _, err := conn.Exec(`UPDATE runs SET alert_receiver_id = $2, created_via = 'alertmanager',
		status = 'Succeeded', observed_phase = 'Succeeded' WHERE id = $1`, f.runID, id); err != nil {
		t.Fatalf("link run: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO alert_fingerprints (receiver_id, fingerprint, suppressed_until, run_id, first_run_at, last_run_at)
		VALUES ($1, 'ab', now() + interval '1 hour', $2, now(), now())`, id, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO alert_deliveries (id, receiver_id, status, firing, resolved, outcome, run_id)
		VALUES ($1, $2, 'firing', 1, 0, 'submitted', $3)`, newULID(t), id, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`DELETE FROM alert_receivers WHERE id = $1`, id); err != nil {
		t.Fatalf("delete alert receiver: %v", err)
	}

	var receiver sql.NullString
	if err := conn.QueryRow(`SELECT alert_receiver_id FROM runs WHERE id = $1`, f.runID).Scan(&receiver); err != nil {
		t.Fatalf("the run went with its receiver: %v", err)
	}
	if receiver.Valid {
		t.Errorf("the run still names deleted receiver %s", receiver.String)
	}
	var left int
	if err := conn.QueryRow(`SELECT (SELECT count(*) FROM alert_fingerprints) + (SELECT count(*) FROM alert_deliveries)`).
		Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d throttle and delivery rows outlived their receiver", left)
	}
}

func TestDeletingARunClearsEveryAlertReferenceToIt(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	id := newAlertReceiver(t, conn, "on-call")

	for _, stmt := range []string{
		`UPDATE alert_receivers SET last_run_id = $2 WHERE id = $1`,
		`INSERT INTO alert_fingerprints (receiver_id, fingerprint, suppressed_until, run_id, first_run_at, last_run_at)
		 VALUES ($1, 'ab', now() + interval '1 hour', $2, now(), now())`,
	} {
		if _, err := conn.Exec(stmt, id, f.runID); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := conn.Exec(`
		INSERT INTO alert_deliveries (id, receiver_id, status, firing, resolved, outcome, run_id)
		VALUES ($3, $1, 'firing', 1, 0, 'submitted', $2)`, id, f.runID, newULID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`DELETE FROM run_attempts WHERE run_id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`DELETE FROM runs WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("run retention could not delete a run an alert receiver started: %v", err)
	}

	var refs int
	if err := conn.QueryRow(`
		SELECT (SELECT count(last_run_id) FROM alert_receivers)
		     + (SELECT count(run_id) FROM alert_fingerprints)
		     + (SELECT count(run_id) FROM alert_deliveries)`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Errorf("%d alert rows still name the deleted run", refs)
	}
	var suppressed int
	if err := conn.QueryRow(`SELECT count(*) FROM alert_fingerprints WHERE suppressed_until > now()`).Scan(&suppressed); err != nil {
		t.Fatal(err)
	}
	if suppressed != 1 {
		t.Error("deleting the run lifted the suppression it claimed; the window outlives the run")
	}
}

func TestAnAlertReceiversChatChannelIsAPlatformAndAnIDOrNeither(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	id := newAlertReceiver(t, conn, "on-call")

	for _, c := range []struct {
		platform, channel any
		ok                bool
	}{
		{nil, nil, true},
		{"mattermost", "channel000000000000000000c", true},
		{"slack", "C0123ABC", true},
		{"mattermost", nil, false},
		{nil, "C0123ABC", false},
		{"teams", "C0123ABC", false},
		{"slack", "", false},
	} {
		_, err := conn.Exec(`UPDATE alert_receivers SET chat_platform = $2, chat_channel_id = $3 WHERE id = $1`,
			id, c.platform, c.channel)
		if (err == nil) != c.ok {
			t.Errorf("platform %v, channel %v: accepted %v, want %v (%v)", c.platform, c.channel, err == nil, c.ok, err)
		}
	}
}
