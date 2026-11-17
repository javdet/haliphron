package app

import (
	"strings"
	"testing"
	"time"
)

var admitted = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

func alertPost(id string, at time.Duration, text string) ChatThreadPost {
	return ChatThreadPost{ID: id, Text: text, CreatedAt: admitted.Add(at), FromBot: true}
}

func TestTheAlertPostNearestToAdmissionWins(t *testing.T) {
	posts := []ChatThreadPost{
		alertPost("early", -4*time.Minute, "fp: 3f2a9c"),
		alertPost("near", 20*time.Second, "fp: 3f2a9c 9b01de"),
		alertPost("late", 3*time.Minute, "fp: 9b01de"),
	}
	got, ok := matchAlertPost(posts, []string{"3f2a9c", "9b01de"}, admitted)
	if !ok || got.ID != "near" {
		t.Fatalf("matched %q, %v; want near", got.ID, ok)
	}
}

func TestOnlyARootPostByOtherAutomationCanBeTheAlertsPost(t *testing.T) {
	reply := alertPost("reply", 0, "fp: 3f2a9c")
	reply.RootID = "root"
	person := alertPost("person", 0, "is fp: 3f2a9c ours?")
	person.FromBot = false
	self := alertPost("self", 0, "fp: 3f2a9c")
	self.FromSelf = true
	system := alertPost("system", 0, "fp: 3f2a9c")
	system.System = true

	for _, p := range []ChatThreadPost{reply, person, self, system} {
		if got, ok := matchAlertPost([]ChatThreadPost{p}, []string{"3f2a9c"}, admitted); ok {
			t.Errorf("%s matched as %q", p.ID, got.ID)
		}
	}
	if _, ok := matchAlertPost([]ChatThreadPost{alertPost("hook", 0, "fp: 3f2a9c")},
		[]string{"3f2a9c"}, admitted); !ok {
		t.Error("a root post by an incoming webhook did not match")
	}
}

func TestAFingerprintMatchesOnlyAsAWholeToken(t *testing.T) {
	for text, want := range map[string]bool{
		"fp: 3f2a9c":             true,
		"fp:3f2a9c,9b01de":       true,
		"(3f2a9c)":               true,
		"3f2a9c":                 true,
		"fp: 03f2a9c":            false,
		"fp: 3f2a9c0":            false,
		"fp: a3f2a9cb 3f2a9cc":   false,
		"fp: x3f2a9c":            false,
		"fp: 13f2a9c 3f2a9c end": true,
	} {
		if got := containsToken(text, "3f2a9c"); got != want {
			t.Errorf("containsToken(%q) = %v, want %v", text, got, want)
		}
	}
	if containsToken("anything", "") {
		t.Error("an empty fingerprint matched")
	}
}

func TestNoFingerprintsMatchNothing(t *testing.T) {
	if _, ok := matchAlertPost([]ChatThreadPost{alertPost("p", 0, "fp: 3f2a9c")}, nil, admitted); ok {
		t.Error("a row with no fingerprints matched a post")
	}
}

func TestTheRootPostHeaderNamesTheAlertsOnceAndDefusesMentions(t *testing.T) {
	h := chatAlertRootHeader([]string{"HighCPU", "HighCPU", "Disk@here"})
	if !strings.HasPrefix(h, "Alerts HighCPU, Disk@​here: no matching post was found") {
		t.Errorf("header is %q", h)
	}
	if !strings.Contains(chatAlertRootHeader([]string{"a", "b", "c", "d", "e", "f", "g"}), "and 2 more") {
		t.Error("more than five alert names are not summarised")
	}
	if h := chatAlertRootHeader(nil); !strings.HasPrefix(h, "Alert (unnamed):") {
		t.Errorf("header without names is %q", h)
	}
}

func TestThePromptSaysWhereTheAnswerGoesOnlyWhenAReplyIsOwed(t *testing.T) {
	p, err := decodeAlertPayload(notification(alert("3f2a9c", "firing", map[string]string{"alertname": "HighCPU"})))
	if err != nil {
		t.Fatal(err)
	}
	firing, _ := p.split()
	without, _, err := renderAlertPrompt("Investigate.", "triage", p, firing, 0, nil, "ab", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "will be posted") {
		t.Errorf("a prompt that owes no reply says where it goes:\n%s", without)
	}
	line := chatAlertPromptLine("#prod-alerts")
	with, _, err := renderAlertPrompt("Investigate.", "triage", p, firing, 0, nil, "ab", line)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(with, "ALERTS-ab>>>\n\n"+line+"\n") {
		t.Errorf("the reply line does not end the prompt after the data block:\n%s", with)
	}
	if line != "Your final answer will be posted in the thread of this alert in #prod-alerts; "+
		"write it for the people on call." {
		t.Errorf("line is %q", line)
	}
	if got := chatAlertPromptLine(""); !strings.Contains(got, "in the team-chat channel;") {
		t.Errorf("line without a channel name is %q", got)
	}
}
