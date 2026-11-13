package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Alert receivers: Alertmanager notifications that start runs.
//
// What a notification means, which of its alerts a run carries, and the
// prompt it is given are app's. This file stores the receiver and owns the one
// statement that makes the throttle safe across replicas and Alertmanager's HA
// peers: the claim in linkAlertFire, which runs inside InsertRun's transaction
// and nowhere else. suppressed_until is written by that statement only.

// Delivery outcomes, as alert_deliveries.outcome and last_outcome record them.
const (
	AlertSubmitted = "submitted"
	AlertThrottled = "throttled"
	AlertResolved  = "resolved"
	AlertRefused   = "refused"
)

// AlertCredentialPrefix tells a receiver's credential from a platform token at
// a glance: it starts runs on one receiver's template and opens nothing else.
const AlertCredentialPrefix = "hla_"

// AlertReceiver is a row. The credential itself is never stored or read back;
// TokenSHA256 is its digest.
type AlertReceiver struct {
	ID          runv1.ULID
	Name        string
	Description string
	Enabled     bool

	// Run is the template, in the public API's snake_case shape.
	Run json.RawMessage

	ThrottleSeconds int32
	TokenSHA256     []byte

	LastReceivedAt *time.Time
	LastOutcome    string
	LastRunID      runv1.ULID
	LastError      string

	CreatedBy string
	CreatedAt time.Time
	UpdatedBy string
	UpdatedAt time.Time
}

// AlertDelivery is one notification and what was done about it.
type AlertDelivery struct {
	ID         runv1.ULID
	ReceiverID runv1.ULID
	ReceivedAt time.Time
	GroupKey   string
	Status     string
	Firing     int32
	Resolved   int32
	// Throttled are the fingerprints left out because an earlier run still
	// suppressed them.
	Throttled []string
	Outcome   string
	RunID     runv1.ULID
	Error     string
}

// AlertSuppression is one alert's throttle row.
type AlertSuppression struct {
	ReceiverID      runv1.ULID
	Fingerprint     string
	Alertname       string
	SuppressedUntil time.Time
	RunID           runv1.ULID
	FirstRunAt      time.Time
	LastRunAt       time.Time
	Runs            int32
	Suppressed      int64
}

// AlertFire is a run admitted for a notification: the alerts it claims and
// the delivery record it commits with.
type AlertFire struct {
	// Fingerprints are the alerts the run carries, each claimed for Window.
	// Alertnames runs parallel to it.
	Fingerprints []string
	Alertnames   []string
	Window       time.Duration
	// Delivery is recorded in the same transaction, with the run's id.
	Delivery AlertDelivery
}

// AlertSettledError is a fire that lost part of its claim: the alerts in Lost
// were claimed by another run since they were read. The run was not inserted.
type AlertSettledError struct {
	Lost []string
}

func (e *AlertSettledError) Error() string {
	return fmt.Sprintf("store: %d alert(s) were claimed by another run first", len(e.Lost))
}

// ErrAlertReceiverNameTaken is a create or rename onto a name another receiver has.
var ErrAlertReceiverNameTaken = errors.New("store: an alert receiver with this name exists")

const alertReceiverFields = `id, name, description, enabled, run, throttle_seconds, token_sha256,
	       last_received_at, last_outcome, last_run_id, last_error,
	       created_by, created_at, updated_by, updated_at`

