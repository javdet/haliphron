package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The chat bot: a message addressed to the bot account is a run, and the run's
// ending is a reply in the thread that asked for it.
//
// The adapter for a platform is transport. It decodes what the chat server
// sends into a ChatMessage and carries posts back out through ChatPlatform;
// every decision — what counts as addressed to the bot, what the prompt is,
// which role it runs as, what the reply says — is made here, so a second
// platform answers by the same rules as the first.
//
// Two properties shape the code.
//
// Every replica sees every message. Each holds its own connection as the bot,
// so a message arrives once per replica, and the first thing done about one is
// a claim in the store keyed by the message's identifier. The loser of the
// claim does nothing, not even fetch the thread.
//
// The reply is owed durably. It is recorded with the run in the run's own
// insert, and posted by a loop that reads the store rather than by the handler
// that admitted the run, so a restart between the two loses nothing. A reply
// may be posted twice — a crash between the post and recording it — and never
// zero times while the chat server keeps accepting posts.

// ChatPlatform is a chat server as the rules need it.
type ChatPlatform interface {
	// Name is the platform's identifier, used as the store's key space and as
	// the run's created_via.
	Name() string
	// Thread returns the posts of the thread rooted at rootID.
	Thread(ctx context.Context, rootID string) ([]ChatThreadPost, error)
	// Post publishes a post and returns its identifier.
	Post(ctx context.Context, p ChatPost) (string, error)
	// React adds the bot's reaction to a post.
	React(ctx context.Context, postID, emoji string) error
	// MaxPostRunes is the longest post the server accepts.
	MaxPostRunes() int
}

// ChatMessage is a message as the adapter decoded it. The adapter reports
// facts; whether to act on them is Handle's decision.
type ChatMessage struct {
	ID        string
	ChannelID string
	// RootID is the thread the message was posted in, empty when it started
	// none.
	RootID   string
	UserID   string
	Username string
	// Text is the message with the bot's mention removed.
	Text      string
	CreatedAt time.Time

	// Direct is a one-to-one conversation with the bot.
	Direct bool
	// MentionsBot is the bot named in the message.
	MentionsBot bool
	// FromSelf is the bot's own post, echoed back by the server.
	FromSelf bool
	// FromBot and FromWebhook are posts by other automation.
	FromBot     bool
	FromWebhook bool
	// System is a server-generated post: a join, a header change.
	System bool
}

// ChatThreadPost is one post of a thread, as context for a prompt.
type ChatThreadPost struct {
	ID        string
	Username  string
	Text      string
	CreatedAt time.Time
	FromSelf  bool
	System    bool
}

// ChatPost is a post the bot publishes.
type ChatPost struct {
	ChannelID string
	RootID    string
	Text      string
	// RunID and Kind are attached to the post as metadata, so a person — or a
	// later version of this code — can tell which run a reply belongs to.
	RunID runv1.ULID
	Kind  string
}

// The kinds of post the bot makes.
const (
	ChatPostAck     = "ack"
	ChatPostResult  = "result"
	ChatPostRefusal = "refusal"
)

// ChatConfig is how the installation's bot runs what it is asked.
type ChatConfig struct {
	// Role is the role every run from the bot is admitted under. The role is
	// the boundary: anyone who can reach the bot can use it.
	Role       string
	RepoURL    string
	BaseBranch string
	// RunURL is a link to a run in the UI, with {id} where the identifier
	// goes. Empty means replies carry no link.
	RunURL string
	// MaxThreadBytes bounds the thread history quoted into a prompt.
	MaxThreadBytes int
}

// ChatOutcome is what Handle did with a message.
type ChatOutcome int

const (
	// ChatIgnored is a message not addressed to the bot.
	ChatIgnored ChatOutcome = iota
	// ChatDuplicate is a message another replica, or an earlier delivery, has
	// already acted on.
	ChatDuplicate
	// ChatStarted is a message that started a run.
	ChatStarted
	// ChatRefused is a message answered with a refusal.
	ChatRefused
)

func (o ChatOutcome) String() string {
	switch o {
	case ChatIgnored:
		return "ignored"
	case ChatDuplicate:
		return "duplicate"
	case ChatStarted:
		return "started"
	case ChatRefused:
		return "refused"
	}
	return "unknown"
}

const (
	// chatHandleTimeout bounds one message: the claim, the thread fetch and
	// admission.
	chatHandleTimeout = time.Minute
	// chatOrphanAfter is how long a claim may go unanswered before the reply
	// loop takes its handler as dead. Comfortably past chatHandleTimeout, so a
	// slow admission is never mistaken for a lost one.
	chatOrphanAfter = 5 * time.Minute
	// chatReplyLease is how long a claimed reply is withheld from the other
	// replicas. Longer than the adapter's HTTP timeout, so a slow post never
	// outlives its own lease and is attempted twice.
	chatReplyLease = time.Minute
	// chatSettleRecheck is how soon a run that ended without its report yet is
	// looked at again.
	chatSettleRecheck = 10 * time.Second
	// chatMaxAttempts is how many failed posts a reply gets before it is given
	// up on.
	chatMaxAttempts = 20
	chatBatch       = 20
	// chatRefusalRetention is how long a refusal is kept once answered. A row
	// with a run is kept for exactly as long as the run.
	chatRefusalRetention = 30 * 24 * time.Hour
)

