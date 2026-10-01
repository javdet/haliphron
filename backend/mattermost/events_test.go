package mattermost

import (
	"encoding/json"
	"testing"
)

var bot = User{ID: "botid", Username: "devops_duty"}

// postedEvent builds the data of a posted event the way the server does: the
// post and the mentions are JSON documents inside JSON strings.
func postedEvent(t *testing.T, channelType string, post map[string]any, mentions []string) json.RawMessage {
	t.Helper()
	encodedPost, err := json.Marshal(post)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"channel_type": channelType,
		"sender_name":  "@alice",
		"post":         string(encodedPost),
	}
	if mentions != nil {
		encoded, _ := json.Marshal(mentions)
		data["mentions"] = string(encoded)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAPostedEventIsDecodedWithItsMentionsAndChannelType(t *testing.T) {
	data := postedEvent(t, "O", map[string]any{
		"id": "p1", "user_id": "u1", "channel_id": "c1", "root_id": "r1",
		"message": "@devops_duty: restart ingress", "create_at": 1700000000000,
	}, []string{"botid"})

	m, ok, err := decodePosted(data, bot)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if m.ID != "p1" || m.ChannelID != "c1" || m.RootID != "r1" || m.UserID != "u1" {
		t.Errorf("identifiers: %+v", m)
	}
	if m.Username != "alice" {
		t.Errorf("username is %q", m.Username)
	}
	if !m.MentionsBot || m.Direct {
		t.Errorf("mentions=%v direct=%v", m.MentionsBot, m.Direct)
	}
	if m.Text != "restart ingress" {
		t.Errorf("text is %q", m.Text)
	}
	if m.CreatedAt.UnixMilli() != 1700000000000 {
		t.Errorf("created at %v", m.CreatedAt)
	}
}

func TestADirectMessageIsMarkedDirectWithoutAMention(t *testing.T) {
	m, ok, err := decodePosted(postedEvent(t, "D", map[string]any{
		"id": "p1", "user_id": "u1", "channel_id": "dm", "message": "what is failing?",
	}, nil), bot)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if !m.Direct || m.MentionsBot || m.Text != "what is failing?" {
		t.Errorf("%+v", m)
	}
}

func TestFromBotIsHonouredAsAStringOrABool(t *testing.T) {
	for _, v := range []any{true, "true", "TRUE"} {
		m, _, err := decodePosted(postedEvent(t, "O", map[string]any{
			"id": "p", "user_id": "u", "channel_id": "c", "message": "@devops_duty hi",
			"props": map[string]any{"from_bot": v},
		}, []string{"botid"}), bot)
		if err != nil || !m.FromBot {
			t.Errorf("from_bot=%#v: FromBot=%v err=%v", v, m.FromBot, err)
		}
	}
	m, _, _ := decodePosted(postedEvent(t, "O", map[string]any{
		"id": "p", "user_id": "u", "channel_id": "c", "message": "hi",
		"props": map[string]any{"from_webhook": "true"},
	}, nil), bot)
	if !m.FromWebhook {
		t.Error("from_webhook was not read")
	}
}

func TestTheBotsOwnPostsAndSystemPostsAreMarked(t *testing.T) {
	m, _, _ := decodePosted(postedEvent(t, "O", map[string]any{
		"id": "p", "user_id": "botid", "channel_id": "c", "message": "Started run X",
	}, nil), bot)
	if !m.FromSelf {
		t.Error("the bot's own post was not recognised")
	}
	m, _, _ = decodePosted(postedEvent(t, "O", map[string]any{
		"id": "p", "user_id": "u", "channel_id": "c", "message": "alice joined", "type": "system_join_channel",
	}, nil), bot)
	if !m.System {
		t.Error("a system post was not recognised")
	}
}

func TestADeletedPostIsNotAMessage(t *testing.T) {
	_, ok, err := decodePosted(postedEvent(t, "O", map[string]any{
		"id": "p", "user_id": "u", "channel_id": "c", "message": "@devops_duty x", "delete_at": 1,
	}, nil), bot)
	if err != nil || ok {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

func TestOnlyALeadingMentionIsRemovedAndALongerNameIsNotTheBot(t *testing.T) {
	for _, c := range []struct {
		in, want string
		named    bool
	}{
		{"@devops_duty restart it", "restart it", true},
		{"@DevOps_Duty, restart it", "restart it", true},
		{"  @devops_duty\nrestart it", "restart it", true},
		{"@devops_duty", "", true},
		{"ask @devops_duty about it", "ask @devops_duty about it", true},
		{"@devops_duty_old restart it", "@devops_duty_old restart it", false},
		{"mail devops_duty@example.com", "mail devops_duty@example.com", false},
		{"no mention here", "no mention here", false},
	} {
		got, named := stripMention(c.in, "devops_duty")
		if got != c.want || named != c.named {
			t.Errorf("stripMention(%q) = %q, %v; want %q, %v", c.in, got, named, c.want, c.named)
		}
	}
}
