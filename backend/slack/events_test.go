package slack

import (
	"encoding/json"
	"testing"
)

var bot = Identity{UserID: "UBOT", BotID: "BBOT", Name: "devops_duty"}

// callback builds an events_api payload the way Slack does.
func callback(t *testing.T, event map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": "event_callback", "event": event})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAnAppMentionIsDecodedWithTheMentionRemovedAndItsIdentifiersJoined(t *testing.T) {
	m, ok, err := decodeEvent(callback(t, map[string]any{
		"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1700000000.000200",
		"thread_ts": "1700000000.000100", "text": "<@UBOT>: restart ingress &amp; check &lt;logs&gt;",
	}), bot)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if m.ID != "C1:1700000000.000200" || m.RootID != "C1:1700000000.000100" || m.ChannelID != "C1" ||
		m.UserID != "U1" {
		t.Errorf("identifiers: %+v", m)
	}
	if !m.MentionsBot || m.Direct || m.FromSelf || m.FromBot || m.System {
		t.Errorf("flags: %+v", m)
	}
	if m.Text != "restart ingress & check <logs>" {
		t.Errorf("text is %q", m.Text)
	}
	if m.CreatedAt.Unix() != 1700000000 || m.CreatedAt.Nanosecond() != 200_000 {
		t.Errorf("created at %v", m.CreatedAt)
	}
}

func TestADirectMessageNeedsNoMention(t *testing.T) {
	m, ok, err := decodeEvent(callback(t, map[string]any{
		"type": "message", "channel_type": "im", "user": "U1", "channel": "D1", "ts": "1.000001",
		"text": "what is failing?",
	}), bot)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if !m.Direct || m.MentionsBot || m.ChannelType != "D" || m.Text != "what is failing?" {
		t.Errorf("%+v", m)
	}
}

func TestTheRootOfAThreadIsNotInsideOne(t *testing.T) {
	m, _, _ := decodeEvent(callback(t, map[string]any{
		"type": "app_mention", "user": "U1", "channel": "C1", "ts": "5.5", "thread_ts": "5.5",
		"text": "<@UBOT> hi",
	}), bot)
	if m.RootID != "" {
		t.Errorf("root is %q, want none", m.RootID)
	}
}

func TestEditsDeletionsAndOtherEventsAreNotMessages(t *testing.T) {
	for name, event := range map[string]map[string]any{
		"edit":     {"type": "message", "subtype": "message_changed", "channel": "C1", "ts": "1.1"},
		"deletion": {"type": "message", "subtype": "message_deleted", "channel": "C1", "ts": "1.1"},
		"reaction": {"type": "reaction_added", "user": "U1", "item": map[string]any{}},
		"hidden":   {"type": "message", "hidden": true, "channel": "C1", "ts": "1.1"},
	} {
		if _, ok, err := decodeEvent(callback(t, event), bot); ok || err != nil {
			t.Errorf("%s: ok=%v err=%v", name, ok, err)
		}
	}
}

func TestTheBotsOwnPostsAndOtherAppsAreRecognised(t *testing.T) {
	self, _, _ := decodeEvent(callback(t, map[string]any{
		"type": "message", "channel_type": "im", "bot_id": "BBOT", "channel": "D1", "ts": "1.1",
		"text": "Started run",
	}), bot)
	if !self.FromSelf || !self.FromBot {
		t.Errorf("own post: %+v", self)
	}
	other, _, _ := decodeEvent(callback(t, map[string]any{
		"type": "app_mention", "bot_id": "BOTHER", "subtype": "bot_message", "channel": "C1", "ts": "1.1",
		"text": "<@UBOT> do it",
	}), bot)
	if other.FromSelf || !other.FromBot || other.System {
		t.Errorf("another app: %+v", other)
	}
	join, _, _ := decodeEvent(callback(t, map[string]any{
		"type": "message", "subtype": "channel_join", "user": "U1", "channel": "C1", "ts": "1.1",
		"text": "<@U1> has joined",
	}), bot)
	if !join.System {
		t.Errorf("join: %+v", join)
	}
}

func TestAMentionLaterInTheTextIsLeftWhereItIs(t *testing.T) {
	for text, want := range map[string]string{
		"<@UBOT> deploy":               "deploy",
		"<@UBOT|devops_duty>, deploy":  "deploy",
		"please ask <@UBOT> to deploy": "please ask <@UBOT> to deploy",
		"<@UBOTTOM> is someone else":   "<@UBOTTOM> is someone else",
		"  <@UBOT>   ":                 "",
	} {
		got, named := stripMention(text, bot.UserID)
		if got != want {
			t.Errorf("%q: text %q, want %q", text, got, want)
		}
		if wantNamed := text != "<@UBOTTOM> is someone else"; named != wantNamed {
			t.Errorf("%q: named=%v", text, named)
		}
	}
}
