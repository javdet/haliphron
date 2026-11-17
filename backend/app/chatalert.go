package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Alert replies in a team-chat thread.
//
// Alertmanager notifies people in a chat channel and, separately, an alert
// receiver, which starts a run. A receiver may name one channel; when it does,
// and that platform's integration is enabled when the notification is
// admitted, the run owes its answer in the thread of the channel post that
// announced the same alerts. The agent never touches the chat. The backend
// finds the post and replies, through the same durable chat_triggers reply a
// chat-started run uses.
//
// The two notifications are joined by Alertmanager's fingerprints, which the
// chat notification must print. Which post is the alert's is decided here, not
// in the adapter: the adapter lists a channel's recent root posts with their
// text rendered, and matchAlertPost chooses. The search runs within a bounded
// window, because neither notification is guaranteed to arrive first; when the
// window closes without a match, the answer is posted as a new post in the
// channel, headed by the alerts it is about, so it is never lost.

const (
	// chatAlertSearchBefore is how far before admission a post may have been
	// made and still be the alert's.
	chatAlertSearchBefore = 5 * time.Minute
	// chatAlertSearchFor is how long after admission the post is looked for.
	chatAlertSearchFor = 10 * time.Minute
	// chatAlertSearchEvery is the wait between two searches of one row, and
	// the lease that keeps the other replicas off it meanwhile.
	chatAlertSearchEvery = 15 * time.Second
	// chatAlertSearchPosts bounds the posts one search reads. A channel with
	// more automation posts than this in the window is out of scope: the
	// window closes and the answer is posted as a post of its own.
	chatAlertSearchPosts = 200
)

// ErrChatChannelNotFound is a channel the platform does not know, or that the
// bot cannot see.
var ErrChatChannelNotFound = errors.New("app: the chat channel does not exist or the bot cannot see it")

// ChatChannel is a channel as ChannelInfo reports it.
type ChatChannel struct {
	ID string
	// Name is what a person would call the channel.
	Name string
	// Member is the bot being a member, which posting and reading need.
	Member bool
}

// AlertChatReply is the channel a receiver's runs answer in.
type AlertChatReply struct {
	Platform  string
	ChannelID string
}

// chatRegistry is the running bot of each platform in this replica. The
// supervisors register a Chat when they start one and remove it when they
// stop it; a receiver's channel is checked through it when it is saved.
type chatRegistry struct {
	mu     sync.RWMutex
	byName map[string]*Chat
}

// RegisterChat records a platform's running bot.
func (s *Service) RegisterChat(c *Chat) {
	s.chats.mu.Lock()
	defer s.chats.mu.Unlock()
	if s.chats.byName == nil {
		s.chats.byName = map[string]*Chat{}
	}
	s.chats.byName[c.platform.Name()] = c
}

// UnregisterChat forgets a platform's bot, if it is still the one recorded.
func (s *Service) UnregisterChat(c *Chat) {
	s.chats.mu.Lock()
	defer s.chats.mu.Unlock()
	if s.chats.byName[c.platform.Name()] == c {
		delete(s.chats.byName, c.platform.Name())
	}
}

func (s *Service) runningChat(platform string) *Chat {
	s.chats.mu.RLock()
	defer s.chats.mu.RUnlock()
	return s.chats.byName[platform]
}

// ChatIntegrationEnabled reports whether a platform's integration is switched
// on: configured by the environment, or saved enabled.
func (s *Service) ChatIntegrationEnabled(ctx context.Context, platform string) (bool, error) {
	if _, ok := s.environmentChat(platform); ok {
		return true, nil
	}
	c, err := s.store.ChatIntegration(ctx, platform)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return c.Enabled, nil
}

// checkAlertChatReply is the rule for a receiver's channel, applied when it is
// set or changed: the platform's integration is enabled, its bot is running
// here, and the channel exists with the bot in it. It returns the channel's
// name, which is stored for display.
func (s *Service) checkAlertChatReply(ctx context.Context, r AlertChatReply) (string, error) {
	invalid := func(field, format string, args ...any) error {
		return &run.InvalidRequestError{Field: "chat_reply." + field, Detail: fmt.Sprintf(format, args...)}
	}
	if r.Platform != ChatPlatformMattermost && r.Platform != ChatPlatformSlack {
		return "", invalid("platform", "%q is not mattermost or slack", r.Platform)
	}
	if r.ChannelID == "" || len(r.ChannelID) > 64 || strings.ContainsAny(r.ChannelID, " \t\r\n/") {
		return "", invalid("channel_id", "a channel ID is 1 to 64 characters with no spaces or slashes")
	}
	enabled, err := s.ChatIntegrationEnabled(ctx, r.Platform)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", invalid("platform", "the %s integration is not enabled; enable it before choosing a channel on it",
			r.Platform)
	}
	chat := s.runningChat(r.Platform)
	if chat == nil {
		return "", invalid("platform", "the %s bot is not running in this replica; it may have failed to start "+
			"(see the backend's log) or be starting: try again shortly", r.Platform)
	}
	info, err := chat.platform.ChannelInfo(ctx, r.ChannelID)
	switch {
	case errors.Is(err, ErrChatChannelNotFound):
		return "", invalid("channel_id", "%s has no channel %q that the bot can see; check the ID, "+
			"and for a private channel invite the bot first", r.Platform, r.ChannelID)
	case err != nil:
		return "", fmt.Errorf("read channel %s from %s: %w", r.ChannelID, r.Platform, err)
	case !info.Member:
		return "", invalid("channel_id", "the bot is not a member of channel %q; invite it to the channel first",
			chatOr(info.Name, r.ChannelID))
	}
	return boundedText(info.Name, 256), nil
}

func chatOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// chatAlertReplyFor is the reply a run admitted for a receiver owes, or nil
// when it owes none: no channel, or the platform's integration switched off.
func (s *Service) chatAlertReplyFor(ctx context.Context, row store.AlertReceiver) (*store.ChatAlertReply, error) {
	if row.ChatPlatform == "" {
		return nil, nil
	}
	enabled, err := s.ChatIntegrationEnabled(ctx, row.ChatPlatform)
	if err != nil {
		return nil, err
	}
	if !enabled {
		// The receiver still starts its run; getting the answer anywhere is
		// then up to the agent and its role.
		s.log.Info("the receiver names a chat channel but the integration is not enabled; no reply is owed",
			"receiver", row.ID, "name", row.Name, "platform", row.ChatPlatform)
		return nil, nil
	}
	return &store.ChatAlertReply{
		Platform:     row.ChatPlatform,
		ChannelID:    row.ChatChannelID,
		ChannelName:  boundedText(row.ChatChannelName, 256),
		ReceiverID:   row.ID,
		ReceiverName: boundedText(row.Name, 64),
		SearchBefore: chatAlertSearchBefore,
		SearchFor:    chatAlertSearchFor,
	}, nil
}

// SearchThreads makes one pass over the alert rows whose post is still to be
// found, and returns how many it found.
func (c *Chat) SearchThreads(ctx context.Context) (int, error) {
	due, err := c.svc.store.DueChatThreadSearches(ctx, c.platform.Name(), chatBatch, chatAlertSearchEvery)
	if err != nil {
		return 0, err
	}
	found := 0
	for _, d := range due {
		posts, err := c.platform.RecentPosts(ctx, d.ChannelID, d.Since, chatAlertSearchPosts)
		if err != nil {
			// The lease brings the row back in chatAlertSearchEvery.
			c.log.Warn("could not read the channel an alert's post is looked for in",
				"message", d.MessageID, "channel", d.ChannelID, "error", err)
			continue
		}
		post, ok := matchAlertPost(posts, d.Fingerprints, d.AdmittedAt)
		if !ok {
			continue
		}
		won, err := c.svc.store.ResolveChatThread(ctx, d.ChatTriggerKey, post.ID)
		if err != nil {
			return found, err
		}
		if !won {
			continue
		}
		found++
		c.log.Info("found the chat post of an alert", "message", d.MessageID, "run", d.RunID,
			"channel", d.ChannelID, "post", post.ID)
		// The same acknowledgement a chat-started run gets, and as best
		// effort: the reply that matters is the durable one.
		if err := c.platform.React(ctx, post.ID, "eyes"); err != nil {
			c.log.Warn("could not react to an alert's chat post", "post", post.ID, "error", err)
		}
	}
	if found > 0 {
		c.Kick()
	}
	return found, nil
}

// matchAlertPost picks the post that announced an alert: a root post made by
// automation other than this bot, whose text holds one of the fingerprints as
// a whole token, created nearest to the run's admission.
func matchAlertPost(posts []ChatThreadPost, fingerprints []string, admitted time.Time) (ChatThreadPost, bool) {
	var (
		best     ChatThreadPost
		bestDist time.Duration
		ok       bool
	)
	for _, p := range posts {
		if p.RootID != "" || !p.FromBot || p.FromSelf || p.System {
			continue
		}
		if !slices.ContainsFunc(fingerprints, func(fp string) bool { return containsToken(p.Text, fp) }) {
			continue
		}
		dist := p.CreatedAt.Sub(admitted)
		if dist < 0 {
			dist = -dist
		}
		if !ok || dist < bestDist {
			best, bestDist, ok = p, dist, true
		}
	}
	return best, ok
}

// containsToken reports whether token occurs in s with no letter or digit
// directly before or after it: a fingerprint inside a longer hex string, or a
// word, is not that fingerprint.
func containsToken(s, token string) bool {
	if token == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(s[i:], token)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(token)
		if (start == 0 || !alnum(s[start-1])) && (end == len(s) || !alnum(s[end])) {
			return true
		}
		i = start + 1
	}
}

func alnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// chatAlertRootHeader opens an alert's reply posted as a post of its own,
// because the post that announced the alerts was never found.
func chatAlertRootHeader(names []string) string {
	var unique []string
	for _, n := range names {
		if n = strings.TrimSpace(chatHeaderValue(n)); n != "" && !slices.Contains(unique, n) {
			unique = append(unique, n)
		}
	}
	const most = 5
	label := "Alert"
	switch {
	case len(unique) == 0:
		unique = []string{"(unnamed)"}
	case len(unique) > 1:
		label = "Alerts"
	}
	if len(unique) > most {
		unique = append(unique[:most:most], fmt.Sprintf("and %d more", len(unique)-most))
	}
	return neutralizeMentions(label + " " + strings.Join(unique, ", ") +
		": no matching post was found in this channel (the notification must print its fingerprints).")
}

// chatAlertPromptLine is the sentence a run that owes a chat reply is told.
// Nothing that identifies the channel or the post goes into it but the
// channel's name.
func chatAlertPromptLine(channelName string) string {
	where := "the team-chat channel"
	if name := strings.TrimPrefix(chatHeaderValue(channelName), "#"); name != "" {
		where = "#" + name
	}
	return "Your final answer will be posted in the thread of this alert in " + where +
		"; write it for the people on call."
}

// ChatAlertThreadUsage is what the metrics endpoint reports about the search
// for alert posts.
func (s *Service) ChatAlertThreadUsage(ctx context.Context) ([]store.ChatAlertThreads, error) {
	return s.store.ChatAlertThreadUsage(ctx)
}
