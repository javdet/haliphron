package restapi

import (
	"net/http"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// The chat integrations, as an operator configures them from the UI. The rules
// — validation, which source is in force, where the tokens are kept — are
// app's; this is the wire shape.

// mattermostPlatform is the platform's identifier, the same string the adapter
// uses. Spelled here rather than imported, because this package does not import
// the adapter: the adapter mounts its webhook here, not the other way round.
const mattermostPlatform = "mattermost"

// mattermostWebhookPath is where the adapter's webhook is mounted, reported
// so the UI can show the address to paste into Mattermost.
const mattermostWebhookPath = BasePath + "/integrations/mattermost/webhook"

type chatIntegrationRequest struct {
	Enabled        bool   `json:"enabled"`
	Transport      string `json:"transport"`
	URL            string `json:"url"`
	Role           string `json:"role"`
	Repo           string `json:"repo"`
	BaseBranch     string `json:"base_branch"`
	RunURL         string `json:"run_url"`
	ReplyPolicy    string `json:"reply_policy"`
	MaxPostRunes   int    `json:"max_post_runes"`
	MaxThreadBytes int    `json:"max_thread_bytes"`
	// Write-only. Absent keeps what is stored.
	BotToken      *string `json:"bot_token,omitempty"`
	WebhookTokens *string `json:"webhook_tokens,omitempty"`
}

type chatIntegrationBody struct {
	Platform               string     `json:"platform"`
	Source                 string     `json:"source"`
	Enabled                bool       `json:"enabled"`
	Transport              string     `json:"transport"`
	URL                    string     `json:"url"`
	Role                   string     `json:"role"`
	Repo                   string     `json:"repo"`
	BaseBranch             string     `json:"base_branch"`
	RunURL                 string     `json:"run_url"`
	ReplyPolicy            string     `json:"reply_policy"`
	MaxPostRunes           int        `json:"max_post_runes"`
	MaxThreadBytes         int        `json:"max_thread_bytes"`
	BotTokenConfigured     bool       `json:"bot_token_configured"`
	WebhookTokenConfigured bool       `json:"webhook_token_configured"`
	WebhookPath            string     `json:"webhook_path"`
	BotTokenSecret         string     `json:"bot_token_secret"`
	WebhookTokenSecret     string     `json:"webhook_token_secret"`
	UpdatedBy              string     `json:"updated_by,omitempty"`
	UpdatedAt              *time.Time `json:"updated_at,omitempty"`
}

func (s *Server) getMattermost(w http.ResponseWriter, r *http.Request, _ caller) {
	c, err := s.app.ChatIntegration(r.Context(), mattermostPlatform)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, chatIntegrationResponse(c))
}

func (s *Server) putMattermost(w http.ResponseWriter, r *http.Request, c caller) {
	var req chatIntegrationRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	saved, err := s.app.PutChatIntegration(r.Context(), mattermostPlatform, app.ChatIntegrationRequest{
		Enabled:        req.Enabled,
		Transport:      req.Transport,
		URL:            req.URL,
		Role:           req.Role,
		RepoURL:        req.Repo,
		BaseBranch:     req.BaseBranch,
		RunURL:         req.RunURL,
		ReplyPolicy:    req.ReplyPolicy,
		MaxPostRunes:   req.MaxPostRunes,
		MaxThreadBytes: req.MaxThreadBytes,
		BotToken:       req.BotToken,
		WebhookTokens:  req.WebhookTokens,
	}, c.Name())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, chatIntegrationResponse(saved))
}

func chatIntegrationResponse(c app.ChatIntegration) chatIntegrationBody {
	body := chatIntegrationBody{
		Platform:               mattermostPlatform,
		Source:                 c.Source,
		Enabled:                c.Enabled,
		Transport:              c.Transport,
		URL:                    c.URL,
		Role:                   c.Role,
		Repo:                   c.RepoURL,
		BaseBranch:             c.BaseBranch,
		RunURL:                 c.RunURL,
		ReplyPolicy:            c.ReplyPolicy,
		MaxPostRunes:           c.MaxPostRunes,
		MaxThreadBytes:         c.MaxThreadBytes,
		BotTokenConfigured:     c.BotTokenConfigured,
		WebhookTokenConfigured: c.WebhookTokenConfigured,
		WebhookPath:            mattermostWebhookPath,
		BotTokenSecret:         app.ChatBotTokenSecret,
		WebhookTokenSecret:     app.ChatWebhookTokenSecret,
		UpdatedBy:              c.UpdatedBy,
	}
	if !c.UpdatedAt.IsZero() {
		at := c.UpdatedAt
		body.UpdatedAt = &at
	}
	return body
}
