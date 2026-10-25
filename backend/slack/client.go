// Package slack is the chat adapter for a Slack workspace: the app's Socket
// Mode connection, the decoding of what arrives on it, and the handful of Web
// API calls the bot makes back.
//
// It is transport, like the mattermost adapter. It reports what a message is —
// who wrote it, where, whether it names the bot — and every decision about what
// to do with one is app.Chat's.
//
// Every connection is outbound. Socket Mode is a WebSocket the backend opens to
// Slack, so a backend with no public address needs an egress rule and nothing
// else; Slack never calls the backend.
//
// The app holds two tokens. The bot token (xoxb-) reads threads, reacts and
// posts; the app-level token (xapp-) only opens the Socket Mode connection.
// Each travels in the Authorization header of its own requests and nowhere
// else: not in a URL, not in an error, not in a log line.
//
// Slack identifies a message by its channel and its timestamp together. The
// rules want one identifier, so every message identifier this package hands
// out is "<channel>:<ts>", and every one it is handed is split back apart.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// Name is the platform's identifier: the chat_triggers key space and the
// created_via of the runs the bot starts.
const Name = app.ChatPlatformSlack

// DefaultAPIURL is the Web API's address. An installation on another Slack
// deployment, GovSlack for one, names its own.
const DefaultAPIURL = "https://slack.com/api"

const (
	requestTimeout = 15 * time.Second
	// maxResponseBytes bounds what one Web API answer may cost to read.
	maxResponseBytes = 8 << 20
	// threadPageSize and maxThreadPages bound a thread read. The prompt quotes
	// at most a few tens of kilobytes of it, so a thread longer than this is
	// read no further.
	threadPageSize = 200
	maxThreadPages = 25
	cacheSize      = 4096
	// postEventType marks the bot's own posts in their message metadata, so a
	// thread read back can tell which kind of post each was.
	postEventType = "haliphron_post"
)

// Config is how to reach the workspace.
type Config struct {
	// BaseURL is the Web API's address; nil is DefaultAPIURL.
	BaseURL *url.URL
	// BotToken is the bot user's token, xoxb-.
	BotToken string
	// AppToken is the app-level token with connections:write, xapp-.
	AppToken string
	// HTTP is the client to use. Proxy settings and a custom CA arrive through
	// its transport; the default honours HTTPS_PROXY and SSL_CERT_FILE.
	HTTP *http.Client
	// MaxPostRunes is the longest message posted.
	MaxPostRunes int
}

// Client is the bot's Web API access to the workspace.
type Client struct {
	base     *url.URL
	botToken string
	appToken string
	http     *http.Client
	maxRunes int

	mu       sync.Mutex
	self     Identity
	users    map[string]string // user id → name
	channels map[string]channel
}

var _ app.ChatPlatform = (*Client)(nil)

// NewClient returns a client for the workspace.
func NewClient(cfg Config) *Client {
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: requestTimeout}
	}
	maxRunes := cfg.MaxPostRunes
	if maxRunes <= 0 {
		maxRunes = app.DefaultSlackPostRunes
	}
	var base url.URL
	if cfg.BaseURL != nil {
		base = *cfg.BaseURL
	} else {
		parsed, _ := url.Parse(DefaultAPIURL)
		base = *parsed
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	return &Client{
		base:     &base,
		botToken: cfg.BotToken,
		appToken: cfg.AppToken,
		http:     h,
		maxRunes: maxRunes,
		users:    make(map[string]string),
		channels: make(map[string]channel),
	}
}

// Identity is the bot as Slack knows it. A message the bot posts carries
// either identifier depending on how it was made, so both are kept.
type Identity struct {
	UserID string
	BotID  string
	Name   string
}

// channel is what the header says about a conversation.
type channel struct {
	name string
	// kind is the rules' type letter: O public, P private, D direct, G group.
	kind string
	// user is the other party of a direct conversation.
	user string
}

