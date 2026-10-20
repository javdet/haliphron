package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Schedules: a run template and when to admit it.
//
// What a schedule means — cron, timezone, the concurrency policy, the starting
// deadline — is app's. This file stores the row and owns the one statement
// that makes firing safe across replicas: the fence in linkScheduleFire, which
// runs inside InsertRun's transaction and nowhere else.

// Schedule outcomes, as last_outcome records them.
const (
	ScheduleSubmitted = "submitted"
	ScheduleSkipped   = "skipped"
	ScheduleMissed    = "missed"
	ScheduleRefused   = "refused"
)

// Schedule is a row.
type Schedule struct {
	ID          runv1.ULID
	Name        string
	Description string

	Cron                    string
	Timezone                string
	Enabled                 bool
	ConcurrencyPolicy       string
	StartingDeadlineSeconds *int32

	// Run is the template, in the public API's snake_case shape.
	Run json.RawMessage

	NextRunAt *time.Time
	// Revision is raised by every edit. A fire is fenced on it as well as on
	// NextRunAt, so an edit that keeps the next slot still wins over a fire
	// that read the schedule before it.
	Revision int64

	LastFiredAt *time.Time
	LastRunID   runv1.ULID
	LastOutcome string
	LastError   string

	CreatedBy string
	CreatedAt time.Time
	UpdatedBy string
	UpdatedAt time.Time
}

// ScheduleFire is a scheduled admission, fenced on the state the scheduler
// read. The run it accompanies commits only if the schedule is still enabled
// and still due at Expected under Revision.
type ScheduleFire struct {
	Expected time.Time
	Revision int64
	Next     time.Time
	At       time.Time
}

// ErrScheduleSettled is a fire that lost its fence: another replica fired the
// schedule first, or it was edited, disabled or deleted while this one was
// being admitted. The run was not inserted.
var ErrScheduleSettled = errors.New("store: the schedule has already fired or changed")

// ErrScheduleNameTaken is a create or rename onto a name another schedule has.
var ErrScheduleNameTaken = errors.New("store: a schedule with this name exists")

const scheduleFields = `id, name, description, cron, timezone, enabled, concurrency_policy,
	       starting_deadline_seconds, run, next_run_at, revision,
	       last_fired_at, last_run_id, last_outcome, last_error,
	       created_by, created_at, updated_by, updated_at`

const scheduleColumns = `SELECT ` + scheduleFields

// InsertSchedule writes a new schedule and returns the stored row.
func (s *Store) InsertSchedule(ctx context.Context, sch Schedule) (Schedule, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO schedules (id, name, description, cron, timezone, enabled, concurrency_policy,
		                       starting_deadline_seconds, run, next_run_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $11)
		RETURNING `+scheduleFields,
		sch.ID, sch.Name, sch.Description, sch.Cron, sch.Timezone, sch.Enabled, sch.ConcurrencyPolicy,
		nullInt32(sch.StartingDeadlineSeconds), []byte(sch.Run), nullTime(sch.NextRunAt), sch.CreatedBy)
	out, err := scanSchedule(row)
	if isUniqueViolation(err, "schedules_name_key") {
		return Schedule{}, ErrScheduleNameTaken
	}
	return out, err
}

// UpdateSchedule replaces a schedule's definition and raises its revision. The
// outcome columns are left alone: they are what the schedule last did, and an
// edit does not change that.
func (s *Store) UpdateSchedule(ctx context.Context, sch Schedule) (Schedule, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE schedules SET
			name                      = $2,
			description               = $3,
			cron                      = $4,
			timezone                  = $5,
			enabled                   = $6,
			concurrency_policy        = $7,
			starting_deadline_seconds = $8,
			run                       = $9::jsonb,
			next_run_at               = $10,
			revision                  = revision + 1,
			updated_by                = $11,
			updated_at                = now()
		WHERE id = $1
		RETURNING `+scheduleFields,
		sch.ID, sch.Name, sch.Description, sch.Cron, sch.Timezone, sch.Enabled, sch.ConcurrencyPolicy,
		nullInt32(sch.StartingDeadlineSeconds), []byte(sch.Run), nullTime(sch.NextRunAt), sch.UpdatedBy)
	out, err := scanSchedule(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Schedule{}, ErrNotFound
	case isUniqueViolation(err, "schedules_name_key"):
		return Schedule{}, ErrScheduleNameTaken
	}
	return out, err
}

