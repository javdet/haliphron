package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Chat triggers: a chat message that asked for a run, and the reply the run
// owes it — or, for a run admitted for an alert notification, the reply it
// owes in its receiver's channel. The semantics are in
// docs/contracts/run-store.md and at the top of
// db/migrations/0007_chat_triggers.sql and 0021_chat_alert_replies.sql; the
// statements below are the whole of how the table is written, and no other
// path writes it.

// ChatTriggerKey identifies a chat message.
type ChatTriggerKey struct {
	Platform  string
	MessageID string
}

// ChatTrigger is a claimed message and where its reply goes.
type ChatTrigger struct {
	ChatTriggerKey
	ChannelID string
	// ThreadID is the root of the thread the reply goes under. It is empty
	// only on an alert row (and its follow-ups) whose post has not been found
	// yet; DueChatReplies returns it empty only once the search window has
	// closed, and the reply is then a root post of its own.
	ThreadID string
	UserID   string
	Username string

	// ChannelName and ChannelType describe the channel to a prompt: the name a
	// person would recognise, and the platform's type letter.
	ChannelName string
	ChannelType string

	// OriginMessageID is set on a follow-up — the row of a child run, which
	// answers its ancestor's message — and names that message. Empty on a row
	// claimed from a message.
	OriginMessageID string

	// Delegated is a run that started a run of its own. Its child has a row of
	// its own and owes the person the answer, so this one posts only what the
	// run itself said, and nothing when it said nothing. Set only by
	// DueChatReplies.
	Delegated bool

	// RunID and Refusal are admission's answer. Both empty on a row returned
	// by DueChatReplies means the handler that claimed it never answered.
	RunID   runv1.ULID
	Refusal string

	Attempts  int
	ClaimedAt time.Time

	// AlertFingerprints and AlertNames are set on an alert row and its
	// follow-ups: the alerts the run carries, matched against the channel's
	// posts, and their alertname labels. ThreadSearchSince and
	// ThreadSearchUntil bound the posts searched and the time spent looking.
	AlertFingerprints []string
	AlertNames        []string
	ThreadSearchSince time.Time
	ThreadSearchUntil time.Time
}

// ChatAlertMessagePrefix starts the message_id of an alert row: the reply an
// Alertmanager notification's run owes in its receiver's channel.
const ChatAlertMessagePrefix = "alert:"

// ChatAlertReply is the reply a run admitted for an alert notification owes
// in the receiver's channel, written in the run's own insert.
//
// ReceiverName is at most 64 bytes and ChannelName at most 256, the columns'
// bounds; app cuts them.
type ChatAlertReply struct {
	Platform     string
	ChannelID    string
	ChannelName  string
	ReceiverID   runv1.ULID
	ReceiverName string
	// Fingerprints are the alerts the run carries, and Alertnames their
	// alertname labels, parallel to it.
	Fingerprints []string
	Alertnames   []string
	// SearchBefore and SearchFor bound the search for the channel post that
	// announced the alerts: posts from SearchBefore before admission, looked
	// for until SearchFor after it.
	SearchBefore time.Duration
	SearchFor    time.Duration
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
		                           channel_name, channel_type, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + make_interval(secs => $9))
		ON CONFLICT (tenant_id, platform, message_id) DO NOTHING`,
		t.Platform, t.MessageID, t.ChannelID, t.ThreadID, t.UserID, t.Username,
		t.ChannelName, t.ChannelType, orphanAfter.Seconds())
	if err != nil {
		return false, fmt.Errorf("store: claim chat message %s: %w", t.MessageID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseChatTrigger deletes a claim its handler decided was not a request
// after all: a message in a listening thread that turned out, on reading the
// thread, to be a conversation between people. It touches only a row with no
// answer yet, so it can never take back a run or a refusal. Without it the
// reply loop would take the claim for an abandoned one and post "mention me
// again" under a message that never asked for anything.
func (s *Store) ReleaseChatTrigger(ctx context.Context, k ChatTriggerKey) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM chat_triggers
		 WHERE platform = $1 AND message_id = $2
		   AND run_id IS NULL AND refusal IS NULL
		   AND replied_at IS NULL AND abandoned_at IS NULL`,
		k.Platform, k.MessageID); err != nil {
		return fmt.Errorf("store: release chat message %s: %w", k.MessageID, err)
	}
	return nil
}