// APIError is a request the Web API refused.
type APIError struct {
	// Method is the Web API method, e.g. chat.postMessage.
	Method string
	// Status is the HTTP status; 200 when Slack answered ok:false.
	Status int
	// Code is Slack's error string, e.g. channel_not_found.
	Code string
	// RetryAfter is what a rate-limited answer asked for.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("slack: %s: %s", e.Method, e.Code)
	}
	return fmt.Sprintf("slack: %s: %d", e.Method, e.Status)
}

// Permanent reports whether repeating the request can succeed. A refusal is
// final except for rate limiting and Slack's own faults.
func (e *APIError) Permanent() bool {
	switch e.Code {
	case "ratelimited", "internal_error", "fatal_error", "service_unavailable", "request_timeout":
		return false
	}
	return e.Status != http.StatusTooManyRequests && e.Status != http.StatusRequestTimeout && e.Status < 500
}

// Unauthorised reports whether the token itself was refused: a revoked,
// mistyped or wrong kind of token, which no retry fixes.
func (e *APIError) Unauthorised() bool {
	switch e.Code {
	case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive",
		"not_allowed_token_type":
		return true
	}
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// Name implements app.ChatPlatform.
func (c *Client) Name() string { return Name }

// MaxPostRunes implements app.ChatPlatform.
func (c *Client) MaxPostRunes() int { return c.maxRunes }

// Me reads the bot's own identity, and remembers it: it is how the bot
// recognises its own messages and its own mentions.
func (c *Client) Me(ctx context.Context) (Identity, error) {
	var out struct {
		UserID string `json:"user_id"`
		BotID  string `json:"bot_id"`
		User   string `json:"user"`
	}
	if err := c.call(ctx, c.botToken, "auth.test", nil, &out); err != nil {
		return Identity{}, err
	}
	id := Identity{UserID: out.UserID, BotID: out.BotID, Name: out.User}
	c.mu.Lock()
	c.self = id
	c.mu.Unlock()
	return id, nil
}

func (c *Client) me() Identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.self
}

// openConnection asks for a Socket Mode address. It is the one call made with
// the app-level token, and the address it returns is good for one connection.
func (c *Client) openConnection(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, c.appToken, "apps.connections.open", nil, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", errors.New("slack: apps.connections.open returned no address")
	}
	return out.URL, nil
}

// wireMessage is a message as the Web API and the Events API both carry it.
type wireMessage struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Username    string `json:"username"`
	BotProfile  *struct {
		Name string `json:"name"`
	} `json:"bot_profile"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Hidden   bool   `json:"hidden"`
	Metadata *struct {
		EventType    string         `json:"event_type"`
		EventPayload map[string]any `json:"event_payload"`
	} `json:"metadata"`
}

// Thread implements app.ChatPlatform.
func (c *Client) Thread(ctx context.Context, rootID string) ([]app.ChatThreadPost, error) {
	ch, ts, err := splitID(rootID)
	if err != nil {
		return nil, err
	}
	var messages []wireMessage
	cursor := ""
	for page := 0; page < maxThreadPages; page++ {
		form := url.Values{
			"channel":              {ch},
			"ts":                   {ts},
			"limit":                {strconv.Itoa(threadPageSize)},
			"include_all_metadata": {"true"},
		}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		var out struct {
			Messages         []wireMessage `json:"messages"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, c.botToken, "conversations.replies", form, &out); err != nil {
			return nil, err
		}
		messages = append(messages, out.Messages...)
		if cursor = out.ResponseMetadata.NextCursor; cursor == "" {
			break
		}
	}

	self := c.me()
	posts := make([]app.ChatThreadPost, 0, len(messages))
	for _, m := range messages {
		if m.TS == "" || m.Hidden {
			continue
		}
		fromSelf := isSelf(m, self)
		p := app.ChatThreadPost{
			ID:        postID(ch, m.TS),
			Text:      unescape(m.Text),
			CreatedAt: parseTS(m.TS),
			FromSelf:  fromSelf,
			FromBot:   isBot(m),
			System:    isSystem(m.Subtype),
		}
		switch {
		case m.User != "":
			// A name that cannot be read is not a reason to lose the thread:
			// the post is quoted under "someone" instead.
			p.Username, _ = c.username(ctx, m.User)
		case m.Username != "":
			p.Username = m.Username
		case m.BotProfile != nil:
			p.Username = m.BotProfile.Name
		}
		// Only this bot's own posts are believed about what they are. Another
		// app can put whatever metadata it likes on its messages.
		if fromSelf && m.Metadata != nil && m.Metadata.EventType == postEventType {
			p.Kind, _ = m.Metadata.EventPayload["kind"].(string)
		}
		posts = append(posts, p)
	}
	return posts, nil
}

