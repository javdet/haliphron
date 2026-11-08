package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Deferred runs: an agent that finds the infrastructure it needs busy hands
// the rest of its task to a run that starts once the locks it names are gone,
// at a time, or both — and then finishes.
//
// The run a deferred row becomes is an ordinary child of the run that
// deferred: admitted by the same rules, counted against the same fan-out and
// depth, carrying the same chat thread. The request is checked against
// admission when the row is saved, the way a schedule is, so a bad one is
// refused to the agent now rather than discovered by a fire nobody watches.
//
// The backend fires a row when it is due and takes no view on whether the
// work still conflicts: two rows waiting on one lock both fire when it goes,
// both agents set their locks, and the write-then-judge rule makes the later
// one yield and defer again. Anything smarter would be the backend deciding
// what conflicts with what.

// Deferral limits.
const (
	MinGiveUpAfter     = time.Minute
	MaxGiveUpAfter     = 7 * 24 * time.Hour
	DefaultGiveUpAfter = 24 * time.Hour
	// MaxWaitForLocks bounds one row's wait list.
	MaxWaitForLocks = 16
)

// DeferInput is a schedule_agent call.
type DeferInput struct {
	Prompt       string
	WaitForLocks []runv1.ULID
	NotBefore    *time.Time
	GiveUpAfter  time.Duration

	Model          string
	TimeoutSeconds int32
}

// DeferredRun is a stored row with its template decoded.
type DeferredRun struct {
	store.DeferredRun
	Template run.SubmitRequest
}

// LocksAlreadyGoneError is a defer whose locks have all been released and
// which has no time to wait for: there is nothing to wait for, and the caller
// should carry on itself.
type LocksAlreadyGoneError struct{}

func (LocksAlreadyGoneError) Error() string {
	return "none of those locks is held any more, so there is nothing to wait for; continue the work yourself " +
		"(set your own lock first)"
}

// ScheduleAgent saves a deferred run for the calling run.
func (s *Service) ScheduleAgent(ctx context.Context, parent *store.Run, in DeferInput) (DeferredRun, error) {
	if parent == nil {
		return DeferredRun{}, ErrNoRunIdentity
	}
	current, err := s.store.RunByID(ctx, parent.ID)
	if err != nil {
		return DeferredRun{}, err
	}
	if isTerminal(current.Status) {
		return DeferredRun{}, store.ErrRunTerminal
	}

	giveUp := in.GiveUpAfter
	if giveUp == 0 {
		giveUp = DefaultGiveUpAfter
	}
	if giveUp < MinGiveUpAfter || giveUp > MaxGiveUpAfter {
		return DeferredRun{}, &run.InvalidRequestError{Field: "give_up_after_seconds", Detail: fmt.Sprintf(
			"between %d and %d seconds", int(MinGiveUpAfter/time.Second), int(MaxGiveUpAfter/time.Second))}
	}
	now := s.now()
	giveUpAt := now.Add(giveUp).UTC().Truncate(time.Second)
	if in.NotBefore != nil {
		at := in.NotBefore.UTC().Truncate(time.Second)
		if !at.Before(giveUpAt) {
			return DeferredRun{}, &run.InvalidRequestError{Field: "not_before",
				Detail: "is at or after the time the run gives up waiting; raise give_up_after_seconds"}
		}
		in.NotBefore = &at
	}

	waits, err := dedupeLockIDs(in.WaitForLocks)
	if err != nil {
		return DeferredRun{}, err
	}
	if len(waits) == 0 && in.NotBefore == nil {
		return DeferredRun{}, &run.InvalidRequestError{Field: "wait_for_locks",
			Detail: "name the lock ids to wait for, a not_before time, or both"}
	}
	if len(waits) > 0 {
		live, err := s.store.LiveLockIDs(ctx, waits)
		if err != nil {
			return DeferredRun{}, err
		}
		if len(live) == 0 && in.NotBefore == nil {
			return DeferredRun{}, LocksAlreadyGoneError{}
		}
		// Only the live ones are kept: a released lock is already gone, and
		// an id that never existed would be waited on as gone anyway.
		waits = live
	}

	req := run.SubmitRequest{
		Prompt:         in.Prompt,
		Model:          in.Model,
		TimeoutSeconds: in.TimeoutSeconds,
		Role:           current.Role,
		RepoURL:        current.RepoURL,
		// A continuation inherits its parent's ceiling, as any child does: an
		// agent that could defer with a fresh budget would have none.
		MaxCostUSD:  current.MaxCostUSD,
		ParentRunID: current.ID,
		Depth:       current.Depth + 1,
		CreatedBy:   "run:" + string(current.ID),
		CreatedVia:  "agent",
	}
	if _, _, err := s.prepare(ctx, req); err != nil {
		return DeferredRun{}, err
	}
	template, err := json.Marshal(templateOf(req))
	if err != nil {
		return DeferredRun{}, fmt.Errorf("encode deferred template: %w", err)
	}

	id, err := run.NewULID(now)
	if err != nil {
		return DeferredRun{}, err
	}
	row, err := s.store.InsertDeferred(ctx, store.DeferredRun{
		ID:           id,
		ParentRunID:  current.ID,
		Template:     template,
		WaitForLocks: waits,
		NotBefore:    in.NotBefore,
		GiveUpAt:     giveUpAt,
		CreatedBy:    req.CreatedBy,
	}, s.defaults.ChildLimit())
	if errors.Is(err, store.ErrTooManyChildren) {
		return DeferredRun{}, run.TooManyChildren(current.ID, s.defaults.ChildLimit())
	}
	if err != nil {
		return DeferredRun{}, err
	}

	s.auditDeferred(ctx, req.CreatedBy, "agent", store.AuditDeferredCreated, row, nil)
	s.log.Info("work deferred", "deferred", row.ID, "parent", row.ParentRunID,
		"locks", len(row.WaitForLocks), "notBefore", row.NotBefore, "giveUpAt", row.GiveUpAt)
	return decodeDeferred(row)
}

