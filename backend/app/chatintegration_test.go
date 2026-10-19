package app

import (
	"errors"
	"slices"
	"testing"

	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

func validIntegration() store.ChatIntegration {
	return store.ChatIntegration{
		Platform: "mattermost", Enabled: true, Transport: store.ChatTransportBoth,
		URL: "https://chat.example", Role: DefaultChatRole, ReplyPolicy: store.ChatReplyIfUnanswered,
		MaxPostRunes: DefaultChatPostRunes, MaxThreadBytes: DefaultChatThreadBytes,
	}
}

func TestAnEnabledIntegrationNeedsItsServerItsRoleAndItsTokens(t *testing.T) {
	if err := ValidateChatIntegration(validIntegration(), true, true); err != nil {
		t.Fatalf("a complete integration was refused: %v", err)
	}
	for field, mutate := range map[string]func(*store.ChatIntegration, *bool, *bool){
		"url":            func(c *store.ChatIntegration, _, _ *bool) { c.URL = "" },
		"role":           func(c *store.ChatIntegration, _, _ *bool) { c.Role = "" },
		"bot_token":      func(_ *store.ChatIntegration, bot, _ *bool) { *bot = false },
		"webhook_tokens": func(_ *store.ChatIntegration, _, hook *bool) { *hook = false },
		"transport":      func(c *store.ChatIntegration, _, _ *bool) { c.Transport = "carrier-pigeon" },
		"reply_policy":   func(c *store.ChatIntegration, _, _ *bool) { c.ReplyPolicy = "sometimes" },
		"run_url":        func(c *store.ChatIntegration, _, _ *bool) { c.RunURL = "https://ui/runs" },
		"base_branch":    func(c *store.ChatIntegration, _, _ *bool) { c.BaseBranch = "main" },
		"max_post_runes": func(c *store.ChatIntegration, _, _ *bool) { c.MaxPostRunes = 10 },
	} {
		c, bot, hook := validIntegration(), true, true
		mutate(&c, &bot, &hook)
		var invalid *run.InvalidRequestError
		if err := ValidateChatIntegration(c, bot, hook); !errors.As(err, &invalid) || invalid.Field != field {
			t.Errorf("%s: got %v", field, err)
		}
	}
}

func TestAWebSocketOnlyIntegrationNeedsNoWebhookToken(t *testing.T) {
	c := validIntegration()
	c.Transport = store.ChatTransportWebSocket
	if err := ValidateChatIntegration(c, true, false); err != nil {
		t.Errorf("refused: %v", err)
	}
}

func TestADisabledIntegrationMayBeSavedHalfFinished(t *testing.T) {
	c := validIntegration()
	c.Enabled, c.URL = false, ""
	if err := ValidateChatIntegration(c, false, false); err != nil {
		t.Errorf("refused: %v", err)
	}
}

func TestWebhookTokensAreOnePerLineOrCommaSeparated(t *testing.T) {
	got := SplitChatWebhookTokens(" abc\r\ndef, ghi\n\n")
	if !slices.Equal(got, []string{"abc", "def", "ghi"}) {
		t.Errorf("got %q", got)
	}
}
