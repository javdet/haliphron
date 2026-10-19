package image_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// The container's stderr: errors, and nothing else.
//
// The run log reaches a reader only through storage. A run whose storage is the
// thing that broke, or a pod killed before its next chunk, leaves nothing
// there — and stderr is collected by the kubelet without any help from the
// process. These rows check that what explains a crash is on it, that it is
// redacted, and that a run which did not fail leaves it empty.

func TestACrashedAgentLeavesItsStderrOnTheContainersStderr(t *testing.T) {
	t.Parallel()
	const key = "sk-ant-api03-a-key-a-stack-trace-printed"
	h := newHarness(t, controlplane.RunRequest{
		Prompt:  "crash",
		Secrets: map[string]string{runv1.SecretKeyLLMAPIKey: key},
	})
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			time.Sleep(2 * time.Millisecond)
			fmt.Fprintln(c.Stderr, "request headers: x-api-key="+key)
			fmt.Fprintln(c.Stderr, "file:///usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js:1")
			fmt.Fprintln(c.Stderr, "TypeError: Cannot read properties of undefined (reading 'content')")
			return entrypoint.CommandResult{ExitCode: 1}, nil
		}
		return entrypoint.CommandResult{}, nil
	}

	run, code := h.executeRun()
	if code != runv1.ExitAgentError {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitAgentError, run.Failure())
	}

	stderr := h.stderr.String()
	for _, want := range []string{
		"TypeError: Cannot read properties of undefined",
		"cli.js:1",
		"phase run failed",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not carry %q:\n%s", want, stderr)
		}
	}
	// Nothing downstream of the kubelet redacts. This is the last chance.
	if strings.Contains(stderr, key) {
		t.Fatalf("the model key reached the container's stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "x-api-key="+entrypoint.Mask) {
		t.Errorf("the line with the key was not masked, or not written:\n%s", stderr)
	}

	// The person reading the run in the UI gets the cause, not only the code.
	if msg := run.Failure().Message(); !strings.Contains(msg, "TypeError") {
		t.Errorf("the failure message does not say why the agent died: %s", msg)
	}
	// And a crash is still a run that persisted what it had.
	h.mustObject(runv1.StorageKeyResult)
}

func TestAnErrorTheCLIReportedItselfReachesStderr(t *testing.T) {
	t.Parallel()

	// A rejected key is the commonest crash there is, and neither CLI puts it
	// on stderr: it is in the structured stream, and stderr is empty.
	tests := []struct {
		name   string
		agent  runv1.AgentType
		stdout string
		want   string
	}{
		{
			name:  "claude-code's is_error result",
			agent: runv1.AgentClaudeCode,
			stdout: mustJSON(t, map[string]any{
				"type": "result", "subtype": "success", "is_error": true,
				"result": "Invalid API key · Fix external API key",
			}),
			want: "Invalid API key",
		},
		{
			name:  "codex's turn.failed event",
			agent: runv1.AgentCodex,
			stdout: `{"type":"thread.started","thread_id":"t1"}` + "\n" +
				`{"type":"turn.failed","error":{"message":"401 Unauthorized: incorrect API key provided"}}`,
			want: "401 Unauthorized",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, controlplane.RunRequest{Prompt: "hello", Agent: tc.agent})
			h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
				launch := (c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p") ||
					(c.Path == "codex" && len(c.Args) > 0 && c.Args[0] == "exec")
				if launch {
					time.Sleep(2 * time.Millisecond)
					fmt.Fprintln(c.Stdout, tc.stdout)
					return entrypoint.CommandResult{ExitCode: 1}, nil
				}
				return entrypoint.CommandResult{}, nil
			}

			run, code := h.executeRun()
			if code != runv1.ExitAgentError {
				t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitAgentError, run.Failure())
			}
			stderr := h.stderr.String()
			if !strings.Contains(stderr, "reported an error: "+tc.want) {
				t.Errorf("stderr does not carry the CLI's own error:\n%s", stderr)
			}
			if !strings.Contains(stderr, "wrote nothing to stderr") {
				t.Errorf("stderr does not say the CLI's stderr was empty:\n%s", stderr)
			}
			if msg := run.Failure().Message(); !strings.Contains(msg, tc.want) {
				t.Errorf("the failure message does not carry the CLI's error: %s", msg)
			}
		})
	}
}

func TestASuccessfulRunWritesNothingToStderr(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	script := agentScript(h, "done", "")
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			// Chatter on stderr from a CLI that then succeeds is not an error,
			// and it stays in the run log.
			fmt.Fprintln(c.Stderr, "warning: something the CLI thought worth mentioning")
		}
		return script(c)
	}

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	if stderr := h.stderr.String(); stderr != "" {
		t.Fatalf("a successful run wrote to the container's stderr — errors only, "+
			"the stream belongs in the chunks:\n%s", stderr)
	}
	if !strings.Contains(string(run.Log().Bytes()), "something the CLI thought worth mentioning") {
		t.Error("the agent's stderr is missing from the run log")
	}
}

func TestAPanicInAPhaseIsAFailureAndNotALostRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")
	h.options = append(h.options, entrypoint.WithCommander(panickingCommander{
		inner: h.commander, path: "claude", firstArg: "-p",
	}))

	run, code := h.executeRun()
	// The phase's own default code, as for any unclassified failure — not the
	// runtime's exit 2, which the controller cannot classify.
	if code != runv1.ExitAgentError {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitAgentError, run.Failure())
	}
	if run.Failure().Reason != "EntrypointPanic" {
		t.Errorf("reason %q, want EntrypointPanic", run.Failure().Reason)
	}

	stderr := h.stderr.String()
	if !strings.Contains(stderr, "phase run panicked: a bug in the entrypoint") {
		t.Errorf("stderr does not carry the panic:\n%s", stderr)
	}
	if !strings.Contains(stderr, "goroutine ") {
		t.Errorf("stderr does not carry the stack:\n%s", stderr)
	}

	// The epilogue ran: the result is in storage and the controller was told.
	h.mustObject(runv1.StorageKeyResult)
	if run.Report() == nil {
		t.Fatal("no completion report was built")
	}
}

// panickingCommander panics, on the caller's goroutine, when one program is
// started — the way a bug in a phase panics.
type panickingCommander struct {
	inner          entrypoint.Commander
	path, firstArg string
}

func (p panickingCommander) Run(ctx context.Context, c entrypoint.Command) (entrypoint.CommandResult, error) {
	if c.Path == p.path && len(c.Args) > 0 && c.Args[0] == p.firstArg {
		panic("a bug in the entrypoint")
	}
	return p.inner.Run(ctx, c)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
