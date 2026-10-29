package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/automagicops/haliphron/backend/app"
)

const (
	testBotToken = "xoxb-test-0123456789abcdef"
	testAppToken = "xapp-test-fedcba9876543210"
)

// api is the smallest Web API the client can talk to. Each method is a
// function of the form it was sent; what it returns is encoded with ok:true
// unless it says otherwise.
type api struct {
	t *testing.T
	*httptest.Server

	mu       sync.Mutex
	calls    []apiCall
	handlers map[string]func(form url.Values) (int, map[string]any)
}

type apiCall struct {
	method string
	token  string
	form   url.Values
}

func newAPI(t *testing.T) *api {
	a := &api{t: t, handlers: map[string]func(url.Values) (int, map[string]any){
		"auth.test": func(url.Values) (int, map[string]any) {
			return 200, map[string]any{"ok": true, "user_id": bot.UserID, "bot_id": bot.BotID, "user": bot.Name}
		},
		"users.info": func(f url.Values) (int, map[string]any) {
			return 200, map[string]any{"ok": true, "user": map[string]any{"name": "name-of-" + f.Get("user")}}
		},
	}}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("unreadable form: %v", err)
		}
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		a.mu.Lock()
		a.calls = append(a.calls, apiCall{method: method, token: token, form: r.PostForm})
		h := a.handlers[method]
		a.mu.Unlock()
		if h == nil {
			t.Errorf("unexpected call to %s", method)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, body := h(r.PostForm)
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *api) handle(method string, h func(url.Values) (int, map[string]any)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers[method] = h
}

func (a *api) callsTo(method string) []apiCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []apiCall
	for _, c := range a.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (a *api) client() *Client {
	base, _ := url.Parse(a.URL + "/api")
	return NewClient(Config{BaseURL: base, BotToken: testBotToken, AppToken: testAppToken})
}

func TestAThreadIsReadPageByPageAndKnowsWhichPostsAreTheBotsOwn(t *testing.T) {
	a := newAPI(t)
	a.handle("conversations.replies", func(f url.Values) (int, map[string]any) {
		if f.Get("channel") != "C1" || f.Get("ts") != "1.000100" || f.Get("include_all_metadata") != "true" {
			t.Errorf("form = %v", f)
		}
		if f.Get("cursor") == "" {
			return 200, map[string]any{"ok": true, "messages": []map[string]any{
				{"ts": "1.000100", "user": "U1", "text": "the build is red &amp; I do not know why"},
			}, "response_metadata": map[string]any{"next_cursor": "page2"}}
		}
		return 200, map[string]any{"ok": true, "messages": []map[string]any{
			{"ts": "1.000200", "bot_id": "BBOT", "text": "The build is red because of the lint step.",
				"metadata": map[string]any{"event_type": "haliphron_post", "event_payload": map[string]any{"kind": "result"}}},
			{"ts": "1.000300", "bot_id": "BOTHER", "subtype": "bot_message", "username": "ci",
				"metadata": map[string]any{"event_type": "haliphron_post", "event_payload": map[string]any{"kind": "result"}}},
			{"ts": "1.000400", "subtype": "channel_join", "user": "U2", "text": "joined"},
		}}
	})
	c := a.client()
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}

	posts, err := c.Thread(context.Background(), "C1:1.000100")
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if len(posts) != 4 {
		t.Fatalf("%d posts: %+v", len(posts), posts)
	}
	if p := posts[0]; p.ID != "C1:1.000100" || p.Username != "name-of-U1" ||
		p.Text != "the build is red & I do not know why" || p.FromBot {
		t.Errorf("first post: %+v", p)
	}
	if p := posts[1]; !p.FromSelf || p.Kind != app.ChatPostResult {
		t.Errorf("own post: %+v", p)
	}
	if p := posts[2]; p.FromSelf || !p.FromBot || p.Kind != "" || p.Username != "ci" {
		t.Errorf("another app's post claimed a kind or lost its name: %+v", p)
	}
	if !posts[3].System {
		t.Errorf("join: %+v", posts[3])
	}
	if n := len(a.callsTo("conversations.replies")); n != 2 {
		t.Errorf("%d pages read", n)
	}
}

