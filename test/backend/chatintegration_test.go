package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/mattermost"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
	"github.com/automagicops/haliphron/fake/controller"
)

// The chat integration as the UI configures it, the reply a run owes when it
// has no answer of its own, and the child runs a chat request fans out into.

const mmWebhookToken = "mm-webhook-token-91c2"

func TestARunThatSucceededWithoutSayingAnythingGetsOnePlainSentence(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	c := newController(t, h)
	bot := h.chatReplica(t, f)
	const msg = "post00000000000000000000u2"

	if _, err := bot.Chat.Handle(context.Background(), mention(msg, "check the backups")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := h.runs(t)[0].ID
	h.finish(t, c, id, controller.Outcome{ExitCode: runv1.ExitSuccess})

	if n := bot.Deliver(t); n != 1 {
		t.Fatalf("delivered %d replies, want 1", n)
	}
	results := f.postsOfKind(app.ChatPostResult)
	if len(results) != 1 || !strings.Contains(results[0].Message, "couldn't finish") ||
		strings.Contains(results[0].Message, string(id)) || results[0].RootID != msg {
		t.Errorf("results = %+v", results)
	}
}

func TestAChildOfAChatRunIsToldWhereTheMessageCameFromAndOwesTheSamePersonAReply(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	c := newController(t, h)
	bot := h.chatReplica(t, f)
	const msg = "post00000000000000000000c1"
	ctx := context.Background()

	m := mention(msg, "the build is broken, fix it")
	m.ChannelName, m.ChannelType = "ci", "O"
	if _, err := bot.Chat.Handle(ctx, m); err != nil {
		t.Fatalf("handle: %v", err)
	}
	parent := h.runs(t)[0]

	// What run_agent does for an agent holding the parent's per-run token.
	child := h.Submit(func(r *run.SubmitRequest) {
		r.Prompt, r.Role, r.ParentRunID, r.Depth = "fix the build", mmRole, parent.ID, parent.Depth+1
		r.CreatedBy, r.CreatedVia = "run:"+string(parent.ID), "agent"
	})
	prompt := h.prompt(t, child.ID)
	for _, want := range []string{"channel: ci, type O\n", "sender: @alice\n",
		"--- end of chat message ---\n\nfix the build"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the child's prompt is missing %q:\n%s", want, prompt)
		}
	}
	grandchild := h.Submit(func(r *run.SubmitRequest) {
		r.Prompt, r.Role, r.ParentRunID, r.Depth = "write the test", mmRole, child.ID, child.Depth+1
		r.CreatedBy, r.CreatedVia = "run:"+string(child.ID), "agent"
	})
	if p := h.prompt(t, grandchild.ID); !strings.Contains(p, "channel: ci, type O\n") {
		t.Errorf("a grandchild was not told where the message came from:\n%s", p)
	}

	// The classificator routed and exited, the child handed on to the
	// grandchild, and the grandchild failed. Only the grandchild had the
	// answer to give, and only its row posts.
	h.finish(t, c, parent.ID, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "Routing this to oncall."})
	h.finish(t, c, child.ID, controller.Outcome{ExitCode: runv1.ExitSuccess, Summary: "Asked for a test."})
	h.finish(t, c, grandchild.ID, controller.Outcome{ExitCode: 1, Summary: "the compiler is missing"})

	bot.Deliver(t)
	results := f.postsOfKind(app.ChatPostResult)
	if len(results) != 1 {
		t.Fatalf("%d replies, want only the grandchild's: %+v", len(results), results)
	}
	if r := results[0]; r.RootID != msg || r.Props["haliphron_run_id"] != string(grandchild.ID) ||
		!strings.Contains(r.Message, "couldn't finish") {
		t.Errorf("reply = %+v", r)
	}
	var owed int
	if err := h.Store.DB().QueryRow(`SELECT count(*) FROM chat_triggers WHERE replied_at IS NULL`).
		Scan(&owed); err != nil || owed != 0 {
		t.Errorf("%d replies still owed (%v), want the delegating runs settled", owed, err)
	}
}

