package slack

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// What a message says, attachments and blocks included, and the two reads an
// alert reply needs: a channel's recent root messages, and a channel's
// description.
//
// An incoming webhook — Alertmanager's slack_configs among them — often puts
// everything it says into legacy attachments or blocks, and the message's own
// text is then a fallback or empty. The text of a message is therefore its
// text followed by each attachment's pretext, title, text, fields and footer,
// and each block's header, section text and fields, and context elements, one
// per line. A rich_text block is skipped: it is the message's own text again.

// wireAttachment is a legacy message attachment.
type wireAttachment struct {
	Pretext string `json:"pretext"`
	Title   string `json:"title"`
	Text    string `json:"text"`
	Fields  []struct {
		Title string `json:"title"`
		Value string `json:"value"`
	} `json:"fields"`
	Footer string `json:"footer"`
}

// wireBlock is a layout block, as far as its text goes.
type wireBlock struct {
	Type     string     `json:"type"`
	Text     *wireText  `json:"text"`
	Fields   []wireText `json:"fields"`
	Elements []wireText `json:"elements"`
}

type wireText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// richText renders a message's attachments and blocks as lines.
func richText(atts []wireAttachment, blocks []wireBlock) string {
	var lines []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			lines = append(lines, s)
		}
	}
	for _, a := range atts {
		add(a.Pretext)
		add(a.Title)
		add(a.Text)
		for _, f := range a.Fields {
			title, value := strings.TrimSpace(f.Title), strings.TrimSpace(f.Value)
			switch {
			case title != "" && value != "":
				lines = append(lines, title+": "+value)
			default:
				add(title + value)
			}
		}
		add(a.Footer)
	}
	for _, b := range blocks {
		switch b.Type {
		case "header", "section":
			if b.Text != nil {
				add(b.Text.Text)
			}
			for _, f := range b.Fields {
				add(f.Text)
			}
		case "context":
			for _, e := range b.Elements {
				if e.Type == "mrkdwn" || e.Type == "plain_text" {
					add(e.Text)
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// text is the message's text and its attachments' and blocks', unescaped.
func (m wireMessage) text() string {
	rich := richText(m.Attachments, m.Blocks)
	switch {
	case rich == "":
		return unescape(m.Text)
	case strings.TrimSpace(m.Text) == "":
		return unescape(rich)
	}
	return unescape(m.Text + "\n" + rich)
}

// historyPageSize and maxHistoryPages bound a channel read.
const (
	historyPageSize = 200
	maxHistoryPages = 5
)

// RecentPosts implements app.ChatPlatform: conversations.history from since,
// keeping the root messages. Slack returns the newest first and pages
// backwards, so the newest limit are kept.
func (c *Client) RecentPosts(ctx context.Context, channelID string, since time.Time,
	limit int) ([]app.ChatThreadPost, error) {

	var messages []wireMessage
	cursor := ""
	oldest := strconv.FormatFloat(float64(since.UnixMicro())/1e6, 'f', 6, 64)
	for page := 0; page < maxHistoryPages; page++ {
		form := url.Values{
			"channel":   {channelID},
			"oldest":    {oldest},
			"inclusive": {"true"},
			"limit":     {strconv.Itoa(historyPageSize)},
		}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		var out struct {
			Messages         []wireMessage `json:"messages"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, c.botToken, "conversations.history", form, &out); err != nil {
			return nil, err
		}
		messages = append(messages, out.Messages...)
		if cursor = out.ResponseMetadata.NextCursor; cursor == "" || (limit > 0 && len(messages) >= limit) {
			break
		}
	}

	self := c.me()
	posts := make([]app.ChatThreadPost, 0, len(messages))
	for _, m := range messages {
		// A reply broadcast to the channel shows up in its history, and is
		// still a reply.
		if m.TS == "" || m.Hidden || (m.ThreadTS != "" && m.ThreadTS != m.TS) {
			continue
		}
		created := parseTS(m.TS)
		if created.Before(since) {
			continue
		}
		p := app.ChatThreadPost{
			ID:        postID(channelID, m.TS),
			UserID:    m.User,
			Text:      m.text(),
			CreatedAt: created,
			FromSelf:  isSelf(m, self),
			FromBot:   isBot(m),
			System:    isSystem(m.Subtype),
		}
		switch {
		case m.Username != "":
			p.Username = m.Username
		case m.BotProfile != nil:
			p.Username = m.BotProfile.Name
		}
		posts = append(posts, p)
	}
	sort.SliceStable(posts, func(i, j int) bool { return posts[i].CreatedAt.Before(posts[j].CreatedAt) })
	if limit > 0 && len(posts) > limit {
		posts = posts[len(posts)-limit:]
	}
	return posts, nil
}

// ChannelInfo implements app.ChatPlatform: conversations.info, whose
// is_member is the bot's own membership.
func (c *Client) ChannelInfo(ctx context.Context, channelID string) (app.ChatChannel, error) {
	var out struct {
		Channel struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			IsMember bool   `json:"is_member"`
		} `json:"channel"`
	}
	err := c.call(ctx, c.botToken, "conversations.info", url.Values{"channel": {channelID}}, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "channel_not_found" {
		return app.ChatChannel{}, app.ErrChatChannelNotFound
	}
	if err != nil {
		return app.ChatChannel{}, err
	}
	id := out.Channel.ID
	if id == "" {
		id = channelID
	}
	return app.ChatChannel{ID: id, Name: out.Channel.Name, Member: out.Channel.IsMember}, nil
}
