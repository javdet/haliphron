package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The scheduler: section 12.4 of the architecture.
//
// Every replica that runs the background loops runs this one too, and there is
// no leader. What keeps two replicas from admitting two runs for one slot is
// the fence: a fire advances the schedule inside the transaction that inserts
// its run, and only from the next_run_at and revision it read. The replica
// that loses the fence has its insert rolled back and says nothing. So does a
// fire that loses to an edit, which is the outcome wanted — the operator
// changed the schedule, and the fire was computed from what it used to be.
//
// A fire that admits no run advances the schedule by the same fence, so that
// "skipped" and "missed" are recorded once too.
//
// Missed slots collapse. The next fire is computed from the time of this one
// rather than from the slot that was due, so a schedule that has been unable
// to fire for a day fires once, now, and then on its next slot.

// FireResult is what one pass did.
type FireResult struct {
	Submitted int
	Skipped   int
	Missed    int
	Refused   int
	// Lost counts fires another replica, or an edit, got to first.
	Lost int
}

// dueBatch bounds one pass. A backlog larger than this is drained over the
// next passes rather than in one long one.
const dueBatch = 100

// RunScheduler fires due schedules on an interval until the context ends.
//
// The interval is the lateness a fire may have. Cron's resolution is a minute,
// so ten seconds is well inside it and costs one indexed query per tick.
func (s *Service) RunScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.FireDueSchedules(ctx, s.now()); err != nil && !ctxDone(ctx) {
				s.log.Error("scheduler pass failed", "error", err)
			}
		}
	}
}

// FireDueSchedules fires every schedule due at now. The time is a parameter so
// that tests can stand at a slot rather than wait for one; RunScheduler passes
// the clock.
//
// A schedule whose fire fails for a reason other than admission refusing it —
// the database, mostly — is logged and left due, to be tried on the next pass.
// The starting deadline, when there is one, is what stops that from becoming
// a fire hours late.
func (s *Service) FireDueSchedules(ctx context.Context, now time.Time) (FireResult, error) {
	now = now.UTC().Truncate(time.Second)

	due, err := s.store.DueSchedules(ctx, now, dueBatch)
	if err != nil {
		return FireResult{}, err
	}

	var result FireResult
	for _, sch := range due {
		outcome, err := s.fire(ctx, sch, now)
		if err != nil {
			if ctxDone(ctx) {
				return result, ctx.Err()
			}
			s.log.Error("a schedule could not be fired", "schedule", sch.ID, "name", sch.Name, "error", err)
			continue
		}
		switch outcome {
		case store.ScheduleSubmitted:
			result.Submitted++
		case store.ScheduleSkipped:
			result.Skipped++
		case store.ScheduleMissed:
			result.Missed++
		case store.ScheduleRefused:
			result.Refused++
		default:
			result.Lost++
		}
	}
	return result, nil
}

// fire handles one due schedule and returns the outcome it recorded, or ""
// when the fence was lost and the outcome is another replica's to record.
func (s *Service) fire(ctx context.Context, row store.Schedule, now time.Time) (string, error) {
	fence := store.ScheduleFire{Expected: *row.NextRunAt, Revision: row.Revision, At: now}

	sch, err := decodeSchedule(row)
	if err != nil {
		return s.advance(ctx, row, fence, now.Add(24*time.Hour), store.ScheduleRefused, err.Error())
	}
	parsed, err := ParseCron(row.Cron, row.Timezone)
	if err != nil {
		// Valid when it was saved, and not now: a timezone this build's
		// tzdata no longer has. Retried daily rather than every tick, and
		// visible on the schedule rather than only in a log.
		return s.advance(ctx, row, fence, now.Add(24*time.Hour), store.ScheduleRefused, err.Error())
	}
	fence.Next = parsed.Next(now)
	if fence.Next.IsZero() {
		return s.advance(ctx, row, fence, now.Add(24*time.Hour), store.ScheduleRefused,
			"the expression never fires again")
	}

	if d := row.StartingDeadlineSeconds; d != nil {
		late := now.Sub(fence.Expected)
		if late > time.Duration(*d)*time.Second {
			return s.advance(ctx, row, fence, fence.Next, store.ScheduleMissed, fmt.Sprintf(
				"due at %s and %s late, past the starting deadline of %ds",
				fence.Expected.Format(time.RFC3339), late.Round(time.Second), *d))
		}
	}

	var live []runv1.ULID
	if row.ConcurrencyPolicy != ConcurrencyAllow {
		if live, err = s.store.LiveRunsOfSchedule(ctx, row.ID); err != nil {
			return "", err
		}
	}
	if row.ConcurrencyPolicy == ConcurrencyForbid && len(live) > 0 {
		return s.advance(ctx, row, fence, fence.Next, store.ScheduleSkipped, fmt.Sprintf(
			"run %s is still live and the policy is Forbid", live[0]))
	}

	req := sch.Template
	req.CreatedBy = "schedule:" + string(row.ID)
	req.CreatedVia = "schedule"

	submitted, err := s.submit(ctx, req, submitLinks{schedule: row.ID, fire: &fence})
	switch {
	case errors.Is(err, store.ErrScheduleSettled):
		return "", nil
	case err != nil:
		var (
			invalid  *run.InvalidRequestError
			tooLarge *run.PromptTooLargeError
		)
		if errors.As(err, &invalid) || errors.As(err, &tooLarge) {
			// Admission refused the template: the role was deleted, a
			// default changed under it. Retrying will not help, so the slot
			// is spent and the reason is put where the operator looks.
			return s.advance(ctx, row, fence, fence.Next, store.ScheduleRefused, err.Error())
		}
		return "", err
	}

	// Replace cancels only once this fire has won. Cancelling first would let
	// a replica that lost the fence cancel the run the winner just admitted.
	if row.ConcurrencyPolicy == ConcurrencyReplace {
		for _, id := range live {
			_, err := s.Cancel(ctx, id, req.CreatedBy,
				fmt.Sprintf("replaced by run %s of schedule %s", submitted.Run.ID, row.Name))
			if err != nil && !errors.Is(err, store.ErrRunTerminal) {
				s.log.Error("could not cancel a run the schedule replaced",
					"schedule", row.ID, "run", id, "error", err)
			}
		}
	}

	s.log.Info("schedule fired", "schedule", row.ID, "name", row.Name, "run", submitted.Run.ID,
		"due", fence.Expected, "next", fence.Next)
	return store.ScheduleSubmitted, nil
}

// advance records a fire that admitted no run and moves the schedule on.
func (s *Service) advance(ctx context.Context, row store.Schedule, fence store.ScheduleFire,
	next time.Time, outcome, reason string) (string, error) {

	fence.Next = next
	err := s.store.AdvanceSchedule(ctx, row.ID, fence, outcome, reason)
	switch {
	case errors.Is(err, store.ErrScheduleSettled):
		return "", nil
	case err != nil:
		return "", err
	}
	s.log.Info("schedule fired without a run", "schedule", row.ID, "name", row.Name,
		"outcome", outcome, "reason", reason, "next", next)
	return outcome, nil
}
