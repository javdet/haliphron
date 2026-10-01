package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Chat triggers: a chat message that asked for a run, and the reply the run
// owes it. The semantics are in docs/contracts/run-store.md and at the top of
// db/migrations/0007_chat_triggers.sql; the statements below are the whole of
// how the table is written, and no other path writes it.

// ChatTriggerKey identifies a chat message.
type ChatTriggerKey struct {
	Platform  string
	MessageID string
}

// ChatTrigger is a claimed message and where its reply goes.
type ChatTrigger struct {
	ChatTriggerKey
	ChannelID string
	ThreadID  string
	UserID    string
	Username  string

	// RunID and Refusal are admission's answer. Both empty on a row returned
	// by DueChatReplies means the handler that claimed it never answered.
	RunID   runv1.ULID
	Refusal string

	Attempts  int
	ClaimedAt time.Time
}

// ErrChatTriggerSettled is a run whose chat message was already answered —
// refused, or given up on by the reply loop — before admission could link it.
// It rolls the run back: the person was told to ask again, and a run they
// will never hear about is spend nobody asked for.
var ErrChatTriggerSettled = errors.New("store: the chat message was already answered")

// ClaimChatTrigger records a message before anything is done about it, and
// reports whether this caller is the one to act on it.
//
// orphanAfter is how long the claim may stay unanswered before the reply loop
// takes it as abandoned. It must comfortably exceed the time admission takes,
// thread fetch included.
func (s *Store) ClaimChatTrigger(ctx context.Context, t ChatTrigger, orphanAfter time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_triggers (platform, message_id, channel_id, thread_id, user_id, username,
		                           next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
		ON CONFLICT (tenant_id, platform, message_id) DO NOTHING`,
		t.Platform, t.MessageID, t.ChannelID, t.ThreadID, t.UserID, t.Username,
		orphanAfter.Seconds())
	if err != nil {
		return false, fmt.Errorf("store: claim chat message %s: %w", t.MessageID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// linkChatTrigger attaches a run to its message inside the run's own insert.
func linkChatTrigger(ctx context.Context, tx *sql.Tx, k ChatTriggerKey, id runv1.ULID) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE chat_triggers
		   SET run_id = $3, next_attempt_at = now()
		 WHERE platform = $1 AND message_id = $2
		   AND run_id IS NULL AND refusal IS NULL
		   AND replied_at IS NULL AND abandoned_at IS NULL`,
		k.Platform, k.MessageID, id)
	if err != nil {
		return fmt.Errorf("store: link chat message %s to run %s: %w", k.MessageID, id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrChatTriggerSettled
	}
	return nil
}

// RefuseChatTrigger records that a message started nothing, and why, and makes
// the refusal due at once. It reports false when the message already has an
// answer: a run linked by a slower admission, or an earlier refusal.
func (s *Store) RefuseChatTrigger(ctx context.Context, k ChatTriggerKey, refusal string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE chat_triggers
		   SET refusal = $3, next_attempt_at = now()
		 WHERE platform = $1 AND message_id = $2
		   AND run_id IS NULL AND refusal IS NULL`,
		k.Platform, k.MessageID, refusal)
	if err != nil {
		return false, fmt.Errorf("store: refuse chat message %s: %w", k.MessageID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DueChatReplies claims up to limit of a platform's rows whose reply can be
// attempted now, and leases each for the given duration.
//
// Only rows with something to say are selected: a refusal, an orphaned claim,
// or a run in a terminal status. A run still executing is not touched, so a
// long run costs the loop nothing per tick. The lease is a forward move of
// next_attempt_at rather than a held lock, which is what lets the post to the
// chat server happen outside any transaction and lets a second replica skip
// the row instead of waiting on it.
func (s *Store) DueChatReplies(ctx context.Context, platform string, limit int,
	lease time.Duration) ([]ChatTrigger, error) {

	rows, err := s.db.QueryContext(ctx, `
		UPDATE chat_triggers t
		   SET next_attempt_at = now() + make_interval(secs => $2)
		 WHERE (t.tenant_id, t.platform, t.message_id) IN (
		       SELECT c.tenant_id, c.platform, c.message_id
		         FROM chat_triggers c
		         LEFT JOIN runs r ON r.id = c.run_id
		        WHERE c.platform = $3
		          AND c.replied_at IS NULL AND c.abandoned_at IS NULL
		          AND c.next_attempt_at <= now()
		          AND (c.run_id IS NULL
		               OR r.status IN ('Succeeded', 'Failed', 'TimedOut', 'Cancelled'))
		        ORDER BY c.next_attempt_at
		        LIMIT $1
		          FOR UPDATE OF c SKIP LOCKED)
		RETURNING t.platform, t.message_id, t.channel_id, t.thread_id, t.user_id, t.username,
		          t.run_id, t.refusal, t.attempts, t.claimed_at`,
		limit, lease.Seconds(), platform)
	if err != nil {
		return nil, fmt.Errorf("store: claim due chat replies: %w", err)
	}
	defer rows.Close()

	var out []ChatTrigger
	for rows.Next() {
		var (
			t       ChatTrigger
			runID   sql.NullString
			refusal sql.NullString
		)
		if err := rows.Scan(&t.Platform, &t.MessageID, &t.ChannelID, &t.ThreadID, &t.UserID,
			&t.Username, &runID, &refusal, &t.Attempts, &t.ClaimedAt); err != nil {
			return nil, fmt.Errorf("store: scan chat trigger: %w", err)
		}
		t.RunID = runv1.ULID(runID.String)
		t.Refusal = refusal.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeferChatReply puts a claimed row back without counting an attempt: the run
// ended but its report is still on its way.
func (s *Store) DeferChatReply(ctx context.Context, k ChatTriggerKey, after time.Duration) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE chat_triggers SET next_attempt_at = now() + make_interval(secs => $3)
		 WHERE platform = $1 AND message_id = $2 AND replied_at IS NULL AND abandoned_at IS NULL`,
		k.Platform, k.MessageID, after.Seconds()); err != nil {
		return fmt.Errorf("store: defer chat reply %s: %w", k.MessageID, err)
	}
	return nil
}