// InsertAlertReceiver writes a new receiver with a freshly minted credential,
// and returns the row and the credential. The credential is returned this
// once and never again.
func (s *Store) InsertAlertReceiver(ctx context.Context, r AlertReceiver) (AlertReceiver, string, error) {
	secret, digest, err := mintAlertCredential()
	if err != nil {
		return AlertReceiver{}, "", err
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO alert_receivers (id, name, description, enabled, run, throttle_seconds,
		                             token_sha256, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $8)
		RETURNING `+alertReceiverFields,
		r.ID, r.Name, r.Description, r.Enabled, []byte(r.Run), r.ThrottleSeconds, digest, r.CreatedBy)
	out, err := scanAlertReceiver(row)
	if isUniqueViolation(err, "alert_receivers_name_key") {
		return AlertReceiver{}, "", ErrAlertReceiverNameTaken
	}
	if err != nil {
		return AlertReceiver{}, "", err
	}
	return out, secret, nil
}

// UpdateAlertReceiver replaces a receiver's definition. The credential, the
// outcome columns and the suppressions are left alone: a shorter window
// applies to the claims made after it.
func (s *Store) UpdateAlertReceiver(ctx context.Context, r AlertReceiver) (AlertReceiver, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE alert_receivers SET
			name             = $2,
			description      = $3,
			enabled          = $4,
			run              = $5::jsonb,
			throttle_seconds = $6,
			updated_by       = $7,
			updated_at       = now()
		WHERE id = $1
		RETURNING `+alertReceiverFields,
		r.ID, r.Name, r.Description, r.Enabled, []byte(r.Run), r.ThrottleSeconds, r.UpdatedBy)
	out, err := scanAlertReceiver(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AlertReceiver{}, ErrNotFound
	case isUniqueViolation(err, "alert_receivers_name_key"):
		return AlertReceiver{}, ErrAlertReceiverNameTaken
	}
	return out, err
}

// RotateAlertReceiverToken replaces a receiver's credential and returns the
// new one. The old one stops working when this commits.
func (s *Store) RotateAlertReceiverToken(ctx context.Context, id runv1.ULID, by string) (AlertReceiver, string, error) {
	secret, digest, err := mintAlertCredential()
	if err != nil {
		return AlertReceiver{}, "", err
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE alert_receivers SET token_sha256 = $2, updated_by = $3, updated_at = now()
		WHERE id = $1
		RETURNING `+alertReceiverFields, id, digest, by)
	out, err := scanAlertReceiver(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertReceiver{}, "", ErrNotFound
	}
	if err != nil {
		return AlertReceiver{}, "", err
	}
	return out, secret, nil
}

// AlertReceiverByID reads one receiver.
func (s *Store) AlertReceiverByID(ctx context.Context, id runv1.ULID) (AlertReceiver, error) {
	out, err := scanAlertReceiver(s.db.QueryRowContext(ctx,
		`SELECT `+alertReceiverFields+` FROM alert_receivers WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AlertReceiver{}, ErrNotFound
	}
	return out, err
}

// ListAlertReceivers returns every receiver, by name. Configuration, like
// schedules, so not paged.
func (s *Store) ListAlertReceivers(ctx context.Context) ([]AlertReceiver, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+alertReceiverFields+` FROM alert_receivers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list alert receivers: %w", err)
	}
	defer rows.Close()

	out := []AlertReceiver{}
	for rows.Next() {
		r, err := scanAlertReceiver(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteAlertReceiver removes a receiver with its suppressions and delivery
// log. Its runs stay, with alert_receiver_id cleared by the foreign key.
func (s *Store) DeleteAlertReceiver(ctx context.Context, id runv1.ULID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM alert_receivers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete alert receiver %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SuppressedAlerts returns the receiver's live suppressions: those of the
// given fingerprints, or every one when fingerprints is nil. It takes no lock;
// the claim in InsertRun is what decides, and this read only saves admitting
// a run that is bound to lose it.
func (s *Store) SuppressedAlerts(ctx context.Context, receiver runv1.ULID, fingerprints []string) ([]AlertSuppression, error) {
	query := `
		SELECT receiver_id, fingerprint, alertname, suppressed_until, run_id,
		       first_run_at, last_run_at, runs, suppressed
		FROM alert_fingerprints
		WHERE receiver_id = $1 AND suppressed_until > now()`
	args := []any{receiver}
	if fingerprints != nil {
		query += ` AND fingerprint = ANY($2::text[])`
		args = append(args, textArray(fingerprints))
	}
	query += ` ORDER BY suppressed_until DESC, fingerprint`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: suppressed alerts of %s: %w", receiver, err)
	}
	defer rows.Close()

	out := []AlertSuppression{}
	for rows.Next() {
		var (
			a   AlertSuppression
			run sql.NullString
		)
		if err := rows.Scan(&a.ReceiverID, &a.Fingerprint, &a.Alertname, &a.SuppressedUntil, &run,
			&a.FirstRunAt, &a.LastRunAt, &a.Runs, &a.Suppressed); err != nil {
			return nil, fmt.Errorf("store: scan alert suppression: %w", err)
		}
		a.RunID = runv1.ULID(run.String)
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountSuppressed adds one to the suppressed counter of each fingerprint. It
// is bookkeeping for the UI, outside the claim, and never touches
// suppressed_until. The rows are locked in fingerprint order, the order the
// claim takes them in, so the two cannot deadlock.
func (s *Store) CountSuppressed(ctx context.Context, receiver runv1.ULID, fingerprints []string) error {
	if len(fingerprints) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE alert_fingerprints f SET suppressed = f.suppressed + 1
		FROM (SELECT fingerprint FROM alert_fingerprints
		      WHERE receiver_id = $1 AND fingerprint = ANY($2::text[])
		      ORDER BY fingerprint
		      FOR UPDATE) l
		WHERE f.receiver_id = $1 AND f.fingerprint = l.fingerprint`,
		receiver, textArray(fingerprints))
	if err != nil {
		return fmt.Errorf("store: count suppressed alerts of %s: %w", receiver, err)
	}
	return nil
}

