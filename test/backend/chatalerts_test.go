package backend

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
	"github.com/automagicops/haliphron/fake/controller"
)

// Alert replies in a team-chat thread: a receiver that names a channel owes
// its runs' answers in the thread of the channel post that announced the same
// alerts, matched by the fingerprints the post prints. The post may arrive
// after the notification, or never; the answer is posted either way.

// chatAlerts is a harness with the Mattermost integration enabled and its bot
// running, as a replica serving the save would have it.
func chatAlerts(t *testing.T) (*harness, *fakeMattermost, *chatReplica) {
	t.Helper()
	h := newHarness(t)
	h.App.UseEnvironmentChat(app.ResolvedChatIntegration{ChatIntegration: store.ChatIntegration{
		Platform: app.ChatPlatformMattermost, Enabled: true, Role: mmRole,
	}})
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	h.App.RegisterChat(bot.Chat)
	return h, f, bot
}

// alertPost is what Alertmanager's slack_configs leaves in the channel, with
// the footer that prints the fingerprints.
func alertPost(f *fakeMattermost, id string, fingerprints ...string) {
	f.Seed(mmPost{
		ID: id, UserID: "user-hookowner", ChannelID: mmChannel, Type: "slack_attachment",
		Props: map[string]any{
			"from_webhook": "true",
			"attachments": []any{map[string]any{
				"title":  "[FIRING:1] HighLatency",
				"text":   "p99 over 2s",
				"footer": "fp: " + strings.Join(fingerprints, " "),
			}},
		},
	})
}

type chatReplyView struct {
	Platform    string `json:"platform"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
}

type receiverWithChat struct {
	alertReceiverBody
	ChatReply       *chatReplyView `json:"chat_reply"`
	PlatformEnabled bool           `json:"chat_reply_platform_enabled"`
}

func (h *harness) alertRow(t *testing.T, runID string) (thread *string, found bool) {
	t.Helper()
	var f *time.Time
	if err := h.Store.DB().QueryRow(`
		SELECT thread_id, thread_found_at FROM chat_triggers WHERE message_id = $1`,
		store.ChatAlertMessagePrefix+runID).Scan(&thread, &f); err != nil {
		t.Fatalf("read the alert row of %s: %v", runID, err)
	}
	return thread, f != nil
}

// searchAgain makes every alert row's next search due, as fifteen seconds
// passing would.
func (h *harness) searchAgain(t *testing.T) {
	t.Helper()
	if _, err := h.Store.DB().Exec(`UPDATE chat_triggers SET next_search_at = now()`); err != nil {
		t.Fatal(err)
	}
}

// closeSearchWindows ends every alert row's search, as ten minutes passing
// would.
func (h *harness) closeSearchWindows(t *testing.T) {
	t.Helper()
	if _, err := h.Store.DB().Exec(`
		UPDATE chat_triggers SET thread_search_until = now() - interval '1 second'
		 WHERE thread_search_until IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
}

func search(t *testing.T, bot *chatReplica) int {
	t.Helper()
	n, err := bot.Chat.SearchThreads(context.Background())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return n
}