// ScheduleByID reads one schedule.
func (s *Store) ScheduleByID(ctx context.Context, id runv1.ULID) (Schedule, error) {
	out, err := scanSchedule(s.db.QueryRowContext(ctx, scheduleColumns+` FROM schedules WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	return out, err
}

// ListSchedules returns every schedule, by name. A schedule is configuration
// rather than history, so the list is not paged, as the role list is not.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	return s.querySchedules(ctx, scheduleColumns+` FROM schedules ORDER BY name`)
}

// DueSchedules returns the enabled schedules whose next fire is at or before
// now, oldest first. It takes no lock: the fence is what decides which replica
// fires, and a row lock held across admission would hold a connection per
// schedule for no gain.
func (s *Store) DueSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	return s.querySchedules(ctx, scheduleColumns+`
		FROM schedules
		WHERE enabled AND next_run_at <= $1
		ORDER BY next_run_at, id
		LIMIT $2`, now, limit)
}

func (s *Store) querySchedules(ctx context.Context, query string, args ...any) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list schedules: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		sch, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

// DeleteSchedule removes a schedule. Its runs stay, with schedule_id cleared
// by the foreign key.
func (s *Store) DeleteSchedule(ctx context.Context, id runv1.ULID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete schedule %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdvanceSchedule records a fire that admitted no run — skipped by the
// concurrency policy, missed past its deadline, refused by admission — and
// moves the schedule to its next slot. It is fenced like a fire that does
// admit one, and reports ErrScheduleSettled when it loses.
func (s *Store) AdvanceSchedule(ctx context.Context, id runv1.ULID, fire ScheduleFire, outcome, reason string) error {
	return advanceSchedule(ctx, s.db, id, fire, outcome, reason)
}

// LiveRunsOfSchedule lists the schedule's runs that have not ended.
func (s *Store) LiveRunsOfSchedule(ctx context.Context, id runv1.ULID) ([]runv1.ULID, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE schedule_id = $1 AND status NOT IN `+terminalStatuses+`
		ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: live runs of schedule %s: %w", id, err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// linkScheduleFire is the fence, inside the run's own insert.
//
// It runs before the run row is written. The UPDATE takes the schedule's row
// lock, so a DELETE racing the fire waits for this transaction and then clears
// the new run's schedule_id, rather than failing the insert on the foreign key.
func linkScheduleFire(ctx context.Context, tx *sql.Tx, id runv1.ULID, fire ScheduleFire) error {
	return advanceSchedule(ctx, tx, id, fire, ScheduleSubmitted, "")
}

// recordScheduleRun points the schedule at the run its fire admitted. A second
// statement because the run has to exist for the foreign key first.
func recordScheduleRun(ctx context.Context, tx *sql.Tx, id, runID runv1.ULID) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE schedules SET last_run_id = $2 WHERE id = $1`, id, runID); err != nil {
		return fmt.Errorf("store: record run %s on schedule %s: %w", runID, id, err)
	}
	return nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func advanceSchedule(ctx context.Context, q execer, id runv1.ULID, fire ScheduleFire, outcome, reason string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE schedules SET
			next_run_at   = $4,
			last_fired_at = $5,
			last_outcome  = $6,
			last_error    = $7
		WHERE id = $1 AND enabled AND next_run_at = $2 AND revision = $3`,
		id, fire.Expected, fire.Revision, fire.Next, fire.At, outcome, nullString(truncate(reason, 4096)))
	if err != nil {
		return fmt.Errorf("store: advance schedule %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrScheduleSettled
	}
	return nil
}

func scanSchedule(row rowScanner) (Schedule, error) {
	var (
		sch      Schedule
		deadline sql.NullInt32
		next     sql.NullTime
		fired    sql.NullTime
		lastRun  sql.NullString
		outcome  sql.NullString
		lastErr  sql.NullString
	)
	err := row.Scan(&sch.ID, &sch.Name, &sch.Description, &sch.Cron, &sch.Timezone, &sch.Enabled,
		&sch.ConcurrencyPolicy, &deadline, &sch.Run, &next, &sch.Revision,
		&fired, &lastRun, &outcome, &lastErr,
		&sch.CreatedBy, &sch.CreatedAt, &sch.UpdatedBy, &sch.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Schedule{}, err
		}
		return Schedule{}, fmt.Errorf("store: scan schedule: %w", err)
	}
	sch.StartingDeadlineSeconds = int32Ptr(deadline)
	sch.NextRunAt = timePtr(next)
	sch.LastFiredAt = timePtr(fired)
	sch.LastRunID = runv1.ULID(lastRun.String)
	sch.LastOutcome = outcome.String
	sch.LastError = lastErr.String
	return sch, nil
}
