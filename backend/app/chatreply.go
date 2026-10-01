package app

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// What the bot says.
//
// Two rules apply to every line here. A status is reported as what it is:
// Succeeded means the agent exited with code 0, and a reply that says "done"
// or "solved" is the system claiming success it did not observe. And anything
// that came from the agent pod — the summary, a status message, a pull request
// link — is untrusted text, written by a model that read a repository that may
// have been written to manipulate it. It is posted with its mentions defused,
// so a summary cannot notify a channel, and never as post metadata.

// DefaultChatPostRunes is Mattermost's default ceiling on a post.
const DefaultChatPostRunes = 16383

func renderAck(id runv1.ULID, role, link string) string {
	text := fmt.Sprintf("Started run `%s`", id)
	if role != "" {
		text += fmt.Sprintf(" with role `%s`", role)
	}
	text += ". I will reply in this thread when it ends."
	if link != "" {
		text += "\n" + link
	}
	return text
}

// renderResult is the reply a settled run owes, at most maxRunes long.
func renderResult(r store.Run, link string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = DefaultChatPostRunes
	}

	var head string
	status := r.ReportedStatus()
	switch {
	case status == clusterv1.StatusCompletedWithoutResult:
		head = fmt.Sprintf("Run `%s` ended **%s**, but its report never arrived, so there is no summary.",
			r.ID, r.Status)
	case r.Status == clusterv1.StatusSucceeded:
		// Not "the task was solved": exit code 0 is what Succeeded means.
		head = fmt.Sprintf("Run `%s` ended **Succeeded**: the agent exited with code 0.", r.ID)
	default:
		head = fmt.Sprintf("Run `%s` ended **%s**", r.ID, status)
		var details []string
		if r.FailureClass != "" {
			details = append(details, "failure class `"+string(r.FailureClass)+"`")
		}
		if r.ExitCode != nil {
			details = append(details, fmt.Sprintf("exit code %d", *r.ExitCode))
		}
		if len(details) > 0 {
			head += " (" + strings.Join(details, ", ") + ")"
		}
		switch {
		case r.StatusMessage != "":
			head += ": " + neutralizeMentions(oneLine(r.StatusMessage, 500))
		case r.StatusReason != "":
			head += ": " + neutralizeMentions(r.StatusReason)
		case r.CancelReason != "":
			head += ": " + neutralizeMentions(oneLine(r.CancelReason, 500))
		}
		head += "."
	}

	var foot []string
	if r.PRURL != "" {
		foot = append(foot, "Pull request: "+neutralizeMentions(r.PRURL))
	}
	if r.CostUSD != "" {
		foot = append(foot, "Cost: $"+string(r.CostUSD))
	}
	if link != "" {
		foot = append(foot, link)
	}
	footer := strings.Join(foot, "\n")

	summary := neutralizeMentions(strings.TrimSpace(r.ResultSummary))
	if summary == "" {
		return joinNonEmpty(head, footer)
	}

	// The summary gets what the status and the links leave, and is cut rather
	// than the post refused: a reply the server rejects for length is a reply
	// that is never seen.
	where := link
	if where == "" {
		where = fmt.Sprintf("`GET /api/v1/runs/%s/result`", r.ID)
	}
	room := maxRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(footer) - 8
	total := utf8.RuneCountInString(summary)
	if total > room {
		note := fmt.Sprintf("\n\n… truncated: the full result is at %s", where)
		summary = cutRunes(summary, room-utf8.RuneCountInString(note)) + note
	}
	return joinNonEmpty(head, summary, footer)
}

// renderRefusal is the answer to a message that started nothing. internal is
// set when the reason is a fault here rather than something about the request;
// its detail is logged and not posted.
func renderRefusal(err error) (text string, internal bool) {
	var (
		tooLarge *run.PromptTooLargeError
		invalid  *run.InvalidRequestError
	)
	switch {
	case errors.As(err, &tooLarge):
		return fmt.Sprintf("I did not start a run: the request is %d bytes and the limit is %d.",
			tooLarge.Size, tooLarge.Limit), false
	case errors.As(err, &invalid) && invalid.Field == "role":
		return "I did not start a run: the role this bot is configured with does not exist. " +
			"An administrator has to create it.", false
	case errors.As(err, &invalid):
		return "I did not start a run: " + neutralizeMentions(invalid.Error()), false
	}
	return "I could not start a run because of an internal error. Try again later.", true
}

// neutralizeMentions keeps text from notifying anyone. A zero-width space
// after every @ leaves the text readable and stops the server parsing
// @channel, @here, @all or any @username out of it.
func neutralizeMentions(s string) string {
	return strings.ReplaceAll(s, "@", "@​")
}

// cutRunes shortens s to at most n characters, at a line break when there is
// one in the last fifth.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	end := 0
	for i := range s {
		if n == 0 {
			end = i
			break
		}
		n--
	}
	cut := s[:end]
	if nl := strings.LastIndexByte(cut, '\n'); nl > 0 && nl >= len(cut)*4/5 {
		cut = cut[:nl]
	}
	return cut
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > limit {
		s = cutRunes(s, limit-1) + "…"
	}
	return s
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}
