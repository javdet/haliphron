package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/automagicops/haliphron/backend/app"
)

// The bot's connection.
//
// One Socket Mode connection per process, held for as long as the process runs
// and opened again whenever it drops. Unlike Mattermost's socket, Slack hands
// each event to one of an app's connections rather than to all of them, so a
// replica sees only its share; the claim in app.Chat.Handle still matters,
// because Slack delivers an event again when its acknowledgement is late.
//
// Slack also ends connections on its own schedule. It warns with a disconnect
// frame a few seconds ahead, and the listener opens a new connection at once
// rather than treating the end as a failure.
//
// Events sent while no replica is connected are queued by Slack for a short
// while and then dropped. A person whose mention went unanswered mentions the
// bot again; the length of every gap is logged, so an operator can tell a
// quiet workspace from a deaf bot.

const (
	pingInterval = 30 * time.Second
	pingTimeout  = 10 * time.Second
	dialTimeout  = 30 * time.Second
	ackTimeout   = 2 * time.Second
	minBackoff   = time.Second
	maxBackoff   = time.Minute
	// stableAfter is how long a connection has to last before its loss is
	// treated as an incident rather than as the next step of a retry loop.
	stableAfter = time.Minute
	// readLimit bounds one frame. An event carries one message, its blocks
	// and its attachments.
	readLimit = 4 << 20
	// workers bounds the messages handled at once. Handling one means a
	// thread read and an admission; a burst of mentions queues behind this
	// rather than opening a database connection each.
	workers = 8
)

var (
	// errRotate is Slack asking for a new connection: routine, and answered
	// at once.
	errRotate = errors.New("slack: the server asked for a new connection")
	// errLinkDisabled is Socket Mode switched off in the app's settings.
	errLinkDisabled = errors.New("slack: Socket Mode is disabled for this app")
)

// Handler is what the listener hands messages to.
type Handler interface {
	Handle(ctx context.Context, m app.ChatMessage) (app.ChatOutcome, error)
}

// Listener holds the bot's Socket Mode connection.
type Listener struct {
	client  *Client
	handler Handler
	log     *slog.Logger

	slots chan struct{}
	wg    sync.WaitGroup

	// The reconnect bounds, fields so a test need not wait a minute.
	minBackoff, maxBackoff time.Duration

	connected chan struct{} // closed on the first hello, for tests
	once      sync.Once
}

// NewListener returns a listener that hands the client's messages to handler.
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

// Connected is closed once the listener has first been greeted by Slack.
func (l *Listener) Connected() <-chan struct{} { return l.connected }

// Run connects and reconnects until the context ends, and returns once every
// message it took has been handled.
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
		case errors.Is(err, errRotate):
			l.log.Debug("Slack rotated the connection")
			continue
		case errors.Is(err, errLinkDisabled):
			backoff = l.maxBackoff
			l.log.Error("Socket Mode is disabled in the Slack app's settings", "retry_in", backoff)
		case errors.As(err, &apiErr) && apiErr.Unauthorised():
			// A revoked or mistyped token does not fix itself, but the API
			// this process also serves is no reason to stop: keep trying at
			// the slowest rate, and say so loudly every time.
			backoff = l.maxBackoff
			l.log.Error("Slack refused the bot's token", "method", apiErr.Method, "error", apiErr.Code,
				"retry_in", backoff)
		case errors.As(err, &apiErr) && apiErr.RetryAfter > 0:
			backoff = max(backoff, apiErr.RetryAfter)
			l.log.Warn("Slack is rate limiting the bot", "method", apiErr.Method, "retry_in", backoff)
		default:
			l.log.Warn("no connection to Slack", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, l.maxBackoff)
	}
}