func TestAnAlertsAnswerIsPostedInTheThreadOfThePostThatArrivedAfterIt(t *testing.T) {
	h, f, bot := chatAlerts(t)
	c := newController(t, h)
	rc := h.alertReceiver(t, map[string]any{
		"chat_reply": map[string]any{"platform": "mattermost", "channel_id": mmChannel},
	})
	var view receiverWithChat
	if status := h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/alert-receivers/"+rc.ID, nil, &view); status != http.StatusOK {
		t.Fatalf("get receiver: %d", status)
	}
	if view.ChatReply == nil || view.ChatReply.ChannelName != "prod-alerts (Prod alerts)" || !view.PlatformEnabled {
		t.Errorf("chat_reply = %+v, enabled %v", view.ChatReply, view.PlatformEnabled)
	}

	if status, answer := h.deliver(t, rc.ID, rc.Token, firing("3f2a9c0000000001")); status != http.StatusOK ||
		answer.Outcome != store.AlertSubmitted {
		t.Fatalf("deliver: %d %+v", status, answer)
	}
	id := h.alertRuns(t, rc.ID)[0].ID
	if p := h.prompt(t, id); !strings.Contains(p, "will be posted in the thread of this alert in #prod-alerts (Prod alerts)") {
		t.Errorf("the prompt does not say where the answer goes:\n%s", p)
	}

	// The chat notification has not arrived yet.
	if n := search(t, bot); n != 0 {
		t.Fatalf("found %d posts in an empty channel", n)
	}
	alertPost(f, "alertpost00000000000000001", "3f2a9c0000000001")
	if n := search(t, bot); n != 0 {
		t.Fatal("a row was searched again before its lease ran out")
	}
	h.searchAgain(t)
	if n := search(t, bot); n != 1 {
		t.Fatalf("found %d posts, want the alert's", n)
	}
	if thread, found := h.alertRow(t, string(id)); thread == nil || *thread != "alertpost00000000000000001" || !found {
		t.Errorf("thread = %v, found %v", thread, found)
	}
	if r := f.Reactions(); len(r) != 1 || r[0].PostID != "alertpost00000000000000001" || r[0].EmojiName != "eyes" {
		t.Errorf("reactions = %+v, want :eyes: on the alert's post", r)
	}
	// Another replica that found the same post loses, and does not react.
	key := store.ChatTriggerKey{Platform: app.ChatPlatformMattermost, MessageID: store.ChatAlertMessagePrefix + string(id)}
	if won, err := h.Store.ResolveChatThread(context.Background(), key, "another"); err != nil || won {
		t.Errorf("a second resolve won=%v err=%v", won, err)
	}

	h.finish(t, c, id, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "node-3 is swapping."})
	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies, want 1", n)
	}
	reply := f.postsOfKind(app.ChatPostResult)[0]
	if reply.RootID != "alertpost00000000000000001" || reply.Message != "node-3 is swapping." {
		t.Errorf("reply %q under %q", reply.Message, reply.RootID)
	}
}

func TestAnAlertsAnswerIsHeldUntilTheWindowClosesAndThenPostedAsANewPost(t *testing.T) {
	h, f, bot := chatAlerts(t)
	c := newController(t, h)
	rc := h.alertReceiver(t, map[string]any{
		"chat_reply": map[string]any{"platform": "mattermost", "channel_id": mmChannel},
	})
	h.deliver(t, rc.ID, rc.Token, firing("3f2a9c0000000002"))
	id := h.alertRuns(t, rc.ID)[0].ID
	// A post that prints a longer fingerprint is not this alert's.
	alertPost(f, "otherpost00000000000000001", "3f2a9c00000000020")

	h.finish(t, c, id, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "Nothing to do."})
	if search(t, bot) != 0 {
		t.Fatal("a post with a different fingerprint matched")
	}
	if n := bot.Deliver(t); n != 0 {
		t.Fatal("the answer was posted while the alert's post could still arrive")
	}

	h.closeSearchWindows(t)
	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies after the window closed, want 1", n)
	}
	posts := f.postsOfKind(app.ChatPostResult)
	if len(posts) != 1 || posts[0].RootID != "" || posts[0].ChannelID != mmChannel {
		t.Fatalf("posts = %+v, want one root post in the receiver's channel", posts)
	}
	want := "Alert HighLatency: no matching post was found in this channel " +
		"(the notification must print its fingerprints).\n\nNothing to do."
	if posts[0].Message != want {
		t.Errorf("root post is %q", posts[0].Message)
	}
	if thread, found := h.alertRow(t, string(id)); thread == nil || *thread != posts[0].ID || found {
		t.Errorf("thread = %v, found %v; want the root post, not found by the search", thread, found)
	}
}

