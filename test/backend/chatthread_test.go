package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/automagicops/haliphron/backend/app"
)

// A thread keeps listening: once the bot has been asked in a thread, the one
// person talking to it there can go on without a mention for the window, and
// a second person, a message naming someone else, or the window passing in
// silence all mean the bot waits to be named again.

const mmThreadRoot = "thread00000000000000000001"

func listening(c *app.ChatConfig) { c.ThreadWindow = 4 * time.Hour }

// say is a person's post in the thread, seeded on the server the way it would
// be by the time the bot reads the thread, and the message the bot is handed.
func say(f *fakeMattermost, id, user, text string, at time.Time) app.ChatMessage {
	f.Seed(mmPost{ID: id, UserID: user, ChannelID: mmChannel, RootID: mmThreadRoot, Message: text,
		CreateAt: at.UnixMilli()})
	return app.ChatMessage{
		ID: id, ChannelID: mmChannel, RootID: mmThreadRoot, UserID: user, Username: f.username(user),
		Text: text, CreatedAt: time.UnixMilli(at.UnixMilli()),
	}
}

// askInThread is alice opening the conversation: her post is the thread's
// root, and it names the bot.
func askInThread(t *testing.T, h *harness, f *fakeMattermost, bot *chatReplica, at time.Time) {
	t.Helper()
	f.Seed(mmPost{ID: mmThreadRoot, UserID: "user-alice", ChannelID: mmChannel,
		Message: "@devops_duty why is CI red?", CreateAt: at.UnixMilli()})
	m := mention(mmThreadRoot, "why is CI red?")
	m.CreatedAt = time.UnixMilli(at.UnixMilli())
	if got, err := bot.Chat.Handle(context.Background(), m); err != nil || got != app.ChatStarted {
		t.Fatalf("the opening mention: %s %v", got, err)
	}
}

func handle(t *testing.T, bot *chatReplica, m app.ChatMessage) app.ChatOutcome {
	t.Helper()
	got, err := bot.Chat.Handle(context.Background(), m)
	if err != nil {
		t.Fatalf("handle %s: %v", m.ID, err)
	}
	return got
}

func (h *harness) chatRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.Store.DB().QueryRow(`SELECT count(*) FROM chat_triggers`).Scan(&n); err != nil {
		t.Fatalf("count chat rows: %v", err)
	}
	return n
}

// age moves every row of the thread into the past, as if the bot had last
// acted there that long ago.
func (h *harness) age(t *testing.T, claimedAgo, repliedAgo time.Duration) {
	t.Helper()
	if _, err := h.Store.DB().Exec(`
		UPDATE chat_triggers
		   SET claimed_at = now() - make_interval(secs => $1),
		       replied_at = CASE WHEN $2 > 0 THEN now() - make_interval(secs => $2) END
		 WHERE thread_id = $3`,
		claimedAgo.Seconds(), repliedAgo.Seconds(), mmThreadRoot); err != nil {
		t.Fatalf("age the thread: %v", err)
	}
}

func TestOnePersonTalkingToTheBotInAThreadNeedsNoMentionUntilTheWindowPasses(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f, listening)
	start := time.Now().Add(-10 * time.Minute)
	askInThread(t, h, f, bot, start)

	if got := handle(t, bot, say(f, "follow00000000000000000001", "user-alice", "and on staging?",
		start.Add(time.Minute))); got != app.ChatStarted {
		t.Fatalf("a follow-up without a mention was %s, want started", got)
	}
	runs := h.runs(t)
	if len(runs) != 2 {
		t.Fatalf("%d runs, want 2", len(runs))
	}
	carried := false
	for _, r := range runs {
		prompt := h.prompt(t, r.ID)
		carried = carried || strings.HasSuffix(prompt, "and on staging?\n") && strings.Contains(prompt, "why is CI red?")
	}
	if !carried {
		t.Error("the follow-up's run does not carry the thread")
	}

	// Measured from the answer, not the question: a run that took hours to
	// answer leaves the whole window after its answer.
	h.age(t, 5*time.Hour, time.Hour)
	if got := handle(t, bot, say(f, "follow00000000000000000002", "user-alice", "and on prod?",
		start.Add(2*time.Minute))); got != app.ChatStarted {
		t.Fatalf("a follow-up an hour after the answer was %s, want started", got)
	}

	h.age(t, 6*time.Hour, 4*time.Hour+time.Minute)
	rows := h.chatRows(t)
	if got := handle(t, bot, say(f, "follow00000000000000000003", "user-alice", "one more thing",
		start.Add(3*time.Minute))); got != app.ChatIgnored {
		t.Fatalf("a follow-up after the window passed in silence was %s, want ignored", got)
	}
	if n := h.chatRows(t); n != rows {
		t.Errorf("an ignored follow-up left a claim: %d rows, want %d", n, rows)
	}

	m := say(f, "follow00000000000000000004", "user-alice", "@devops_duty one more thing", start.Add(4*time.Minute))
	m.Text, m.MentionsBot = "one more thing", true
	if got := handle(t, bot, m); got != app.ChatStarted {
		t.Errorf("a mention after the window was %s, want started", got)
	}
}

