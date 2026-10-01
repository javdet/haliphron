// Package mattermost is the chat adapter for a Mattermost server: the bot's
// WebSocket connection, the decoding of what arrives on it, and the handful of
// REST calls the bot makes back.
//
// It is transport, like restapi and mcp. It reports what a post is — who wrote
// it, where, whether it names the bot — and every decision about what to do
// with one is app.Chat's.
//
// The connection is outbound, the same direction as everything else in this
// system: the backend dials the chat server as the bot and holds the socket
// open. Nothing needs to reach the backend, so a chat server on another network
// needs an egress rule and no ingress.
//
// The bot's access token is the one secret this package holds. It travels in
// the Authorization header of each request and the WebSocket handshake, and
// nowhere else: not in a URL, not in an error, not in a log line.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// Name is the platform's identifier: the chat_triggers key space and the
// created_via of the runs the bot starts.
const Name = "mattermost"

const (
	requestTimeout = 15 * time.Second
	// maxResponseBytes bounds what one REST answer may cost to read. A long
	// thread is the largest thing asked for.
	maxResponseBytes = 32 << 20
	userCacheSize    = 4096
)

// Config is how to reach the server.
type Config struct {
	// BaseURL is the server's address, the same one a browser opens.
	BaseURL *url.URL
	Token   string
	// HTTP is the client to use. Proxy settings and a custom CA arrive through
	// its transport; the default honours HTTPS_PROXY and SSL_CERT_FILE.
	HTTP *http.Client
	// MaxPostRunes is the server's post length limit, MaxPostSize in its
	// configuration.
	MaxPostRunes int
}

// Client is the bot's REST access to the server.
type Client struct {
	base     *url.URL
	token    string
	http     *http.Client
	maxRunes int

	mu    sync.Mutex
	self  User
	users map[string]string // user id → username
}

var _ app.ChatPlatform = (*Client)(nil)

// NewClient returns a client for the server.
func NewClient(cfg Config) *Client {
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: requestTimeout}
	}
	maxRunes := cfg.MaxPostRunes
	if maxRunes <= 0 {
		maxRunes = app.DefaultChatPostRunes
	}
	base := *cfg.BaseURL
	base.Path = strings.TrimSuffix(base.Path, "/")
	return &Client{
		base:     &base,
		token:    cfg.Token,
		http:     h,
		maxRunes: maxRunes,
		users:    make(map[string]string),
	}
}

// User is an account as the bot needs one.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// APIError is a request the server refused.
type APIError struct {
	Status int
	// ID is the server's error identifier, e.g. api.context.permissions.app_error.
	ID      string
	Message string
}

func (e *APIError) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("mattermost: %d %s: %s", e.Status, e.ID, e.Message)
	}
	return fmt.Sprintf("mattermost: %d", e.Status)
}

// Permanent reports whether repeating the request can succeed. A refusal is
// final except for rate limiting and the server's own faults.
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests &&
		e.Status != http.StatusRequestTimeout
}

// Name implements app.ChatPlatform.
func (c *Client) Name() string { return Name }

// MaxPostRunes implements app.ChatPlatform.
func (c *Client) MaxPostRunes() int { return c.maxRunes }

// Me reads the bot's own account, and remembers it: it is how the bot
// recognises its own posts and its own mentions.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/api/v4/users/me", nil, &u); err != nil {
		return User{}, err
	}
	c.mu.Lock()
	c.self = u
	c.remember(u.ID, u.Username)
	c.mu.Unlock()
	return u, nil
}

func (c *Client) me() User {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.self
}

type wirePost struct {
	ID        string         `json:"id"`
	CreateAt  int64          `json:"create_at"`
	DeleteAt  int64          `json:"delete_at"`
	UserID    string         `json:"user_id"`
	ChannelID string         `json:"channel_id"`
	RootID    string         `json:"root_id"`
	Message   string         `json:"message"`
	Type      string         `json:"type"`
	Props     map[string]any `json:"props"`
}

