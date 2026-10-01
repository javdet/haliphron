package mattermost

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/automagicops/haliphron/backend/app"
)

// What arrives on the WebSocket.
//
// Every frame is an event envelope. The bot acts on one event, posted, and its
// payload is encoded twice: data.post and data.mentions are JSON documents
// carried as JSON strings. Edits arrive as post_edited and are not requests —
// a person correcting a typo has not asked for a second run.

type envelope struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
	Seq   int64           `json:"seq"`
}

type postedData struct {
	ChannelType string `json:"channel_type"`
	SenderName  string `json:"sender_name"`
	Post        string `json:"post"`
	Mentions    string `json:"mentions"`
}

// decodePosted turns a posted event into a message. ok is false for events
// that are not new posts; the error is a posted event that could not be read.
func decodePosted(data json.RawMessage, self User) (m app.ChatMessage, ok bool, err error) {
	var d postedData
	if err := json.Unmarshal(data, &d); err != nil {
		return app.ChatMessage{}, false, fmt.Errorf("mattermost: decode posted event: %w", err)
	}
	var p wirePost
	if err := json.Unmarshal([]byte(d.Post), &p); err != nil {
		return app.ChatMessage{}, false, fmt.Errorf("mattermost: decode posted post: %w", err)
	}
	if p.ID == "" || p.DeleteAt != 0 {
		return app.ChatMessage{}, false, nil
	}
	var mentions []string
	if d.Mentions != "" {
		if err := json.Unmarshal([]byte(d.Mentions), &mentions); err != nil {
			return app.ChatMessage{}, false, fmt.Errorf("mattermost: decode mentions: %w", err)
		}
	}

	text, named := stripMention(p.Message, self.Username)
	return app.ChatMessage{
		ID:          p.ID,
		ChannelID:   p.ChannelID,
		RootID:      p.RootID,
		UserID:      p.UserID,
		Username:    strings.TrimPrefix(d.SenderName, "@"),
		Text:        text,
		CreatedAt:   time.UnixMilli(p.CreateAt),
		Direct:      d.ChannelType == "D",
		MentionsBot: named || (self.ID != "" && slices.Contains(mentions, self.ID)),
		FromSelf:    self.ID != "" && p.UserID == self.ID,
		FromBot:     truthy(p.Props["from_bot"]) || truthy(p.Props["from_plugin"]),
		FromWebhook: truthy(p.Props["from_webhook"]),
		System:      p.Type != "",
	}, true, nil
}

// truthy reads a flag the server writes as a boolean in some versions and as
// the string "true" in others.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

// stripMention removes the bot's mention from the start of a message, with
// the punctuation people put after it, and reports whether the message names
// the bot anywhere. A mention later in the text is left where it is: removing
// it would change the sentence, and the agent can read "@devops_duty" as well
// as a person can.
func stripMention(text, username string) (string, bool) {
	if username == "" {
		return strings.TrimSpace(text), false
	}
	mention := "@" + strings.ToLower(username)
	lower := strings.ToLower(text)

	named := false
	for i := 0; ; {
		j := strings.Index(lower[i:], mention)
		if j < 0 {
			break
		}
		end := i + j + len(mention)
		if end == len(lower) || !usernameChar(lower[end]) {
			named = true
			break
		}
		i = end
	}

	trimmed := strings.TrimSpace(text)
	if n := len(mention); len(trimmed) >= n && strings.EqualFold(trimmed[:n], mention) &&
		(len(trimmed) == n || !usernameChar(lowerASCII(trimmed[n]))) {
		named = true
		trimmed = strings.TrimLeft(trimmed[n:], " \t\r\n,:;")
	}
	return strings.TrimSpace(trimmed), named
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// usernameChar is a byte that can continue a Mattermost username. A trailing
// dot is ambiguous — "@devops_duty." ends a sentence — and the server treats
// it as punctuation, so it is here too.
func usernameChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}
