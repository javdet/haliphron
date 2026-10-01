package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/mattermost"
	"github.com/automagicops/haliphron/backend/store"
	"github.com/automagicops/haliphron/fake/controller"
)

// The Mattermost bot, end to end: a post on a WebSocket becomes a run under the
// configured role, and the run's ending becomes a reply in the post's thread.
//
// The server is a fake written here rather than in fake/: it is not one of the
// four contracts, it is a third party's API, and the fake models the handful of
// its endpoints the bot uses. It echoes the bot's own posts back as events,
// as the real server does, so the rule that the bot never answers itself is
// exercised by every test that posts.

const (
	mmBotID    = "botuserid0000000000000000a"
	mmBotName  = "devops_duty"
	mmToken    = "mm-bot-token-6f1d0c2a9b"
	mmRole     = "oncall"
	mmChannel  = "channel000000000000000000c"
	mmRepo     = "https://github.com/example/infra"
	mmSettleBy = 10 * time.Second
)

type mmPost struct {
	ID        string         `json:"id"`
	UserID    string         `json:"user_id"`
	ChannelID string         `json:"channel_id"`
	RootID    string         `json:"root_id"`
	Message   string         `json:"message"`
	CreateAt  int64          `json:"create_at"`
	Type      string         `json:"type,omitempty"`
	Props     map[string]any `json:"props,omitempty"`
}

type mmReaction struct {
	UserID    string `json:"user_id"`
	PostID    string `json:"post_id"`
	EmojiName string `json:"emoji_name"`
}

type fakeMattermost struct {
	t *testing.T
	*httptest.Server

	mu        sync.Mutex
	users     map[string]string
	posts     map[string]mmPost
	botPosts  []mmPost
	reactions []mmReaction
	conns     []*websocket.Conn
	seq       int
	// failPosts makes the next posts by the bot fail with this status.
	failPosts int
	failCount int
}

func newFakeMattermost(t *testing.T) *fakeMattermost {
	t.Helper()
	f := &fakeMattermost{
		t:     t,
		users: map[string]string{mmBotID: mmBotName, "user-alice": "alice", "user-bob": "bob"},
		posts: map[string]mmPost{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/users/me", f.authorised(func(w http.ResponseWriter, r *http.Request) {
		writeMM(w, http.StatusOK, map[string]string{"id": mmBotID, "username": mmBotName})
	}))
	mux.HandleFunc("POST /api/v4/users/ids", f.authorised(func(w http.ResponseWriter, r *http.Request) {
		var ids []string
		_ = json.NewDecoder(r.Body).Decode(&ids)
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []map[string]string
		for _, id := range ids {
			if name, ok := f.users[id]; ok {
				out = append(out, map[string]string{"id": id, "username": name})
			}
		}
		writeMM(w, http.StatusOK, out)
	}))
	mux.HandleFunc("GET /api/v4/posts/{id}/thread", f.authorised(func(w http.ResponseWriter, r *http.Request) {
		root := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		thread := map[string]mmPost{}
		var order []string
		for id, p := range f.posts {
			if id == root || p.RootID == root {
				thread[id] = p
				order = append(order, id)
			}
		}
		writeMM(w, http.StatusOK, map[string]any{"order": order, "posts": thread})
	}))
	mux.HandleFunc("POST /api/v4/posts", f.authorised(func(w http.ResponseWriter, r *http.Request) {
		var p mmPost
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		if f.failCount > 0 {
			f.failCount--
			status := f.failPosts
			f.mu.Unlock()
			writeMM(w, status, map[string]any{"id": "api.post.create_post.fake", "message": "refused by the test"})
			return
		}
		f.seq++
		p.ID = fmt.Sprintf("botpost%019d", f.seq)
		p.UserID = mmBotID
		p.CreateAt = time.Now().UnixMilli()
		f.posts[p.ID] = p
		f.botPosts = append(f.botPosts, p)
		f.mu.Unlock()
		writeMM(w, http.StatusCreated, p)
		// The server tells every connection about the bot's own post, the bot's
		// connection included.
		f.broadcast(p, "O", nil)
	}))
	mux.HandleFunc("POST /api/v4/reactions", f.authorised(func(w http.ResponseWriter, r *http.Request) {
		var reaction mmReaction
		_ = json.NewDecoder(r.Body).Decode(&reaction)
		f.mu.Lock()
		f.reactions = append(f.reactions, reaction)
		f.mu.Unlock()
		writeMM(w, http.StatusOK, reaction)
	}))
	mux.HandleFunc("GET /api/v4/websocket", f.authorised(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		_ = f.write(conn, map[string]any{"event": "hello", "data": map[string]any{"server_version": "10.0.0"}})
		<-conn.CloseRead(r.Context()).Done()
	}))
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMattermost) authorised(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+mmToken {
			writeMM(w, http.StatusUnauthorized, map[string]string{"id": "api.context.session_expired.app_error"})
			return
		}
		next(w, r)
	}
}