// session holds one connection until it ends. up is when it was established,
// zero if it never was.
func (l *Listener) session(ctx context.Context, self Identity, lostAt time.Time) (up time.Time, err error) {
	address, err := l.client.openConnection(ctx)
	if err != nil {
		return time.Time{}, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	// The address is a one-time ticket. It is never logged, and a failed dial
	// is reported without it.
	conn, _, err := websocket.Dial(dialCtx, address, &websocket.DialOptions{HTTPClient: l.client.http})
	if err != nil {
		return time.Time{}, scrubURLError(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)
	up = time.Now()

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
			l.log.Warn("unreadable frame from Slack", "error", err)
			continue
		}
		if ev.EnvelopeID != "" {
			if err := l.ack(sessionCtx, conn, ev.EnvelopeID); err != nil {
				return up, err
			}
		}

		switch ev.Type {
		case "hello":
			if lostAt.IsZero() {
				l.log.Info("connected to Slack", "bot", self.Name)
			} else {
				l.log.Info("reconnected to Slack", "bot", self.Name,
					"gap", time.Since(lostAt).Round(time.Second).String())
			}
			l.once.Do(func() { close(l.connected) })
		case "disconnect":
			_ = conn.Close(websocket.StatusNormalClosure, "")
			if ev.Reason == "link_disabled" {
				return up, errLinkDisabled
			}
			return up, errRotate
		case "events_api":
			m, ok, err := decodeEvent(ev.Payload, self)
			if err != nil {
				l.log.Warn("unreadable event from Slack", "error", err)
				continue
			}
			if ok {
				l.dispatch(ctx, m)
			}
		}
	}
}

// ack acknowledges an envelope, which is all Slack waits for. It is sent
// before the message is looked at: the three seconds Slack allows are less
// than a thread read and an admission take.
func (l *Listener) ack(ctx context.Context, conn *websocket.Conn, id string) error {
	body, err := json.Marshal(map[string]string{"envelope_id": id})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, ackTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, body)
}

// dispatch hands a message to the handler on a worker, waiting for a free
// one. The wait is what applies back-pressure: a burst faster than messages
// can be admitted is read more slowly, not buffered without bound. A read
// delayed past Slack's deadline is redelivered, and the claim makes the
// redelivery a duplicate.
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
		ctx := context.WithoutCancel(ctx)
		l.describe(ctx, &m)
		outcome, err := l.handler.Handle(ctx, m)
		switch {
		case err != nil:
			l.log.Error("a message could not be handled", "message", m.ID, "error", err)
		case outcome != app.ChatIgnored:
			l.log.Debug("message handled", "message", m.ID, "outcome", outcome.String())
		}
	}()
}

// describe fills in what an event leaves out — who the sender is by name, and
// what the channel is called — for a message that may be a request. It is
// best effort: a header without a name is still a header, and a lookup that
// fails is no reason to ignore the person.
func (l *Listener) describe(ctx context.Context, m *app.ChatMessage) {
	if m.FromSelf || m.FromBot || m.System || !(m.MentionsBot || m.Direct) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if name, err := l.client.username(ctx, m.UserID); err == nil {
		m.Username = name
	} else {
		l.log.Warn("could not read the sender of a message", "message", m.ID, "error", err)
	}
	ch, err := l.client.channelInfo(ctx, m.ChannelID)
	if err != nil {
		l.log.Warn("could not read the channel of a message", "message", m.ID, "error", err)
		return
	}
	m.ChannelName = ch.name
	if m.ChannelType == "" {
		m.ChannelType = ch.kind
	}
	if ch.kind == "D" {
		m.Direct = true
		// A direct conversation has no name; the person on the other side is
		// what the platform shows for it.
		m.ChannelDisplayName = m.Username
	}
}

// keepAlive pings Slack, and ends the session when a ping goes unanswered: a
// connection a NAT or a proxy has silently dropped otherwise looks exactly
// like a quiet workspace, for ever.
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
				l.log.Warn("Slack stopped answering pings", "error", err)
				stop()
				return
			}
		}
	}
}

// jitter spreads reconnects over ±20%, so replicas that lost Slack at the same
// moment do not all return to it at the same moment.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}