func TestAPostGoesInTheThreadAndCannotNotifyAChannel(t *testing.T) {
	a := newAPI(t)
	a.handle("chat.postMessage", func(f url.Values) (int, map[string]any) {
		return 200, map[string]any{"ok": true, "channel": f.Get("channel"), "ts": "9.000900"}
	})
	c := a.client()

	id, err := c.Post(context.Background(), app.ChatPost{
		ChannelID: "C1", RootID: "C1:1.000100", RunID: "01J0000000000000000000000A", Kind: app.ChatPostResult,
		Text: "Run ended **Succeeded**. <!channel> look & see <@U1>",
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if id != "C1:9.000900" {
		t.Errorf("id is %q", id)
	}
	calls := a.callsTo("chat.postMessage")
	if len(calls) != 1 {
		t.Fatalf("%d posts", len(calls))
	}
	f := calls[0].form
	if f.Get("channel") != "C1" || f.Get("thread_ts") != "1.000100" {
		t.Errorf("form = %v", f)
	}
	if got := f.Get("text"); got != "Run ended *Succeeded*. &lt;!channel&gt; look &amp; see &lt;@U1&gt;" {
		t.Errorf("text = %q", got)
	}
	var metadata struct {
		EventType    string            `json:"event_type"`
		EventPayload map[string]string `json:"event_payload"`
	}
	if err := json.Unmarshal([]byte(f.Get("metadata")), &metadata); err != nil ||
		metadata.EventType != postEventType || metadata.EventPayload["kind"] != app.ChatPostResult ||
		metadata.EventPayload["run_id"] != "01J0000000000000000000000A" {
		t.Errorf("metadata = %q (%v)", f.Get("metadata"), err)
	}
	if calls[0].token != testBotToken {
		t.Error("the post was not made with the bot token")
	}
}

func TestAPostIsNeverMadeIntoAThreadOfAnotherChannel(t *testing.T) {
	a := newAPI(t)
	if _, err := a.client().Post(context.Background(), app.ChatPost{ChannelID: "C1", RootID: "C2:1.1"}); err == nil {
		t.Error("posted into a thread of another channel")
	}
}

func TestReactingTwiceIsNotAFailure(t *testing.T) {
	a := newAPI(t)
	a.handle("reactions.add", func(f url.Values) (int, map[string]any) {
		if f.Get("channel") != "C1" || f.Get("timestamp") != "1.000100" || f.Get("name") != "eyes" {
			t.Errorf("form = %v", f)
		}
		return 200, map[string]any{"ok": false, "error": "already_reacted"}
	})
	if err := a.client().React(context.Background(), "C1:1.000100", "eyes"); err != nil {
		t.Errorf("react: %v", err)
	}
}

func TestARefusalIsPermanentUnlessSlackIsBusyOrBroken(t *testing.T) {
	a := newAPI(t)
	for _, tc := range []struct {
		status    int
		code      string
		permanent bool
	}{
		{200, "channel_not_found", true},
		{200, "not_in_channel", true},
		{200, "ratelimited", false},
		{200, "internal_error", false},
		{http.StatusTooManyRequests, "", false},
		{http.StatusBadGateway, "", false},
	} {
		a.handle("chat.postMessage", func(url.Values) (int, map[string]any) {
			return tc.status, map[string]any{"ok": false, "error": tc.code}
		})
		_, err := a.client().Post(context.Background(), app.ChatPost{ChannelID: "C1", Text: "hi"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%d %s: %v", tc.status, tc.code, err)
		}
		if apiErr.Permanent() != tc.permanent {
			t.Errorf("%d %s: permanent=%v", tc.status, tc.code, apiErr.Permanent())
		}
		if tc.status == http.StatusTooManyRequests && apiErr.RetryAfter.Seconds() != 7 {
			t.Errorf("retry after %v", apiErr.RetryAfter)
		}
		if strings.Contains(err.Error(), testBotToken) {
			t.Error("the token is in the error")
		}
	}
}

func TestOnlyTheConnectionIsOpenedWithTheAppToken(t *testing.T) {
	a := newAPI(t)
	a.handle("apps.connections.open", func(url.Values) (int, map[string]any) {
		return 200, map[string]any{"ok": true, "url": "wss://example/link?ticket=secret"}
	})
	c := a.client()
	if _, err := c.openConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.callsTo("apps.connections.open"); len(got) != 1 || got[0].token != testAppToken {
		t.Errorf("connections.open calls: %+v", got)
	}
	if got := a.callsTo("auth.test"); len(got) != 1 || got[0].token != testBotToken {
		t.Errorf("auth.test calls: %+v", got)
	}
}
