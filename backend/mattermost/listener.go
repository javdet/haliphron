package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/automagicops/haliphron/backend/app"
)

// The bot's connection.
//
// One socket per process, held for as long as the process runs and redialled
// whenever it drops. Every replica holds its own, so every replica receives
// every post; the claim in app.Chat.Handle is what turns those deliveries into
// one run.
//
// Posts made while no replica is connected are not seen. The server does not
// replay them to a new connection, and the bot does not go looking: a person
// whose mention went unanswered mentions it again. The length of every gap is
// logged, so an operator can tell a quiet channel from a deaf bot.

const (
	pingInterval = 30 * time.Second
	pingTimeout  = 10 * time.Second
	dialTimeout  = 30 * time.Second
	minBackoff   = time.Second
	maxBackoff   = time.Minute
	// stableAfter is how long a connection has to last before its loss is
	// treated as an incident rather than as the next step of a retry loop.
	stableAfter = time.Minute
	// readLimit bounds one frame. The library's default, 32 KiB, is smaller
	// than a long post once it is encoded twice inside its envelope.
	readLimit = 4 << 20
	// workers bounds the posts handled at once. Handling one means a thread
	// fetch and an admission; a burst of mentions queues behind this rather
	// than opening a database connection each.
	workers = 8
)

// Handler is what the listener hands posts to.
type Handler interface {
	Handle(ctx context.Context, m app.ChatMessage) (app.ChatOutcome, error)
}

// Listener holds the bot's WebSocket connection.
type Listener struct {
	client  *Client
	handler Handler
	log     *slog.Logger

	slots chan struct{}
	wg    sync.WaitGroup

	// The reconnect bounds, fields so a test need not wait a minute.
	minBackoff, maxBackoff time.Duration

	connected chan struct{} // closed on the first successful connection, for tests
	once      sync.Once
}

// NewListener returns a listener that hands the client's posts to handler.
func NewListener(c *Client, handler Handler, log *slog.Logger) *Listener {
	return &Listener{
		client:     c,
		handler:    handler,
		log:        log.With("platform", Name),
		slots:      make(chan struct{}, workers),
		connected:  make(chan struct{}),
		minBackoff: minBackoff,
		maxBackoff: maxBackoff,
	}
}

// Connected is closed once the listener has first connected.
func (l *Listener) Connected() <-chan struct{} { return l.connected }

// Run connects and reconnects until the context ends, and returns once every
// post it took has been handled.
func (l *Listener) Run(ctx context.Context) {
	defer l.wg.Wait()

	backoff := l.minBackoff
	var lostAt time.Time
	for {
		self, err := l.client.Me(ctx)
		if err == nil {
			var up time.Time
			up, err = l.session(ctx, self, lostAt)
			if !up.IsZero() {
				lostAt = time.Now()
				if time.Since(up) >= stableAfter {
					backoff = l.minBackoff
				}
			}
		}
		if ctx.Err() != nil {
			return
		}

		var apiErr *APIError
		switch {
		case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
			// A revoked or mistyped token does not fix itself, but the API
			// this process also serves is no reason to stop: keep trying at
			// the slowest rate, and say so loudly every time.
			backoff = l.maxBackoff
			l.log.Error("the Mattermost server refused the bot's token", "status", apiErr.Status, "retry_in", backoff)
		default:
			l.log.Warn("no connection to the Mattermost server", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, l.maxBackoff)
	}
}

// session holds one connection until it fails. up is when it was established,
// zero if it never was.
func (l *Listener) session(ctx context.Context, self User, lostAt time.Time) (up time.Time, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, l.client.websocketURL(), &websocket.DialOptions{
		HTTPClient: l.client.http,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + l.client.token}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return time.Time{}, &APIError{Status: resp.StatusCode}
		}
		return time.Time{}, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)

	up = time.Now()
	if lostAt.IsZero() {
		l.log.Info("connected to the Mattermost server", "bot", self.Username)
	} else {
		l.log.Info("reconnected to the Mattermost server; posts made in the gap were not seen",
			"bot", self.Username, "gap", up.Sub(lostAt).Round(time.Second).String())
	}
	l.once.Do(func() { close(l.connected) })

	sessionCtx, stop := context.WithCancel(ctx)
	defer stop()
	go l.keepAlive(sessionCtx, conn, stop)

	for {
		_, frame, err := conn.Read(sessionCtx)
		if err != nil {
			if ctx.Err() != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "shutting down")
			}
			return up, err
		}
		var ev envelope
		if err := json.Unmarshal(frame, &ev); err != nil {
			l.log.Warn("unreadable frame from the Mattermost server", "error", err)
			continue
		}
		if ev.Event != "posted" {
			continue
		}
		m, ok, err := decodePosted(ev.Data, self)
		if err != nil {
			l.log.Warn("unreadable post from the Mattermost server", "error", err)
			continue
		}
		if !ok {
			continue
		}
		l.dispatch(ctx, m)
	}
}

// dispatch hands a post to the handler on a worker, waiting for a free one.
// The wait is what applies back-pressure to the socket: a server that sends
// faster than posts can be admitted is read more slowly, not buffered without
// bound.
func (l *Listener) dispatch(ctx context.Context, m app.ChatMessage) {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() { <-l.slots }()
		// The handler outlives a shutdown that starts mid-message: a claim
		// abandoned halfway is one the reply loop answers five minutes later
		// with "ask again", and finishing is cheaper than that.
		outcome, err := l.handler.Handle(context.WithoutCancel(ctx), m)
		switch {
		case err != nil:
			l.log.Error("a post could not be handled", "message", m.ID, "error", err)
		case outcome != app.ChatIgnored:
			l.log.Debug("post handled", "message", m.ID, "outcome", outcome.String())
		}
	}()
}

// keepAlive pings the server, and ends the session when a ping goes
// unanswered: a connection a NAT or a proxy has silently dropped otherwise
// looks exactly like a quiet channel, for ever.
func (l *Listener) keepAlive(ctx context.Context, conn *websocket.Conn, stop context.CancelFunc) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				l.log.Warn("the Mattermost server stopped answering pings", "error", err)
				stop()
				return
			}
		}
	}
}

// jitter spreads reconnects over ±20%, so replicas that lost the server at the
// same moment do not all return to it at the same moment.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}
