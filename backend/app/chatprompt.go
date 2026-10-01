package app

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// The prompt a chat message becomes.
//
// A message outside a thread is the prompt, exactly: what the person typed,
// with the mention removed. A message inside a thread is a request that leans
// on what was said before it — "now do the same for staging" — so the thread is
// quoted ahead of it as context, bounded, and the message itself is set apart
// at the end as the thing to act on.
//
// The bound has two parts. The thread may use at most maxThreadBytes, and the
// whole prompt may not exceed the admission ceiling, so a long thread is cut
// rather than turning a short request into a refused one. What is cut is the
// middle: the thread's first post usually states the problem and the latest
// ones are what the request answers, and the posts between them are the ones a
// reader skims too.

const (
	chatPromptPreamble = "You were asked for help in a chat thread. The earlier messages of the thread " +
		"are quoted below as context. Act on the request at the end.\n\n"
	chatThreadOpen  = "--- thread ---\n"
	chatThreadClose = "--- end of thread ---\n\n"
	chatRequestHead = "--- request from %s ---\n"
	chatEmptyAsk    = "(no text beyond the mention: act on the thread above)"
)

// BuildChatPrompt renders a message, and the thread it was posted in, as a
// prompt of at most maxBytes.
func BuildChatPrompt(thread []ChatThreadPost, trigger ChatMessage, maxBytes, maxThreadBytes int) string {
	posts := contextPosts(thread, trigger)
	if len(posts) == 0 {
		return trigger.Text
	}

	ask := trigger.Text
	if strings.TrimSpace(ask) == "" {
		ask = chatEmptyAsk
	}
	request := fmt.Sprintf(chatRequestHead, speaker(trigger.Username, false)) + ask + "\n"

	budget := maxBytes - len(chatPromptPreamble) - len(chatThreadOpen) - len(chatThreadClose) - len(request)
	if maxThreadBytes > 0 && maxThreadBytes < budget {
		budget = maxThreadBytes
	}
	quoted := quoteThread(posts, trigger.RootID, budget)
	if quoted == "" {
		// No room for any of it. The request alone is still the request, and
		// whether it fits is admission's call.
		return trigger.Text
	}
	return chatPromptPreamble + chatThreadOpen + quoted + chatThreadClose + request
}

// contextPosts is the part of a thread that is context for a message: what was
// posted before it, by people or by the bot, in order. The message itself and
// anything posted after it are not — a reply that arrived while the thread was
// being fetched was not part of what the person was looking at.
func contextPosts(thread []ChatThreadPost, trigger ChatMessage) []ChatThreadPost {
	out := make([]ChatThreadPost, 0, len(thread))
	for _, p := range thread {
		if p.ID == trigger.ID || p.System || strings.TrimSpace(p.Text) == "" {
			continue
		}
		if !trigger.CreatedAt.IsZero() && p.CreatedAt.After(trigger.CreatedAt) {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// quoteThread renders posts within budget bytes: the root, then as many of the
// newest as fit, with a note where the ones between were left out.
func quoteThread(posts []ChatThreadPost, rootID string, budget int) string {
	// Room for the omission note is held back from the start, so adding it
	// after the choice is made cannot break the budget. At most one note is
	// ever written: the root is the oldest post and the rest are the newest, so
	// what is left out is one contiguous run.
	const noteRoom = 48
	budget -= noteRoom
	if budget <= 0 {
		return ""
	}
	// No single post may take more than a quarter of the room, or one pasted
	// log would push out the whole conversation around it.
	perPost := max(budget/4, 256)

	rendered := make([]string, len(posts))
	for i, p := range posts {
		rendered[i] = quotePost(p, perPost)
	}

	root := 0
	for i, p := range posts {
		if p.ID == rootID {
			root = i
			break
		}
	}

	keep := make([]bool, len(posts))
	used := 0
	if len(rendered[root]) <= budget {
		keep[root] = true
		used += len(rendered[root])
	}
	for i := len(posts) - 1; i >= 0; i-- {
		if keep[i] {
			continue
		}
		if used+len(rendered[i]) > budget {
			break
		}
		keep[i] = true
		used += len(rendered[i])
	}

	var b strings.Builder
	omitted := 0
	for i := range posts {
		if !keep[i] {
			omitted++
			continue
		}
		if omitted > 0 {
			fmt.Fprintf(&b, "[%d earlier messages omitted]\n", omitted)
			omitted = 0
		}
		b.WriteString(rendered[i])
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// quotePost renders one post as "name: text", with continuation lines indented
// so where one post ends and the next begins is never ambiguous.
func quotePost(p ChatThreadPost, limit int) string {
	head := speaker(p.Username, p.FromSelf) + ": "
	text := strings.ReplaceAll(strings.TrimSpace(p.Text), "\n", "\n  ")
	if room := limit - len(head) - 1; len(text) > room {
		text = cutBytes(text, room-len(" […]")) + " […]"
	}
	return head + text + "\n"
}

func speaker(username string, self bool) string {
	name := username
	if name == "" {
		name = "someone"
	}
	if self {
		name += " (you)"
	}
	return name
}

// cutBytes shortens s to at most n bytes without splitting a character.
func cutBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
