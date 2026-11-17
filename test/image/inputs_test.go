package image_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// The inputs phase: the files a person attached to a chat message, read from
// the controller into /workspace/.haliphron/inputs before the agent starts.

// screenshot is bytes that sniff as a PNG and that no other input shares.
var screenshot = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("pixels "), 300)...)

var quarterly = []byte("%PDF-1.7\nthe quarterly numbers\n%%EOF\n")

// isAgentStart reports whether a command is the agent being started, as
// opposed to a version check or an MCP listing.
func isAgentStart(c entrypoint.Command) bool {
	return (c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p") ||
		(c.Path == "codex" && len(c.Args) > 0 && c.Args[0] == "exec")
}

// phaseIndex is where a phase sits in the timings, or -1.
func phaseIndex(run *entrypoint.Run, p runv1.RuntimePhase) int {
	for i, t := range run.Timings() {
		if t.Phase == p {
			return i
		}
	}
	return -1
}

func TestAPhotoAttachedInChatIsInTheInputsDirectoryBeforeTheAgentStarts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "what does the error on the screenshot say?",
		Inputs: []controlplane.InputFile{
			{Name: "Screen Shot 2026-11-17.png", MediaType: "image/png", Body: screenshot},
			{Name: "q3.pdf", MediaType: "application/pdf", Body: quarterly},
		},
	})
	photo := filepath.Join(h.layout.Inputs, "1-Screen_Shot_2026-11-17.png")
	pdf := filepath.Join(h.layout.Inputs, "2-q3.pdf")

	// What the agent would find, looked at from inside the agent's own start:
	// "before the agent starts" is a claim about that moment, not about the
	// directory after the run.
	type seen struct {
		body []byte
		mode fs.FileMode
		err  error
	}
	var mu sync.Mutex
	found := map[string]seen{}
	script := agentScript(h, "the error says 42", "")
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if isAgentStart(c) {
			mu.Lock()
			for _, path := range []string{photo, pdf} {
				var s seen
				s.body, s.err = os.ReadFile(path)
				if info, err := os.Stat(path); err == nil {
					s.mode = info.Mode().Perm()
				}
				found[path] = s
			}
			mu.Unlock()
		}
		return script(c)
	}

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}

	mu.Lock()
	defer mu.Unlock()
	for path, want := range map[string][]byte{photo: screenshot, pdf: quarterly} {
		got, ok := found[path]
		switch {
		case !ok:
			t.Fatal("the agent was never started")
		case got.err != nil:
			t.Errorf("%s was not there when the agent started: %v", filepath.Base(path), got.err)
		case !bytes.Equal(got.body, want):
			t.Errorf("%s holds %d bytes that are not the %d the person sent",
				filepath.Base(path), len(got.body), len(want))
		case got.mode != 0o444:
			t.Errorf("%s is %v; what the person sent is evidence, read-only to the agent",
				filepath.Base(path), got.mode)
		}
	}

	if got := h.phase(run, runv1.RuntimePhaseInputs); got != runv1.PhaseOutcomeOK {
		t.Errorf("the inputs phase is %s, want ok", got)
	}
	inputs, clone, agent := phaseIndex(run, runv1.RuntimePhaseInputs),
		phaseIndex(run, runv1.RuntimePhaseClone), phaseIndex(run, runv1.RuntimePhaseRun)
	if !(clone < inputs && inputs < agent) {
		t.Errorf("the timings place clone at %d, inputs at %d and run at %d; inputs belongs "+
			"after the clone, which owns the workspace, and before the agent", clone, inputs, agent)
	}
	if !slices.Contains(h.cp.Phases(h.prepared.RunID), runv1.RuntimePhaseInputs) {
		t.Error("the inputs phase was not reported to the controller")
	}

	// Nothing but leftovers of a transfer would be in the directory besides the
	// two files.
	entries, err := os.ReadDir(h.layout.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the inputs directory holds %v, want the two inputs and nothing else", names)
	}

	// The names are the customer's and the run log is uploaded.
	log := string(run.Log().Bytes())
	for _, name := range []string{"Screen", "q3.pdf", "Screen_Shot"} {
		if strings.Contains(log, name) {
			t.Errorf("the run log names an input (%q); names are customer data", name)
		}
	}
	if !strings.Contains(log, "inputs: 2 files") {
		t.Errorf("the run log does not say how many inputs were read:\n%s", log)
	}
}