// chatLostClaim is the answer to a message whose handler died before deciding
// anything. Asking again is safe: nothing was started.
const chatLostClaim = "I could not start a run for this message. Nothing was started; mention me again to retry."

// Chat is the bot's rules bound to one platform.
type Chat struct {
	svc      *Service
	platform ChatPlatform
	cfg      ChatConfig
	log      *slog.Logger
	kick     chan struct{}
}

// NewChat binds the bot's rules to a platform.
func (s *Service) NewChat(p ChatPlatform, cfg ChatConfig) *Chat {
	if cfg.MaxThreadBytes <= 0 {
		cfg.MaxThreadBytes = DefaultChatThreadBytes
	}
	return &Chat{
		svc:      s,
		platform: p,
		cfg:      cfg,
		log:      s.log.With("platform", p.Name()),
		kick:     make(chan struct{}, 1),
	}
}

// DefaultChatThreadBytes is the thread history a prompt carries when the
// installation does not say.
const DefaultChatThreadBytes = 64 << 10

// CheckRole reports whether the configured role exists. A missing role is not
// fatal — it can be created after the bot is enabled — but every mention until
// then is refused, and an operator should hear that at startup rather than
// from a user.
func (c *Chat) CheckRole(ctx context.Context) error {
	if c.cfg.Role == "" {
		return nil
	}
	_, err := c.svc.store.RoleByName(ctx, c.cfg.Role)
	return err
}

// Handle acts on one message.
func (c *Chat) Handle(ctx context.Context, m ChatMessage) (ChatOutcome, error) {
	if !addressedToBot(m) {
		return ChatIgnored, nil
	}
	ctx, cancel := context.WithTimeout(ctx, chatHandleTimeout)
	defer cancel()

	thread := m.RootID
	if thread == "" {
		thread = m.ID
	}
	key := store.ChatTriggerKey{Platform: c.platform.Name(), MessageID: m.ID}
	won, err := c.svc.store.ClaimChatTrigger(ctx, store.ChatTrigger{
		ChatTriggerKey: key,
		ChannelID:      m.ChannelID,
		ThreadID:       thread,
		UserID:         m.UserID,
		Username:       m.Username,
	}, chatOrphanAfter)
	if err != nil {
		return 0, err
	}
	if !won {
		return ChatDuplicate, nil
	}

	var history []ChatThreadPost
	if m.RootID != "" {
		if history, err = c.platform.Thread(ctx, m.RootID); err != nil {
			// Running without the thread would run a different request from
			// the one asked: "now do the same for staging" means nothing on
			// its own. The person is asked to try again instead.
			c.log.Warn("could not read the thread a message was posted in",
				"message", m.ID, "thread", m.RootID, "error", err)
			return c.refuse(ctx, key, "I could not read this thread, so I did not start a run. Mention me again to retry.")
		}
	}

	if strings.TrimSpace(m.Text) == "" && len(contextPosts(history, m)) == 0 {
		return c.refuse(ctx, key, "Tell me what to do: mention me followed by the task.")
	}

	submitted, err := c.svc.submit(ctx, run.SubmitRequest{
		Prompt:     BuildChatPrompt(history, m, c.svc.maxPromptBytes(), c.cfg.MaxThreadBytes),
		Role:       c.cfg.Role,
		RepoURL:    c.cfg.RepoURL,
		BaseBranch: c.cfg.BaseBranch,
		CreatedBy:  chatActor(c.platform.Name(), m),
		CreatedVia: c.platform.Name(),
	}, &key)
	if errors.Is(err, store.ErrChatTriggerSettled) {
		// The reply loop took the claim for dead and has already answered.
		return ChatDuplicate, nil
	}
	if err != nil {
		text, internal := renderRefusal(err)
		if internal {
			c.log.Error("a chat message could not be admitted", "message", m.ID, "error", err)
		}
		return c.refuse(ctx, key, text)
	}

	id := submitted.Run.ID
	c.log.Info("a chat message started a run", "message", m.ID, "channel", m.ChannelID, "run", id)

	// The acknowledgement is best effort. The reply that matters is the
	// durable one, and a person whose acknowledgement was lost still gets it.
	if err := c.platform.React(ctx, m.ID, "eyes"); err != nil {
		c.log.Warn("could not react to a chat message", "message", m.ID, "error", err)
	}
	if _, err := c.platform.Post(ctx, ChatPost{
		ChannelID: m.ChannelID, RootID: thread, RunID: id, Kind: ChatPostAck,
		Text: renderAck(id, c.cfg.Role, c.runLink(id)),
	}); err != nil {
		c.log.Warn("could not acknowledge a chat message", "message", m.ID, "run", id, "error", err)
	}
	return ChatStarted, nil
}

