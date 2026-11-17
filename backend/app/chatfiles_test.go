package app

import (
	"strings"
	"testing"
	"time"
)

func TestAPromptWithoutFilesIsExactlyWhatItWas(t *testing.T) {
	m := ChatMessage{ID: "m", Username: "alice", Text: "restart staging", CreatedAt: time.Unix(100, 0)}
	thread := []ChatThreadPost{{ID: "r", Username: "bob", Text: "staging is down", CreatedAt: time.Unix(10, 0)}}
	if r := chatFileReserve(thread, m, InputLimits{MaxFiles: 10}); r != 0 {
		t.Fatalf("a thread with no files holds back %d bytes", r)
	}
	if got, want := buildChatPrompt("mattermost", thread, m, 1<<20, 64<<10, chatAttachments{}),
		BuildChatPrompt("mattermost", thread, m, 1<<20, 64<<10); got != want {
		t.Errorf("the prompt changed for a message without files:\n%s\n---\n%s", got, want)
	}
}

func TestAPostLeftOutForTheBudgetIsLeftOutWithItsFiles(t *testing.T) {
	m := ChatMessage{ID: "m", RootID: "r", Username: "alice", Text: "what failed?",
		CreatedAt: time.Unix(100, 0), Files: []ChatFile{{ID: "f-own", Name: "own.png"}}}
	thread := []ChatThreadPost{
		{ID: "r", Username: "bob", Text: "the deploy", CreatedAt: time.Unix(10, 0),
			Files: []ChatFile{{ID: "f-root", Name: "root.log"}}},
		{ID: "mid", Username: "bob", Text: strings.Repeat("noise ", 400), CreatedAt: time.Unix(20, 0),
			Files: []ChatFile{{ID: "f-mid", Name: "mid.png"}}},
		{ID: "late", Username: "bob", Text: "and this", CreatedAt: time.Unix(30, 0),
			Files: []ChatFile{{ID: "f-late", Name: "late.png"}}},
		{ID: "bot", Username: "devops", Text: "mine", CreatedAt: time.Unix(40, 0), FromSelf: true,
			Files: []ChatFile{{ID: "f-bot", Name: "bot.png"}}},
	}
	reserve := chatFileReserve(thread, m, InputLimits{MaxFiles: 10})
	if reserve <= 0 {
		t.Fatal("files in the thread hold back no room")
	}
	header := RenderChatHeader(chatOriginOf("mattermost", m))
	// Room for the header, the request, the reserve and a small thread.
	maxBytes := len(header) + len(chatPromptPreamble) + 300 + reserve
	quoted := chatQuotedPosts("mattermost", thread, m, maxBytes, 64<<10, reserve)

	var ids []string
	for _, c := range chatCandidates(quoted, m) {
		ids = append(ids, c.file.ID)
	}
	got := strings.Join(ids, ",")
	if got != "f-own,f-late,f-root" {
		t.Errorf("candidates = %s, want the message's own, then the quoted posts newest first, "+
			"without the post left out or the bot's", got)
	}
}
