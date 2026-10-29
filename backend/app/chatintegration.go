package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The chat integration as an operator configures it: from the deployment's
// environment, or from the UI into chat_integrations.
//
// One source at a time. When the environment names a server it wins outright,
// the UI shows it read-only and a save is refused — two sources merged field by
// field is how "I changed it and nothing happened" becomes a support
// conversation. Otherwise the row is the configuration, and every replica polls
// its version and reconnects when it moves.
//
// The credentials are managed secrets under fixed names rather than columns:
// they are encrypted like every other credential, listed with the others, and
// never returned by a read.

// The chat platforms, as the chat_integrations and chat_triggers keys spell
// them. Each adapter's Name is one of these.
const (
	ChatPlatformMattermost = "mattermost"
	ChatPlatformSlack      = "slack"
)

// The secrets the integrations' credentials are kept in.
const (
	// ChatBotTokenSecret is the Mattermost bot account's access token: the
	// WebSocket, the thread reads, the reactions and the backend's own posts.
	ChatBotTokenSecret = "mattermost-bot-token"
	// ChatWebhookTokenSecret holds the outgoing webhooks' tokens, one per line.
	// Mattermost issues a token per webhook, and one bot may be fed by several.
	ChatWebhookTokenSecret = "mattermost-webhook-token"
	// SlackBotTokenSecret is the Slack app's bot token (xoxb-): the thread
	// reads, the reactions and the backend's own posts.
	SlackBotTokenSecret = "slack-bot-token"
	// SlackAppTokenSecret is the Slack app's app-level token (xapp-). It opens
	// the Socket Mode connection and is used for nothing else.
	SlackAppTokenSecret = "slack-app-token"
)

// ChatSecrets names the secrets a platform's credentials are kept in. Webhook
// and App are empty for a platform without that credential: Mattermost has no
// app token, and Slack no outgoing webhooks.
type ChatSecrets struct {
	Bot     string
	Webhook string
	App     string
}

// ChatSecretsFor is where a platform's credentials are kept.
func ChatSecretsFor(platform string) ChatSecrets {
	if platform == ChatPlatformSlack {
		return ChatSecrets{Bot: SlackBotTokenSecret, App: SlackAppTokenSecret}
	}
	return ChatSecrets{Bot: ChatBotTokenSecret, Webhook: ChatWebhookTokenSecret}
}

// ChatCredentials is which of a platform's credentials are present.
type ChatCredentials struct {
	Bot     bool
	Webhook bool
	App     bool
}

// Where a chat integration's configuration comes from.
const (
	ChatSourceNone        = "none"
	ChatSourceEnvironment = "environment"
	ChatSourceDatabase    = "database"
)

// DefaultChatRole is the role a chat integration saved without one runs
// under: the classificator, which decides which role the request needs.
const DefaultChatRole = "classificator"

// ErrChatIntegrationManaged is a save refused because the deployment's
// environment configures the integration.
var ErrChatIntegrationManaged = errors.New("app: the chat integration is configured by the deployment's environment")

// ChatIntegration is an integration's configuration, credentials excluded.
type ChatIntegration struct {
	store.ChatIntegration
	// Source is ChatSourceEnvironment, ChatSourceDatabase or ChatSourceNone.
	Source                 string
	BotTokenConfigured     bool
	WebhookTokenConfigured bool
	AppTokenConfigured     bool
}

// ResolvedChatIntegration is what a running bot needs, credentials included.
// It never leaves the process.
type ResolvedChatIntegration struct {
	store.ChatIntegration
	BotToken      string
	WebhookTokens []string
	// AppToken opens Slack's Socket Mode connection; empty for Mattermost.
	AppToken string
}

// WebSocket reports whether the bot holds a WebSocket connection.
func (c ResolvedChatIntegration) WebSocket() bool {
	return c.Transport == store.ChatTransportWebSocket || c.Transport == store.ChatTransportBoth
}

// Webhook reports whether the bot accepts outgoing-webhook deliveries.
func (c ResolvedChatIntegration) Webhook() bool {
	return c.Transport == store.ChatTransportWebhook || c.Transport == store.ChatTransportBoth
}

