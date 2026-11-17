package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// alertmanagerPost is a post as Mattermost stores what Alertmanager's
// slack_configs posted through an incoming webhook, with the footer the alert
// reply needs: an empty message, everything in one attachment.
func alertmanagerPost(id, root string, createAt int64, footer string) map[string]any {
	return map[string]any{
		"id": id, "user_id": "hookowner", "channel_id": "c1", "root_id": root, "message": "",
		"create_at": createAt, "type": "slack_attachment",
		"props": map[string]any{
			"from_webhook":      "true",
			"override_username": "alertmanager",
			"attachments": []any{map[string]any{
				"id": 0, "color": "danger",
				"fallback":   "[FIRING:2] HighCPU (prod)",
				"title":      "[FIRING:2] HighCPU (prod)",
				"title_link": "https://alertmanager.example/#/alerts",
				"text":       "**Alert:** CPU above 90% on node-1",
				"fields":     []any{map[string]any{"title": "severity", "value": "critical", "short": true}},
				"footer":     footer,
			}},
		},
	}
}

func TestAnAlertmanagerPostReadsWithItsAttachmentText(t *testing.T) {
	var p wirePost
	raw, _ := json.Marshal(alertmanagerPost("p1", "", 1, "fp: 3f2a9c 9b01de"))
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	want := "[FIRING:2] HighCPU (prod)\n**Alert:** CPU above 90% on node-1\nseverity: critical\nfp: 3f2a9c 9b01de"
	if got := p.text(); got != want {
		t.Errorf("text = %q", got)
	}
	p.Message = "see below"
	if got := p.text(); got != "see below\n"+want {
		t.Errorf("text with a message = %q", got)
	}
}

// channelServer answers the channel reads RecentPosts and ChannelInfo make.
func channelServer(t *testing.T, posts map[string]any, wantSince string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/channels/{id}/posts", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != wantSince {
			t.Errorf("since = %q, want %q", got, wantSince)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"order": []string{}, "posts": posts})
	})
	mux.HandleFunc("POST /api/v4/users/ids", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "hookowner", "username": "ops"}})
	})
	mux.HandleFunc("GET /api/v4/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"app.channel.get.existing.app_error"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": r.PathValue("id"), "name": "prod-alerts", "display_name": "Prod alerts"})
	})
	mux.HandleFunc("GET /api/v4/channels/{id}/members/me", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "member" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"app.channel.get_member.missing.app_error"}`))
			return
		}
		_, _ = w.Write([]byte(`{"channel_id":"member","user_id":"botid"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	return NewClient(Config{BaseURL: base, Token: testToken})
}

func TestRecentPostsKeepsRootPostsCreatedSinceTheGivenTimeOldestFirst(t *testing.T) {
	since := time.UnixMilli(1700000000000)
	c := channelServer(t, map[string]any{
		"late":   alertmanagerPost("late", "", 1700000030000, "fp: 9b01de"),
		"early":  alertmanagerPost("early", "", 1700000010000, "fp: 3f2a9c"),
		"reply":  alertmanagerPost("reply", "early", 1700000020000, "fp: 3f2a9c"),
		"edited": alertmanagerPost("edited", "", 1699999990000, "fp: 3f2a9c"),
	}, "1700000000000")

	posts, err := c.RecentPosts(context.Background(), "c1", since, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || posts[0].ID != "early" || posts[1].ID != "late" {
		t.Fatalf("posts = %+v", posts)
	}
	if !posts[0].FromBot || posts[0].System || posts[0].Text == "" || posts[0].Username != "ops" {
		t.Errorf("first post = %+v", posts[0])
	}

	limited, err := c.RecentPosts(context.Background(), "c1", since, 1)
	if err != nil || len(limited) != 1 || limited[0].ID != "late" {
		t.Errorf("a limit of one kept %+v, %v; want the newest", limited, err)
	}
}

func TestChannelInfoReportsMembershipAndAnUnknownChannel(t *testing.T) {
	c := channelServer(t, nil, "")
	ch, err := c.ChannelInfo(context.Background(), "member")
	if err != nil || !ch.Member || ch.Name != "prod-alerts (Prod alerts)" {
		t.Errorf("member = %+v, %v", ch, err)
	}
	if ch, err := c.ChannelInfo(context.Background(), "outsider"); err != nil || ch.Member {
		t.Errorf("outsider = %+v, %v", ch, err)
	}
	if _, err := c.ChannelInfo(context.Background(), "missing"); !errors.Is(err, app.ErrChatChannelNotFound) {
		t.Errorf("missing error = %v", err)
	}
}