// ChatThreadListening reports whether a thread may be talked to without a
// mention by the given user: the bot acted on a message in it, or answered
// one, within the window, and every message it acted on there was that user's.
//
// It is the cheap first half of the rule, read before anything is claimed.
// This table holds only the messages that were requests, so a second person
// who never addressed the bot is invisible here; the caller reads the thread
// itself for that.
func (s *Store) ChatThreadListening(ctx context.Context, platform, threadID, userID string,
	window time.Duration) (bool, error) {

	if window <= 0 || threadID == "" {
		return false, nil
	}
	var listening bool
	err := s.db.QueryRowContext(ctx, `
		SELECT coalesce(bool_and(user_id = $3), false)
		       AND coalesce(max(greatest(claimed_at, replied_at)), '-infinity')
		           > now() - make_interval(secs => $4)
		  FROM chat_triggers
		 WHERE platform = $1 AND thread_id = $2`,
		platform, threadID, userID, window.Seconds()).Scan(&listening)
	if err != nil {
		return false, fmt.Errorf("store: read chat thread %s: %w", threadID, err)
	}
	return listening, nil
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

// linkChatAlertReply records the reply a run admitted for an alert
// notification owes in its receiver's channel, inside the run's own insert. A
// plain insert: the run is new, so nothing can have claimed its row.
//
// The thread is not known yet. The row is due for a search at once, and due
// for its reply once the run ends and either the search has found the post or
// the window has closed.
func linkChatAlertReply(ctx context.Context, tx *sql.Tx, a ChatAlertReply, id runv1.ULID) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_triggers (platform, message_id, channel_id, thread_id, user_id, username,
		                           channel_name, channel_type, run_id, next_attempt_at,
		                           alert_fingerprints, alert_names,
		                           thread_search_since, thread_search_until, next_search_at)
		VALUES ($1, $2, $3, NULL, $4, $5, $6, '', $7, now(), $8::text[], $9::text[],
		        now() - make_interval(secs => $10), now() + make_interval(secs => $11), now())`,
		a.Platform, ChatAlertMessagePrefix+string(id), a.ChannelID,
		"alert-receiver:"+string(a.ReceiverID), a.ReceiverName, a.ChannelName, id,
		textArray(a.Fingerprints), textArray(a.Alertnames),
		a.SearchBefore.Seconds(), a.SearchFor.Seconds()); err != nil {
		return fmt.Errorf("store: link the chat reply of alert run %s: %w", id, err)
	}
	return nil
}

// linkChatFollowUp gives a child run the reply its parent owes, inside the
// child's own insert.
//
// The child's row copies where the parent's reply goes, and names the message
// the whole tree answers. A parent with no
// row — a run not started from chat — gives its child none, and a grandchild
// inherits through its parent's follow-up by the same statement. The row is due
// at once and selected only when the child ends, like any row with a run.
//
// The follow-up of an alert row whose post is not found yet copies the
// missing thread and the search window. It never searches itself: it reads the
// thread its origin's row finds, through DueChatReplies' join.
func linkChatFollowUp(ctx context.Context, tx *sql.Tx, parent, child runv1.ULID) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_triggers (tenant_id, platform, message_id, channel_id, thread_id, user_id,
		                           username, channel_name, channel_type,
		                           origin_message_id, run_id, next_attempt_at,
		                           alert_fingerprints, alert_names,
		                           thread_search_since, thread_search_until)
		SELECT tenant_id, platform, 'run:' || $2::text, channel_id, thread_id, user_id,
		       username, channel_name, channel_type,
		       coalesce(origin_message_id, message_id), $2::text::ulid, now(),
		       alert_fingerprints, alert_names,
		       thread_search_since, thread_search_until
		  FROM chat_triggers
		 WHERE run_id = $1`,
		parent, child); err != nil {
		return fmt.Errorf("store: link chat follow-up of %s to %s: %w", parent, child, err)
	}
	return nil
}