// ChatIntegrationRequest saves an integration. A nil token keeps the stored
// one; a token is never read back, so "unchanged" has to be expressible.
type ChatIntegrationRequest struct {
	Enabled        bool
	Transport      string
	URL            string
	Role           string
	RepoURL        string
	BaseBranch     string
	MaxPostRunes   int
	MaxThreadBytes int
	BotToken       *string
	WebhookTokens  *string
	AppToken       *string
}

// chatEnvironment is the integration the deployment's environment configures,
// keyed by platform.
type chatEnvironment struct {
	mu   sync.RWMutex
	byID map[string]ResolvedChatIntegration
}

// UseEnvironmentChat records that the deployment's environment configures a
// platform's integration. It is called once at startup, before any listener.
func (s *Service) UseEnvironmentChat(c ResolvedChatIntegration) {
	s.chatEnv.mu.Lock()
	defer s.chatEnv.mu.Unlock()
	if s.chatEnv.byID == nil {
		s.chatEnv.byID = map[string]ResolvedChatIntegration{}
	}
	c.UpdatedBy = "environment"
	s.chatEnv.byID[c.Platform] = c
}

func (s *Service) environmentChat(platform string) (ResolvedChatIntegration, bool) {
	s.chatEnv.mu.RLock()
	defer s.chatEnv.mu.RUnlock()
	c, ok := s.chatEnv.byID[platform]
	return c, ok
}

// ChatIntegration reports a platform's integration, credentials excluded.
func (s *Service) ChatIntegration(ctx context.Context, platform string) (ChatIntegration, error) {
	if env, ok := s.environmentChat(platform); ok {
		return ChatIntegration{
			ChatIntegration:        env.ChatIntegration,
			Source:                 ChatSourceEnvironment,
			BotTokenConfigured:     env.BotToken != "",
			WebhookTokenConfigured: len(env.WebhookTokens) > 0,
			AppTokenConfigured:     env.AppToken != "",
		}, nil
	}

	out := ChatIntegration{Source: ChatSourceDatabase}
	stored, err := s.store.ChatIntegration(ctx, platform)
	switch {
	case err == nil:
		out.ChatIntegration = stored
	case errors.Is(err, store.ErrNotFound):
		out.Source = ChatSourceNone
		out.ChatIntegration = defaultChatIntegration(platform)
	default:
		return ChatIntegration{}, err
	}
	have, err := s.chatCredentials(ctx, platform)
	if err != nil {
		return ChatIntegration{}, err
	}
	out.BotTokenConfigured, out.WebhookTokenConfigured, out.AppTokenConfigured = have.Bot, have.Webhook, have.App
	return out, nil
}

// chatCredentials reports which of a platform's stored credentials exist.
func (s *Service) chatCredentials(ctx context.Context, platform string) (ChatCredentials, error) {
	var (
		have    ChatCredentials
		err     error
		secrets = ChatSecretsFor(platform)
	)
	for _, c := range []struct {
		name string
		into *bool
	}{{secrets.Bot, &have.Bot}, {secrets.Webhook, &have.Webhook}, {secrets.App, &have.App}} {
		if c.name == "" {
			continue
		}
		if *c.into, err = s.store.SecretExists(ctx, c.name); err != nil {
			return ChatCredentials{}, err
		}
	}
	return have, nil
}

func defaultChatIntegration(platform string) store.ChatIntegration {
	c := store.ChatIntegration{
		Platform:       platform,
		Transport:      store.ChatTransportWebSocket,
		Role:           DefaultChatRole,
		MaxPostRunes:   DefaultChatPostRunes,
		MaxThreadBytes: DefaultChatThreadBytes,
	}
	if platform == ChatPlatformSlack {
		c.MaxPostRunes = DefaultSlackPostRunes
	}
	return c
}

