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
		URL: "https://chat.example", Role: DefaultChatRole,
		MaxPostRunes: DefaultChatPostRunes, MaxThreadBytes: DefaultChatThreadBytes,
	}
}

func TestAnEnabledIntegrationNeedsItsServerItsRoleAndItsTokens(t *testing.T) {
	all := ChatCredentials{Bot: true, Webhook: true}
	if err := ValidateChatIntegration(validIntegration(), all); err != nil {
		t.Fatalf("a complete integration was refused: %v", err)
	}
	for field, mutate := range map[string]func(*store.ChatIntegration, *ChatCredentials){
		"url":            func(c *store.ChatIntegration, _ *ChatCredentials) { c.URL = "" },
		"role":           func(c *store.ChatIntegration, _ *ChatCredentials) { c.Role = "" },
		"bot_token":      func(_ *store.ChatIntegration, have *ChatCredentials) { have.Bot = false },
		"webhook_tokens": func(_ *store.ChatIntegration, have *ChatCredentials) { have.Webhook = false },
		"transport":      func(c *store.ChatIntegration, _ *ChatCredentials) { c.Transport = "carrier-pigeon" },
		"base_branch":    func(c *store.ChatIntegration, _ *ChatCredentials) { c.BaseBranch = "main" },
		"max_post_runes": func(c *store.ChatIntegration, _ *ChatCredentials) { c.MaxPostRunes = 10 },
	} {
		c, have := validIntegration(), all
		mutate(&c, &have)
		var invalid *run.InvalidRequestError
		if err := ValidateChatIntegration(c, have); !errors.As(err, &invalid) || invalid.Field != field {
			t.Errorf("%s: got %v", field, err)
		}
	}
}

func TestAWebSocketOnlyIntegrationNeedsNoWebhookToken(t *testing.T) {
	c := validIntegration()
	c.Transport = store.ChatTransportWebSocket
	if err := ValidateChatIntegration(c, ChatCredentials{Bot: true}); err != nil {
		t.Errorf("refused: %v", err)
	}
}

func TestADisabledIntegrationMayBeSavedHalfFinished(t *testing.T) {
	c := validIntegration()
	c.Enabled, c.URL = false, ""
	if err := ValidateChatIntegration(c, ChatCredentials{}); err != nil {
		t.Errorf("refused: %v", err)
	}
}

func validSlack() store.ChatIntegration {
	c := defaultChatIntegration(ChatPlatformSlack)
	c.Enabled = true
	return c
}

func TestASlackIntegrationNeedsNoAddressButBothOfItsTokens(t *testing.T) {
	if err := ValidateChatIntegration(validSlack(), ChatCredentials{Bot: true, App: true}); err != nil {
		t.Fatalf("a complete Slack integration was refused: %v", err)
	}
	for field, have := range map[string]ChatCredentials{
		"bot_token": {App: true},
		"app_token": {Bot: true, Webhook: true},
	} {
		var invalid *run.InvalidRequestError
		if err := ValidateChatIntegration(validSlack(), have); !errors.As(err, &invalid) || invalid.Field != field {
			t.Errorf("%s: got %v", field, err)
		}
	}
}

func TestSlackIsReachedOverSocketModeOnly(t *testing.T) {
	for _, transport := range []string{store.ChatTransportWebhook, store.ChatTransportBoth} {
		c := validSlack()
		c.Transport = transport
		var invalid *run.InvalidRequestError
		if err := ValidateChatIntegration(c, ChatCredentials{Bot: true, App: true, Webhook: true}); !errors.As(err, &invalid) ||
			invalid.Field != "transport" {
			t.Errorf("%s: got %v", transport, err)
		}
	}
}

func TestSlackDefaultsToItsOwnPostLimit(t *testing.T) {
	c := defaultChatIntegration(ChatPlatformSlack)
	if c.MaxPostRunes != DefaultSlackPostRunes || c.Role != DefaultChatRole ||
		c.Transport != store.ChatTransportWebSocket {
		t.Errorf("defaults = %+v", c)
	}
}

func TestEachPlatformKeepsItsCredentialsUnderItsOwnSecrets(t *testing.T) {
	mm, sl := ChatSecretsFor(ChatPlatformMattermost), ChatSecretsFor(ChatPlatformSlack)
	if mm != (ChatSecrets{Bot: "mattermost-bot-token", Webhook: "mattermost-webhook-token"}) {
		t.Errorf("mattermost = %+v", mm)
	}
	if sl != (ChatSecrets{Bot: "slack-bot-token", App: "slack-app-token"}) {
		t.Errorf("slack = %+v", sl)
	}
}

func TestWebhookTokensAreOnePerLineOrCommaSeparated(t *testing.T) {
	got := SplitChatWebhookTokens(" abc\r\ndef, ghi\n\n")
	if !slices.Equal(got, []string{"abc", "def", "ghi"}) {
		t.Errorf("got %q", got)
	}
}