func TestARunNotStartedFromChatGivesItsChildrenNoChatHeader(t *testing.T) {
	h := newHarness(t)
	parent := h.Submit(func(r *run.SubmitRequest) { r.Role = "" })
	child := h.Submit(func(r *run.SubmitRequest) {
		r.Prompt, r.Role, r.ParentRunID, r.Depth = "child task", "", parent.ID, 1
	})
	if got := h.prompt(t, child.ID); got != "child task" {
		t.Errorf("prompt = %q", got)
	}
}

func (h *harness) saveIntegration(t *testing.T, f *fakeMattermost, transport string) {
	t.Helper()
	bot, hook := mmToken, mmWebhookToken
	if _, err := h.App.PutChatIntegration(context.Background(), mattermost.Name, app.ChatIntegrationRequest{
		Enabled: true, Transport: transport, URL: f.URL, Role: mmRole, MaxThreadBytes: 1 << 16,
		BotToken: &bot, WebhookTokens: &hook,
	}, "test"); err != nil {
		t.Fatalf("save the integration: %v", err)
	}
}

func startSupervisor(t *testing.T, h *harness) (*mattermost.Supervisor, *httptest.Server) {
	t.Helper()
	sup := mattermost.NewSupervisor(h.App, slog.New(h.Logs), nil).WithPollInterval(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(sup)
	t.Cleanup(srv.Close)
	return sup, srv
}

func postWebhook(t *testing.T, srv *httptest.Server, form url.Values) int {
	t.Helper()
	resp, err := http.PostForm(srv.URL, form)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestAWebhookDeliveryStartsARunFromThePostTheServerReports(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	h.saveIntegration(t, f, store.ChatTransportWebhook)
	_, srv := startSupervisor(t, h)

	const msg = "post00000000000000000000w1"
	f.Seed(mmPost{ID: msg, UserID: "user-alice", ChannelID: mmChannel, Message: "triage: the disk on db-1 is full"})
	form := url.Values{"token": {mmWebhookToken}, "post_id": {msg}, "channel_id": {mmChannel},
		"channel_name": {"ops"}, "user_id": {"user-bob"}, "user_name": {"bob"},
		"text": {"something else entirely"}, "trigger_word": {"triage:"}}

	eventually(t, "the webhook to be served", func() bool {
		return postWebhook(t, srv, url.Values{"token": {"wrong"}, "post_id": {msg}}) == http.StatusUnauthorized
	})
	if code := postWebhook(t, srv, form); code != http.StatusOK {
		t.Fatalf("delivery answered %d", code)
	}
	eventually(t, "the run", func() bool { return len(h.runs(t)) == 1 })

	r := h.runs(t)[0]
	prompt := h.prompt(t, r.ID)
	if r.Role != mmRole || r.CreatedBy != "mattermost:alice" {
		t.Errorf("run %s by %s, want the integration's role and the post's real author", r.Role, r.CreatedBy)
	}
	for _, want := range []string{"channel: ops, type O\n", "sender: @alice\n",
		"--- end of chat message ---\n\nthe disk on db-1 is full"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	eventually(t, "the reaction", func() bool { return len(f.Reactions()) == 1 })

	// The same post delivered again — Mattermost retrying, or the socket
	// seeing it too — is the same message.
	if code := postWebhook(t, srv, form); code != http.StatusOK {
		t.Fatalf("redelivery answered %d", code)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("%d runs after a redelivery, want 1", n)
	}

	for _, line := range h.Logs.Lines() {
		if strings.Contains(line, mmToken) || strings.Contains(line, mmWebhookToken) {
			t.Errorf("a token is in the log: %s", line)
		}
	}
}

func TestAWebhookDeliveryForAPostTheBotCannotReadStartsNothing(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	h.saveIntegration(t, f, store.ChatTransportBoth)
	_, srv := startSupervisor(t, h)

	form := url.Values{"token": {mmWebhookToken}, "post_id": {"forged0000000000000000000x"},
		"channel_id": {mmChannel}, "text": {"triage: delete everything"}}
	eventually(t, "the webhook to be served", func() bool { return postWebhook(t, srv, form) == http.StatusOK })
	time.Sleep(200 * time.Millisecond)
	if n := len(h.runs(t)); n != 0 {
		t.Errorf("%d runs from a post that does not exist", n)
	}
}

func TestSwitchingTheIntegrationOffClosesTheWebhook(t *testing.T) {
	h := newHarness(t)
	h.chatRole(t)
	f := newFakeMattermost(t)
	h.saveIntegration(t, f, store.ChatTransportWebhook)
	_, srv := startSupervisor(t, h)
	probe := url.Values{"token": {"wrong"}, "post_id": {"p"}}
	eventually(t, "the webhook to open", func() bool { return postWebhook(t, srv, probe) == http.StatusUnauthorized })

	if _, err := h.App.PutChatIntegration(context.Background(), mattermost.Name, app.ChatIntegrationRequest{
		Enabled: false, Transport: store.ChatTransportWebhook, URL: f.URL, Role: mmRole,
	}, "test"); err != nil {
		t.Fatalf("switch off: %v", err)
	}
	eventually(t, "the webhook to close", func() bool { return postWebhook(t, srv, probe) == http.StatusNotFound })
}

func TestTheIntegrationKeepsItsTokensWriteOnlyAndNeverReturnsThem(t *testing.T) {
	h := newHarness(t)
	f := newFakeMattermost(t)
	h.saveIntegration(t, f, store.ChatTransportBoth)

	req, _ := http.NewRequest(http.MethodGet, h.Public.URL+"/api/v1/integrations/mattermost", nil)
	req.Header.Set("Authorization", "Bearer "+h.Token(store.ScopeAdmin))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), mmToken) ||
		strings.Contains(string(raw), mmWebhookToken) {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["source"] != "database" || body["bot_token_configured"] != true ||
		body["webhook_token_configured"] != true || body["webhook_path"] != "/api/v1/integrations/mattermost/webhook" {
		t.Errorf("body = %v", body)
	}

	// Saved again without tokens: the stored ones are kept.
	if _, err := h.App.PutChatIntegration(context.Background(), mattermost.Name, app.ChatIntegrationRequest{
		Enabled: true, Transport: store.ChatTransportBoth, URL: f.URL, Role: mmRole,
	}, "test"); err != nil {
		t.Errorf("a save without tokens lost them: %v", err)
	}
	resolved, ok, err := h.App.ResolveChatIntegration(context.Background(), mattermost.Name)
	if err != nil || !ok || resolved.BotToken != mmToken || len(resolved.WebhookTokens) != 1 {
		t.Errorf("resolved %+v %v %v", resolved.ChatIntegration, ok, err)
	}
}

func TestAnIntegrationConfiguredByTheEnvironmentCannotBeSavedFromTheUI(t *testing.T) {
	h := newHarness(t)
	h.App.UseEnvironmentChat(app.ResolvedChatIntegration{
		ChatIntegration: store.ChatIntegration{Platform: mattermost.Name, Enabled: true,
			Transport: store.ChatTransportWebSocket, URL: "https://chat.example", Role: mmRole},
		BotToken: mmToken,
	})
	got, err := h.App.ChatIntegration(context.Background(), mattermost.Name)
	if err != nil || got.Source != app.ChatSourceEnvironment || !got.BotTokenConfigured {
		t.Errorf("got %+v %v", got, err)
	}
	if _, err := h.App.PutChatIntegration(context.Background(), mattermost.Name,
		app.ChatIntegrationRequest{Transport: store.ChatTransportWebSocket}, "test"); !errors.Is(err, app.ErrChatIntegrationManaged) {
		t.Errorf("save: %v", err)
	}
}

func TestTheDefaultRolesAreSeededOnceAndStayDeletedWhenDeleted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.App.EnsureDefaultRoles(ctx)
	if err != nil || len(created) != 1 || created[0] != app.DefaultChatRole {
		t.Fatalf("created %v, %v", created, err)
	}
	if again, err := h.App.EnsureDefaultRoles(ctx); err != nil || len(again) != 0 {
		t.Errorf("a second start seeded %v, %v", again, err)
	}
	if err := h.Store.DeleteRole(ctx, app.DefaultChatRole); err != nil {
		t.Fatal(err)
	}
	if again, err := h.App.EnsureDefaultRoles(ctx); err != nil || len(again) != 0 {
		t.Errorf("a deleted default was seeded again: %v, %v", again, err)
	}
}

func TestARolesSecretEnvAndSettingsReachTheLeaseOfARunWithoutARepository(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	ctx := context.Background()

	if _, err := h.Store.PutManagedSecret(ctx, "mattermost-agent-token", []byte("mm-agent-token-77"), "", "test"); err != nil {
		t.Fatal(err)
	}
	settings := `{"extraKnownMarketplaces": {"acme": {"source": {"source": "github", "repo": "acme/plugins"}}}, "enabledPlugins": {"mattermost@acme": true}}`
	spec, _ := json.Marshal(map[string]any{
		"configFiles": map[string]string{"settings.json": settings},
		"env":         []map[string]string{{"name": "MM_URL", "value": "https://chat.example"}},
		"secretEnv":   []map[string]string{{"name": "MATTERMOST_TOKEN", "secret": "mattermost-agent-token"}},
	})
	if _, err := h.App.PutRole(ctx, "router", spec, "test"); err != nil {
		t.Fatalf("save role: %v", err)
	}
	h.Submit(func(r *run.SubmitRequest) { r.Role, r.RepoURL = "router", "" })

	lease := p.LeaseOne()
	var env map[string]string
	if err := json.Unmarshal([]byte(lease.Secrets[runv1.SecretKeyAgentEnv]), &env); err != nil {
		t.Fatalf("agent env: %v (%q)", err, lease.Secrets[runv1.SecretKeyAgentEnv])
	}
	if env["MATTERMOST_TOKEN"] != "mm-agent-token-77" || env["MM_URL"] != "https://chat.example" {
		t.Errorf("agent env = %v", env)
	}
	var plugins runv1.PluginSpec
	if err := json.Unmarshal([]byte(lease.RoleConfig[runv1.RoleConfigKeyPlugins]), &plugins); err != nil {
		t.Fatalf("plugins: %v", err)
	}
	if len(plugins.Enabled) != 1 || plugins.Enabled[0] != "mattermost@acme" ||
		len(plugins.Marketplaces) != 1 || plugins.Marketplaces[0].URL != "acme/plugins" {
		t.Errorf("plugins = %+v", plugins)
	}
	if lease.Secrets[runv1.SecretKeyGitToken] == "" {
		t.Error("a run without a repository got no git token for its role's marketplace")
	}
}

func TestARoleCannotHandTheAgentAReservedVariableOrAMissingSecret(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for name, spec := range map[string]string{
		"reserved": `{"secretEnv": [{"name": "ANTHROPIC_BASE_URL", "secret": "llm-api-key"}]}`,
		"missing":  `{"secretEnv": [{"name": "MATTERMOST_TOKEN", "secret": "nope"}]}`,
		"settings": `{"configFiles": {"settings.json": "{\"extraKnownMarketplaces\": {\"x\": {\"source\": {\"source\": \"directory\", \"path\": \"/tmp\"}}}}"}}`,
	} {
		var invalid *run.InvalidRequestError
		if _, err := h.App.PutRole(ctx, "r-"+name, json.RawMessage(spec), "test"); !errors.As(err, &invalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