// PutChatIntegration validates and saves a platform's integration.
//
// The tokens are written before the row. The row's version is what tells the
// replicas to reconnect, so it moves last: a replica that sees it has the
// credentials it is about to read.
func (s *Service) PutChatIntegration(ctx context.Context, platform string, req ChatIntegrationRequest,
	by string) (ChatIntegration, error) {

	if _, ok := s.environmentChat(platform); ok {
		return ChatIntegration{}, ErrChatIntegrationManaged
	}

	c := store.ChatIntegration{
		Platform:       platform,
		Enabled:        req.Enabled,
		Transport:      strings.TrimSpace(req.Transport),
		URL:            strings.TrimRight(strings.TrimSpace(req.URL), "/"),
		Role:           strings.TrimSpace(req.Role),
		RepoURL:        strings.TrimSpace(req.RepoURL),
		BaseBranch:     strings.TrimSpace(req.BaseBranch),
		MaxPostRunes:   req.MaxPostRunes,
		MaxThreadBytes: req.MaxThreadBytes,
		UpdatedBy:      by,
	}
	if c.Transport == "" {
		c.Transport = store.ChatTransportWebSocket
	}
	if c.Role == "" {
		c.Role = DefaultChatRole
	}
	if c.MaxPostRunes == 0 {
		c.MaxPostRunes = defaultChatIntegration(platform).MaxPostRunes
	}

	secrets := ChatSecretsFor(platform)
	if secrets.Webhook == "" && trimmedToken(req.WebhookTokens) != "" {
		return ChatIntegration{}, &run.InvalidRequestError{Field: "webhook_tokens",
			Detail: platform + " has no outgoing webhooks"}
	}
	if secrets.App == "" && trimmedToken(req.AppToken) != "" {
		return ChatIntegration{}, &run.InvalidRequestError{Field: "app_token",
			Detail: platform + " has no app-level token"}
	}

	var webhookTokens []string
	if req.WebhookTokens != nil {
		webhookTokens = SplitChatWebhookTokens(*req.WebhookTokens)
	}
	botToken := trimmedToken(req.BotToken)
	appToken := trimmedToken(req.AppToken)

	have, err := s.chatCredentials(ctx, platform)
	if err != nil {
		return ChatIntegration{}, err
	}
	have.Bot = have.Bot || botToken != ""
	have.App = have.App || appToken != ""
	if req.WebhookTokens != nil {
		have.Webhook = len(webhookTokens) > 0
	}
	if err := ValidateChatIntegration(c, have); err != nil {
		return ChatIntegration{}, err
	}

	if botToken != "" {
		if _, err := s.store.PutManagedSecret(ctx, secrets.Bot, []byte(botToken), "", by); err != nil {
			return ChatIntegration{}, err
		}
	}
	if appToken != "" {
		if _, err := s.store.PutManagedSecret(ctx, secrets.App, []byte(appToken), "", by); err != nil {
			return ChatIntegration{}, err
		}
	}
	if len(webhookTokens) > 0 {
		if _, err := s.store.PutManagedSecret(ctx, secrets.Webhook,
			[]byte(strings.Join(webhookTokens, "\n")), "", by); err != nil {
			return ChatIntegration{}, err
		}
	}

	stored, err := s.store.PutChatIntegration(ctx, c)
	if err != nil {
		return ChatIntegration{}, err
	}
	if err := s.store.Audit(ctx, store.AuditEntry{
		Actor: by, ActorKind: "user", Action: store.AuditChatIntegrationSaved,
		SubjectKind: "chat_integration", SubjectID: platform,
		Payload: map[string]any{
			"enabled": c.Enabled, "transport": c.Transport, "url": c.URL, "role": c.Role,
			"bot_token_replaced": req.BotToken != nil, "webhook_tokens_replaced": len(webhookTokens) > 0,
			"app_token_replaced": appToken != "",
		},
	}); err != nil {
		return ChatIntegration{}, err
	}
	return ChatIntegration{
		ChatIntegration:        stored,
		Source:                 ChatSourceDatabase,
		BotTokenConfigured:     have.Bot,
		WebhookTokenConfigured: have.Webhook,
		AppTokenConfigured:     have.App,
	}, nil
}