// RecordAlertDelivery writes a delivery that started no run, and puts its
// outcome on the receiver. A delivery that started one is recorded by
// InsertRun, in the run's own transaction.
func (s *Store) RecordAlertDelivery(ctx context.Context, d AlertDelivery) error {
	if d.Outcome == AlertSubmitted {
		return fmt.Errorf("store: a submitted delivery is recorded with its run")
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		return recordAlertDelivery(ctx, tx, d)
	})
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}

// AlertDeliveries pages through a receiver's delivery log, newest first.
func (s *Store) AlertDeliveries(ctx context.Context, receiver, before runv1.ULID, limit int) ([]AlertDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `
		SELECT id, receiver_id, received_at, group_key, status, firing, resolved,
		       throttled, outcome, run_id, error
		FROM alert_deliveries
		WHERE receiver_id = $1`
	args := []any{receiver}
	if before != "" {
		args = append(args, before)
		query += ` AND id < $2`
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: deliveries of %s: %w", receiver, err)
	}
	defer rows.Close()

	out := []AlertDelivery{}
	for rows.Next() {
		var (
			d      AlertDelivery
			run    sql.NullString
			reason sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.ReceiverID, &d.ReceivedAt, &d.GroupKey, &d.Status,
			&d.Firing, &d.Resolved, pgArray(&d.Throttled), &d.Outcome, &run, &reason); err != nil {
			return nil, fmt.Errorf("store: scan alert delivery: %w", err)
		}
		d.RunID = runv1.ULID(run.String)
		d.Error = reason.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClearAlertSuppressions deletes a receiver's suppression of one fingerprint,
// or of every alert it currently suppresses when fingerprint is empty, and
// returns how many rows went. The next delivery of a cleared alert starts a
// run. A delete is the one write to these rows outside the claim, and it can
// only cause an extra run, never a lost one.
func (s *Store) ClearAlertSuppressions(ctx context.Context, receiver runv1.ULID, fingerprint string) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if fingerprint == "" {
		res, err = s.db.ExecContext(ctx, `
			DELETE FROM alert_fingerprints WHERE receiver_id = $1 AND suppressed_until > now()`, receiver)
	} else {
		res, err = s.db.ExecContext(ctx, `
			DELETE FROM alert_fingerprints WHERE receiver_id = $1 AND fingerprint = $2`, receiver, fingerprint)
	}
	if err != nil {
		return 0, fmt.Errorf("store: clear suppressions of %s: %w", receiver, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// alertPurgeBatch bounds one DELETE, so retention never holds a long lock on
// a table a delivery is writing to.
const alertPurgeBatch = 1000

// PurgeAlerts deletes deliveries received more than deliveries ago, and
// throttle rows that stopped suppressing more than fingerprints ago. Deleting
// a throttle row can never cause a duplicate run: an expired row already lets
// the next delivery through, and only its counters go.
func (s *Store) PurgeAlerts(ctx context.Context, deliveries, fingerprints time.Duration) (int64, int64, error) {
	d, err := s.purgeInBatches(ctx, `
		DELETE FROM alert_deliveries WHERE id IN (
			SELECT id FROM alert_deliveries
			WHERE received_at < now() - make_interval(secs => $1)
			LIMIT $2)`, deliveries)
	if err != nil {
		return 0, 0, fmt.Errorf("store: purge alert deliveries: %w", err)
	}
	f, err := s.purgeInBatches(ctx, `
		DELETE FROM alert_fingerprints WHERE (receiver_id, fingerprint) IN (
			SELECT receiver_id, fingerprint FROM alert_fingerprints
			WHERE suppressed_until < now() - make_interval(secs => $1)
			LIMIT $2)`, fingerprints)
	if err != nil {
		return d, 0, fmt.Errorf("store: purge alert fingerprints: %w", err)
	}
	return d, f, nil
}

func (s *Store) purgeInBatches(ctx context.Context, statement string, age time.Duration) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, statement, age.Seconds(), alertPurgeBatch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < alertPurgeBatch || ctx.Err() != nil {
			return total, nil
		}
	}
}

// AlertUsage is what the metrics endpoint reports about alert receivers.
type AlertUsage struct {
	ReceiversEnabled  int64
	ReceiversDisabled int64
	// Suppressions are the live throttle rows across every receiver.
	Suppressions int64
	// Deliveries counts the last day's deliveries by receiver name and outcome.
	Deliveries []AlertDeliveryCount
}

// AlertDeliveryCount is one series of AlertUsage.Deliveries.
type AlertDeliveryCount struct {
	Receiver string
	Outcome  string
	Count    int64
}

// AlertUsage reads the receiver gauges.
func (s *Store) AlertUsage(ctx context.Context) (AlertUsage, error) {
	var u AlertUsage
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE enabled), count(*) FILTER (WHERE NOT enabled),
		       (SELECT count(*) FROM alert_fingerprints WHERE suppressed_until > now())
		FROM alert_receivers`).Scan(&u.ReceiversEnabled, &u.ReceiversDisabled, &u.Suppressions); err != nil {
		return AlertUsage{}, fmt.Errorf("store: alert usage: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.name, d.outcome, count(*)
		FROM alert_deliveries d JOIN alert_receivers r ON r.id = d.receiver_id
		WHERE d.received_at > now() - interval '24 hours'
		GROUP BY r.name, d.outcome
		ORDER BY r.name, d.outcome`)
	if err != nil {
		return AlertUsage{}, fmt.Errorf("store: alert deliveries by outcome: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c AlertDeliveryCount
		if err := rows.Scan(&c.Receiver, &c.Outcome, &c.Count); err != nil {
			return AlertUsage{}, fmt.Errorf("store: scan alert delivery count: %w", err)
		}
		u.Deliveries = append(u.Deliveries, c)
	}
	return u, rows.Err()
}

// linkAlertFire is the claim, inside the run's own insert, after the run row
// exists for the foreign keys.
//
// Every alert the statement does not return was claimed by another run since
// app read it, and the whole insert is rolled back with AlertSettledError
// naming them: a run that carries an alert another run is already handling is
// the duplicate this table exists to prevent. The rows are taken in
// fingerprint order. now() is the transaction's, never the app's clock: the
// comparison is between database times only.
func linkAlertFire(ctx context.Context, tx *sql.Tx, receiver, runID runv1.ULID, fire AlertFire) error {
	fingerprints, names := sortedClaim(fire.Fingerprints, fire.Alertnames)
	if len(fingerprints) == 0 {
		return fmt.Errorf("store: an alert fire claims no alerts")
	}

	rows, err := tx.QueryContext(ctx, `
		INSERT INTO alert_fingerprints AS f
		       (receiver_id, fingerprint, alertname, suppressed_until, run_id, first_run_at, last_run_at)
		SELECT $1, t.fp, t.an, now() + make_interval(secs => $3), $4, now(), now()
		FROM unnest($2::text[], $5::text[]) AS t(fp, an)
		ORDER BY t.fp
		ON CONFLICT (receiver_id, fingerprint) DO UPDATE SET
			alertname        = EXCLUDED.alertname,
			suppressed_until = EXCLUDED.suppressed_until,
			run_id           = EXCLUDED.run_id,
			last_run_at      = EXCLUDED.last_run_at,
			runs             = f.runs + 1
		WHERE f.suppressed_until <= now()
		RETURNING f.fingerprint`,
		receiver, textArray(fingerprints), fire.Window.Seconds(), runID, textArray(names))
	if err != nil {
		return fmt.Errorf("store: claim alerts for run %s: %w", runID, err)
	}
	won := make(map[string]bool, len(fingerprints))
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan claimed alert: %w", err)
		}
		won[fp] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: claim alerts for run %s: %w", runID, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: claim alerts for run %s: %w", runID, err)
	}

	var lost []string
	for _, fp := range fingerprints {
		if !won[fp] {
			lost = append(lost, fp)
		}
	}
	if len(lost) > 0 {
		return &AlertSettledError{Lost: lost}
	}

	d := fire.Delivery
	d.ReceiverID, d.Outcome, d.RunID, d.Error = receiver, AlertSubmitted, runID, ""
	return recordAlertDelivery(ctx, tx, d)
}