// refuse records a refusal and wakes the reply loop to post it. It is recorded
// rather than posted from here so that a refusal survives the same failures a
// result does, and is posted by one replica.
func (c *Chat) refuse(ctx context.Context, key store.ChatTriggerKey, text string) (ChatOutcome, error) {
	// The refusal is written even when the handler's own deadline is what went
	// wrong: an unrecorded refusal is a claim the loop answers five minutes
	// later with a vaguer message.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := c.svc.store.RefuseChatTrigger(ctx, key, text); err != nil {
		return 0, err
	}
	c.Kick()
	return ChatRefused, nil
}

// Kick wakes the reply loop.
func (c *Chat) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// RunReplier posts owed replies until the context ends.
func (c *Chat) RunReplier(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.kick:
		}
		if _, err := c.Deliver(ctx); err != nil && !ctxDone(ctx) {
			c.log.Error("chat reply pass failed", "error", err)
		}
	}
}

// Deliver makes one pass over the replies that are due, and returns how many
// were posted.
func (c *Chat) Deliver(ctx context.Context) (int, error) {
	due, err := c.svc.store.DueChatReplies(ctx, c.platform.Name(), chatBatch, chatReplyLease)
	if err != nil {
		return 0, err
	}
	posted := 0
	for _, t := range due {
		ok, err := c.deliver(ctx, t)
		if err != nil {
			return posted, err
		}
		if ok {
			posted++
		}
	}
	return posted, nil
}

// deliver posts one reply. It reports whether a post was made; an error is the
// store failing, not the chat server.
func (c *Chat) deliver(ctx context.Context, t store.ChatTrigger) (bool, error) {
	var (
		text string
		kind = ChatPostRefusal
	)
	switch {
	case t.RunID != "":
		r, err := c.svc.store.RunByID(ctx, t.RunID)
		if errors.Is(err, store.ErrNotFound) {
			// Deleted between the claim and the read; the row went with it.
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !c.svc.ResultSettled(r) {
			return false, c.svc.store.DeferChatReply(ctx, t.ChatTriggerKey, chatSettleRecheck)
		}
		text = renderResult(r, c.runLink(r.ID), c.platform.MaxPostRunes())
		kind = ChatPostResult
	case t.Refusal != "":
		text = t.Refusal
	default:
		// The handler that claimed this died before deciding. Converting the
		// claim is conditional on admission not having linked a run since, so
		// the two cannot both answer.
		converted, err := c.svc.store.RefuseChatTrigger(ctx, t.ChatTriggerKey, chatLostClaim)
		if err != nil || !converted {
			return false, err
		}
		c.log.Warn("a chat message was claimed and never answered", "message", t.MessageID)
		text = chatLostClaim
	}

	postID, err := c.platform.Post(ctx, ChatPost{
		ChannelID: t.ChannelID, RootID: t.ThreadID, Text: text, RunID: t.RunID, Kind: kind,
	})
	if err != nil {
		abandon := isPermanent(err) || t.Attempts+1 >= chatMaxAttempts
		if abandon {
			c.log.Error("giving up on a chat reply", "message", t.MessageID, "run", t.RunID,
				"attempts", t.Attempts+1, "error", err)
		} else {
			c.log.Warn("could not post a chat reply", "message", t.MessageID, "run", t.RunID,
				"attempts", t.Attempts+1, "error", err)
		}
		return false, c.svc.store.FailChatReply(ctx, t.ChatTriggerKey, err.Error(),
			chatBackoff(t.Attempts), abandon)
	}
	if err := c.svc.store.MarkChatReplied(ctx, t.ChatTriggerKey, postID); err != nil {
		return true, err
	}
	c.log.Info("chat reply posted", "message", t.MessageID, "run", t.RunID, "kind", kind)
	return true, nil
}

func (c *Chat) runLink(id runv1.ULID) string {
	if c.cfg.RunURL == "" {
		return ""
	}
	return strings.ReplaceAll(c.cfg.RunURL, "{id}", string(id))
}

// addressedToBot is the rule for which messages are requests. A direct
// conversation needs no mention: there is nobody else to address. Anything the
// bot itself or other automation posted is never a request, which is also what
// stops two bots answering each other for ever.
func addressedToBot(m ChatMessage) bool {
	if m.FromSelf || m.FromBot || m.FromWebhook || m.System {
		return false
	}
	return m.MentionsBot || m.Direct
}

func chatActor(platform string, m ChatMessage) string {
	if m.Username != "" {
		return platform + ":" + m.Username
	}
	return platform + ":" + m.UserID
}

// chatBackoff is the wait after the given number of failed posts.
func chatBackoff(attempts int) time.Duration {
	d := 10 * time.Second
	for i := 0; i < attempts && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}

// isPermanent reports whether the chat server refused a post in a way a retry
// will not change.
func isPermanent(err error) bool {
	var p interface{ Permanent() bool }
	return errors.As(err, &p) && p.Permanent()
}

func (s *Service) maxPromptBytes() int {
	if s.defaults.MaxPromptBytes > 0 {
		return s.defaults.MaxPromptBytes
	}
	return run.MaxPromptBytes
}