func trimmedToken(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// ChatIntegrationVersion is what a replica polls: it moves whenever the
// configuration in force changes. The environment's never does.
func (s *Service) ChatIntegrationVersion(ctx context.Context, platform string) (time.Time, error) {
	if _, ok := s.environmentChat(platform); ok {
		return time.Time{}, nil
	}
	return s.store.ChatIntegrationVersion(ctx, platform)
}

// ResolveChatIntegration is the configuration in force with its credentials,
// for the bot itself. ok is false when the platform is not configured, or is
// configured and switched off.
func (s *Service) ResolveChatIntegration(ctx context.Context, platform string) (
	c ResolvedChatIntegration, ok bool, err error) {

	if env, found := s.environmentChat(platform); found {
		return env, true, nil
	}
	stored, err := s.store.ChatIntegration(ctx, platform)
	if errors.Is(err, store.ErrNotFound) {
		return ResolvedChatIntegration{}, false, nil
	}
	if err != nil {
		return ResolvedChatIntegration{}, false, err
	}
	if !stored.Enabled {
		return ResolvedChatIntegration{ChatIntegration: stored}, false, nil
	}

	secrets := ChatSecretsFor(platform)
	c = ResolvedChatIntegration{ChatIntegration: stored}
	token, err := s.store.ResolveSecret(ctx, secrets.Bot)
	if err != nil {
		return ResolvedChatIntegration{}, false, fmt.Errorf("resolve the bot token: %w", err)
	}
	c.BotToken = strings.TrimSpace(string(token))
	if secrets.App != "" {
		token, err := s.store.ResolveSecret(ctx, secrets.App)
		if err != nil {
			return ResolvedChatIntegration{}, false, fmt.Errorf("resolve the app token: %w", err)
		}
		c.AppToken = strings.TrimSpace(string(token))
	}
	if secrets.Webhook != "" && c.Webhook() {
		raw, err := s.store.ResolveSecret(ctx, secrets.Webhook)
		switch {
		case err == nil:
			c.WebhookTokens = SplitChatWebhookTokens(string(raw))
		case errors.Is(err, store.ErrNotFound):
			// Validated at save; gone since. Every delivery is refused, which
			// is the safe reading of "no token".
		default:
			return ResolvedChatIntegration{}, false, fmt.Errorf("resolve the webhook tokens: %w", err)
		}
	}
	return c, true, nil
}

// SplitChatWebhookTokens reads a list of webhook tokens, one per line or
// separated by commas.
func SplitChatWebhookTokens(raw string) []string {
	var out []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	}) {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}

// ValidateChatIntegration is the rule for an integration's configuration, from
// whichever source. A switched-off integration is held to the shape rules only:
// an operator may save half a configuration and finish it later.
func ValidateChatIntegration(c store.ChatIntegration, have ChatCredentials) error {
	invalid := func(field, format string, args ...any) error {
		return &run.InvalidRequestError{Field: field, Detail: fmt.Sprintf(format, args...)}
	}
	slack := c.Platform == ChatPlatformSlack
	switch {
	case slack && c.Transport != store.ChatTransportWebSocket:
		// Socket Mode is the only way in: the backend dials Slack, and Slack
		// never calls the backend.
		return invalid("transport", "%q is not websocket, the only transport Slack has here", c.Transport)
	case c.Transport == store.ChatTransportWebSocket, c.Transport == store.ChatTransportWebhook,
		c.Transport == store.ChatTransportBoth:
	default:
		return invalid("transport", "%q is not websocket, webhook or both", c.Transport)
	}
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return invalid("url", "%q is not the server's http(s) address", c.URL)
		}
	}
	if c.BaseBranch != "" && c.RepoURL == "" {
		return invalid("base_branch", "a base branch needs a repository")
	}
	if c.MaxPostRunes < 1000 || c.MaxPostRunes > 65535 {
		return invalid("max_post_runes", "%d is not between 1000 and 65535", c.MaxPostRunes)
	}
	if c.MaxThreadBytes < 0 {
		return invalid("max_thread_bytes", "cannot be negative")
	}
	if !c.Enabled {
		return nil
	}
	// Slack's address is optional: empty is slack.com.
	if c.URL == "" && !slack {
		return invalid("url", "an enabled integration needs the server's address")
	}
	if c.Role == "" {
		// Required rather than defaulted at this point: the role is what
		// anyone who can reach the bot may do, and that is a decision.
		return invalid("role", "an enabled integration needs a role")
	}
	if !have.Bot {
		return invalid("bot_token", "an enabled integration needs the bot's access token")
	}
	if slack && !have.App {
		return invalid("app_token", "an enabled Slack integration needs the app-level token "+
			"that opens its Socket Mode connection")
	}
	if (c.Transport == store.ChatTransportWebhook || c.Transport == store.ChatTransportBoth) && !have.Webhook {
		return invalid("webhook_tokens", "a webhook transport needs at least one outgoing-webhook token, "+
			"or every delivery would be refused")
	}
	return nil
}
