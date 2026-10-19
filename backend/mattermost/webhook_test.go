package mattermost

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

func webhookSupervisor(transport string, tokens ...string) *Supervisor {
	s := NewSupervisor(nil, slog.New(slog.DiscardHandler), nil)
	s.current.Store(&running{cfg: app.ResolvedChatIntegration{
		ChatIntegration: store.ChatIntegration{Transport: transport},
		WebhookTokens:   tokens,
	}})
	return s
}

func deliver(s *Supervisor, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/mattermost/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestTheWebhookIsNotThereWhileItsTransportIsOff(t *testing.T) {
	if rec := deliver(NewSupervisor(nil, slog.New(slog.DiscardHandler), nil),
		"application/json", `{"token":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("no bot running: %d", rec.Code)
	}
	if rec := deliver(webhookSupervisor(store.ChatTransportWebSocket, "secret"),
		"application/json", `{"token":"secret","post_id":"p"}`); rec.Code != http.StatusNotFound {
		t.Errorf("websocket only: %d", rec.Code)
	}
}

func TestAWebhookDeliveryWithAnUnknownTokenIsRefusedWithoutDetail(t *testing.T) {
	s := webhookSupervisor(store.ChatTransportBoth, "first", "second")
	for _, body := range []string{`{"token":"third","post_id":"p"}`, `{"post_id":"p"}`} {
		rec := deliver(s, "application/json", body)
		if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != "{}" {
			t.Errorf("%s: %d %q", body, rec.Code, rec.Body.String())
		}
	}
}

func TestAWebhookDeliveryIsReadAsAFormOrAsJSON(t *testing.T) {
	form := url.Values{"token": {"t"}, "post_id": {"p1"}, "channel_id": {"c1"},
		"channel_name": {"ops"}, "user_name": {"alice"}, "text": {"triage: disk full"},
		"trigger_word": {"triage:"}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	p, err := decodeWebhook(req)
	if err != nil || p.Token != "t" || p.PostID != "p1" || p.ChannelName != "ops" || p.TriggerWord != "triage:" {
		t.Errorf("form: %+v %v", p, err)
	}

	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"token":"t","post_id":"p2","channel_name":"ops"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	p, err = decodeWebhook(req)
	if err != nil || p.Token != "t" || p.PostID != "p2" {
		t.Errorf("json: %+v %v", p, err)
	}
}

func TestTokensAreComparedAgainstEveryConfiguredOne(t *testing.T) {
	if !tokenMatches("second", []string{"first", "second"}) {
		t.Error("the second token was not accepted")
	}
	if tokenMatches("", []string{""}) || tokenMatches("x", nil) {
		t.Error("an empty or unconfigured token was accepted")
	}
}

func TestAWebhookMessageIsBuiltFromThePostNotThePayload(t *testing.T) {
	post := wirePost{ID: "p1", UserID: "u-real", ChannelID: "c1", RootID: "r1",
		Message: "Triage: @devops_duty the disk is full", CreateAt: 1700000000000}
	payload := webhookPayload{PostID: "p1", ChannelID: "c1", ChannelName: "ops",
		UserID: "u-forged", Text: "something else", TriggerWord: "triage:"}
	m := webhookMessage(post, payload, bot, "alice")

	if m.UserID != "u-real" || m.Text != "the disk is full" || m.RootID != "r1" || m.ChannelName != "ops" {
		t.Errorf("message is %+v", m)
	}
	if !m.MentionsBot || m.Direct {
		t.Errorf("a delivery is addressed to the bot and never direct: %+v", m)
	}
}

func TestTheBotsOwnPostDeliveredByAWebhookIsNotARequest(t *testing.T) {
	post := wirePost{ID: "p1", UserID: bot.ID, ChannelID: "c1", Message: "Run started"}
	m := webhookMessage(post, webhookPayload{PostID: "p1"}, bot, bot.Username)
	if !m.FromSelf {
		t.Error("the bot's own post was not recognised")
	}
	post = wirePost{ID: "p2", UserID: "other-bot", ChannelID: "c1", Message: "hi",
		Props: map[string]any{"from_bot": "true"}}
	if m := webhookMessage(post, webhookPayload{PostID: "p2"}, bot, "x"); !m.FromBot {
		t.Error("another bot's post was not recognised")
	}
}
