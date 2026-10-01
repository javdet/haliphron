package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/automagicops/haliphron/backend/app"
)

const testToken = "bot-token-0123456789abcdef"

// server is the smallest Mattermost the listener can talk to: users/me and a
// WebSocket that sends one posted event per connection and then hangs up.
type server struct {
	t *testing.T
	*httptest.Server

	mu          sync.Mutex
	connections int
	refuseFirst bool
	urls        []string
}

func newServer(t *testing.T) *server {
	s := &server{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/users/me", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorised(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(bot)
	})
	mux.HandleFunc("GET /api/v4/websocket", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorised(w, r) {
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		s.mu.Lock()
		s.connections++
		n := s.connections
		s.mu.Unlock()

		ctx := r.Context()
		_ = writeJSON(ctx, conn, map[string]any{"event": "hello", "data": map[string]any{}})
		post, _ := json.Marshal(map[string]any{
			"id": "post-" + string(rune('0'+n)), "user_id": "u1", "channel_id": "c1",
			"message": "@devops_duty hello",
		})
		mentions, _ := json.Marshal([]string{bot.ID})
		_ = writeJSON(ctx, conn, map[string]any{"event": "posted", "data": map[string]any{
			"channel_type": "O", "sender_name": "@alice", "post": string(post), "mentions": string(mentions),
		}})
		_ = writeJSON(ctx, conn, map[string]any{"event": "typing", "data": map[string]any{}})
		// Hang up, so the listener has to come back.
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close(websocket.StatusGoingAway, "restarting")
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *server) authorised(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	s.urls = append(s.urls, r.URL.String())
	refuse := s.refuseFirst
	s.refuseFirst = false
	s.mu.Unlock()
	if refuse || r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"id":"api.context.session_expired.app_error","message":"Invalid or expired session"}`))
		return false
	}
	return true
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	raw, _ := json.Marshal(v)
	return c.Write(ctx, websocket.MessageText, raw)
}

type recorder struct {
	mu   sync.Mutex
	seen []app.ChatMessage
	got  chan struct{}
}

func (r *recorder) Handle(_ context.Context, m app.ChatMessage) (app.ChatOutcome, error) {
	r.mu.Lock()
	r.seen = append(r.seen, m)
	r.mu.Unlock()
	r.got <- struct{}{}
	return app.ChatStarted, nil
}

func newListener(t *testing.T, s *server, logs *bytes.Buffer) (*Listener, *recorder) {
	t.Helper()
	base, _ := url.Parse(s.URL)
	rec := &recorder{got: make(chan struct{}, 16)}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	l := NewListener(NewClient(Config{BaseURL: base, Token: testToken}), rec, log)
	l.minBackoff, l.maxBackoff = 20*time.Millisecond, 100*time.Millisecond
	return l, rec
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestTheListenerHandsPostsOverAndReconnectsAfterTheServerDropsIt(t *testing.T) {
	s := newServer(t)
	var logs bytes.Buffer
	l, rec := newListener(t, s, &logs)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	waitFor(t, rec.got, "the first post")
	waitFor(t, rec.got, "a post on the second connection")
	cancel()
	waitFor(t, done, "the listener to stop")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.seen[0].ID == rec.seen[1].ID {
		t.Errorf("both connections delivered %s", rec.seen[0].ID)
	}
	if m := rec.seen[0]; !m.MentionsBot || m.Text != "hello" || m.Username != "alice" {
		t.Errorf("first post decoded as %+v", m)
	}
}

func TestTheBotTokenTravelsOnlyInTheAuthorizationHeader(t *testing.T) {
	s := newServer(t)
	s.refuseFirst = true
	var logs bytes.Buffer
	l, rec := newListener(t, s, &logs)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	waitFor(t, rec.got, "a post after the token was first refused")
	cancel()
	waitFor(t, done, "the listener to stop")

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.urls {
		if strings.Contains(u, testToken) {
			t.Errorf("the token is in a URL: %s", u)
		}
	}
	if strings.Contains(logs.String(), testToken) {
		t.Errorf("the token is in the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "refused the bot's token") {
		t.Errorf("a refused token was not logged as such:\n%s", logs.String())
	}
}

func TestAnAPIRefusalIsPermanentUnlessItIsRateLimitingOrTheServersFault(t *testing.T) {
	for status, want := range map[int]bool{400: true, 403: true, 404: true, 408: false, 429: false, 500: false, 503: false} {
		if got := (&APIError{Status: status}).Permanent(); got != want {
			t.Errorf("%d: permanent=%v", status, got)
		}
	}
}
