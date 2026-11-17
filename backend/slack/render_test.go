package slack

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// alertmanagerMessage is a message as Slack stores what Alertmanager's
// slack_configs posted through an incoming webhook, with the footer the alert
// reply needs: no text of its own, everything in one legacy attachment.
func alertmanagerMessage(ts, footer string) map[string]any {
	return map[string]any{
		"type": "message", "subtype": "bot_message", "bot_id": "B0ALERTMGR", "username": "alertmanager",
		"text": "", "ts": ts,
		"attachments": []map[string]any{{
			"id": 1, "color": "a63636",
			"fallback":   "[FIRING:2] HighCPU (prod)",
			"title":      "[FIRING:2] HighCPU (prod)",
			"title_link": "https://alertmanager.example/#/alerts?receiver=chat-prod-alerts",
			"text":       "*Alert:* CPU above 90% on node-1 &amp; node-2",
			"fields":     []map[string]any{{"title": "severity", "value": "critical", "short": true}},
			"footer":     footer,
			"mrkdwn_in":  []string{"fallback", "pretext", "text"},
		}},
	}
}

func TestAnAlertmanagerMessageReadsWithItsAttachmentText(t *testing.T) {
	a := newAPI(t)
	a.handle("conversations.replies", func(url.Values) (int, map[string]any) {
		return 200, map[string]any{"ok": true, "messages": []map[string]any{
			alertmanagerMessage("1.000100", "fp: 3f2a9c 9b01de"),
		}}
	})
	posts, err := a.client().Thread(context.Background(), "C1:1.000100")
	if err != nil || len(posts) != 1 {
		t.Fatalf("thread: %v, %d posts", err, len(posts))
	}
	want := "[FIRING:2] HighCPU (prod)\n*Alert:* CPU above 90% on node-1 & node-2\nseverity: critical\nfp: 3f2a9c 9b01de"
	if posts[0].Text != want || !posts[0].FromBot {
		t.Errorf("text = %q, bot = %v", posts[0].Text, posts[0].FromBot)
	}
}

func TestBlocksAreRenderedButRichTextIsNotRepeated(t *testing.T) {
	m := wireMessage{
		Text: "summary",
		Blocks: []wireBlock{
			{Type: "header", Text: &wireText{Type: "plain_text", Text: "HighCPU"}},
			{Type: "rich_text", Elements: []wireText{{Type: "rich_text_section", Text: "summary"}}},
			{Type: "section", Text: &wireText{Type: "mrkdwn", Text: "CPU is high"},
				Fields: []wireText{{Type: "mrkdwn", Text: "*env:* prod"}}},
			{Type: "context", Elements: []wireText{{Type: "image"}, {Type: "mrkdwn", Text: "fp: 3f2a9c"}}},
		},
	}
	if got := m.text(); got != "summary\nHighCPU\nCPU is high\n*env:* prod\nfp: 3f2a9c" {
		t.Errorf("text = %q", got)
	}
}

func TestRecentPostsKeepsRootMessagesSinceTheGivenTimeOldestFirst(t *testing.T) {
	a := newAPI(t)
	since := time.Unix(1700000000, 0)
	a.handle("conversations.history", func(f url.Values) (int, map[string]any) {
		if f.Get("channel") != "C1" || f.Get("oldest") != "1700000000.000000" || f.Get("inclusive") != "true" {
			t.Errorf("form = %v", f)
		}
		// Newest first, as Slack answers.
		return 200, map[string]any{"ok": true, "messages": []map[string]any{
			{"ts": "1700000090.000200", "thread_ts": "1700000010.000100", "subtype": "thread_broadcast",
				"user": "U1", "text": "a reply broadcast to the channel"},
			alertmanagerMessage("1700000030.000100", "fp: 9b01de"),
			alertmanagerMessage("1700000010.000100", "fp: 3f2a9c"),
			{"ts": "1699999990.000100", "user": "U1", "text": "before since"},
		}}
	})
	posts, err := a.client().RecentPosts(context.Background(), "C1", since, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || posts[0].ID != "C1:1700000010.000100" || posts[1].ID != "C1:1700000030.000100" {
		t.Fatalf("posts = %+v", posts)
	}
	if posts[0].RootID != "" || !posts[0].FromBot || posts[0].Username != "alertmanager" {
		t.Errorf("first post = %+v", posts[0])
	}

	limited, err := a.client().RecentPosts(context.Background(), "C1", since, 1)
	if err != nil || len(limited) != 1 || limited[0].ID != "C1:1700000030.000100" {
		t.Errorf("a limit of one kept %+v, %v; want the newest", limited, err)
	}
}

func TestChannelInfoReportsMembershipAndAnUnknownChannel(t *testing.T) {
	a := newAPI(t)
	a.handle("conversations.info", func(f url.Values) (int, map[string]any) {
		if f.Get("channel") == "C404" {
			return 200, map[string]any{"ok": false, "error": "channel_not_found"}
		}
		return 200, map[string]any{"ok": true, "channel": map[string]any{
			"id": f.Get("channel"), "name": "prod-alerts", "is_member": f.Get("channel") == "C1"}}
	})
	c := a.client()
	if ch, err := c.ChannelInfo(context.Background(), "C1"); err != nil || !ch.Member || ch.Name != "prod-alerts" {
		t.Errorf("C1 = %+v, %v", ch, err)
	}
	if ch, err := c.ChannelInfo(context.Background(), "C2"); err != nil || ch.Member {
		t.Errorf("C2 = %+v, %v", ch, err)
	}
	if _, err := c.ChannelInfo(context.Background(), "C404"); !errors.Is(err, app.ErrChatChannelNotFound) {
		t.Errorf("C404 error = %v", err)
	}
}
