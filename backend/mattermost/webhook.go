package mattermost

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// The outgoing-webhook transport: Mattermost calls the backend.
//
// It is the second way in beside the WebSocket, for an installation that
// already feeds a bot this way — n8n's pattern — or cannot hold a socket open.
// It sees less: an outgoing webhook fires only in public channels, on a trigger
// word or on every post of one channel, never in a direct message.
//
// Authentication is the token Mattermost issues per webhook and sends in the
// body. It is compared in constant time against every configured token, and a
// delivery that matches none gets a bare 401.
//
// The payload is then treated as a notification and nothing more. The post it
// names is read back from the server with the bot's own token, and everything
// the rules look at — the author, the channel, the thread, the text, whether a
// bot wrote it — comes from that answer. A leaked webhook token therefore lets
// its holder make the bot look at a post that exists, in a channel the bot can
// read, and nothing else: it cannot put words in anyone's mouth.

const (
	// maxWebhookBytes bounds a delivery. The payload carries one post's text
	// and a handful of identifiers.
	maxWebhookBytes = 64 << 10
	// webhookHandleTimeout bounds the work done for one delivery after it has
	// been answered: the post read, the claim and admission.
	webhookHandleTimeout = 2 * time.Minute
)

// webhookPayload is an outgoing webhook's body, in either encoding Mattermost
// sends it.
type webhookPayload struct {
	Token       string `json:"token"`
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	PostID      string `json:"post_id"`
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	Text        string `json:"text"`
	TriggerWord string `json:"trigger_word"`
}

// decodeWebhook reads a delivery. Mattermost sends a form by default and JSON
// when the webhook is configured for it.
func decodeWebhook(r *http.Request) (webhookPayload, error) {
	var p webhookPayload
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return webhookPayload{}, fmt.Errorf("the body is not a webhook payload: %w", err)
		}
		return p, nil
	}
	if err := r.ParseForm(); err != nil {
		return webhookPayload{}, fmt.Errorf("the body is not a webhook payload: %w", err)
	}
	f := r.PostForm
	return webhookPayload{
		Token:       f.Get("token"),
		TeamID:      f.Get("team_id"),
		ChannelID:   f.Get("channel_id"),
		ChannelName: f.Get("channel_name"),
		PostID:      f.Get("post_id"),
		UserID:      f.Get("user_id"),
		UserName:    f.Get("user_name"),
		Text:        f.Get("text"),
		TriggerWord: f.Get("trigger_word"),
	}, nil
}

// tokenMatches compares a delivery's token against every configured one, in
// constant time, without stopping at the first match.
func tokenMatches(got string, want []string) bool {
	if got == "" {
		return false
	}
	match := 0
	for _, w := range want {
		match |= subtle.ConstantTimeCompare([]byte(got), []byte(w))
	}
	return match == 1
}

// ServeHTTP is the webhook endpoint.
func (s *Supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt := s.current.Load()
	if rt == nil || !rt.cfg.Webhook() {
		// Not "forbidden": as far as the caller can tell there is nothing
		// here, which is the truth while the transport is off.
		webhookAnswer(w, http.StatusNotFound, "the Mattermost webhook is not enabled")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBytes)
	p, err := decodeWebhook(r)
	if err != nil {
		webhookAnswer(w, http.StatusBadRequest, "unreadable payload")
		return
	}
	if !tokenMatches(p.Token, rt.cfg.WebhookTokens) {
		s.log.Warn("a webhook delivery carried an unknown token", "remote", r.RemoteAddr)
		webhookAnswer(w, http.StatusUnauthorized, "")
		return
	}
	if p.PostID == "" || len(p.PostID) > 64 {
		webhookAnswer(w, http.StatusBadRequest, "the payload names no post")
		return
	}

	// A free worker is taken before answering. Answering first and queueing
	// would hold every delivery of a burst in memory; waiting here makes the
	// server's own timeout the bound, as the socket's reads are for the
	// WebSocket.
	select {
	case s.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	}

	// An empty JSON object: Mattermost posts the body's text, if it has any,
	// as the bot's reply, and the reply here is the run's to make.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		// Detached from the request, which ends with the answer above, and
		// from shutdown: a claim abandoned halfway is answered five minutes
		// later with "ask again", and finishing is cheaper than that.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), webhookHandleTimeout)
		defer cancel()
		s.handleWebhook(ctx, rt, p)
	}()
}

func webhookAnswer(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if message == "" {
		_, _ = w.Write([]byte("{}"))
		return
	}
	body, _ := json.Marshal(map[string]map[string]string{"error": {"message": message}})
	_, _ = w.Write(body)
}

// handleWebhook reads the post a delivery names and hands it to the rules.
func (s *Supervisor) handleWebhook(ctx context.Context, rt *running, p webhookPayload) {
	post, err := rt.client.getPost(ctx, p.PostID)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Permanent() {
			// A post the bot cannot read — deleted, in a channel it is not a
			// member of, or never existed — is not acted on.
			s.log.Warn("a webhook delivery names a post the bot cannot read",
				"post", p.PostID, "status", apiErr.Status)
			return
		}
		s.log.Error("could not read the post a webhook delivery names", "post", p.PostID, "error", err)
		return
	}
	if post.ID != p.PostID || (p.ChannelID != "" && post.ChannelID != p.ChannelID) {
		s.log.Warn("a webhook delivery does not match the post it names", "post", p.PostID)
		return
	}

	self := rt.client.me()
	if self.ID == "" {
		if self, err = rt.client.Me(ctx); err != nil {
			s.log.Error("could not read the bot's own account", "error", err)
			return
		}
	}
	username, err := rt.client.username(ctx, post.UserID)
	if err != nil || username == "" {
		username = p.UserName
	}

	m := webhookMessage(post, p, self, username)
	outcome, err := rt.chat.Handle(ctx, m)
	switch {
	case err != nil:
		s.log.Error("a webhook delivery could not be handled", "message", m.ID, "error", err)
	case outcome != app.ChatIgnored:
		s.log.Debug("webhook delivery handled", "message", m.ID, "outcome", outcome.String())
	}
}

// webhookMessage is the message a delivery stands for, built from the post as
// the server reported it.
//
// A delivery is addressed to the bot by construction: an operator pointed the
// webhook at it, on a trigger word or on a channel. The trigger word is taken
// off the front of the text the way a mention is, and a mention of the bot is
// taken off as well, so "@bot help" and "triage: help" are both "help".
func webhookMessage(post wirePost, p webhookPayload, self User, username string) app.ChatMessage {
	text := strings.TrimSpace(post.Message)
	if word := strings.TrimSpace(p.TriggerWord); word != "" &&
		len(text) >= len(word) && strings.EqualFold(text[:len(word)], word) {
		text = strings.TrimLeft(text[len(word):], " \t\r\n,:;")
	}
	text, _ = stripMention(text, self.Username)

	return app.ChatMessage{
		ID:          post.ID,
		ChannelID:   post.ChannelID,
		ChannelName: p.ChannelName,
		// Outgoing webhooks fire in public channels only.
		ChannelType: "O",
		RootID:      post.RootID,
		UserID:      post.UserID,
		Username:    username,
		Text:        text,
		CreatedAt:   time.UnixMilli(post.CreateAt),
		MentionsBot: true,
		FromSelf:    self.ID != "" && post.UserID == self.ID,
		FromBot:     fromBot(post),
		FromWebhook: truthy(post.Props["from_webhook"]),
		System:      post.Type != "" || post.DeleteAt != 0,
	}
}