// username resolves one author, from the cache when it can.
func (c *Client) username(ctx context.Context, userID string) (string, error) {
	c.mu.Lock()
	name, ok := c.users[userID]
	c.mu.Unlock()
	if ok {
		return name, nil
	}
	var out struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, c.botToken, "users.info", url.Values{"user": {userID}}, &out); err != nil {
		return "", err
	}
	name = out.User.Name
	if name == "" {
		name = out.User.Profile.DisplayName
	}
	if name == "" {
		name = out.User.Profile.RealName
	}
	c.mu.Lock()
	remember(c.users, userID, name)
	c.mu.Unlock()
	return name, nil
}

// channelInfo resolves a conversation's name and type, from the cache when it
// can.
func (c *Client) channelInfo(ctx context.Context, id string) (channel, error) {
	c.mu.Lock()
	ch, ok := c.channels[id]
	c.mu.Unlock()
	if ok {
		return ch, nil
	}
	var out struct {
		Channel struct {
			Name      string `json:"name"`
			IsIM      bool   `json:"is_im"`
			IsMPIM    bool   `json:"is_mpim"`
			IsPrivate bool   `json:"is_private"`
			User      string `json:"user"`
		} `json:"channel"`
	}
	if err := c.call(ctx, c.botToken, "conversations.info", url.Values{"channel": {id}}, &out); err != nil {
		return channel{}, err
	}
	ch = channel{name: out.Channel.Name, kind: "O", user: out.Channel.User}
	switch {
	case out.Channel.IsIM:
		ch.kind = "D"
	case out.Channel.IsMPIM:
		ch.kind = "G"
	case out.Channel.IsPrivate:
		ch.kind = "P"
	}
	c.mu.Lock()
	remember(c.channels, id, ch)
	c.mu.Unlock()
	return ch, nil
}

// remember caches a value. The cache is a bound rather than an eviction
// policy: names change rarely, and a full cache is simply emptied.
func remember[V any](cache map[string]V, key string, v V) {
	if key == "" {
		return
	}
	if len(cache) >= cacheSize {
		clear(cache)
	}
	cache[key] = v
}

// Post implements app.ChatPlatform.
func (c *Client) Post(ctx context.Context, p app.ChatPost) (string, error) {
	form := url.Values{
		"channel": {p.ChannelID},
		"text":    {toMrkdwn(p.Text)},
		// A result that links a pull request is not the place for a preview
		// of it.
		"unfurl_links": {"false"},
		"unfurl_media": {"false"},
	}
	if p.RootID != "" {
		ch, ts, err := splitID(p.RootID)
		if err != nil {
			return "", err
		}
		if ch != p.ChannelID {
			return "", fmt.Errorf("slack: thread %s is not in channel %s", p.RootID, p.ChannelID)
		}
		form.Set("thread_ts", ts)
	}
	// Only values this process chose are put into metadata. Nothing the agent
	// wrote goes here.
	if p.RunID != "" || p.Kind != "" {
		metadata, err := json.Marshal(map[string]any{
			"event_type":    postEventType,
			"event_payload": map[string]string{"run_id": string(p.RunID), "kind": p.Kind},
		})
		if err != nil {
			return "", err
		}
		form.Set("metadata", string(metadata))
	}
	var out struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := c.call(ctx, c.botToken, "chat.postMessage", form, &out); err != nil {
		return "", err
	}
	if out.Channel == "" {
		out.Channel = p.ChannelID
	}
	return postID(out.Channel, out.TS), nil
}