func TestAnInputWhoseBytesDoNotMatchItsDigestFailsAsInfraAndTheAgentNeverRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "look at this",
		Inputs: []controlplane.InputFile{{Name: "shot.png", MediaType: "image/png", Body: screenshot}},
	})
	h.cp.CorruptInputs(0)
	h.commander.handle = agentScript(h, "should not happen", "")

	run, code := h.executeRun()
	if code != runv1.ExitStorage {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitStorage, run.Failure())
	}
	for _, c := range h.commander.calls {
		if isAgentStart(c) {
			t.Fatal("the agent was started over a file that is not the one the person sent")
		}
	}
	report, ok := h.cp.AcceptedReport(h.prepared.RunID)
	if !ok {
		t.Fatal("the failure was not reported")
	}
	if report.FailedPhase != runv1.RuntimePhaseInputs {
		t.Errorf("failedPhase %q, want inputs", report.FailedPhase)
	}
	if report.Reason != "InputDigestMismatch" {
		t.Errorf("reason %q, want InputDigestMismatch", report.Reason)
	}
	if report.FailureClass != runv1.FailureInfra || !report.FailureClass.Retriable() {
		t.Errorf("failureClass %q; the controller held the file, so a bad transfer is infra and retriable",
			report.FailureClass)
	}
	if got := h.phase(run, runv1.RuntimePhaseRun); got != runv1.PhaseOutcomeSkipped {
		t.Errorf("the run phase is %s, want skipped", got)
	}
	// Neither the file nor a half of it is left for anything to read.
	entries, _ := os.ReadDir(h.layout.Inputs)
	if len(entries) != 0 {
		t.Errorf("the inputs directory holds %d entries after a refused transfer", len(entries))
	}
}

func TestARunWithoutInputsSkipsThePhase(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	if got := h.phase(run, runv1.RuntimePhaseInputs); got != runv1.PhaseOutcomeSkipped {
		t.Errorf("the inputs phase is %s, want skipped", got)
	}
	if !strings.Contains(string(run.Log().Bytes()), "this run has no inputs") {
		t.Error("the skip does not say why")
	}
	if n := h.cp.InputReads(h.prepared.RunID); n != 0 {
		t.Errorf("%d reads of inputs a run does not have", n)
	}
}

func TestAControllerThatIsBrieflyUnavailableIsAskedAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "look at this",
		Inputs: []controlplane.InputFile{{Name: "shot.png", MediaType: "image/png", Body: screenshot}},
	})
	h.cp.FailInputs(http.StatusServiceUnavailable, 1)
	h.commander.handle = agentScript(h, "done", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	if n := h.cp.InputReads(h.prepared.RunID); n != 2 {
		t.Errorf("%d reads, want 2: one refused with 503 and the retry", n)
	}
}

func TestAnInputTheControllerDoesNotHoldIsNotAskedForTwice(t *testing.T) {
	t.Parallel()
	// A manifest naming a key the controller serves nothing under: 404, which
	// another try a second later would only repeat.
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "look at this",
		Secrets: map[string]string{runv1.SecretKeyInputs: `{"inputs":[{"key":"inputs/1-shot.png",` +
			`"name":"shot.png","mediaType":"image/png","sizeBytes":3,` +
			`"sha256":"` + strings.Repeat("ab", 32) + `"}]}`},
	})
	h.commander.handle = agentScript(h, "should not happen", "")

	run, code := h.executeRun()
	if code != runv1.ExitStorage {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitStorage, run.Failure())
	}
	if f := run.Failure(); f.Reason != "InputUnavailable" {
		t.Errorf("reason %q, want InputUnavailable", f.Reason)
	}
	if n := h.cp.InputReads(h.prepared.RunID); n != 1 {
		t.Errorf("%d reads, want 1: a 404 is final", n)
	}
}

func TestAnInputLeftReadOnlyByAnEarlierAttemptIsReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "look at this",
		Inputs: []controlplane.InputFile{{Name: "shot.png", MediaType: "image/png", Body: screenshot}},
	})
	// The emptyDir outlives a container restart, so a retry finds the earlier
	// attempt's copy, read-only.
	path := filepath.Join(h.layout.Inputs, "1-shot.png")
	if err := os.MkdirAll(h.layout.Inputs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("an earlier, partial copy"), 0o444); err != nil {
		t.Fatal(err)
	}
	h.commander.handle = agentScript(h, "done", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, screenshot) {
		t.Errorf("the earlier copy was not replaced (%d bytes, %v)", len(got), err)
	}
}

func TestAMalformedInputManifestIsAConfigurationDefect(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("ab", 32)
	for name, manifest := range map[string]string{
		"it does not parse":         `{"inputs":`,
		"a key escapes the inputs":  `{"inputs":[{"key":"inputs/../result.md","sizeBytes":1,"sha256":"` + digest + `"}]}`,
		"a key is not under inputs": `{"inputs":[{"key":"artifacts/1-a.png","sizeBytes":1,"sha256":"` + digest + `"}]}`,
		"a digest is not one":       `{"inputs":[{"key":"inputs/1-a.png","sizeBytes":1,"sha256":"ABC"}]}`,
		"a key is listed twice": `{"inputs":[{"key":"inputs/1-a.png","sizeBytes":1,"sha256":"` + digest +
			`"},{"key":"inputs/1-a.png","sizeBytes":1,"sha256":"` + digest + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, controlplane.RunRequest{
				Prompt:  "hello",
				Secrets: map[string]string{runv1.SecretKeyInputs: manifest},
			})
			f := h.loadFails()
			if f.Code != runv1.ExitConfig || f.Reason != "MalformedSecret" {
				t.Errorf("exit %d %s, want %d MalformedSecret", f.Code, f.Reason, runv1.ExitConfig)
			}
		})
	}
}

