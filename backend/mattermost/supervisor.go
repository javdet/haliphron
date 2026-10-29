package mattermost

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// The bot's lifecycle, driven by its configuration.
//
// The configuration is data an operator changes from the UI, so the bot is
// not started once at boot but kept in step with it: each replica polls the
// configuration's version and, when it moves, stops the running bot — socket,
// reply loop and all — and starts it again from what is stored now. A
// configuration taken from the environment never moves, and is started once.
//
// What is running is published through an atomic pointer, which is what the
// webhook endpoint reads: a delivery is judged against the tokens and the
// transport in force at the moment it arrives.

// DefaultPollInterval is how often a replica looks for a changed configuration.
const DefaultPollInterval = 15 * time.Second

// Supervisor runs the Mattermost bot as its configuration says, and serves
// the webhook endpoint.
type Supervisor struct {
	svc  *app.Service
	log  *slog.Logger
	http *http.Client
	poll time.Duration

	current atomic.Pointer[running]

	// slots bounds the webhook deliveries handled at once, as the listener's
	// own slots do for the socket.
	slots chan struct{}
	wg    sync.WaitGroup
}

// running is one configuration, started.
type running struct {
	cfg    app.ResolvedChatIntegration
	client *Client
	chat   *app.Chat
	stop   context.CancelFunc
	done   sync.WaitGroup
}

// NewSupervisor returns a supervisor over the use cases. httpClient may be nil.
func NewSupervisor(svc *app.Service, log *slog.Logger, httpClient *http.Client) *Supervisor {
	return &Supervisor{
		svc:   svc,
		log:   log.With("platform", Name),
		http:  httpClient,
		poll:  DefaultPollInterval,
		slots: make(chan struct{}, workers),
	}
}

// WithPollInterval changes how often the configuration is checked, for tests.
func (s *Supervisor) WithPollInterval(d time.Duration) *Supervisor {
	if d > 0 {
		s.poll = d
	}
	return s
}

// Run keeps the bot in step with its configuration until the context ends,
// and returns once everything it started has stopped.
func (s *Supervisor) Run(ctx context.Context) {
	defer s.wg.Wait()
	defer s.stop()

	var (
		version time.Time
		known   bool
	)
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		v, err := s.svc.ChatIntegrationVersion(ctx, Name)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				s.log.Warn("could not read the Mattermost configuration's version", "error", err)
			}
		case !known || !v.Equal(version):
			if s.reload(ctx) {
				version, known = v, true
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reload stops what is running and starts what is configured. It reports
// whether the configuration could be read; when it could not, the running bot
// is left as it was and the next tick tries again.
func (s *Supervisor) reload(ctx context.Context) bool {
	cfg, enabled, err := s.svc.ResolveChatIntegration(ctx, Name)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("could not read the Mattermost configuration; the bot is left as it was", "error", err)
		}
		return false
	}
	s.stop()
	if !enabled {
		s.log.Info("the Mattermost bot is not enabled")
		return true
	}
	if err := s.start(ctx, cfg); err != nil {
		s.log.Error("the Mattermost bot could not be started", "error", err)
	}
	return true
}

func (s *Supervisor) start(ctx context.Context, cfg app.ResolvedChatIntegration) error {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return err
	}
	client := NewClient(Config{
		BaseURL:      base,
		Token:        cfg.BotToken,
		HTTP:         s.http,
		MaxPostRunes: cfg.MaxPostRunes,
	})
	chat := s.svc.NewChat(client, app.ChatConfig{
		Role:           cfg.Role,
		RepoURL:        cfg.RepoURL,
		BaseBranch:     cfg.BaseBranch,
		MaxThreadBytes: cfg.MaxThreadBytes,
	})
	if err := chat.CheckRole(ctx); errors.Is(err, store.ErrNotFound) {
		s.log.Warn("the Mattermost bot's role does not exist; every request is refused until it is created",
			"role", cfg.Role)
	} else if err != nil {
		s.log.Warn("could not check the Mattermost bot's role", "role", cfg.Role, "error", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	rt := &running{cfg: cfg, client: client, chat: chat, stop: cancel}
	if cfg.WebSocket() {
		listener := NewListener(client, chat, s.log)
		rt.done.Add(1)
		go func() { defer rt.done.Done(); listener.Run(runCtx) }()
	}
	rt.done.Add(1)
	go func() { defer rt.done.Done(); chat.RunReplier(runCtx, 5*time.Second) }()

	s.current.Store(rt)
	s.log.Info("the Mattermost bot is running",
		"url", cfg.URL, "transport", cfg.Transport, "role", cfg.Role,
		"configured_by", cfg.UpdatedBy)
	return nil
}

// stop ends the running bot, if any, and waits for it.
func (s *Supervisor) stop() {
	rt := s.current.Swap(nil)
	if rt == nil {
		return
	}
	rt.stop()
	rt.done.Wait()
}