func TestOnceASecondPersonPostsInTheThreadTheBotWaitsForAMention(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f, listening)
	start := time.Now().Add(-10 * time.Minute)
	askInThread(t, h, f, bot, start)

	if got := handle(t, bot, say(f, "bob0000000000000000000001", "user-bob", "I saw that too",
		start.Add(time.Minute))); got != app.ChatIgnored {
		t.Errorf("the second person's post was %s, want ignored", got)
	}
	rows := h.chatRows(t)
	if got := handle(t, bot, say(f, "follow00000000000000000001", "user-alice", "bob, which job?",
		start.Add(2*time.Minute))); got != app.ChatIgnored {
		t.Fatalf("alice's post after bob joined was %s, want ignored", got)
	}
	if n := h.chatRows(t); n != rows {
		t.Errorf("the released claim is still there: %d rows, want %d", n, rows)
	}
	// Nothing is owed for a released claim: no "mention me again" under a
	// message that asked for nothing, now or once the orphan grace passes.
	if _, err := h.Store.DB().Exec(`UPDATE chat_triggers SET next_attempt_at = now() - interval '1 minute'
		WHERE run_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	bot.Deliver(t)
	if posts := f.postsOfKind(app.ChatPostRefusal); len(posts) != 0 {
		t.Errorf("the bot answered a message that did not name it: %+v", posts)
	}

	m := say(f, "follow00000000000000000002", "user-alice", "@devops_duty check the job bob means",
		start.Add(3*time.Minute))
	m.Text, m.MentionsBot = "check the job bob means", true
	if got := handle(t, bot, m); got != app.ChatStarted {
		t.Errorf("a mention in a shared thread was %s, want started", got)
	}
	if n := len(h.runs(t)); n != 2 {
		t.Errorf("%d runs, want 2", n)
	}
}

func TestAThreadSomeoneElseStartedNeedsAMentionEveryTime(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f, listening)
	start := time.Now().Add(-10 * time.Minute)

	f.Seed(mmPost{ID: mmThreadRoot, UserID: "user-bob", ChannelID: mmChannel,
		Message: "prod-eu returns 502", CreateAt: start.UnixMilli()})
	m := say(f, "ask0000000000000000000001", "user-alice", "@devops_duty find the cause", start.Add(time.Minute))
	m.Text, m.MentionsBot = "find the cause", true
	if got := handle(t, bot, m); got != app.ChatStarted {
		t.Fatalf("the mention was %s", got)
	}
	if got := handle(t, bot, say(f, "follow00000000000000000001", "user-alice", "and on staging?",
		start.Add(2*time.Minute))); got != app.ChatIgnored {
		t.Errorf("a follow-up in bob's thread was %s, want ignored", got)
	}
}

func TestAThreadUnderAnAlertListensAndANameInTheMessageDoesNot(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f, listening)
	start := time.Now().Add(-10 * time.Minute)

	// An incoming webhook posts under its creator's account; it is still not
	// a second person.
	f.Seed(mmPost{ID: mmThreadRoot, UserID: "user-bob", ChannelID: mmChannel,
		Message: "FIRING: KubePodCrashLooping", CreateAt: start.UnixMilli(),
		Props: map[string]any{"from_webhook": "true"}})
	m := say(f, "ask0000000000000000000001", "user-alice", "@devops_duty look", start.Add(time.Minute))
	m.Text, m.MentionsBot = "look", true
	if got := handle(t, bot, m); got != app.ChatStarted {
		t.Fatalf("the mention was %s", got)
	}

	named := say(f, "follow00000000000000000001", "user-alice", "@bob is this yours?", start.Add(2*time.Minute))
	named.MentionsOthers = true
	if got := handle(t, bot, named); got != app.ChatIgnored {
		t.Errorf("a follow-up naming bob was %s, want ignored", got)
	}
	if got := handle(t, bot, say(f, "follow00000000000000000002", "user-alice", "restart it",
		start.Add(3*time.Minute))); got != app.ChatStarted {
		t.Errorf("a follow-up under an alert was %s, want started", got)
	}
}

func TestAThreadDoesNotListenWithTheWindowSwitchedOff(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	start := time.Now().Add(-10 * time.Minute)
	askInThread(t, h, f, bot, start)

	if got := handle(t, bot, say(f, "follow00000000000000000001", "user-alice", "and on staging?",
		start.Add(time.Minute))); got != app.ChatIgnored {
		t.Errorf("a follow-up with the window off was %s, want ignored", got)
	}
}

func TestAFollowUpOverTheWebSocketStartsOneRunAcrossReplicas(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	one, two := h.chatReplica(t, f, listening), h.chatReplica(t, f, listening)
	one.Connect(t, h)
	two.Connect(t, h)

	f.Emit(mmPost{ID: mmThreadRoot, UserID: "user-alice", ChannelID: mmChannel,
		Message: "@devops_duty rotate the certs"}, "O", []string{mmBotID})
	for _, r := range []*chatReplica{one, two} {
		r.Outcome(t)
	}
	f.Emit(mmPost{ID: "follow00000000000000000001", UserID: "user-alice", ChannelID: mmChannel,
		RootID: mmThreadRoot, Message: "on staging too"}, "O", nil)
	outcomes := map[app.ChatOutcome]int{}
	for _, r := range []*chatReplica{one, two} {
		outcomes[r.Outcome(t)]++
	}
	if outcomes[app.ChatStarted] != 1 || outcomes[app.ChatDuplicate] != 1 {
		t.Errorf("follow-up outcomes = %v, want one started and one duplicate", outcomes)
	}

	// Typed by a person, a name in the text is decoded as one: the bot
	// stays out of a message addressed to bob.
	f.Emit(mmPost{ID: "follow00000000000000000002", UserID: "user-alice", ChannelID: mmChannel,
		RootID: mmThreadRoot, Message: "@bob can you approve the PR?"}, "O", nil)
	for _, r := range []*chatReplica{one, two} {
		if got := r.Outcome(t); got != app.ChatIgnored {
			t.Errorf("a follow-up naming bob was %s, want ignored", got)
		}
	}
	if n := len(h.runs(t)); n != 2 {
		t.Errorf("%d runs, want 2", n)
	}
}

// Message seeds a message and sends it as the channel message event an app
// subscribed to message.channels receives for every message in its channels.
func (f *fakeSlack) Message(m slMessage) string {
	f.Seed(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("envelope-%d", f.seq)
	event := map[string]any{"type": "message", "channel_type": "channel", "user": m.User,
		"channel": slChannel, "ts": m.TS, "text": m.Text}
	if m.ThreadTS != "" {
		event["thread_ts"] = m.ThreadTS
	}
	raw, _ := json.Marshal(map[string]any{"type": "events_api", "envelope_id": id,
		"payload": map[string]any{"type": "event_callback", "event": event}})
	for _, c := range f.conns {
		_ = c.Write(context.Background(), websocket.MessageText, raw)
	}
	return id
}

func TestASlackThreadListensToTheOnePersonTalkingToTheApp(t *testing.T) {
	h := newHarness(t)
	h.classificator(t)
	f := newFakeSlack(t)
	h.saveSlack(t, f)
	startSlack(t, h)
	eventually(t, "the Socket Mode connection", f.connected)

	f.Mention(slMessage{TS: slRoot, User: "U0ALICE", Text: "<@" + slBotUser + "> why is CI red?"})
	eventually(t, "the first run", func() bool { return len(h.runs(t)) == 1 })

	f.Message(slMessage{TS: "1700000000.000200", ThreadTS: slRoot, User: "U0ALICE", Text: "and on staging?"})
	eventually(t, "the follow-up's run", func() bool { return len(h.runs(t)) == 2 })
	for _, r := range h.runs(t) {
		if prompt := h.prompt(t, r.ID); strings.HasSuffix(prompt, "and on staging?\n") &&
			!strings.Contains(prompt, "sender: @alice\n") {
			t.Errorf("the follow-up's sender was not described:\n%s", prompt)
		}
	}

	// A message to the channel, outside the thread, is nobody's request.
	top := f.Message(slMessage{TS: "1700000000.000250", User: "U0ALICE", Text: "lunch?"})
	// Bob joins; from then on the thread needs a mention.
	f.Message(slMessage{TS: "1700000000.000300", ThreadTS: slRoot, User: "U0BOB", Text: "it is the runner"})
	last := f.Message(slMessage{TS: "1700000000.000400", ThreadTS: slRoot, User: "U0ALICE", Text: "thanks"})
	eventually(t, "the envelopes to be acknowledged", func() bool { return f.acked(top) && f.acked(last) })
	// An ignored message leaves nothing to wait on; give the workers time to
	// have started a run if they were going to.
	time.Sleep(500 * time.Millisecond)
	if n := len(h.runs(t)); n != 2 {
		t.Errorf("%d runs, want 2: a message after bob joined started one", n)
	}
}