func writeMM(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeMattermost) write(conn *websocket.Conn, v any) error {
	raw, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, raw)
}

// Seed records a post as already in a thread, without telling anyone.
func (f *fakeMattermost) Seed(p mmPost) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.CreateAt == 0 {
		p.CreateAt = time.Now().UnixMilli()
	}
	f.posts[p.ID] = p
}

// Emit is a person posting: the post is stored and announced on every
// connection.
func (f *fakeMattermost) Emit(p mmPost, channelType string, mentions []string) {
	f.Seed(p)
	f.broadcast(p, channelType, mentions)
}

func (f *fakeMattermost) broadcast(p mmPost, channelType string, mentions []string) {
	if p.CreateAt == 0 {
		p.CreateAt = time.Now().UnixMilli()
	}
	encoded, _ := json.Marshal(p)
	data := map[string]any{
		"channel_type": channelType,
		"sender_name":  "@" + f.username(p.UserID),
		"post":         string(encoded),
	}
	if mentions != nil {
		m, _ := json.Marshal(mentions)
		data["mentions"] = string(m)
	}
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		_ = f.write(c, map[string]any{"event": "posted", "data": data})
	}
}

func (f *fakeMattermost) username(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[id]
}

// BotPosts is what the bot has posted, in order.
func (f *fakeMattermost) BotPosts() []mmPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mmPost(nil), f.botPosts...)
}

func (f *fakeMattermost) postsOfKind(kind string) []mmPost {
	var out []mmPost
	for _, p := range f.BotPosts() {
		if p.Props["haliphron_kind"] == kind {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeMattermost) Reactions() []mmReaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mmReaction(nil), f.reactions...)
}

// FailPosts makes the bot's next n posts fail with status.
func (f *fakeMattermost) FailPosts(n, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCount, f.failPosts = n, status
}

// chatReplica is one backend replica's bot: its client, its rules, and — once
// started — its connection.
type chatReplica struct {
	Client   *mattermost.Client
	Chat     *app.Chat
	outcomes chan app.ChatOutcome
	stop     context.CancelFunc
	done     chan struct{}
}

type mmChatConfig func(*app.ChatConfig)

