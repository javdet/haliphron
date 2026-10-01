package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The chat rules that need no database: what a prompt is made of, which posts
// are requests, and what a reply may say. The paths through the store are in
// test/backend/mattermost_test.go.

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func post(id, user, text string, minute int) ChatThreadPost {
	return ChatThreadPost{ID: id, Username: user, Text: text, CreatedAt: t0.Add(time.Duration(minute) * time.Minute)}
}

func trigger(id, root, text string, minute int) ChatMessage {
	return ChatMessage{ID: id, RootID: root, Username: "alice", Text: text, MentionsBot: true,
		CreatedAt: t0.Add(time.Duration(minute) * time.Minute)}
}

func TestAMessageOutsideAThreadIsThePromptExactly(t *testing.T) {
	got := BuildChatPrompt(nil, trigger("m1", "", "restart the ingress on staging", 0), 1<<20, 0)
	if got != "restart the ingress on staging" {
		t.Errorf("prompt is %q", got)
	}
}

func TestAThreadIsQuotedOldestFirstWithTheTriggerLast(t *testing.T) {
	thread := []ChatThreadPost{
		post("m3", "devops_duty", "Started run X", 2),
		post("root", "bob", "prod-eu is returning 502s", 0),
		post("m2", "carol", "only on /api", 1),
	}
	thread[0].FromSelf = true
	got := BuildChatPrompt(thread, trigger("m4", "root", "find out why", 3), 1<<20, 0)

	order := []string{"bob: prod-eu is returning 502s", "carol: only on /api",
		"devops_duty (you): Started run X", "request from alice", "find out why"}
	at := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 {
			t.Fatalf("prompt is missing %q:\n%s", want, got)
		}
		if i < at {
			t.Fatalf("%q is out of order:\n%s", want, got)
		}
		at = i
	}
	if !strings.HasSuffix(got, "find out why\n") {
		t.Errorf("the request is not the last thing in the prompt:\n%s", got)
	}
}

func TestPostsAfterTheTriggerAndSystemPostsAreNotContext(t *testing.T) {
	thread := []ChatThreadPost{
		post("root", "bob", "deploy is stuck", 0),
		{ID: "sys", Text: "carol joined the channel", CreatedAt: t0.Add(time.Minute), System: true},
		post("late", "carol", "never mind, it is fine", 5),
	}
	got := BuildChatPrompt(thread, trigger("m2", "root", "look at it", 2), 1<<20, 0)
	if strings.Contains(got, "joined the channel") {
		t.Errorf("a system post was quoted:\n%s", got)
	}
	if strings.Contains(got, "never mind") {
		t.Errorf("a post made after the request was quoted:\n%s", got)
	}
}