// DeferredRuns lists rows.
func (s *Service) DeferredRuns(ctx context.Context, f store.DeferredFilter) ([]DeferredRun, error) {
	rows, err := s.store.ListDeferred(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]DeferredRun, 0, len(rows))
	for _, row := range rows {
		d, err := decodeDeferred(row)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// DeferredRun reads one row.
func (s *Service) DeferredRun(ctx context.Context, id runv1.ULID) (DeferredRun, error) {
	row, err := s.store.DeferredByID(ctx, id)
	if err != nil {
		return DeferredRun{}, err
	}
	return decodeDeferred(row)
}

// ErrDeferredNotYours is an agent cancelling a deferred run another run
// created.
var ErrDeferredNotYours = errors.New("app: a run's own token cancels only the deferred runs that run created")

// CancelDeferred ends a waiting row. An agent's token reaches only the rows
// its own run created.
func (s *Service) CancelDeferred(ctx context.Context, id runv1.ULID, caller *store.Run, by, via string) (DeferredRun, error) {
	if via == "agent" {
		row, err := s.store.DeferredByID(ctx, id)
		if err != nil {
			return DeferredRun{}, err
		}
		if caller == nil || row.ParentRunID != caller.ID {
			return DeferredRun{}, ErrDeferredNotYours
		}
	}
	row, err := s.store.CancelDeferred(ctx, id, "cancelled by "+by)
	if err != nil {
		return DeferredRun{}, err
	}
	s.auditDeferred(ctx, by, actorKindFor(via), store.AuditDeferredCancelled, row, nil)
	return decodeDeferred(row)
}

// DeferredFireResult is what one pass did.
type DeferredFireResult struct {
	Submitted int
	Expired   int
	// Lost counts rows another replica, or a cancel, got to first.
	Lost int
}

// FireDueDeferred turns every due row into a run. Called from the scheduler
// loop, so there is one loop per replica and no leader: the fence in InsertRun
// decides which replica's run commits.
//
// A row admission refuses at fire time — the role was deleted, the parent's
// fan-out was spent meanwhile — cannot start by waiting longer, so it is
// expired with the reason. Any other failure (the database, mostly) leaves it
// waiting for the next pass.
func (s *Service) FireDueDeferred(ctx context.Context) (DeferredFireResult, error) {
	due, err := s.store.DueDeferred(ctx, dueBatch)
	if err != nil {
		return DeferredFireResult{}, err
	}

	var result DeferredFireResult
	for _, row := range due {
		outcome, err := s.fireDeferred(ctx, row)
		if err != nil {
			if ctxDone(ctx) {
				return result, ctx.Err()
			}
			s.log.Error("a deferred run could not be fired", "deferred", row.ID, "error", err)
			continue
		}
		switch outcome {
		case store.DeferredSubmitted:
			result.Submitted++
		case store.DeferredExpired:
			result.Expired++
		default:
			result.Lost++
		}
	}
	return result, nil
}

func (s *Service) fireDeferred(ctx context.Context, row store.DeferredRun) (string, error) {
	d, err := decodeDeferred(row)
	if err != nil {
		return s.expireDeferred(ctx, row, err.Error())
	}
	parent, err := s.store.RunByID(ctx, row.ParentRunID)
	if err != nil {
		return "", err
	}

	req := d.Template
	req.ParentRunID = parent.ID
	req.Depth = parent.Depth + 1
	req.CreatedBy = row.CreatedBy
	req.CreatedVia = "agent"

	submitted, err := s.submit(ctx, req, submitLinks{deferred: row.ID})
	switch {
	case errors.Is(err, store.ErrDeferredSettled):
		return "", nil
	case err != nil:
		var (
			invalid  *run.InvalidRequestError
			tooLarge *run.PromptTooLargeError
		)
		if errors.As(err, &invalid) || errors.As(err, &tooLarge) {
			return s.expireDeferred(ctx, row, "admission refused the run: "+err.Error())
		}
		return "", err
	}

	row.RunID = submitted.Run.ID
	row.State = store.DeferredSubmitted
	s.auditDeferred(ctx, "scheduler", "system", store.AuditDeferredFired, row, nil)
	s.log.Info("deferred run fired", "deferred", row.ID, "parent", row.ParentRunID, "run", submitted.Run.ID)
	return store.DeferredSubmitted, nil
}

func (s *Service) expireDeferred(ctx context.Context, row store.DeferredRun, reason string) (string, error) {
	ended, err := s.store.ExpireDeferredRun(ctx, row.ID, reason)
	switch {
	case errors.Is(err, store.ErrDeferredSettled):
		return "", nil
	case err != nil:
		return "", err
	}
	s.auditDeferred(ctx, "scheduler", "system", store.AuditDeferredExpired, ended, nil)
	s.log.Warn("a deferred run was expired without starting", "deferred", row.ID, "reason", reason)
	return store.DeferredExpired, nil
}

func (s *Service) auditDeferred(ctx context.Context, actor, kind, action string, row store.DeferredRun, extra map[string]any) {
	waits := make([]string, 0, len(row.WaitForLocks))
	for _, id := range row.WaitForLocks {
		waits = append(waits, string(id))
	}
	payload := map[string]any{
		"parent_run_id": string(row.ParentRunID), "wait_for_locks": waits,
		"give_up_at": row.GiveUpAt.UTC().Format(time.RFC3339), "state": row.State,
	}
	if row.RunID != "" {
		payload["run_id"] = string(row.RunID)
	}
	if row.Reason != "" {
		payload["reason"] = row.Reason
	}
	for k, v := range extra {
		payload[k] = v
	}
	if err := s.store.Audit(ctx, store.AuditEntry{
		Actor: actor, ActorKind: kind, Action: action,
		SubjectKind: "deferred_run", SubjectID: string(row.ID), RunID: row.ParentRunID,
		Payload: payload,
	}); err != nil {
		s.log.Error("could not audit a deferred run", "deferred", row.ID, "action", action, "error", err)
	}
}

func dedupeLockIDs(in []runv1.ULID) ([]runv1.ULID, error) {
	if len(in) > MaxWaitForLocks {
		return nil, &run.InvalidRequestError{Field: "wait_for_locks",
			Detail: fmt.Sprintf("%d locks is over the limit of %d", len(in), MaxWaitForLocks)}
	}
	seen := make(map[runv1.ULID]bool, len(in))
	out := make([]runv1.ULID, 0, len(in))
	for _, id := range in {
		if !run.ValidULID(id) {
			return nil, &run.InvalidRequestError{Field: "wait_for_locks",
				Detail: fmt.Sprintf("%q is not a lock id; use the lock_id from list_locks or set_lock", id)}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func decodeDeferred(row store.DeferredRun) (DeferredRun, error) {
	req, err := decodeTemplate(row.Template)
	if err != nil {
		return DeferredRun{}, fmt.Errorf("decode the template of deferred run %s: %w", row.ID, err)
	}
	return DeferredRun{DeferredRun: row, Template: req}, nil
}

func isTerminal(status string) bool {
	switch status {
	case "Succeeded", "Failed", "TimedOut", "Cancelled":
		return true
	}
	return false
}