// sortedClaim deduplicates the fingerprints and sorts them, keeping each
// one's alertname beside it. A fingerprint twice in one INSERT ... ON
// CONFLICT DO UPDATE is an error rather than a no-op.
func sortedClaim(fingerprints, names []string) ([]string, []string) {
	byFP := make(map[string]string, len(fingerprints))
	for i, fp := range fingerprints {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		byFP[fp] = name
	}
	fps := make([]string, 0, len(byFP))
	for fp := range byFP {
		fps = append(fps, fp)
	}
	slices.Sort(fps)
	out := make([]string, len(fps))
	for i, fp := range fps {
		out[i] = byFP[fp]
	}
	return fps, out
}

func recordAlertDelivery(ctx context.Context, tx *sql.Tx, d AlertDelivery) error {
	throttled := d.Throttled
	if throttled == nil {
		throttled = []string{}
	}
	reason := truncate(d.Error, 4096)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO alert_deliveries (id, receiver_id, group_key, status, firing, resolved,
		                              throttled, outcome, run_id, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7::text[], $8, $9, $10)`,
		d.ID, d.ReceiverID, truncate(d.GroupKey, 1024), d.Status, d.Firing, d.Resolved,
		textArray(throttled), d.Outcome, nullString(string(d.RunID)), nullString(reason)); err != nil {
		return fmt.Errorf("store: record alert delivery %s: %w", d.ID, err)
	}
	// last_run_id is the last run the receiver started; a delivery that
	// started none leaves it where it was.
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_receivers SET
			last_received_at = now(),
			last_outcome     = $2,
			last_run_id      = coalesce($3, last_run_id),
			last_error       = $4
		WHERE id = $1`,
		d.ReceiverID, d.Outcome, nullString(string(d.RunID)), nullString(reason)); err != nil {
		return fmt.Errorf("store: record outcome on alert receiver %s: %w", d.ReceiverID, err)
	}
	return nil
}

