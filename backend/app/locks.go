package app

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Resource locks: agents telling each other what shared infrastructure they
// are working on.
//
// The backend's part is housekeeping and order, never judgement. It does not
// compare two locks, does not refuse a lock because of another, and does not
// hold a run back because of one; whether two locks concern the same thing is
// for the agents holding them to decide. What it does guarantee is that the
// holder is the calling run and nothing else (identity comes from the token),
// that only the holder removes a lock through an agent's path, that a lock
// dies with its run, that it expires, and that a set returns every earlier
// lock of another run — the rule that lets exactly one of two agents back off.
//
// Lock text is written by one agent and read by every other: a channel for
// prompt injection between runs. It is bounded and refused, never sanitised,
// as artifact keys are, and every rendering of it labels it as data.

// Lock text limits.
const (
	MaxLockNameBytes        = 128
	MaxLockDescriptionBytes = 1024
	// MinLockTTL is the shortest lock: shorter is a lock that expires
	// between two tool calls.
	MinLockTTL = time.Minute
)

// ErrNoRunIdentity is a lock or defer call from a credential that is not a
// run's own. The holder of a lock is the run whose token set it, and an
// operator's token names no run.
var ErrNoRunIdentity = errors.New("app: only a run's own token can hold locks or defer work")

// LockInput is a set_lock call.
type LockInput struct {
	Name        string
	Description string
	// TTL is how long the lock lives. Zero is as long as the run can, and
	// anything longer than the run's timeout is clamped to it: a lock cannot
	// outlive its run anyway.
	TTL time.Duration
}

// SetLock registers a lock for the calling run and returns it with the live
// locks of other runs that were set before it.
func (s *Service) SetLock(ctx context.Context, holder *store.Run, in LockInput) (store.Lock, []store.Lock, error) {
	if holder == nil {
		return store.Lock{}, nil, ErrNoRunIdentity
	}
	if err := validateLockText("name", in.Name, MaxLockNameBytes, true); err != nil {
		return store.Lock{}, nil, err
	}
	if err := validateLockText("description", in.Description, MaxLockDescriptionBytes, false); err != nil {
		return store.Lock{}, nil, err
	}
	if in.TTL < 0 || (in.TTL > 0 && in.TTL < MinLockTTL) {
		return store.Lock{}, nil, &run.InvalidRequestError{Field: "ttl_seconds",
			Detail: fmt.Sprintf("a lock lives at least %d seconds; omit it to hold the lock for as long as the run can live",
				int(MinLockTTL/time.Second))}
	}

	lock, earlier, err := s.store.SetLock(ctx, holder.ID, in.Name, in.Description, in.TTL)
	switch {
	case errors.Is(err, store.ErrTooManyLocks):
		return store.Lock{}, nil, &run.InvalidRequestError{Field: "name", Detail: fmt.Sprintf(
			"this run already holds %d locks, which is the limit; unset one it no longer needs", store.MaxLocksPerRun)}
	case err != nil:
		return store.Lock{}, nil, err
	}

	s.auditLock(ctx, "run:"+string(holder.ID), "agent", store.AuditLockSet, lock, map[string]any{
		"earlier": len(earlier),
	})
	return lock, earlier, nil
}

// UnsetLocks removes the calling run's locks named by id or by name, or all of
// them when neither is given. Another run's lock is never removed: the store's
// WHERE clause is on the holder, so a foreign id is simply not in the answer.
func (s *Service) UnsetLocks(ctx context.Context, holder *store.Run, ids []runv1.ULID, names []string) ([]runv1.ULID, error) {
	if holder == nil {
		return nil, ErrNoRunIdentity
	}
	for _, id := range ids {
		if !run.ValidULID(id) {
			return nil, &run.InvalidRequestError{Field: "lock_id",
				Detail: fmt.Sprintf("%q is not an identifier this system issues", id)}
		}
	}
	removed, err := s.store.UnsetLocks(ctx, holder.ID, ids, names)
	if err != nil {
		return nil, err
	}
	if len(removed) > 0 {
		ids := make([]string, 0, len(removed))
		for _, id := range removed {
			ids = append(ids, string(id))
		}
		if err := s.store.Audit(ctx, store.AuditEntry{
			Actor: "run:" + string(holder.ID), ActorKind: "agent", Action: store.AuditLockUnset,
			SubjectKind: "run", SubjectID: string(holder.ID), RunID: holder.ID,
			Payload: map[string]any{"lock_ids": ids},
		}); err != nil {
			s.log.Error("could not audit a lock release", "run", holder.ID, "error", err)
		}
	}
	return removed, nil
}

// Locks lists the live locks, optionally of one holder.
func (s *Service) Locks(ctx context.Context, f store.LockFilter) ([]store.Lock, error) {
	return s.store.ListLocks(ctx, f)
}

// ForceUnsetLock removes any run's lock. An operator's verb, admin-scoped in
// REST, and never reachable from an agent's token.
func (s *Service) ForceUnsetLock(ctx context.Context, id runv1.ULID, by, via string) error {
	if via == "agent" {
		return ErrLockFromAgent
	}
	lock, err := s.store.ForceUnsetLock(ctx, id)
	if err != nil {
		return err
	}
	s.auditLock(ctx, by, actorKindFor(via), store.AuditLockForceUnset, lock, nil)
	s.log.Info("lock removed by an operator", "lock", id, "holder", lock.RunID, "name", lock.Name, "by", by)
	return nil
}

// ErrLockFromAgent is an agent's token on an operator's lock path.
var ErrLockFromAgent = errors.New("app: a run's own token cannot remove another run's lock")

// validateLockText applies the limits to a lock's text. Refused rather than
// cleaned: text that has been silently altered is text the writer did not
// write, and the reader cannot tell.
func validateLockText(field, value string, limit int, required bool) error {
	switch {
	case required && value == "":
		return &run.InvalidRequestError{Field: field, Detail: "is required"}
	case len(value) > limit:
		return &run.InvalidRequestError{Field: field,
			Detail: fmt.Sprintf("%d bytes is over the %d-byte limit", len(value), limit)}
	case !utf8.ValidString(value):
		return &run.InvalidRequestError{Field: field, Detail: "is not valid UTF-8"}
	}
	for _, r := range value {
		if unicode.IsControl(r) || isBidiControl(r) {
			return &run.InvalidRequestError{Field: field, Detail: fmt.Sprintf(
				"contains the control character %U; write it on one line, in plain text", r)}
		}
	}
	return nil
}

// isBidiControl reports the bidirectional overrides and isolates, which make
// text render in an order other than the one it is read in.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F
}

func (s *Service) auditLock(ctx context.Context, actor, kind, action string, lock store.Lock, extra map[string]any) {
	payload := map[string]any{
		"lock_id": string(lock.ID), "name": lock.Name, "seq": lock.Seq,
		"expires_at": lock.ExpiresAt.UTC().Format(time.RFC3339),
	}
	for k, v := range extra {
		payload[k] = v
	}
	if err := s.store.Audit(ctx, store.AuditEntry{
		Actor: actor, ActorKind: kind, Action: action,
		SubjectKind: "lock", SubjectID: string(lock.ID), RunID: lock.RunID,
		Payload: payload,
	}); err != nil {
		// The lock is set; failing the call now would invite a retry the
		// agent reads as a second, different answer.
		s.log.Error("could not audit a lock", "lock", lock.ID, "action", action, "error", err)
	}
}