// MarkChatReplied records the reply as posted.
func (s *Store) MarkChatReplied(ctx context.Context, k ChatTriggerKey, postID string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE chat_triggers
		   SET replied_at = now(), reply_post_id = $3, attempts = attempts + 1, last_error = NULL
		 WHERE platform = $1 AND message_id = $2 AND replied_at IS NULL AND abandoned_at IS NULL`,
		k.Platform, k.MessageID, nullString(postID)); err != nil {
		return fmt.Errorf("store: mark chat reply %s: %w", k.MessageID, err)
	}
	return nil
}

// FailChatReply records a post that did not go through: retried after
// retryAfter, or given up on for good when abandon is set.
func (s *Store) FailChatReply(ctx context.Context, k ChatTriggerKey, msg string,
	retryAfter time.Duration, abandon bool) error {

	if _, err := s.db.ExecContext(ctx, `
		UPDATE chat_triggers
		   SET attempts = attempts + 1,
		       last_error = left($3, 1024),
		       next_attempt_at = now() + make_interval(secs => $4),
		       abandoned_at = CASE WHEN $5 THEN now() END
		 WHERE platform = $1 AND message_id = $2 AND replied_at IS NULL AND abandoned_at IS NULL`,
		k.Platform, k.MessageID, msg, retryAfter.Seconds(), abandon); err != nil {
		return fmt.Errorf("store: record failed chat reply %s: %w", k.MessageID, err)
	}
	return nil
}

// PurgeChatTriggers removes answered rows that have no run to cascade with —
// refusals and orphans — once they are older than the given age. A row with a
// run lives exactly as long as the run does, retention included.
func (s *Store) PurgeChatTriggers(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM chat_triggers
		 WHERE run_id IS NULL
		   AND (replied_at IS NOT NULL OR abandoned_at IS NOT NULL)
		   AND claimed_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: purge chat triggers: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
