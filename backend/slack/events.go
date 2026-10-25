package slack

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/automagicops/haliphron/backend/app"
)

// What arrives on the Socket Mode connection.
//
// Every frame is an envelope. The ones that carry an envelope_id must be
// acknowledged within three seconds or Slack delivers them again, so the
// listener acknowledges first and decides afterwards; a redelivery is a second
// claim of the same message, which the store refuses.
//
// The bot acts on two events: app_mention, a message in a channel that names
// it, and message with channel_type im, anything said to it directly. Edits
// arrive as message_changed and are not requests — a person correcting a typo
// has not asked for a second run.

// envelope is one Socket Mode frame.
type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	// Reason is a disconnect's: warning and refresh_requested ask for a new
	// connection, link_disabled says Socket Mode has been switched off.
	Reason string `json:"reason"`
}

// eventCallback is an events_api envelope's payload.
type eventCallback struct {
	Type  string      `json:"type"`
	Event wireMessage `json:"event"`
}

// decodeEvent turns an events_api payload into a message. ok is false for
// events that are not new messages; the error is a payload that could not be
// read.
func decodeEvent(payload json.RawMessage, self Identity) (m app.ChatMessage, ok bool, err error) {
	var cb eventCallback
	if err := json.Unmarshal(payload, &cb); err != nil {
		return app.ChatMessage{}, false, fmt.Errorf("slack: decode event: %w", err)
	}
	e := cb.Event
	if cb.Type != "event_callback" || (e.Type != "app_mention" && e.Type != "message") {
		return app.ChatMessage{}, false, nil
	}
	switch e.Subtype {
	case "message_changed", "message_deleted", "message_replied":
		return app.ChatMessage{}, false, nil
	}
	if e.Hidden || e.TS == "" || e.Channel == "" {
		return app.ChatMessage{}, false, nil
	}

	text, named := stripMention(e.Text, self.UserID)
	m = app.ChatMessage{
		ID:          postID(e.Channel, e.TS),
		ChannelID:   e.Channel,
		ChannelType: channelTypes[e.ChannelType],
		UserID:      e.User,
		Text:        unescape(text),
		CreatedAt:   parseTS(e.TS),
		Direct:      e.ChannelType == "im",
		MentionsBot: e.Type == "app_mention" || named,
		FromSelf:    isSelf(e, self),
		FromBot:     isBot(e),
		System:      isSystem(e.Subtype),
	}
	// A thread's root carries its own ts as thread_ts once it has replies; it
	// is not inside a thread, it is the thread.
	if e.ThreadTS != "" && e.ThreadTS != e.TS {
		m.RootID = postID(e.Channel, e.ThreadTS)
	}
	if m.UserID == "" {
		m.UserID = e.BotID
	}
	return m, true, nil
}

// channelTypes maps an event's channel_type to the rules' type letter. An
// app_mention carries none; the listener asks for it.
var channelTypes = map[string]string{
	"channel": "O",
	"group":   "P",
	"im":      "D",
	"mpim":    "G",
}

// stripMention removes the bot's mention from the start of a message, with
// the punctuation people put after it, and reports whether the message names
// the bot anywhere. Slack writes a mention as <@U…>, or <@U…|name> in older
// clients. A mention later in the text is left where it is: removing it would
// change the sentence.
func stripMention(text, userID string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if userID == "" {
		return trimmed, false
	}
	plain, labelled := "<@"+userID+">", "<@"+userID+"|"
	named := strings.Contains(text, plain) || strings.Contains(text, labelled)

	switch {
	case strings.HasPrefix(trimmed, plain):
		trimmed = trimmed[len(plain):]
	case strings.HasPrefix(trimmed, labelled):
		end := strings.IndexByte(trimmed, '>')
		if end < 0 {
			return trimmed, named
		}
		trimmed = trimmed[end+1:]
	default:
		return trimmed, named
	}
	return strings.TrimSpace(strings.TrimLeft(trimmed, " \t\r\n,:;")), true
}