func TestCodexIsHandedEachAttachedImageAndNotTheOtherFiles(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "what is on these?",
		Agent:  runv1.AgentCodex,
		Inputs: []controlplane.InputFile{
			{Name: "a.png", MediaType: "image/png", Body: screenshot},
			{Name: "q3.pdf", MediaType: "application/pdf", Body: quarterly},
			{Name: "b.jpg", MediaType: "image/jpeg", Body: []byte("\xff\xd8\xff a jpeg")},
		},
	})
	var mu sync.Mutex
	var args []string
	script := agentScript(h, "done", "")
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if isAgentStart(c) {
			mu.Lock()
			args = append([]string(nil), c.Args...)
			mu.Unlock()
		}
		return script(c)
	}

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	mu.Lock()
	defer mu.Unlock()

	var images []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--image" && i+1 < len(args) {
			images = append(images, args[i+1])
			// The option takes one or more values; the next argument must
			// start with a dash or it would be swallowed as one more image.
			if i+2 >= len(args) || !strings.HasPrefix(args[i+2], "-") {
				t.Errorf("--image %s is followed by %q, which codex would read as another image",
					args[i+1], args[min(i+2, len(args)-1)])
			}
		}
	}
	want := []string{
		filepath.Join(h.layout.Inputs, "1-a.png"),
		filepath.Join(h.layout.Inputs, "3-b.jpg"),
	}
	if !slices.Equal(images, want) {
		t.Errorf("codex was handed images %v, want %v (args %q)", images, want, args)
	}
	if args[len(args)-1] != "-" {
		t.Errorf("the prompt no longer comes from stdin: args end %q", args[len(args)-1])
	}
}

func TestClaudeCodeWithoutItsReadToolIsWarnedAbout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt:      "look at this",
		DeniedTools: []string{"Bash", "Read"},
		Inputs:      []controlplane.InputFile{{Name: "shot.png", MediaType: "image/png", Body: screenshot}},
	})
	h.commander.handle = agentScript(h, "done", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	if !strings.Contains(string(run.Log().Bytes()), "may not be able to open") {
		t.Error("a role that denies Read was not warned that the agent may not see the attached files")
	}
}

func TestThePodCannotReadAKeyOutsideItsRunsManifest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "mine",
		Inputs: []controlplane.InputFile{{Name: "mine.png", MediaType: "image/png", Body: screenshot}},
	})
	theirs := h.cp.Prepare(controlplane.RunRequest{
		Prompt: "theirs",
		Inputs: []controlplane.InputFile{
			{Name: "mine.png", MediaType: "image/png", Body: []byte("theirs")},
			{Name: "salary.pdf", MediaType: "application/pdf", Body: quarterly},
		},
	})

	read := func(token string, run runv1.ULID, key string) (int, []byte, string) {
		t.Helper()
		q := url.Values{runv1.QueryRunID: {string(run)}, runv1.QueryKey: {key}}
		req, err := http.NewRequest(http.MethodGet,
			h.prepared.CallbackURL+runv1.CallbackPathInputs+"?"+q.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		return resp.StatusCode, body, resp.Header.Get(runv1.HeaderSHA256)
	}

	mine, me := h.prepared.CallbackToken, h.prepared.RunID
	for _, tc := range []struct {
		name  string
		token string
		run   runv1.ULID
		key   string
		want  int
	}{
		{"its own input", mine, me, "inputs/1-mine.png", http.StatusOK},
		// Same shape, same allow-list, another run's manifest.
		{"a key only another run's manifest lists", mine, me, "inputs/2-salary.pdf", http.StatusNotFound},
		{"another run, with its own token", mine, theirs.RunID, "inputs/2-salary.pdf", http.StatusUnauthorized},
		{"no token", "", me, "inputs/1-mine.png", http.StatusUnauthorized},
		{"a token nobody minted", "deadbeef", me, "inputs/1-mine.png", http.StatusUnauthorized},
		{"a key no input may have", mine, me, "inputs/../result.md", http.StatusBadRequest},
		{"a key outside inputs", mine, me, "result.md", http.StatusBadRequest},
	} {
		status, body, digest := read(tc.token, tc.run, tc.key)
		if status != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, status, tc.want, body)
			continue
		}
		if status == http.StatusOK {
			if !bytes.Equal(body, screenshot) {
				t.Errorf("%s: served another run's bytes", tc.name)
			}
			if digest == "" {
				t.Errorf("%s: no %s header", tc.name, runv1.HeaderSHA256)
			}
		}
	}
}
