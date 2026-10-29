package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/slack"
	"github.com/automagicops/haliphron/backend/store"
	"github.com/automagicops/haliphron/fake/controller"
)

// The Slack bot, end to end: an event on the Socket Mode connection becomes a
// run under the classificator, with the whole thread quoted into its prompt,
// and the ending of that run and of every run it starts becomes a reply in the
// thread.
//
// The workspace is a fake written here, for the reason the Mattermost one is:
// it is a third party's API, and the fake models the handful of Web API
// methods and the Socket Mode frames the bot uses.

const (
	slBotUser  = "UBOT0000001"
	slBotID    = "BBOT0000001"
	slBotToken = "xoxb-test-5c0ffee"
	slAppToken = "xapp-test-1deadbeef"
	slChannel  = "C0OPS00001"
	slRoot     = "1700000000.000100"
)

type slMessage struct {
	TS       string         `json:"ts"`
	ThreadTS string         `json:"thread_ts,omitempty"`
	User     string         `json:"user,omitempty"`
	BotID    string         `json:"bot_id,omitempty"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
	channel  string
}

type fakeSlack struct {
	t *testing.T
	*httptest.Server

	mu        sync.Mutex
	users     map[string]string
	messages  []slMessage
	botPosts  []slMessage
	reactions []url.Values
	acks      []string
	conns     []*websocket.Conn
	seq       int
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{t: t, users: map[string]string{"U0ALICE": "alice", "U0BOB": "bob", slBotUser: "devops_duty"}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSlack) apiURL() string { return f.URL + "/api" }

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		f.socket(w, r)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	_ = r.ParseForm()
	want := slBotToken
	if method == "apps.connections.open" {
		want = slAppToken
	}
	if r.Header.Get("Authorization") != "Bearer "+want {
		writeSlack(w, map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	form := r.PostForm

	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "auth.test":
		writeSlack(w, map[string]any{"ok": true, "user_id": slBotUser, "bot_id": slBotID, "user": "devops_duty"})
	case "apps.connections.open":
		writeSlack(w, map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.URL, "http") + "/socket"})
	case "users.info":
		writeSlack(w, map[string]any{"ok": true, "user": map[string]any{"name": f.users[form.Get("user")]}})
	case "conversations.info":
		writeSlack(w, map[string]any{"ok": true, "channel": map[string]any{"name": "ops"}})
	case "conversations.replies":
		var thread []slMessage
		for _, m := range f.messages {
			if m.channel == form.Get("channel") && (m.TS == form.Get("ts") || m.ThreadTS == form.Get("ts")) {
				thread = append(thread, m)
			}
		}
		sort.Slice(thread, func(i, j int) bool { return thread[i].TS < thread[j].TS })
		writeSlack(w, map[string]any{"ok": true, "messages": thread})
	case "chat.postMessage":
		f.seq++
		m := slMessage{TS: fmt.Sprintf("%d.%06d", time.Now().Unix(), f.seq), ThreadTS: form.Get("thread_ts"),
			BotID: slBotID, Text: form.Get("text"), channel: form.Get("channel")}
		if raw := form.Get("metadata"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &m.Metadata)
		}
		f.messages = append(f.messages, m)
		f.botPosts = append(f.botPosts, m)
		writeSlack(w, map[string]any{"ok": true, "channel": m.channel, "ts": m.TS})
	case "reactions.add":
		f.reactions = append(f.reactions, form)
		writeSlack(w, map[string]any{"ok": true})
	default:
		writeSlack(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func writeSlack(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// socket is the Socket Mode end: it greets the connection, and records every
// acknowledgement it reads back.
func (f *fakeSlack) socket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"type": "hello"})
	_ = conn.Write(r.Context(), websocket.MessageText, raw)
	for {
		_, frame, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var ack struct {
			EnvelopeID string `json:"envelope_id"`
		}
		_ = json.Unmarshal(frame, &ack)
		f.mu.Lock()
		f.acks = append(f.acks, ack.EnvelopeID)
		f.mu.Unlock()
	}
}

// Seed puts a message into the workspace without telling the bot.
func (f *fakeSlack) Seed(m slMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.channel = slChannel
	f.messages = append(f.messages, m)
}

// Mention seeds a message and sends it to the bot as an app_mention, the
// same envelope to every connection: Slack's redelivery.
func (f *fakeSlack) Mention(m slMessage) string {
	f.Seed(m)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("envelope-%d", f.seq)
	event := map[string]any{"type": "app_mention", "user": m.User, "channel": slChannel, "ts": m.TS,
		"text": m.Text}
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

func (f *fakeSlack) postsOfKind(kind string) []slMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []slMessage
	for _, m := range f.botPosts {
		if payload, _ := m.Metadata["event_payload"].(map[string]any); payload["kind"] == kind {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeSlack) connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns) > 0
}

func (f *fakeSlack) acked(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.acks {
		if a == id {
			return true
		}
	}
	return false
}

// classificator creates the role a Slack integration routes through, as the
// default seed would.
func (h *harness) classificator(t *testing.T) {
	t.Helper()
	if _, err := h.Store.UpsertRole(context.Background(), app.DefaultChatRole,
		json.RawMessage(`{"description":"routes chat requests"}`), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}
}

func (h *harness) saveSlack(t *testing.T, f *fakeSlack) {
	t.Helper()
	bot, appToken := slBotToken, slAppToken
	if _, err := h.App.PutChatIntegration(context.Background(), slack.Name, app.ChatIntegrationRequest{
		Enabled: true, URL: f.apiURL(), BotToken: &bot, AppToken: &appToken,
	}, "test"); err != nil {
		t.Fatalf("save the Slack integration: %v", err)
	}
}

func startSlack(t *testing.T, h *harness) {
	t.Helper()
	sup := slack.NewSupervisor(h.App, slog.New(h.Logs), nil).WithPollInterval(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// slackChat is the bot's rules over the fake, without a connection, for the
// tests that drive messages and the reply loop by hand.
func slackChat(t *testing.T, h *harness, f *fakeSlack) *app.Chat {
	t.Helper()
	base, _ := url.Parse(f.apiURL())
	client := slack.NewClient(slack.Config{BaseURL: base, BotToken: slBotToken, AppToken: slAppToken})
	if _, err := client.Me(context.Background()); err != nil {
		t.Fatalf("bot identity: %v", err)
	}
	return h.App.NewChat(client, app.ChatConfig{Role: app.DefaultChatRole})
}

func TestASlackMentionInAThreadStartsAClassificatorRunThatCarriesTheWholeThread(t *testing.T) {
	h := newHarness(t)
	h.classificator(t)
	f := newFakeSlack(t)
	h.saveSlack(t, f)
	startSlack(t, h)
	eventually(t, "the Socket Mode connection", f.connected)

	f.Seed(slMessage{TS: slRoot, User: "U0ALICE", Text: "the nightly backup failed &amp; nobody noticed"})
	f.Seed(slMessage{TS: "1700000000.000200", ThreadTS: slRoot, User: "U0BOB", Text: "it is the db-1 disk again"})
	envelope := f.Mention(slMessage{TS: "1700000000.000300", ThreadTS: slRoot, User: "U0ALICE",
		Text: "<@" + slBotUser + "> can you look into it?"})

	eventually(t, "the envelope to be acknowledged", func() bool { return f.acked(envelope) })
	eventually(t, "the run", func() bool { return len(h.runs(t)) == 1 })
	r := h.runs(t)[0]
	if r.Role != app.DefaultChatRole || r.CreatedVia != "slack" || r.CreatedBy != "slack:alice" {
		t.Errorf("run %s via %s by %s, want the classificator, from Slack, by alice", r.Role, r.CreatedVia, r.CreatedBy)
	}
	prompt := h.prompt(t, r.ID)
	for _, want := range []string{
		"platform: slack\n", "channel: ops, type O\n", "sender: @alice\n",
		"alice: the nightly backup failed & nobody noticed\n",
		"bob: it is the db-1 disk again\n",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if !strings.HasSuffix(prompt, "--- request from alice ---\ncan you look into it?\n") {
		t.Errorf("the request is not set apart at the end:\n%s", prompt)
	}

	if strings.Contains(prompt, "U0ALICE") || strings.Contains(prompt, "1700000000.000300") {
		t.Errorf("the prompt carries the message's identifiers:\n%s", prompt)
	}

	// The reaction is the whole acknowledgement.
	var reactions []url.Values
	eventually(t, "the reaction", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		reactions = append([]url.Values(nil), f.reactions...)
		return len(reactions) > 0
	})
	if posts := f.postsOfKind(app.ChatPostResult); len(posts) != 0 {
		t.Errorf("the bot posted before the run ended: %+v", posts)
	}
	if len(reactions) != 1 || reactions[0].Get("timestamp") != "1700000000.000300" || reactions[0].Get("name") != "eyes" {
		t.Errorf("reactions = %v", reactions)
	}

	for _, line := range h.Logs.Lines() {
		if strings.Contains(line, slBotToken) || strings.Contains(line, slAppToken) {
			t.Errorf("a token is in the log: %s", line)
		}
	}
}

func TestTheClassifiersLineAndTheAnswerOfTheRunItStartedArePostedInTheSlackThread(t *testing.T) {
	h := newHarness(t)
	h.classificator(t)
	h.chatRole(t)
	f := newFakeSlack(t)
	c := newController(t, h)
	chat := slackChat(t, h, f)
	ctx := context.Background()

	const msg = slChannel + ":1700000000.000400"
	if got, err := chat.Handle(ctx, app.ChatMessage{
		ID: msg, ChannelID: slChannel, ChannelName: "ops", ChannelType: "O", UserID: "U0ALICE",
		Username: "alice", Text: "restart the ingress on staging", CreatedAt: time.Now(), MentionsBot: true,
	}); err != nil || got != app.ChatStarted {
		t.Fatalf("handle: %s %v", got, err)
	}
	parent := h.runs(t)[0]

	// What run_agent does for the classificator holding its per-run token.
	child := h.Submit(func(r *run.SubmitRequest) {
		r.Prompt, r.Role, r.ParentRunID, r.Depth = "restart the staging ingress", mmRole, parent.ID, parent.Depth+1
		r.CreatedBy, r.CreatedVia = "run:"+string(parent.ID), "agent"
	})
	if p := h.prompt(t, child.ID); !strings.Contains(p, "platform: slack\n") ||
		!strings.Contains(p, "sender: @alice\n") {
		t.Errorf("the child was not told where the request came from:\n%s", p)
	}

	h.finish(t, c, parent.ID, controller.Outcome{ExitCode: runv1.ExitSuccess,
		Summary: "Routed to oncall: it is an ingress restart."})
	h.finish(t, c, child.ID, controller.Outcome{ExitCode: runv1.ExitSuccess,
		Summary: "**Ingress restarted.** <!channel>"})

	if n, err := chat.Deliver(ctx); err != nil || n != 2 {
		t.Fatalf("delivered %d replies (%v), want the classifier's and the child's", n, err)
	}
	results := f.postsOfKind(app.ChatPostResult)
	if len(results) != 2 {
		t.Fatalf("%d results", len(results))
	}
	if got := results[0].Text; got != "Routed to oncall: it is an ingress restart." {
		t.Errorf("the first reply is not the classifier's line alone:\n%s", got)
	}
	r := results[1]
	if strings.Contains(r.Text, string(child.ID)) || !strings.Contains(r.Text, "*Ingress restarted.*") {
		t.Errorf("the second reply is not the child's answer alone:\n%s", r.Text)
	}
	if strings.Contains(r.Text, "<!channel>") || strings.Contains(r.Text, "**") {
		t.Errorf("the reply notifies the channel or is not mrkdwn:\n%s", r.Text)
	}
	for _, r := range results {
		if r.channel != slChannel || r.ThreadTS != "1700000000.000400" {
			t.Errorf("reply posted in %s under %q, want the message's thread", r.channel, r.ThreadTS)
		}
	}
	if n, err := chat.Deliver(ctx); err != nil || n != 0 {
		t.Errorf("a second pass posted %d more (%v)", n, err)
	}
}

func TestTwoSlackReplicasGivenTheSameEventStartOneRun(t *testing.T) {
	h := newHarness(t)
	h.classificator(t)
	f := newFakeSlack(t)
	a, b := slackChat(t, h, f), slackChat(t, h, f)
	m := app.ChatMessage{ID: slChannel + ":1700000000.000500", ChannelID: slChannel, UserID: "U0ALICE",
		Username: "alice", Text: "check the backups", CreatedAt: time.Now(), MentionsBot: true}

	first, err := a.Handle(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Handle(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if first != app.ChatStarted || second != app.ChatDuplicate {
		t.Errorf("outcomes %s and %s, want one start and one duplicate", first, second)
	}
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("%d runs", n)
	}
}

func TestTheSlackIntegrationKeepsBothTokensWriteOnly(t *testing.T) {
	h := newHarness(t)
	f := newFakeSlack(t)
	h.saveSlack(t, f)

	req, _ := http.NewRequest(http.MethodGet, h.Public.URL+"/api/v1/integrations/slack", nil)
	req.Header.Set("Authorization", "Bearer "+h.Token(store.ScopeAdmin))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), slBotToken) ||
		strings.Contains(string(raw), slAppToken) {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["platform"] != "slack" || body["source"] != "database" || body["bot_token_configured"] != true ||
		body["app_token_configured"] != true || body["reply_policy"] != nil || body["run_url"] != nil ||
		body["role"] != app.DefaultChatRole || body["app_token_secret"] != app.SlackAppTokenSecret ||
		body["webhook_path"] != "" {
		t.Errorf("body = %v", body)
	}

	// Saved again without tokens: the stored ones are kept.
	if _, err := h.App.PutChatIntegration(context.Background(), slack.Name, app.ChatIntegrationRequest{
		Enabled: true, URL: f.apiURL(),
	}, "test"); err != nil {
		t.Errorf("a save without tokens lost them: %v", err)
	}
	resolved, ok, err := h.App.ResolveChatIntegration(context.Background(), slack.Name)
	if err != nil || !ok || resolved.BotToken != slBotToken || resolved.AppToken != slAppToken {
		t.Errorf("resolved %+v %v %v", resolved.ChatIntegration, ok, err)
	}

	// Mattermost's integration is untouched by Slack's.
	if mm, err := h.App.ChatIntegration(context.Background(), app.ChatPlatformMattermost); err != nil ||
		mm.Source != app.ChatSourceNone || mm.BotTokenConfigured {
		t.Errorf("mattermost = %+v %v", mm, err)
	}
}