// mintAlertCredential makes a receiver's credential and its digest, with the
// generator platform tokens use.
func mintAlertCredential() (string, []byte, error) {
	secret, err := randomToken()
	if err != nil {
		return "", nil, err
	}
	secret = AlertCredentialPrefix + secret
	digest := sha256.Sum256([]byte(secret))
	return secret, digest[:], nil
}

// AlertCredentialDigest is the digest a presented credential is compared by.
func AlertCredentialDigest(presented string) []byte {
	digest := sha256.Sum256([]byte(strings.TrimSpace(presented)))
	return digest[:]
}

func scanAlertReceiver(row rowScanner) (AlertReceiver, error) {
	var (
		r        AlertReceiver
		received sql.NullTime
		outcome  sql.NullString
		lastRun  sql.NullString
		lastErr  sql.NullString
	)
	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Enabled, &r.Run, &r.ThrottleSeconds, &r.TokenSHA256,
		&received, &outcome, &lastRun, &lastErr,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedBy, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AlertReceiver{}, err
		}
		return AlertReceiver{}, fmt.Errorf("store: scan alert receiver: %w", err)
	}
	r.LastReceivedAt = timePtr(received)
	r.LastOutcome = outcome.String
	r.LastRunID = runv1.ULID(lastRun.String)
	r.LastError = lastErr.String
	return r, nil
}
