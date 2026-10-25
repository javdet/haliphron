package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// socket is the Socket Mode end of a fake Slack: each connection is greeted,
// sent one mention, and then told to reconnect.
type socket struct {
	*httptest.Server

	mu          sync.Mutex
	connections int
	acks        []string
}

func newSocket(t *testing.T) *socket {
	s := &socket{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ticket") != "one-time" {
			w.WriteHeader(http.StatusForbidden)
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
		write := func(v any) {
			raw, _ := json.Marshal(v)
			_ = conn.Write(ctx, websocket.MessageText, raw)
		}
		write(map[string]any{"type": "hello", "num_connections": 1})
		envelopeID := fmt.Sprintf("env-%d", n)
		write(map[string]any{"type": "events_api", "envelope_id": envelopeID, "payload": map[string]any{
			"type": "event_callback", "event": map[string]any{
				"type": "app_mention", "user": "U1", "channel": "C1",
				"ts": fmt.Sprintf("1700000000.00000%d", n), "text": "<@UBOT> hello",
			},
		}})

		// The acknowledgement comes back before anything else is done.
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, ack, err := conn.Read(readCtx)
		cancel()
		if err == nil {
			var body struct {
				EnvelopeID string `json:"envelope_id"`
			}
			_ = json.Unmarshal(ack, &body)
			s.mu.Lock()
			s.acks = append(s.acks, body.EnvelopeID)
			s.mu.Unlock()
		}
		write(map[string]any{"type": "disconnect", "reason": "refresh_requested"})
		_, _, _ = conn.Read(ctx)
	}))
	t.Cleanup(s.Close)
	return s
}

// blockingHandler records messages and holds each until released, so a test
// can see the acknowledgement arrive while the message is still being handled.
type blockingHandler struct {
	release chan struct{}
	mu      sync.Mutex
	got     []app.ChatMessage
}

func (h *blockingHandler) Handle(_ context.Context, m app.ChatMessage) (app.ChatOutcome, error) {
	h.mu.Lock()
	h.got = append(h.got, m)
	h.mu.Unlock()
	<-h.release
	return app.ChatStarted, nil
}

func (h *blockingHandler) messages() []app.ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]app.ChatMessage(nil), h.got...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAnEventIsAcknowledgedBeforeItIsHandledAndARotationIsFollowedAtOnce(t *testing.T) {
	a := newAPI(t)
	sock := newSocket(t)
	wsURL := "ws" + strings.TrimPrefix(sock.URL, "http") + "/link?ticket=one-time"
	a.handle("apps.connections.open", func(url.Values) (int, map[string]any) {
		return 200, map[string]any{"ok": true, "url": wsURL}
	})
	a.handle("conversations.info", func(f url.Values) (int, map[string]any) {
		return 200, map[string]any{"ok": true, "channel": map[string]any{"name": "ops", "is_private": true}}
	})

	h := &blockingHandler{release: make(chan struct{})}
	l := NewListener(a.client(), h, quietLog())
	// A rotation must not wait out a backoff; a minute here would fail the
	// test by its deadline.
	l.minBackoff, l.maxBackoff = time.Minute, time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()

	eventually(t, "two connections, each acknowledged", func() bool {
		sock.mu.Lock()
		defer sock.mu.Unlock()
		return len(sock.acks) >= 2
	})
	sock.mu.Lock()
	if sock.acks[0] != "env-1" || sock.acks[1] != "env-2" {
		t.Errorf("acks = %v", sock.acks)
	}
	sock.mu.Unlock()
	// Neither message has been let through the handler yet.
	eventually(t, "both messages reaching the handler", func() bool { return len(h.messages()) >= 2 })
	close(h.release)

	m := h.messages()[0]
	if m.ID != "C1:1700000000.000001" || !m.MentionsBot || m.Text != "hello" {
		t.Errorf("message: %+v", m)
	}
	if m.Username != "name-of-U1" || m.ChannelName != "ops" || m.ChannelType != "P" {
		t.Errorf("the message was not described: %+v", m)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not stop")
	}
}

func TestARefusedTokenIsRetriedAtTheSlowestRate(t *testing.T) {
	a := newAPI(t)
	a.handle("auth.test", func(url.Values) (int, map[string]any) {
		return 200, map[string]any{"ok": false, "error": "invalid_auth"}
	})
	l := NewListener(a.client(), &blockingHandler{release: make(chan struct{})}, quietLog())
	l.minBackoff, l.maxBackoff = time.Millisecond, 300*time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Run(ctx)
	// The first attempt is refused, and the next waits the longest backoff:
	// longer than this test runs.
	if n := len(a.callsTo("auth.test")); n != 1 {
		t.Errorf("auth.test was called %d times, want 1", n)
	}
	if n := len(a.callsTo("apps.connections.open")); n != 0 {
		t.Errorf("a connection was opened with a refused bot token")
	}
}