func TestADelegatedChildOfAnAlertRunRepliesInTheSameThread(t *testing.T) {
	h, f, bot := chatAlerts(t)
	c := newController(t, h)
	rc := h.alertReceiver(t, map[string]any{
		"chat_reply": map[string]any{"platform": "mattermost", "channel_id": mmChannel},
	})
	h.deliver(t, rc.ID, rc.Token, firing("3f2a9c0000000003"))
	parent := h.alertRuns(t, rc.ID)[0]

	// The child starts while the post is still being looked for.
	child := h.Submit(func(r *run.SubmitRequest) {
		r.Prompt, r.Role, r.ParentRunID, r.Depth = "check node-3", alertRole, parent.ID, parent.Depth+1
		r.CreatedBy, r.CreatedVia = "run:"+string(parent.ID), "agent"
	})
	alertPost(f, "alertpost00000000000000003", "3f2a9c0000000003")
	if search(t, bot) != 1 {
		t.Fatal("the alert's post was not found")
	}

	h.finish(t, c, parent.ID, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "Handing to the node team."})
	h.finish(t, c, child.ID, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "node-3 rebooted."})
	if n := bot.Deliver(t); n != 2 {
		t.Fatalf("delivered %d replies, want the parent's line and the child's answer", n)
	}
	for _, p := range f.postsOfKind(app.ChatPostResult) {
		if p.RootID != "alertpost00000000000000003" {
			t.Errorf("%q posted under %q, want the alert's thread", p.Message, p.RootID)
		}
	}
}

func TestAChildWhoseOriginNeverFoundAPostRepliesUnderTheFirstRootPost(t *testing.T) {
	h, f, bot := chatAlerts(t)
	c := newController(t, h)
	rc := h.alertReceiver(t, map[string]any{
		"chat_reply": map[string]any{"platform": "mattermost", "channel_id": mmChannel},
	})
	h.deliver(t, rc.ID, rc.Token, firing("3f2a9c0000000004"))
	parent := h.alertRuns(t, rc.ID)[0]
	child := h.Submit(func(r *run.SubmitRequest) {
		r.Prompt, r.Role, r.ParentRunID, r.Depth = "check node-4", alertRole, parent.ID, parent.Depth+1
		r.CreatedBy, r.CreatedVia = "run:"+string(parent.ID), "agent"
	})
	h.finish(t, c, child.ID, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "node-4 rebooted."})
	h.closeSearchWindows(t)
	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d, want the child's root post", n)
	}
	h.finish(t, c, parent.ID, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "Handed to the node team."})
	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d, want the parent's line", n)
	}
	posts := f.postsOfKind(app.ChatPostResult)
	if len(posts) != 2 || posts[0].RootID != "" || posts[1].RootID != posts[0].ID {
		t.Errorf("posts = %+v, want one root post and the parent's line under it", posts)
	}
}

