package mattermost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// What a post says, attachments included, and the two reads an alert reply
// needs: a channel's recent root posts, and a channel's description.
//
// An incoming webhook — Alertmanager's slack_configs among them — often puts
// everything it says into message attachments and leaves the message empty.
// The text of a post is therefore the message followed by each attachment's
// pretext, title, text, fields and footer, one per line, which is what the
// alert search matches against and what a thread quoted into a prompt shows.

// attachmentText renders a post's message attachments as lines.
func attachmentText(props map[string]any) string {
	raw, _ := props["attachments"].([]any)
	var lines []string
	add := func(v any) {
		if s, ok := v.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				lines = append(lines, s)
			}
		}
	}
	for _, a := range raw {
		att, ok := a.(map[string]any)
		if !ok {
			continue
		}
		add(att["pretext"])
		add(att["title"])
		add(att["text"])
		fields, _ := att["fields"].([]any)
		for _, f := range fields {
			field, ok := f.(map[string]any)
			if !ok {
				continue
			}
			title, _ := field["title"].(string)
			value := fieldValue(field["value"])
			switch {
			case title != "" && value != "":
				lines = append(lines, strings.TrimSpace(title)+": "+value)
			case value != "":
				lines = append(lines, value)
			case title != "":
				add(title)
			}
		}
		add(att["footer"])
	}
	return strings.Join(lines, "\n")
}

// fieldValue reads a field's value, which integrations send as a string or a
// number.
func fieldValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// system is a post the server generated: a join, a header change. Mattermost
// marks those with a type, and also gives a post with message attachments the
// type slack_attachment, which is someone's post — an incoming webhook's,
// typically, and the alert a receiver's reply is looking for.
func (p wirePost) system() bool {
	return p.Type != "" && p.Type != "slack_attachment"
}

// text is the post's message and its attachments' text.
func (p wirePost) text() string {
	att := attachmentText(p.Props)
	switch {
	case att == "":
		return p.Message
	case strings.TrimSpace(p.Message) == "":
		return att
	}
	return p.Message + "\n" + att
}

// RecentPosts implements app.ChatPlatform: GET /api/v4/channels/{id}/posts
// with since, keeping the root posts. The server answers since with every
// post created or changed after it, without paging, so the cap is applied
// here, to the newest.
func (c *Client) RecentPosts(ctx context.Context, channelID string, since time.Time,
	limit int) ([]app.ChatThreadPost, error) {

	var list struct {
		Order []string            `json:"order"`
		Posts map[string]wirePost `json:"posts"`
	}
	path := "/api/v4/channels/" + url.PathEscape(channelID) + "/posts?since=" +
		strconv.FormatInt(since.UnixMilli(), 10)
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	cutoff := since.UnixMilli()
	posts := make([]wirePost, 0, len(list.Posts))
	for _, p := range list.Posts {
		// since also returns posts edited after it; one created before it
		// is not a candidate.
		if p.DeleteAt == 0 && p.RootID == "" && p.CreateAt >= cutoff {
			posts = append(posts, p)
		}
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].CreateAt < posts[j].CreateAt })
	if limit > 0 && len(posts) > limit {
		posts = posts[len(posts)-limit:]
	}
	return c.threadPosts(ctx, posts)
}

// ChannelInfo implements app.ChatPlatform: the channel, and the bot's own
// membership of it.
func (c *Client) ChannelInfo(ctx context.Context, channelID string) (app.ChatChannel, error) {
	var ch struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	path := "/api/v4/channels/" + url.PathEscape(channelID)
	if err := c.do(ctx, http.MethodGet, path, nil, &ch); err != nil {
		if notFound(err) {
			return app.ChatChannel{}, app.ErrChatChannelNotFound
		}
		return app.ChatChannel{}, err
	}
	out := app.ChatChannel{ID: ch.ID, Name: ch.Name}
	if ch.DisplayName != "" && ch.DisplayName != ch.Name {
		out.Name = fmt.Sprintf("%s (%s)", ch.Name, ch.DisplayName)
	}

	err := c.do(ctx, http.MethodGet, path+"/members/me", nil, nil)
	switch {
	case err == nil:
		out.Member = true
	case notFound(err):
	default:
		return app.ChatChannel{}, err
	}
	return out, nil
}

// notFound is a refusal that means the thing is not there for this bot: 404,
// or 403 for a private channel it cannot see.
func notFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusForbidden)
}
