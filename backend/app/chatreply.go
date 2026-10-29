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
// A reply is the agent's answer as it wrote it, and nothing about the run: the
// status, the cost and the links are in the UI. Where there is no answer to
// post, the reply is one plain sentence. That is the rule for a run that did
// not succeed — whatever it wrote before it stopped is not the answer it was
// on its way to — and for one that exited 0 without saying anything. Neither
// ever tells the person the work was done: exit code 0 is all Succeeded means,
// so only the agent's own words may say more.
//
// The answer is untrusted text, written by a model that read a repository that
// may have been written to manipulate it. It is posted with its mentions
// defused, so it cannot notify a channel, and never as post metadata.

// DefaultChatPostRunes is Mattermost's default ceiling on a post.
const DefaultChatPostRunes = 16383

// DefaultSlackPostRunes is the longest message Slack recommends. It truncates
// at ten times that, but a reply past it reads as a wall in the thread.
const DefaultSlackPostRunes = 4000

const (
	// chatNoAnswer is the reply to a run that failed, or ended without an
	// answer of its own.
	chatNoAnswer = "Sorry, I couldn't finish this. Please try again."
	// chatCancelled is the reply to a run someone stopped.
	chatCancelled = "This request was cancelled."
	// chatCut ends an answer cut to the post limit.
	chatCut = "\n\n…"
)

// renderAnswer is the reply a settled run owes, at most maxRunes long.
func renderAnswer(r store.Run, maxRunes int) string {
	if r.Status == clusterv1.StatusCancelled {
		return chatCancelled
	}
	if answer, ok := agentAnswer(r, maxRunes); ok {
		return answer
	}
	return chatNoAnswer
}

// agentAnswer is what the agent answered, ready to post, and whether it
// answered at all: a run that did not succeed, whose report never arrived, or
// that wrote nothing but the entrypoint's stub has no answer.
func agentAnswer(r store.Run, maxRunes int) (string, bool) {
	if maxRunes <= 0 {
		maxRunes = DefaultChatPostRunes
	}
	answer := strings.TrimSpace(r.ResultSummary)
	if r.Status != clusterv1.StatusSucceeded ||
		r.ReportedStatus() == clusterv1.StatusCompletedWithoutResult ||
		answer == "" || runv1.IsNoAnswerSummary(r.ID, answer) {
		return "", false
	}
	answer = neutralizeMentions(answer)
	// Cut rather than refused: a reply the server rejects for length is a
	// reply that is never seen.
	if utf8.RuneCountInString(answer) > maxRunes {
		answer = cutRunes(answer, maxRunes-utf8.RuneCountInString(chatCut)) + chatCut
	}
	return answer, true
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
