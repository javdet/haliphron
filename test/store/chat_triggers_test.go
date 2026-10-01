package store

import (
	"database/sql"
	"testing"
)

// chat_triggers: the claim that turns a message delivered to every replica into
// one run, and the row that remembers the reply the run owes.

func claimMessage(t *testing.T, conn *sql.DB, messageID string) bool {
	t.Helper()
	res, err := conn.Exec(`
		INSERT INTO chat_triggers (platform, message_id, channel_id, thread_id, user_id, username,
		                           next_attempt_at)
		VALUES ('mattermost', $1, 'channel', $1, 'user', 'alice', now() + interval '5 minutes')
		ON CONFLICT (tenant_id, platform, message_id) DO NOTHING`, messageID)
	if err != nil {
		t.Fatalf("claim %s: %v", messageID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func TestAChatMessageCanBeClaimedOnlyOnce(t *testing.T) {
	t.Parallel()
	conn := newDB(t)

	if !claimMessage(t, conn, "post-1") {
		t.Fatal("the first claim lost")
	}
	if claimMessage(t, conn, "post-1") {
		t.Error("a second replica claimed the same message")
	}
	if !claimMessage(t, conn, "post-2") {
		t.Error("a different message could not be claimed")
	}
}

func TestATriggerCannotCarryBothARunAndARefusal(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	claimMessage(t, conn, "post-1")

	if _, err := conn.Exec(`UPDATE chat_triggers SET run_id = $1 WHERE message_id = 'post-1'`, f.runID); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := conn.Exec(`UPDATE chat_triggers SET refusal = 'no' WHERE message_id = 'post-1'`); err == nil {
		t.Error("a message that started a run was also refused")
	}
	if _, err := conn.Exec(`UPDATE chat_triggers SET replied_at = now(), abandoned_at = now()
		WHERE message_id = 'post-1'`); err == nil {
		t.Error("a reply was both posted and given up on")
	}
}

func TestOneRunAnswersAtMostOneMessage(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	claimMessage(t, conn, "post-1")
	claimMessage(t, conn, "post-2")

	if _, err := conn.Exec(`UPDATE chat_triggers SET run_id = $1 WHERE message_id = 'post-1'`, f.runID); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := conn.Exec(`UPDATE chat_triggers SET run_id = $1 WHERE message_id = 'post-2'`, f.runID); err == nil {
		t.Error("one run was linked to two messages")
	}
}

func TestDeletingARunDeletesWhatItOwedAChat(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	claimMessage(t, conn, "post-1")
	if _, err := conn.Exec(`UPDATE chat_triggers SET run_id = $1 WHERE message_id = 'post-1'`, f.runID); err != nil {
		t.Fatalf("link: %v", err)
	}

	if _, err := conn.Exec(`DELETE FROM run_attempts WHERE run_id = $1`, f.runID); err != nil {
		t.Fatalf("delete attempts: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM runs WHERE id = $1`, f.runID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM chat_triggers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d triggers outlived their run", n)
	}
}

func TestARunAdmittedFromMattermostSaysSo(t *testing.T) {
	t.Parallel()
	conn := newDB(t)
	f := seed(t, conn)
	if _, err := conn.Exec(`UPDATE runs SET created_via = 'mattermost' WHERE id = $1`, f.runID); err != nil {
		t.Errorf("created_via refuses mattermost: %v", err)
	}
}