// React implements app.ChatPlatform.
func (c *Client) React(ctx context.Context, postID, emoji string) error {
	ch, ts, err := splitID(postID)
	if err != nil {
		return err
	}
	err = c.call(ctx, c.botToken, "reactions.add", url.Values{
		"channel": {ch}, "timestamp": {ts}, "name": {emoji},
	}, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "already_reacted" {
		// Another replica, or an earlier delivery, got there first.
		return nil
	}
	return err
}

// call makes one Web API request. Every method accepts a form, so every
// request is one. Errors name the method and Slack's error string, never the
// request.
func (c *Client) call(ctx context.Context, token, method string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	u := *c.base
	u.Path += "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, scrubURLError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("slack: read %s: %w", method, err)
	}

	if resp.StatusCode >= 300 {
		apiErr := &APIError{Method: method, Status: resp.StatusCode}
		if resp.StatusCode == http.StatusTooManyRequests {
			apiErr.Code = "ratelimited"
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				apiErr.RetryAfter = time.Duration(s) * time.Second
			}
		}
		return apiErr
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("slack: decode %s: %w", method, err)
	}
	if !status.OK {
		code := status.Error
		if code == "" {
			code = "unknown_error"
		}
		return &APIError{Method: method, Status: resp.StatusCode, Code: code}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("slack: decode %s: %w", method, err)
	}
	return nil
}

// scrubURLError drops the URL from a transport error. The Web API's URLs
// carry no secret, but a Socket Mode address does, and one rule for both is
// easier to keep.
func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// postID is the identifier the rules know a message by.
func postID(channel, ts string) string {
	return channel + ":" + ts
}

// splitID takes an identifier postID made apart.
func splitID(id string) (channel, ts string, err error) {
	channel, ts, ok := strings.Cut(id, ":")
	if !ok || channel == "" || ts == "" {
		return "", "", fmt.Errorf("slack: %q is not a channel:timestamp identifier", id)
	}
	return channel, ts, nil
}

// parseTS reads a message timestamp, seconds and microseconds since the epoch
// written as a decimal.
func parseTS(ts string) time.Time {
	secs, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var micros int64
	if frac != "" {
		frac = (frac + "000000")[:6]
		micros, _ = strconv.ParseInt(frac, 10, 64)
	}
	return time.Unix(s, micros*1000)
}

// toMrkdwn renders the rules' Markdown as Slack's mrkdwn.
//
// The three characters Slack's markup is made of are escaped first. That is
// what Slack asks of any text, and it is also what keeps an agent's summary
// from writing <!channel> or <@U…> into a post: once escaped they are text.
// Then **bold** becomes *bold*; everything else the replies use — `code`,
// bare links, line breaks — means the same in both.
func toMrkdwn(s string) string {
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	return strings.ReplaceAll(s, "**", "*")
}

// unescape reverses Slack's escaping of a message's text, for a prompt.
func unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

func isSelf(m wireMessage, self Identity) bool {
	return (self.UserID != "" && m.User == self.UserID) || (self.BotID != "" && m.BotID == self.BotID)
}

// isBot is a message made by an app: a bot user, an incoming webhook, a
// workflow.
func isBot(m wireMessage) bool {
	return m.BotID != "" || m.Subtype == "bot_message"
}

// isSystem is a message Slack generated rather than someone wrote: a join, a
// topic change. A broadcast reply, a file shared with a comment and a bot's
// message are written by someone.
func isSystem(subtype string) bool {
	switch subtype {
	case "", "bot_message", "thread_broadcast", "file_share", "me_message":
		return false
	}
	return true
}