func (h *harness) chatReplica(t *testing.T, f *fakeMattermost, opts ...mmChatConfig) *chatReplica {
	t.Helper()
	base, err := url.Parse(f.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := app.ChatConfig{Role: mmRole, RepoURL: mmRepo, RunURL: "https://haliphron.example/runs/{id}"}
	for _, opt := range opts {
		opt(&cfg)
	}
	client := mattermost.NewClient(mattermost.Config{BaseURL: base, Token: mmToken})
	if _, err := client.Me(context.Background()); err != nil {
		t.Fatalf("bot identity: %v", err)
	}
	return &chatReplica{
		Client:   client,
		Chat:     h.App.NewChat(client, cfg),
		outcomes: make(chan app.ChatOutcome, 64),
	}
}

// Handle records what the bot's rules did, for the tests that need to wait for
// a post to have been dealt with.
func (r *chatReplica) Handle(ctx context.Context, m app.ChatMessage) (app.ChatOutcome, error) {
	outcome, err := r.Chat.Handle(ctx, m)
	r.outcomes <- outcome
	return outcome, err
}

// Connect starts the replica's WebSocket listener and waits for it to be up.
func (r *chatReplica) Connect(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := mattermost.NewListener(r.Client, r, slog.New(h.Logs))
	r.stop, r.done = cancel, make(chan struct{})
	go func() { l.Run(ctx); close(r.done) }()
	t.Cleanup(r.Stop)
	select {
	case <-l.Connected():
	case <-time.After(mmSettleBy):
		t.Fatal("the bot did not connect")
	}
}

func (r *chatReplica) Stop() {
	if r.stop != nil {
		r.stop()
		<-r.done
		r.stop = nil
	}
}

// Outcome waits for the next post this replica handled.
func (r *chatReplica) Outcome(t *testing.T) app.ChatOutcome {
	t.Helper()
	select {
	case o := <-r.outcomes:
		return o
	case <-time.After(mmSettleBy):
		t.Fatal("no post was handled")
		return 0
	}
}

// Deliver is one pass of the reply loop.
func (r *chatReplica) Deliver(t *testing.T) int {
	t.Helper()
	n, err := r.Chat.Deliver(context.Background())
	if err != nil {
		t.Fatalf("deliver replies: %v", err)
	}
	return n
}

func (h *harness) chatRole(t *testing.T) {
	t.Helper()
	if _, err := h.Store.UpsertRole(context.Background(), mmRole,
		json.RawMessage(`{"description":"answers the on-call channel"}`), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}
}

func (h *harness) runs(t *testing.T) []store.Run {
	t.Helper()
	runs, err := h.Store.ListRuns(context.Background(), store.RunFilter{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	return runs
}

func (h *harness) prompt(t *testing.T, id runv1.ULID) string {
	t.Helper()
	var prompt string
	if err := h.Store.DB().QueryRow(`SELECT prompt FROM runs WHERE id = $1`, id).Scan(&prompt); err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	return prompt
}

// finish drives a run through a correct controller to the given outcome.
func (h *harness) finish(t *testing.T, c *controller.Controller, id runv1.ULID, outcome controller.Outcome) {
	t.Helper()
	ctx := context.Background()
	h.Sweep()
	if _, err := c.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := c.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Finish(ctx, id, outcome); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func mention(id, text string) app.ChatMessage {
	return app.ChatMessage{
		ID: id, ChannelID: mmChannel, UserID: "user-alice", Username: "alice",
		Text: text, CreatedAt: time.Now(), MentionsBot: true,
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(mmSettleBy)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAMentionStartsARunWithTheConfiguredRoleAndSaysWhereItCameFrom(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	bot.Connect(t, h)

	f.Emit(mmPost{ID: "post0000000000000000000001", UserID: "user-alice", ChannelID: mmChannel,
		Message: "@devops_duty restart the ingress on staging"}, "O", []string{mmBotID})
	// Two posts are handled: the mention, and the bot's own acknowledgement,
	// which the server echoes back while the mention's handler is still
	// returning. Either may finish first; the second must start nothing.
	outcomes := map[app.ChatOutcome]int{}
	outcomes[bot.Outcome(t)]++
	outcomes[bot.Outcome(t)]++
	if outcomes[app.ChatStarted] != 1 || outcomes[app.ChatIgnored] != 1 {
		t.Fatalf("outcomes = %v, want the mention started and the bot's own post ignored", outcomes)
	}

	runs := h.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.Role != mmRole || r.RepoURL != mmRepo {
		t.Errorf("role %q repo %q, want the bot's", r.Role, r.RepoURL)
	}
	if r.CreatedVia != "mattermost" || r.CreatedBy != "mattermost:alice" {
		t.Errorf("created %s via %s", r.CreatedBy, r.CreatedVia)
	}
	if got := h.prompt(t, r.ID); got != "restart the ingress on staging" {
		t.Errorf("prompt = %q, want the message without the mention", got)
	}

	eventually(t, "the acknowledgement", func() bool { return len(f.postsOfKind(app.ChatPostAck)) == 1 })
	ack := f.postsOfKind(app.ChatPostAck)[0]
	if ack.RootID != "post0000000000000000000001" || ack.ChannelID != mmChannel {
		t.Errorf("ack posted in %s under %q, want the trigger's thread", ack.ChannelID, ack.RootID)
	}
	if !strings.Contains(ack.Message, string(r.ID)) || !strings.Contains(ack.Message, "https://haliphron.example/runs/"+string(r.ID)) {
		t.Errorf("ack does not name the run or link to it: %q", ack.Message)
	}
	if rs := f.Reactions(); len(rs) != 1 || rs[0].EmojiName != "eyes" || rs[0].UserID != mmBotID {
		t.Errorf("reactions = %+v, want the bot's eyes", rs)
	}

	if n := len(h.runs(t)); n != 1 {
		t.Errorf("%d runs after the bot's own post, want 1", n)
	}

	// The bot's token is a credential that speaks as the bot to everyone it
	// can reach; nothing logs it, at any level.
	for _, line := range h.Logs.Lines() {
		if strings.Contains(line, mmToken) {
			t.Errorf("the bot token is in the log: %s", line)
		}
	}
}

func TestTheResultIsPostedInTheThreadOfTheMessageThatAskedForIt(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	c := newController(t, h)
	bot := h.chatReplica(t, f)
	ctx := context.Background()

	if got, err := bot.Chat.Handle(ctx, mention("post0000000000000000000002", "why is CI red?")); err != nil || got != app.ChatStarted {
		t.Fatalf("handle: %s %v", got, err)
	}
	id := h.runs(t)[0].ID

	if n := bot.Deliver(t); n != 0 {
		t.Fatalf("a reply was posted for a run that has not ended")
	}
	h.finish(t, c, id, controller.Outcome{ExitCode: runv1.ExitSuccess,
		Summary: "The lint step fails on main.go:12.", PRURL: "https://github.com/example/infra/pull/9"})

	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies, want 1", n)
	}
	results := f.postsOfKind(app.ChatPostResult)
	if len(results) != 1 {
		t.Fatalf("%d result posts, want 1", len(results))
	}
	reply := results[0]
	if reply.RootID != "post0000000000000000000002" || reply.ChannelID != mmChannel {
		t.Errorf("reply posted in %s under %q, want the trigger's thread", reply.ChannelID, reply.RootID)
	}
	for _, want := range []string{"The lint step fails on main.go:12.", "exited with code 0",
		"https://github.com/example/infra/pull/9"} {
		if !strings.Contains(reply.Message, want) {
			t.Errorf("reply is missing %q:\n%s", want, reply.Message)
		}
	}
	if reply.Props["haliphron_run_id"] != string(id) {
		t.Errorf("reply metadata = %v", reply.Props)
	}

	if n := bot.Deliver(t); n != 0 {
		t.Errorf("a second pass posted %d more replies", n)
	}
}

func TestAMentionInsideAThreadCarriesTheThreadAsContext(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	start := time.Now().Add(-time.Hour)

	f.Seed(mmPost{ID: "root000000000000000000000r", UserID: "user-bob", ChannelID: mmChannel,
		Message: "prod-eu returns 502 on /api", CreateAt: start.UnixMilli()})
	f.Seed(mmPost{ID: "reply00000000000000000000a", UserID: "user-alice", ChannelID: mmChannel,
		RootID: "root000000000000000000000r", Message: "started after the 14:00 deploy",
		CreateAt: start.Add(time.Minute).UnixMilli()})

	m := mention("ask0000000000000000000000a", "find the cause")
	m.RootID = "root000000000000000000000r"
	f.Seed(mmPost{ID: m.ID, UserID: m.UserID, ChannelID: mmChannel, RootID: m.RootID,
		Message: "@devops_duty find the cause", CreateAt: m.CreatedAt.UnixMilli()})
	if got, err := bot.Chat.Handle(context.Background(), m); err != nil || got != app.ChatStarted {
		t.Fatalf("handle: %s %v", got, err)
	}

	prompt := h.prompt(t, h.runs(t)[0].ID)
	for _, want := range []string{"bob: prod-eu returns 502 on /api", "alice: started after the 14:00 deploy",
		"request from alice", "find the cause"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Count(prompt, "find the cause") != 1 {
		t.Errorf("the request is quoted as its own context:\n%s", prompt)
	}
	acks := f.postsOfKind(app.ChatPostAck)
	if len(acks) != 1 || acks[0].RootID != m.RootID {
		t.Errorf("acks = %+v, want one under the thread's root", acks)
	}
}

func TestTheSameMessageSeenByTwoReplicasStartsOneRunAndOneAck(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	one, two := h.chatReplica(t, f), h.chatReplica(t, f)
	one.Connect(t, h)
	two.Connect(t, h)

	f.Emit(mmPost{ID: "post0000000000000000000003", UserID: "user-alice", ChannelID: mmChannel,
		Message: "@devops_duty rotate the certs"}, "O", []string{mmBotID})
	// Each replica handles two posts: the mention, and the one acknowledgement
	// the winner posted, echoed to both.
	outcomes := map[app.ChatOutcome]int{}
	for _, r := range []*chatReplica{one, two, one, two} {
		outcomes[r.Outcome(t)]++
	}
	if outcomes[app.ChatStarted] != 1 || outcomes[app.ChatDuplicate] != 1 || outcomes[app.ChatIgnored] != 2 {
		t.Errorf("outcomes = %v, want one started, one duplicate and the ack ignored twice", outcomes)
	}
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("%d runs, want 1", n)
	}
	eventually(t, "the acknowledgement", func() bool { return len(f.postsOfKind(app.ChatPostAck)) >= 1 })
	if n := len(f.postsOfKind(app.ChatPostAck)); n != 1 {
		t.Errorf("%d acknowledgements, want 1", n)
	}
	if n := len(f.Reactions()); n != 1 {
		t.Errorf("%d reactions, want 1", n)
	}
}

func TestAReplyOwedBeforeARestartIsPostedAfterIt(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	c := newController(t, h)

	before := h.chatReplica(t, f)
	if _, err := before.Chat.Handle(context.Background(), mention("post0000000000000000000004", "scale the workers")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := h.runs(t)[0].ID
	// The replica that admitted the run is gone before the run ends.
	h.finish(t, c, id, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "scaled to 6"})

	after := h.chatReplica(t, f)
	if n := after.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies after the restart, want 1", n)
	}
	results := f.postsOfKind(app.ChatPostResult)
	if len(results) != 1 || !strings.Contains(results[0].Message, "scaled to 6") {
		t.Errorf("results = %+v", results)
	}
	if n := before.Deliver(t); n != 0 {
		t.Errorf("the old replica posted the reply a second time")
	}
}

func TestAFailedRunIsReportedWithItsStatusAndNotAsASuccess(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	c := newController(t, h)
	bot := h.chatReplica(t, f)

	if _, err := bot.Chat.Handle(context.Background(), mention("post0000000000000000000005", "migrate the db")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := h.runs(t)[0].ID
	h.finish(t, c, id, controller.Outcome{ExitCode: 1, Summary: "could not reach the database"})
	if status := h.Run(id).Status; status != clusterv1.StatusFailed {
		t.Fatalf("status = %s, want Failed", status)
	}

	bot.Deliver(t)
	results := f.postsOfKind(app.ChatPostResult)
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	if msg := results[0].Message; !strings.Contains(msg, "**Failed**") || strings.Contains(msg, "code 0") {
		t.Errorf("a failure was reported as:\n%s", msg)
	}
}

func TestARunWhoseReportNeverArrivesIsAnsweredAfterTheGrace(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	c := newController(t, h)
	bot := h.chatReplica(t, f)

	if _, err := bot.Chat.Handle(context.Background(), mention("post0000000000000000000006", "check the backups")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := h.runs(t)[0].ID
	h.finish(t, c, id, controller.Outcome{ExitCode: runv1.ExitSuccess, SkipCompletion: true})

	if n := bot.Deliver(t); n != 0 {
		t.Fatalf("a reply was posted while the report could still arrive")
	}

	// The report is past its grace, and the deferral has run out.
	if _, err := h.Store.DB().Exec(`UPDATE runs SET finished_at = now() - interval '10 minutes' WHERE id = $1`, id); err != nil {
		t.Fatalf("age the run: %v", err)
	}
	if _, err := h.Store.DB().Exec(`UPDATE chat_triggers SET next_attempt_at = now()`); err != nil {
		t.Fatalf("make the reply due: %v", err)
	}
	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies after the grace, want 1", n)
	}
	if msg := f.postsOfKind(app.ChatPostResult)[0].Message; !strings.Contains(msg, "report never arrived") {
		t.Errorf("reply is:\n%s", msg)
	}
}

func TestARefusedMentionIsAnsweredAndStartsNothing(t *testing.T) {
	h := newHarness(t)
	// No role is created: the bot's role does not exist.
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)

	got, err := bot.Chat.Handle(context.Background(), mention("post0000000000000000000007", "do something"))
	if err != nil || got != app.ChatRefused {
		t.Fatalf("handle: %s %v", got, err)
	}
	if n := len(h.runs(t)); n != 0 {
		t.Errorf("%d runs started from a refused mention", n)
	}
	if n := len(f.postsOfKind(app.ChatPostAck)); n != 0 {
		t.Errorf("a refused mention was acknowledged as started")
	}

	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies, want the refusal", n)
	}
	refusals := f.postsOfKind(app.ChatPostRefusal)
	if len(refusals) != 1 || !strings.Contains(refusals[0].Message, "role") ||
		refusals[0].RootID != "post0000000000000000000007" {
		t.Errorf("refusals = %+v", refusals)
	}
}

func TestAClaimAbandonedMidAdmissionIsAnsweredRatherThanLeftSilent(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	ctx := context.Background()

	// A handler claimed the message and died before deciding anything.
	m := mention("post0000000000000000000008", "restart it")
	key := store.ChatTriggerKey{Platform: mattermost.Name, MessageID: m.ID}
	if won, err := h.Store.ClaimChatTrigger(ctx, store.ChatTrigger{
		ChatTriggerKey: key, ChannelID: mmChannel, ThreadID: m.ID, UserID: m.UserID, Username: m.Username,
	}, 0); err != nil || !won {
		t.Fatalf("claim: %v %v", won, err)
	}

	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies for the orphaned claim, want 1", n)
	}
	refusals := f.postsOfKind(app.ChatPostRefusal)
	if len(refusals) != 1 || !strings.Contains(refusals[0].Message, "mention me again") {
		t.Errorf("refusals = %+v", refusals)
	}

	// The same message arriving late is not a second chance to start a run
	// the person has already been told was not started.
	if got, err := bot.Chat.Handle(ctx, m); err != nil || got != app.ChatDuplicate {
		t.Errorf("handle after the orphan was answered: %s %v", got, err)
	}
	if n := len(h.runs(t)); n != 0 {
		t.Errorf("%d runs, want none", n)
	}
}

func TestADirectMessageNeedsNoMentionAndOtherAutomationStartsNothing(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	bot.Connect(t, h)

	f.Emit(mmPost{ID: "hook0000000000000000000001", UserID: "user-bob", ChannelID: mmChannel,
		Message: "@devops_duty deploy finished", Props: map[string]any{"from_webhook": "true"}}, "O", []string{mmBotID})
	if got := bot.Outcome(t); got != app.ChatIgnored {
		t.Errorf("a webhook's post was %s", got)
	}
	f.Emit(mmPost{ID: "chat0000000000000000000001", UserID: "user-bob", ChannelID: mmChannel,
		Message: "lunch?"}, "O", nil)
	if got := bot.Outcome(t); got != app.ChatIgnored {
		t.Errorf("a post that does not name the bot was %s", got)
	}

	f.Emit(mmPost{ID: "dm00000000000000000000001", UserID: "user-alice", ChannelID: "dm-alice",
		Message: "which pods are crashlooping?"}, "D", nil)
	// The direct message and the acknowledgement it causes, in either order.
	outcomes := map[app.ChatOutcome]int{}
	outcomes[bot.Outcome(t)]++
	outcomes[bot.Outcome(t)]++
	if outcomes[app.ChatStarted] != 1 || outcomes[app.ChatIgnored] != 1 {
		t.Fatalf("outcomes = %v, want the direct message started", outcomes)
	}
	runs := h.runs(t)
	if len(runs) != 1 || h.prompt(t, runs[0].ID) != "which pods are crashlooping?" {
		t.Errorf("runs = %+v", runs)
	}
}

func TestAPostMattermostRefusesIsRetriedAndThenAbandoned(t *testing.T) {
	h := newHarness(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	ctx := context.Background()

	// A refusal is the quickest reply to owe.
	if _, err := bot.Chat.Handle(ctx, mention("post0000000000000000000009", "x")); err != nil {
		t.Fatalf("handle: %v", err)
	}

	f.FailPosts(1, http.StatusServiceUnavailable)
	if n := bot.Deliver(t); n != 0 {
		t.Fatalf("a refused post counted as delivered")
	}
	var (
		attempts  int
		lastError string
		abandoned *time.Time
	)
	read := func() {
		t.Helper()
		if err := h.Store.DB().QueryRow(`SELECT attempts, coalesce(last_error, ''), abandoned_at
			FROM chat_triggers WHERE message_id = 'post0000000000000000000009'`).
			Scan(&attempts, &lastError, &abandoned); err != nil {
			t.Fatalf("read trigger: %v", err)
		}
	}
	read()
	if attempts != 1 || !strings.Contains(lastError, "503") || abandoned != nil {
		t.Fatalf("after a 503: attempts=%d error=%q abandoned=%v", attempts, lastError, abandoned)
	}
	if n := bot.Deliver(t); n != 0 {
		t.Errorf("the retry did not wait")
	}

	// A refusal no retry can change ends it.
	if _, err := h.Store.DB().Exec(`UPDATE chat_triggers SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	f.FailPosts(1, http.StatusForbidden)
	bot.Deliver(t)
	read()
	if abandoned == nil || attempts != 2 {
		t.Errorf("after a 403: attempts=%d abandoned=%v, want given up", attempts, abandoned)
	}
	if _, err := h.Store.DB().Exec(`UPDATE chat_triggers SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	if n := bot.Deliver(t); n != 0 || len(f.BotPosts()) != 0 {
		t.Errorf("an abandoned reply was attempted again")
	}
}

func TestDeletingARunDeletesTheReplyItOwed(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	bot := h.chatReplica(t, f)
	ctx := context.Background()

	if _, err := bot.Chat.Handle(ctx, mention("post000000000000000000000a", "something")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := h.runs(t)[0].ID
	if _, err := h.App.Cancel(ctx, id, "test", "not needed"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := h.App.Delete(ctx, id, "test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var left int
	if err := h.Store.DB().QueryRow(`SELECT count(*) FROM chat_triggers`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d chat triggers outlived their run", left)
	}
}