// ChatTriggerForRun returns the chat row a run answers — its own claim, or the
// follow-up it inherited — or ErrNotFound for a run not descended from chat.
func (s *Store) ChatTriggerForRun(ctx context.Context, id runv1.ULID) (ChatTrigger, error) {
	var (
		t      ChatTrigger
		thread sql.NullString
		origin sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT platform, message_id, channel_id, thread_id, user_id, username,
		       channel_name, channel_type, origin_message_id, claimed_at
		  FROM chat_triggers WHERE run_id = $1`, id).Scan(
		&t.Platform, &t.MessageID, &t.ChannelID, &thread, &t.UserID, &t.Username,
		&t.ChannelName, &t.ChannelType, &origin, &t.ClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatTrigger{}, ErrNotFound
	}
	if err != nil {
		return ChatTrigger{}, fmt.Errorf("store: read chat trigger of run %s: %w", id, err)
	}
	t.RunID = id
	t.ThreadID = thread.String
	t.OriginMessageID = origin.String
	return t, nil
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
//
// An alert row, and a follow-up of one, is held back until its thread is
// known — its own, or for a follow-up its origin's, which is read through the
// join so a child inserted while the search ran needs no second write — or its
// search window has closed. The thread returned is the coalesced one, empty
// only when the window closed without a match.
//
// The rows come back in the order they were claimed. RETURNING keeps no order
// of its own, and a parent's row — the classificator's line saying who takes
// the request — must not be posted after its child's answer when both are due
// in one pass. A follow-up is always inserted after its parent's row.
func (s *Store) DueChatReplies(ctx context.Context, platform string, limit int,
	lease time.Duration) ([]ChatTrigger, error) {

	rows, err := s.db.QueryContext(ctx, `
		UPDATE chat_triggers t
		   SET next_attempt_at = now() + make_interval(secs => $2)
		 WHERE (t.tenant_id, t.platform, t.message_id) IN (
		       SELECT c.tenant_id, c.platform, c.message_id
		         FROM chat_triggers c
		         LEFT JOIN runs r ON r.id = c.run_id
		         LEFT JOIN chat_triggers o
		                ON o.tenant_id = c.tenant_id AND o.platform = c.platform
		               AND o.message_id = c.origin_message_id
		        WHERE c.platform = $3
		          AND c.replied_at IS NULL AND c.abandoned_at IS NULL
		          AND c.next_attempt_at <= now()
		          AND (c.run_id IS NULL
		               OR r.status IN ('Succeeded', 'Failed', 'TimedOut', 'Cancelled'))
		          AND (coalesce(c.thread_id, o.thread_id) IS NOT NULL
		               OR c.thread_search_until <= now())
		        ORDER BY c.next_attempt_at
		        LIMIT $1
		          FOR UPDATE OF c SKIP LOCKED)
		RETURNING t.platform, t.message_id, t.channel_id,
		          coalesce(t.thread_id,
		                   (SELECT o.thread_id FROM chat_triggers o
		                     WHERE o.tenant_id = t.tenant_id AND o.platform = t.platform
		                       AND o.message_id = t.origin_message_id)),
		          t.user_id, t.username,
		          t.channel_name, t.channel_type, t.origin_message_id,
		          t.run_id, t.refusal, t.attempts, t.claimed_at,
		          t.run_id IS NOT NULL
		            AND EXISTS (SELECT 1 FROM runs c WHERE c.parent_run_id = t.run_id),
		          t.alert_fingerprints, t.alert_names,
		          t.thread_search_since, t.thread_search_until`,
		limit, lease.Seconds(), platform)
	if err != nil {
		return nil, fmt.Errorf("store: claim due chat replies: %w", err)
	}
	defer rows.Close()

	var out []ChatTrigger
	for rows.Next() {
		var (
			t       ChatTrigger
			thread  sql.NullString
			origin  sql.NullString
			runID   sql.NullString
			refusal sql.NullString
			since   sql.NullTime
			until   sql.NullTime
		)
		if err := rows.Scan(&t.Platform, &t.MessageID, &t.ChannelID, &thread, &t.UserID,
			&t.Username, &t.ChannelName, &t.ChannelType, &origin,
			&runID, &refusal, &t.Attempts, &t.ClaimedAt, &t.Delegated,
			pgArray(&t.AlertFingerprints), pgArray(&t.AlertNames), &since, &until); err != nil {
			return nil, fmt.Errorf("store: scan chat trigger: %w", err)
		}
		t.ThreadID = thread.String
		t.ThreadSearchSince, t.ThreadSearchUntil = since.Time, until.Time
		t.OriginMessageID = origin.String
		t.RunID = runv1.ULID(runID.String)
		t.Refusal = refusal.String
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ClaimedAt.Before(out[j].ClaimedAt) })
	return out, nil
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

// MarkChatRootPosted records a reply that had no thread to go under — an
// alert row, or a follow-up of one, whose search window closed without a
// match — and was posted as a root post of its own. The post becomes the
// thread: of this row, and of its origin's row when it has one and that row
// has none yet, so the rest of the tree replies under it.
func (s *Store) MarkChatRootPosted(ctx context.Context, k ChatTriggerKey, origin, postID string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE chat_triggers
			   SET replied_at = now(), reply_post_id = $3, attempts = attempts + 1, last_error = NULL,
			       thread_id = coalesce(thread_id, $3)
			 WHERE platform = $1 AND message_id = $2 AND replied_at IS NULL AND abandoned_at IS NULL`,
			k.Platform, k.MessageID, postID); err != nil {
			return fmt.Errorf("store: mark chat root post %s: %w", k.MessageID, err)
		}
		if origin == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE chat_triggers SET thread_id = $3
			 WHERE platform = $1 AND message_id = $2 AND thread_id IS NULL`,
			k.Platform, origin, postID); err != nil {
			return fmt.Errorf("store: give chat message %s the thread %s: %w", origin, postID, err)
		}
		return nil
	})
}

// ChatThreadSearch is an alert row whose channel post is being looked for.
type ChatThreadSearch struct {
	ChatTriggerKey
	ChannelID    string
	RunID        runv1.ULID
	Fingerprints []string
	// Since is the earliest post that can match, and Until when the search
	// gives up. AdmittedAt is the run's admission, which the nearest match is
	// measured from.
	Since      time.Time
	Until      time.Time
	AdmittedAt time.Time
}

// DueChatThreadSearches claims up to limit of a platform's alert rows whose
// post is still to be found and whose next search is due, and leases each for
// the given duration — which is also when it is searched again. Follow-ups
// never search: they read their origin's thread.
func (s *Store) DueChatThreadSearches(ctx context.Context, platform string, limit int,
	lease time.Duration) ([]ChatThreadSearch, error) {

	rows, err := s.db.QueryContext(ctx, `
		UPDATE chat_triggers t
		   SET next_search_at = now() + make_interval(secs => $2)
		 WHERE (t.tenant_id, t.platform, t.message_id) IN (
		       SELECT c.tenant_id, c.platform, c.message_id
		         FROM chat_triggers c
		        WHERE c.platform = $3
		          AND c.thread_id IS NULL
		          AND c.origin_message_id IS NULL
		          AND c.message_id LIKE 'alert:%'
		          AND c.replied_at IS NULL AND c.abandoned_at IS NULL
		          AND c.thread_search_until > now()
		          AND c.next_search_at <= now()
		        ORDER BY c.next_search_at
		        LIMIT $1
		          FOR UPDATE OF c SKIP LOCKED)
		RETURNING t.platform, t.message_id, t.channel_id, t.run_id, t.alert_fingerprints,
		          t.thread_search_since, t.thread_search_until, t.claimed_at`,
		limit, lease.Seconds(), platform)
	if err != nil {
		return nil, fmt.Errorf("store: claim due chat thread searches: %w", err)
	}
	defer rows.Close()

	var out []ChatThreadSearch
	for rows.Next() {
		var (
			c     ChatThreadSearch
			runID sql.NullString
		)
		if err := rows.Scan(&c.Platform, &c.MessageID, &c.ChannelID, &runID, pgArray(&c.Fingerprints),
			&c.Since, &c.Until, &c.AdmittedAt); err != nil {
			return nil, fmt.Errorf("store: scan chat thread search: %w", err)
		}
		c.RunID = runv1.ULID(runID.String)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveChatThread records the post an alert row's search found as its
// thread, and reports whether this caller is the one that set it: every
// replica may find the same post, and only the winner reacts to it.
func (s *Store) ResolveChatThread(ctx context.Context, k ChatTriggerKey, postID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE chat_triggers SET thread_id = $3, thread_found_at = now()
		 WHERE platform = $1 AND message_id = $2 AND thread_id IS NULL
		   AND replied_at IS NULL AND abandoned_at IS NULL`,
		k.Platform, k.MessageID, postID)
	if err != nil {
		return false, fmt.Errorf("store: resolve the thread of chat message %s: %w", k.MessageID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
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

// ChatAlertThreads counts the alert rows of the last day by platform and by
// what became of their search: found, not_found (the window closed and the
// answer went out as a root post, or will), and searching.
type ChatAlertThreads struct {
	Platform string
	Outcome  string
	Count    int64
}

// ChatAlertThreadUsage is what the metrics endpoint reports about the search
// for alert posts.
func (s *Store) ChatAlertThreadUsage(ctx context.Context) ([]ChatAlertThreads, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT platform,
		       CASE WHEN thread_found_at IS NOT NULL THEN 'found'
		            WHEN thread_search_until <= now() THEN 'not_found'
		            ELSE 'searching' END AS outcome,
		       count(*)
		  FROM chat_triggers
		 WHERE message_id LIKE 'alert:%' AND origin_message_id IS NULL
		   AND claimed_at > now() - interval '24 hours'
		 GROUP BY 1, 2
		 ORDER BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("store: count chat alert threads: %w", err)
	}
	defer rows.Close()
	var out []ChatAlertThreads
	for rows.Next() {
		var c ChatAlertThreads
		if err := rows.Scan(&c.Platform, &c.Outcome, &c.Count); err != nil {
			return nil, fmt.Errorf("store: scan chat alert threads: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