func TestNoReplyIsOwedWhileTheReceiversPlatformIsSwitchedOff(t *testing.T) {
	h, _, _ := chatAlerts(t)
	rc := h.alertReceiver(t, nil)
	// Saved while Slack was on; Slack has since been switched off.
	if _, err := h.Store.DB().Exec(`
		UPDATE alert_receivers SET chat_platform = 'slack', chat_channel_id = 'C0123ABC', chat_channel_name = 'prod'
		 WHERE id = $1`, rc.ID); err != nil {
		t.Fatal(err)
	}
	var view receiverWithChat
	h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/alert-receivers/"+rc.ID, nil, &view)
	if view.ChatReply == nil || view.ChatReply.Platform != "slack" || view.PlatformEnabled {
		t.Errorf("chat_reply = %+v, enabled %v; want slack shown as switched off", view.ChatReply, view.PlatformEnabled)
	}

	if _, answer := h.deliver(t, rc.ID, rc.Token, firing("3f2a9c0000000005")); answer.Outcome != store.AlertSubmitted {
		t.Fatalf("the notification did not start its run: %+v", answer)
	}
	id := h.alertRuns(t, rc.ID)[0].ID
	if p := h.prompt(t, id); strings.Contains(p, "will be posted") {
		t.Errorf("a run that owes no reply was told where it goes:\n%s", p)
	}
	var rows int
	if err := h.Store.DB().QueryRow(`SELECT count(*) FROM chat_triggers`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d chat rows (%v), want none", rows, err)
	}

	// An edit that leaves the channel alone is not refused for it.
	var edited receiverWithChat
	if status := h.call(t, h.Token(store.ScopeAdmin), http.MethodPatch, "/api/v1/alert-receivers/"+rc.ID,
		map[string]any{"description": "edited"}, &edited); status != http.StatusOK || edited.ChatReply == nil {
		t.Errorf("an edit that left the channel alone: %d, chat_reply %+v", status, edited.ChatReply)
	}
	if status := h.call(t, h.Token(store.ScopeAdmin), http.MethodPatch, "/api/v1/alert-receivers/"+rc.ID,
		map[string]any{"chat_reply": nil}, &edited); status != http.StatusOK || edited.ChatReply != nil {
		t.Errorf("clearing the channel: %d, chat_reply %+v", status, edited.ChatReply)
	}
}

func TestAChannelIsCheckedAgainstThePlatformWhenItIsSaved(t *testing.T) {
	h, _, _ := chatAlerts(t)
	rc := h.alertReceiver(t, nil)
	for _, c := range []struct {
		reply map[string]any
		field string
		says  string
	}{
		{map[string]any{"platform": "mattermost", "channel_id": "nosuchchannel"}, "chat_reply.channel_id", "no channel"},
		{map[string]any{"platform": "mattermost", "channel_id": "channelnobot"}, "chat_reply.channel_id", "not a member"},
		{map[string]any{"platform": "slack", "channel_id": "C0123ABC"}, "chat_reply.platform", "not enabled"},
		{map[string]any{"platform": "teams", "channel_id": "C0123ABC"}, "chat_reply.platform", "not mattermost or slack"},
	} {
		var problem struct {
			Error struct {
				Message string `json:"message"`
				Field   string `json:"field"`
			} `json:"error"`
		}
		status := h.call(t, h.Token(store.ScopeAdmin), http.MethodPatch, "/api/v1/alert-receivers/"+rc.ID,
			map[string]any{"chat_reply": c.reply}, &problem)
		if status != http.StatusUnprocessableEntity || problem.Error.Field != c.field ||
			!strings.Contains(problem.Error.Message, c.says) {
			t.Errorf("%v: %d %+v, want 422 on %s saying %q", c.reply, status, problem.Error, c.field, c.says)
		}
	}
}

func TestConcurrentDeliveriesOweOneChatReplyWithTheirOneRun(t *testing.T) {
	h, _, _ := chatAlerts(t)
	rc := h.alertReceiver(t, map[string]any{
		"chat_reply": map[string]any{"platform": "mattermost", "channel_id": mmChannel},
	})
	payload := firing("3f2a9c0000000006", "3f2a9c0000000007")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.deliver(t, rc.ID, rc.Token, payload) }()
	}
	wg.Wait()

	runs := h.alertRuns(t, rc.ID)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	var (
		n     int
		runID string
		fps   string
	)
	if err := h.Store.DB().QueryRow(`
		SELECT count(*), min(run_id::text), min(alert_fingerprints::text) FROM chat_triggers`).Scan(&n, &runID, &fps); err != nil {
		t.Fatal(err)
	}
	if n != 1 || runID != string(runs[0].ID) || fps != "{3f2a9c0000000006,3f2a9c0000000007}" {
		t.Errorf("%d chat rows for run %s with %s; want one for %s carrying both alerts", n, runID, fps, runs[0].ID)
	}
}