// Thread implements app.ChatPlatform.
func (c *Client) Thread(ctx context.Context, rootID string) ([]app.ChatThreadPost, error) {
	var thread struct {
		Order []string            `json:"order"`
		Posts map[string]wirePost `json:"posts"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v4/posts/"+url.PathEscape(rootID)+"/thread", nil, &thread); err != nil {
		return nil, err
	}

	posts := make([]wirePost, 0, len(thread.Posts))
	for _, p := range thread.Posts {
		if p.DeleteAt == 0 {
			posts = append(posts, p)
		}
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].CreateAt < posts[j].CreateAt })

	names, err := c.usernames(ctx, posts)
	if err != nil {
		return nil, err
	}
	self := c.me()
	out := make([]app.ChatThreadPost, 0, len(posts))
	for _, p := range posts {
		out = append(out, app.ChatThreadPost{
			ID:        p.ID,
			Username:  names[p.UserID],
			Text:      p.Message,
			CreatedAt: time.UnixMilli(p.CreateAt),
			FromSelf:  self.ID != "" && p.UserID == self.ID,
			System:    p.Type != "",
		})
	}
	return out, nil
}

// usernames resolves the authors of posts, asking the server only for the ones
// not already known.
func (c *Client) usernames(ctx context.Context, posts []wirePost) (map[string]string, error) {
	names := make(map[string]string)
	var missing []string
	c.mu.Lock()
	for _, p := range posts {
		if _, seen := names[p.UserID]; seen || p.UserID == "" {
			continue
		}
		if name, ok := c.users[p.UserID]; ok {
			names[p.UserID] = name
			continue
		}
		names[p.UserID] = ""
		missing = append(missing, p.UserID)
	}
	c.mu.Unlock()
	if len(missing) == 0 {
		return names, nil
	}

	var users []User
	if err := c.do(ctx, http.MethodPost, "/api/v4/users/ids", missing, &users); err != nil {
		return nil, err
	}
	c.mu.Lock()
	for _, u := range users {
		names[u.ID] = u.Username
		c.remember(u.ID, u.Username)
	}
	c.mu.Unlock()
	return names, nil
}

// remember caches a username. The cache is a bound rather than an eviction
// policy: names change rarely, and a full cache is simply emptied.
func (c *Client) remember(id, username string) {
	if id == "" || username == "" {
		return
	}
	if len(c.users) >= userCacheSize {
		clear(c.users)
		if c.self.ID != "" {
			c.users[c.self.ID] = c.self.Username
		}
	}
	c.users[id] = username
}

// Post implements app.ChatPlatform.
func (c *Client) Post(ctx context.Context, p app.ChatPost) (string, error) {
	body := map[string]any{
		"channel_id": p.ChannelID,
		"root_id":    p.RootID,
		"message":    p.Text,
	}
	// Only values this process chose are put into props. Nothing the agent
	// wrote goes here: props drive rendering and integrations on the server,
	// and the agent's text is untrusted.
	if p.RunID != "" || p.Kind != "" {
		body["props"] = map[string]any{
			"haliphron_run_id": string(p.RunID),
			"haliphron_kind":   p.Kind,
		}
	}
	var created wirePost
	if err := c.do(ctx, http.MethodPost, "/api/v4/posts", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// React implements app.ChatPlatform.
func (c *Client) React(ctx context.Context, postID, emoji string) error {
	self := c.me()
	if self.ID == "" {
		var err error
		if self, err = c.Me(ctx); err != nil {
			return err
		}
	}
	return c.do(ctx, http.MethodPost, "/api/v4/reactions", map[string]string{
		"user_id": self.ID, "post_id": postID, "emoji_name": emoji,
	}, nil)
}

// do makes one request. Errors name the method, the path and the status — the
// path holds identifiers only — and never the request.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("mattermost: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	u := *c.base
	u.Path += path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("mattermost: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mattermost: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("mattermost: read %s %s: %w", method, path, err)
	}

	if resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode}
		var problem struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &problem) == nil {
			apiErr.ID, apiErr.Message = problem.ID, problem.Message
		}
		return fmt.Errorf("%s %s: %w", method, path, apiErr)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mattermost: decode %s %s: %w", method, path, err)
	}
	return nil
}

// websocketURL is the server's WebSocket endpoint.
func (c *Client) websocketURL() string {
	u := *c.base
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path += "/api/v4/websocket"
	return u.String()
}