func TestThreadHistoryIsDroppedToStayUnderThePromptCeiling(t *testing.T) {
	var thread []ChatThreadPost
	thread = append(thread, post("root", "bob", "the original problem", 0))
	for i := 1; i <= 400; i++ {
		// Multibyte on purpose: a cut that counts runes instead of bytes, or
		// splits one, is the failure this is looking for.
		thread = append(thread, post(fmt.Sprintf("m%d", i), "carol", strings.Repeat("ошибка ", 40), i))
	}
	trig := trigger("last", "root", "summarise", 401)

	for _, limit := range []int{2 << 10, 16 << 10, 64 << 10} {
		got := BuildChatPrompt(thread, trig, limit, 0)
		if len(got) > limit {
			t.Errorf("limit %d: prompt is %d bytes", limit, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: prompt is not valid UTF-8", limit)
		}
		if !strings.Contains(got, "earlier messages omitted") {
			t.Errorf("limit %d: nothing says history was left out", limit)
		}
		if !strings.HasSuffix(got, "summarise\n") {
			t.Errorf("limit %d: the request was lost", limit)
		}
	}
}

func TestTheRootOfAThreadIsKeptWhenEarlierMessagesAreDropped(t *testing.T) {
	thread := []ChatThreadPost{post("root", "bob", "THE ORIGINAL PROBLEM", 0)}
	for i := 1; i <= 50; i++ {
		thread = append(thread, post(fmt.Sprintf("m%d", i), "carol", strings.Repeat("x", 200), i))
	}
	thread = append(thread, post("newest", "carol", "THE LATEST WORD", 51))

	got := BuildChatPrompt(thread, trigger("t", "root", "go", 52), 1<<20, 2<<10)
	for _, want := range []string{"THE ORIGINAL PROBLEM", "THE LATEST WORD", "earlier messages omitted"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
}

func TestOneHugePostCannotPushOutTheRestOfTheThread(t *testing.T) {
	thread := []ChatThreadPost{
		post("root", "bob", "here is the log", 0),
		post("log", "bob", strings.Repeat("panic: nil map\n", 5000), 1),
		post("q", "carol", "is it the config change?", 2),
	}
	got := BuildChatPrompt(thread, trigger("t", "root", "check", 3), 1<<20, 8<<10)
	if !strings.Contains(got, "is it the config change?") || !strings.Contains(got, "here is the log") {
		t.Errorf("the pasted log pushed out the conversation:\n%.400s", got)
	}
	if !strings.Contains(got, "[…]") {
		t.Errorf("the oversized post was not marked as cut")
	}
}

func TestAMentionWithNothingElseInAThreadAsksAboutTheThread(t *testing.T) {
	thread := []ChatThreadPost{post("root", "bob", "why is CI red?", 0)}
	got := BuildChatPrompt(thread, trigger("t", "root", "", 1), 1<<20, 0)
	if !strings.Contains(got, "why is CI red?") || !strings.Contains(got, chatEmptyAsk) {
		t.Errorf("prompt is:\n%s", got)
	}
}

func TestTheBotIgnoresItselfOtherBotsWebhooksAndSystemPosts(t *testing.T) {
	base := ChatMessage{ID: "m", MentionsBot: true}
	if !addressedToBot(base) {
		t.Fatal("a plain mention is not a request")
	}
	for name, m := range map[string]ChatMessage{
		"its own post":   {ID: "m", MentionsBot: true, FromSelf: true},
		"another bot":    {ID: "m", MentionsBot: true, FromBot: true},
		"a webhook":      {ID: "m", MentionsBot: true, FromWebhook: true},
		"a system post":  {ID: "m", MentionsBot: true, System: true},
		"a self-DM echo": {ID: "m", Direct: true, FromSelf: true},
		"no mention":     {ID: "m"},
	} {
		if addressedToBot(m) {
			t.Errorf("%s was taken as a request", name)
		}
	}
}

func TestEveryDirectMessageIsAMention(t *testing.T) {
	if !addressedToBot(ChatMessage{ID: "m", Direct: true}) {
		t.Error("a direct message without a mention was ignored")
	}
}

func finished(status string) store.Run {
	finishedAt := t0
	received := t0
	return store.Run{
		ID: "01J0000000000000000000000A", Status: status,
		ObservedPhase: runv1.Phase(status), FinishedAt: &finishedAt, CompletionReceivedAt: &received,
	}
}

func TestASucceededReplySaysExitCodeZeroAndNothingMore(t *testing.T) {
	r := finished(clusterv1.StatusSucceeded)
	r.ResultSummary = "Restarted the deployment."
	got := renderResult(r, "", 0)
	if !strings.Contains(got, "the agent exited with code 0") {
		t.Errorf("reply does not say what Succeeded means:\n%s", got)
	}
	for _, claim := range []string{"solved", "fixed", "done!", "success!"} {
		if strings.Contains(strings.ToLower(got), claim) {
			t.Errorf("reply claims %q:\n%s", claim, got)
		}
	}
	if !strings.Contains(got, "Restarted the deployment.") {
		t.Errorf("summary is missing:\n%s", got)
	}
}

func TestAFailedReplyNamesTheStatusTheClassAndTheExitCode(t *testing.T) {
	r := finished(clusterv1.StatusFailed)
	code := int32(2)
	r.ExitCode = &code
	r.FailureClass = runv1.FailureClass("agent")
	r.StatusMessage = "claude exited"
	got := renderResult(r, "https://ui/runs/x", 0)
	for _, want := range []string{"**Failed**", "`agent`", "exit code 2", "claude exited", "https://ui/runs/x"} {
		if !strings.Contains(got, want) {
			t.Errorf("reply is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "code 0") {
		t.Errorf("a failure was worded as a success:\n%s", got)
	}
}

func TestARunWhoseReportNeverArrivedSaysSo(t *testing.T) {
	r := finished(clusterv1.StatusSucceeded)
	r.CompletionReceivedAt = nil
	got := renderResult(r, "", 0)
	if !strings.Contains(got, "report never arrived") || strings.Contains(got, "exited with code 0") {
		t.Errorf("reply is:\n%s", got)
	}
}

func TestALongResultIsCutToThePostLimitWithANote(t *testing.T) {
	r := finished(clusterv1.StatusSucceeded)
	r.ResultSummary = strings.Repeat("строка результата\n", 4000)
	r.PRURL = "https://git.example/pr/1"
	got := renderResult(r, "https://ui/runs/x", 16383)
	if n := utf8.RuneCountInString(got); n > 16383 {
		t.Errorf("reply is %d runes", n)
	}
	if !strings.Contains(got, "truncated") || !strings.Contains(got, "https://ui/runs/x") {
		t.Errorf("a cut reply does not say so or where the rest is:\n%.300s", got[len(got)-300:])
	}
	if !strings.Contains(got, "https://git.example/pr/1") {
		t.Error("the pull request link was cut along with the summary")
	}
}

func TestAnAgentsSummaryCannotNotifyAChannel(t *testing.T) {
	r := finished(clusterv1.StatusSucceeded)
	r.ResultSummary = "ping @channel and @all, then @here and @ceo"
	got := renderResult(r, "", 0)
	for _, m := range []string{"@channel", "@all", "@here", "@ceo"} {
		if strings.Contains(got, m) {
			t.Errorf("%s survived into the reply:\n%s", m, got)
		}
	}
}

func TestARefusalSaysWhatTheRequesterCanActOnAndHidesInternalFaults(t *testing.T) {
	text, internal := renderRefusal(&run.PromptTooLargeError{Size: 600000, Limit: 524288})
	if internal || !strings.Contains(text, "524288") {
		t.Errorf("too large: %q internal=%v", text, internal)
	}
	text, internal = renderRefusal(&run.InvalidRequestError{Field: "role", Detail: `no role named "x"`})
	if internal || !strings.Contains(text, "role") {
		t.Errorf("role: %q internal=%v", text, internal)
	}
	text, internal = renderRefusal(errors.New("store: begin: connection refused"))
	if !internal || strings.Contains(text, "connection refused") {
		t.Errorf("an internal error leaked its detail: %q", text)
	}
}

func TestRetriesBackOffToTenMinutes(t *testing.T) {
	if got := chatBackoff(0); got != 10*time.Second {
		t.Errorf("first retry after %v", got)
	}
	if got := chatBackoff(3); got != 80*time.Second {
		t.Errorf("fourth retry after %v", got)
	}
	if got := chatBackoff(50); got != 10*time.Minute {
		t.Errorf("retry ceiling is %v", got)
	}
}
