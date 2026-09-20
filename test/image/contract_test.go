package image_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// Section 18 of the runtime contract, as tests. Each name is the row it checks.

// --- configuration and startup ---------------------------------------------

func TestAContractMajorAboveTheImagesRefusesToStart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt:        "hello",
		ContractMajor: runv1.ContractMajor + 1,
	}, withCommands(func(entrypoint.Command) (entrypoint.CommandResult, error) {
		t.Fatal("a command was started under a contract this image does not implement")
		return entrypoint.CommandResult{}, nil
	}))

	f := h.loadFails()
	if f.Code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d", f.Code, runv1.ExitConfig)
	}
	if f.Reason != "ContractMismatch" {
		t.Fatalf("reason %q, want ContractMismatch — without it this looks like "+
			"a run that mysteriously cannot find its prompt", f.Reason)
	}
	// Nothing was written, so nothing was spent. Zero rather than one: the
	// prompt used to sit under this prefix before the pod started, and it is a
	// column in the control plane now.
	if keys := h.cp.RunKeys(h.prepared.RunID); len(keys) != 0 {
		t.Fatalf("the run touched storage before checking the contract: %v", keys)
	}
}

func TestAMissingRequiredVariableFailsBeforeAnyNetworkCall(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "hello",
		Env:    map[string]string{runv1.EnvPromptSHA256: "", runv1.EnvModel: ""},
	})

	f := h.loadFails()
	if f.Code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d", f.Code, runv1.ExitConfig)
	}
	// Reported together. A user who fixes one variable, waits for a pod and
	// learns about the next has been made to pay for our convenience.
	for _, name := range []string{runv1.EnvPromptSHA256, runv1.EnvModel} {
		if !strings.Contains(f.Message(), name) {
			t.Errorf("the message does not name %s: %s", name, f.Message())
		}
	}
	if h.cp.CallbackAttempts() != 0 {
		t.Fatal("the controller was called before the configuration was checked")
	}
}

func TestAMissingSecretFileNamesTheFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})

	// Remove the callback token, as a Secret created without that key would.
	removeSecret(t, h, runv1.SecretKeyCallbackToken)

	f := h.loadFails()
	if f.Code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d", f.Code, runv1.ExitConfig)
	}
	if !strings.Contains(f.Message(), runv1.SecretKeyCallbackToken) {
		t.Fatalf("the message does not name the file, which is the one fact "+
			"that sends the operator to the right place: %s", f.Message())
	}
}

// --- prompt and storage -----------------------------------------------------

func TestAPromptDigestMismatchNeverCallsTheModel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "add a postgres database",
		// The digest of something else entirely.
		PromptSHA256: strings.Repeat("ab", 32),
	}, withCommands(agentScript(nil, "", "")))

	run, code := h.executeRun()
	if code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d", code, runv1.ExitConfig)
	}
	if run.Failure().Reason != "PromptDigestMismatch" {
		t.Fatalf("reason %q, want PromptDigestMismatch", run.Failure().Reason)
	}
	if h.commander.called("claude") {
		t.Fatal("the model was called on a prompt that is not the one the backend admitted")
	}
	if h.phase(run, runv1.RuntimePhaseRun) != runv1.PhaseOutcomeSkipped {
		t.Fatalf("the run phase is %s, want skipped", h.phase(run, runv1.RuntimePhaseRun))
	}
}

func TestAMissingCheckpointIsASkipAndNotAFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0: an absent HALIPHRON_COMPLETED_PHASES is the normal "+
			"answer on a first attempt and an image that fails on it fails every run (%v)",
			code, run.Failure())
	}
	if got := h.phase(run, runv1.RuntimePhaseCheckpoint); got != runv1.PhaseOutcomeSkipped {
		t.Fatalf("the checkpoint phase is %s, want skipped", got)
	}
}

// The phases go to the controller as they happen, which is what replaced
// writing them into an object. A pod killed between two phases has still
// recorded the one it finished.
func TestEveryCompletedPhaseIsReportedAsItHappens(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}

	reported := h.cp.Phases(h.prepared.RunID)
	if len(reported) == 0 {
		t.Fatal("no phase was reported; the next attempt would pay for the model again")
	}
	// In execution order, because the resume rule is "every phase before the
	// first unfinished one is done" and that is only meaningful against a fixed
	// sequence.
	order := map[runv1.RuntimePhase]int{}
	for i, p := range runv1.RuntimePhases {
		order[p] = i
	}
	for i := 1; i < len(reported); i++ {
		if order[reported[i]] < order[reported[i-1]] {
			t.Fatalf("phases were reported out of order: %v", reported)
		}
	}
	// And the ones that matter are among them.
	got := map[runv1.RuntimePhase]bool{}
	for _, p := range reported {
		got[p] = true
	}
	for _, want := range []runv1.RuntimePhase{runv1.RuntimePhaseRun, runv1.RuntimePhasePersist} {
		if !got[want] {
			t.Errorf("phase %s was never reported: %v", want, reported)
		}
	}
	// A skipped or failed phase is not reported: a later attempt may not assume
	// it was done, and the one phase whose replay costs money is exactly where
	// getting that wrong would skip a model call that never happened.
	if got[runv1.RuntimePhaseCheckpoint] {
		t.Error("a skipped phase was reported as completed")
	}

	// The report carries the same list, as the copy that survives a pod whose
	// last few reports did not get through.
	if len(run.Report().CompletedPhases) == 0 {
		t.Error("the completion report carries no checkpoint")
	}
}

func TestARefusedUploadIsInfraAndNotConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")

	// An expired signature. The controller reissues the bundle before the next
	// attempt, which makes this the one failure the cluster repairs by itself —
	// so it must be retryable, and exit 30 is not.
	h.cp.FailStorage(http.MethodPut, runv1.StorageKeyResult, http.StatusForbidden, 0)

	run, code := h.executeRun()
	if code != runv1.ExitStorage {
		t.Fatalf("exit %d, want %d", code, runv1.ExitStorage)
	}
	if class := runv1.FailureClassForExitCode(code); !class.Retriable() {
		t.Fatalf("class %s is not retriable; a run whose result was already "+
			"obtained would die for good", class)
	}
	if run.Failure().Phase != runv1.RuntimePhasePersist {
		t.Fatalf("failed phase %s, want persist", run.Failure().Phase)
	}
}

// The prompt cannot be unreachable any more: it is a variable, and the fetch
// phase makes no network call at all. What is left to check is that its absence
// is refused before the model is called, and named.
func TestAMissingPromptIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "hello",
		// An empty override removes the variable, which is how a Secret written
		// without the key, or a container built without the reference, reaches
		// the pod.
		Env: map[string]string{runv1.EnvPrompt: ""},
	})

	// Caught while the configuration is being read, which is before anything at
	// all happens — not at the fetch phase, and certainly not at the run phase
	// against an empty string.
	f := h.loadFails()
	if f.Code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d", f.Code, runv1.ExitConfig)
	}
	if !strings.Contains(f.Message(), runv1.EnvPrompt) {
		t.Errorf("the message does not name the variable: %s", f.Message())
	}
}

// --- resumption -------------------------------------------------------------

func TestAResumedAttemptDoesNotPayForTheModelAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "the first attempt's answer", `{"pr":"opened"}`)

	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("the first attempt exited %d", code)
	}
	firstResult := h.mustObject(runv1.StorageKeyResult)

	// The controller reissues the bundle and starts attempt two.
	second := h.cp.Reissue(h.prepared, 2)
	h.stage(second)
	h.commander = &scriptedCommander{handle: func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			t.Error("the model was called on a resumed attempt")
		}
		return entrypoint.CommandResult{}, nil
	}}

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("the resumed attempt exited %d (%v)", code, run.Failure())
	}
	for _, p := range []runv1.RuntimePhase{
		runv1.RuntimePhaseRun, runv1.RuntimePhaseParse,
		runv1.RuntimePhaseOutput, runv1.RuntimePhasePersist,
	} {
		if got := h.phase(run, p); got != runv1.PhaseOutcomeSkipped {
			t.Errorf("phase %s is %s on a resumed attempt, want skipped", p, got)
		}
	}
	if got := h.mustObject(runv1.StorageKeyResult); !bytes.Equal(got, firstResult) {
		t.Fatal("the resumed attempt overwrote the result it was supposed to inherit")
	}
	report := run.Report()
	if report.ResultRef == nil || report.OutputRef == nil {
		t.Fatal("the resumed attempt reported no references; skipping the paid " +
			"phase and then having nothing to report is worse than paying twice")
	}
}

// Any doubt is resolved in favour of a full run. An extra bill for the model is
// money; a wrongly resumed attempt is an incorrect result reported as correct,
// and nobody notices that on the day it happens.
func TestACheckpointIsRefusedWhenAnythingAboutItIsWrong(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		attempt int32
		phases  []runv1.RuntimePhase
	}{
		{
			name:    "it is empty",
			attempt: 2,
		},
		{
			name:    "the run phase is not in it",
			attempt: 2,
			phases:  []runv1.RuntimePhase{runv1.RuntimePhaseInit, runv1.RuntimePhaseClone},
		},
		{
			// The stricter half of the resume rule. persist is what made the
			// model's product durable; an attempt that ran the model and did
			// not reach persist has its output nowhere, and skipping the paid
			// phase would leave this one with nothing to report.
			name:    "the run phase is in it and persist is not",
			attempt: 2,
			phases:  []runv1.RuntimePhase{runv1.RuntimePhaseRun},
		},
		{
			// A checkpoint on a first attempt belongs to a previous owner of
			// the work. This pod has none of that attempt's product.
			name:    "it arrived on a first attempt",
			attempt: 1,
			phases:  []runv1.RuntimePhase{runv1.RuntimePhaseRun, runv1.RuntimePhasePersist},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
			h.commander.handle = agentScript(h, "first", "")
			if code := h.execute(); code != runv1.ExitSuccess {
				t.Fatalf("the first attempt exited %d", code)
			}

			h.stage(h.cp.ReissueWithPhases(h.prepared, tc.attempt, tc.phases))
			called := false
			h.commander = &scriptedCommander{handle: func(c entrypoint.Command) (entrypoint.CommandResult, error) {
				if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
					called = true
					h.writeOutput("{}")
					c.Stdout.Write([]byte(claudeSummary("second")))
				}
				return entrypoint.CommandResult{}, nil
			}}

			run, code := h.executeRun()
			if code != runv1.ExitSuccess {
				t.Fatalf("exit %d (%v)", code, run.Failure())
			}
			if !called {
				t.Fatal("the attempt resumed from a checkpoint it had no business trusting; " +
					"any doubt is resolved in favour of a full run")
			}
		})
	}
}

// A phase name this image does not recognise is dropped rather than carried.
// The list is "phases you may skip", and skipping one whose name means nothing
// here is the single way this variable could cost money — an image a version
// behind must not be talked into believing it has already run the model.
func TestAnUnknownPhaseNameCannotCauseAResume(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "first", "")
	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("the first attempt exited %d", code)
	}

	// A control plane a version ahead, naming the expensive phase something
	// this image has never heard of.
	second := h.cp.ReissueWithPhases(h.prepared, 2, nil)
	second.Env[runv1.EnvCompletedPhases] = "init,invoke-model,store"
	h.stage(second)

	called := false
	h.commander = &scriptedCommander{handle: func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			called = true
			h.writeOutput("{}")
			c.Stdout.Write([]byte(claudeSummary("second")))
		}
		return entrypoint.CommandResult{}, nil
	}}

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	if !called {
		t.Fatal("a phase name this image does not implement was taken as grounds to skip the model")
	}
}

func TestChunkNumberingContinuesAcrossAttempts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello", LogChunkSeconds: 1})
	h.commander.handle = agentScript(h, "first", "")
	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("the first attempt exited %d", code)
	}
	first := h.cp.Keys(h.prefix() + runv1.StoragePrefixChunks)
	if len(first) == 0 {
		t.Fatal("the first attempt uploaded no log chunks at all")
	}

	second := h.cp.Reissue(h.prepared, 2)
	h.stage(second)
	h.commander = &scriptedCommander{handle: agentScript(h, "second", "")}
	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("the second attempt exited %d", code)
	}

	// Numbering from zero again would overwrite the first attempt's chunks and
	// the reader would see two runs spliced into one without a seam.
	after := h.cp.Keys(h.prefix() + runv1.StoragePrefixChunks)
	if len(after) <= len(first) {
		t.Fatalf("the second attempt overwrote chunks instead of continuing: %v then %v", first, after)
	}
}

// --- ordering and durability ------------------------------------------------

func TestAFailedPushLeavesTheResultAlreadyInStorage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "change something", RepoURL: "https://forge.invalid/org/repo.git",
		GitProvider: runv1.GitProviderGitHub, BaseBranch: "main",
		TargetBranch: "haliphron/abc-change", CreatePR: true,
	})
	h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "git" && gitSubcommand(c.Args) == "push" {
			c.Stdout.Write([]byte("remote: Internal Server Error\nfatal: unable to access: 500\n"))
			return entrypoint.CommandResult{ExitCode: 128}, nil
		}
		return agentScript(h, "I changed a file", `{"summary":"ok"}`)(c)
	})

	run, code := h.executeRun()
	if code != runv1.ExitGit {
		t.Fatalf("exit %d, want %d: a 500 from the forge is surmountable (%v)",
			code, runv1.ExitGit, run.Failure())
	}
	// This is the whole of the persist-before-git ordering. Forty minutes and
	// twenty dollars are already durable when the push fails.
	if _, ok := h.object(runv1.StorageKeyResult); !ok {
		t.Error("result.md is not in storage; the second attempt would skip the model and find nothing")
	}
	if _, ok := h.object(runv1.StorageKeyOutput); !ok {
		t.Error("output.json is not in storage")
	}
	if got := h.phase(run, runv1.RuntimePhasePersist); got != runv1.PhaseOutcomeOK {
		t.Errorf("the persist phase is %s; it must run before the git phases, not after", got)
	}
}

func TestA403OnPushIsConfigAnd500IsGit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer string
		want   int32
	}{
		{
			// A retry would reproduce the same answer three times and spend
			// three pod runs doing it.
			name:   "a protected branch",
			answer: "remote: error: GH006: Protected branch update failed\n! [remote rejected] (protected branch hook declined)\n",
			want:   runv1.ExitConfig,
		},
		{
			name:   "an expired token",
			answer: "remote: Invalid username or password.\nfatal: Authentication failed\n",
			want:   runv1.ExitConfig,
		},
		{
			name:   "the forge had a bad minute",
			answer: "remote: Internal Server Error\nfatal: the remote end hung up unexpectedly\n",
			want:   runv1.ExitGit,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, controlplane.RunRequest{
				Prompt: "change something", RepoURL: "https://forge.invalid/org/repo.git",
				GitProvider: runv1.GitProviderGitHub, BaseBranch: "main",
				TargetBranch: "haliphron/abc-change",
			})
			h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
				if c.Path == "git" && gitSubcommand(c.Args) == "push" {
					c.Stdout.Write([]byte(tc.answer))
					return entrypoint.CommandResult{ExitCode: 128}, nil
				}
				return agentScript(h, "done", "")(c)
			})

			_, code := h.executeRun()
			if code != tc.want {
				t.Fatalf("exit %d, want %d — the pod has the cause and the "+
					"controller only has the effect", code, tc.want)
			}
		})
	}
}

func TestTheStoredReportIsTheBytesTheControllerReceived(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")

	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("exit %d", code)
	}
	reports := h.cp.Reports(h.prepared.RunID)
	if len(reports) != 1 {
		t.Fatalf("the controller saw %d reports, want 1", len(reports))
	}
	stored := h.mustObject(runv1.StorageKeyCompletion)
	// Either copy may turn out to be the one that survived, and a difference
	// between them is a question nobody can answer afterwards.
	if !bytes.Equal(stored, reports[0].Body) {
		t.Fatalf("completion.json and the webhook body differ:\n stored: %s\nwebhook: %s",
			stored, reports[0].Body)
	}
	if reports[0].Idempotency != string(h.prepared.RunID)+"/1" {
		t.Fatalf("Idempotency-Key is %q, want {runID}/{attempt}", reports[0].Idempotency)
	}
}

func TestAnUndeliveredReportDoesNotChangeTheExitCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")
	// Unreachable for every attempt the pod will make.
	h.cp.FailCallback(http.StatusServiceUnavailable, 0)

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0: the report is a message about the result, "+
			"not the result (%v)", code, run.Failure())
	}
	if _, ok := h.object(runv1.StorageKeyCompletion); !ok {
		t.Fatal("the report is neither at the controller nor in storage")
	}
	if attempts := h.cp.CallbackAttempts(); attempts < 2 {
		t.Fatalf("the pod made %d delivery attempts; the contract asks for retries", attempts)
	}
}

// --- agent and output -------------------------------------------------------

func TestADeclaredNodeSchemaMakesTheOutputMandatory(t *testing.T) {
	t.Parallel()
	schema := `{"type":"object","required":["ticket"],"properties":{"ticket":{"type":"string"}}}`

	t.Run("the file is missing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, controlplane.RunRequest{
			Prompt:     "hello",
			RoleConfig: map[string]string{runv1.RoleConfigKeyOutputSchema: schema},
		})
		h.commander.handle = agentScript(h, "I forgot", "")

		run, code := h.executeRun()
		if code != runv1.ExitOutputInvalid {
			t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitOutputInvalid, run.Failure())
		}
	})

	t.Run("the file does not match", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, controlplane.RunRequest{
			Prompt:     "hello",
			RoleConfig: map[string]string{runv1.RoleConfigKeyOutputSchema: schema},
		})
		h.commander.handle = agentScript(h, "here you go", `{"ticket":42}`)

		run, code := h.executeRun()
		if code != runv1.ExitOutputInvalid {
			t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitOutputInvalid, run.Failure())
		}
		// The one fact the user needs is which field, not that two documents
		// disagree somewhere.
		if !strings.Contains(run.Failure().Message(), "ticket") {
			t.Fatalf("the message does not say where the mismatch is: %s", run.Failure().Message())
		}
	})

	t.Run("the file matches", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, controlplane.RunRequest{
			Prompt:     "hello",
			RoleConfig: map[string]string{runv1.RoleConfigKeyOutputSchema: schema},
		})
		h.commander.handle = agentScript(h, "here you go", `{"ticket":"OPS-1"}`)

		run, code := h.executeRun()
		if code != runv1.ExitSuccess {
			t.Fatalf("exit %d (%v)", code, run.Failure())
		}
		var data struct {
			Ticket string `json:"ticket"`
		}
		if err := json.Unmarshal(h.envelope().Data, &data); err != nil {
			t.Fatalf("data does not parse: %v", err)
		}
		if data.Ticket != "OPS-1" {
			t.Fatalf("data.ticket is %q; the payload must reach the envelope untouched", data.Ticket)
		}
	})
}

func TestNoSchemaAndNoFileIsASuccessWithAnEmptyPayload(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "just tell me something"})
	h.commander.handle = agentScript(h, "here is the analysis", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0: in v1 nothing consumes structured output, and "+
			"failing a completed run over a file nobody reads is pure loss (%v)",
			code, run.Failure())
	}
	env := h.envelope()
	if string(env.Data) != "{}" {
		t.Fatalf("data is %s, want {}; a consumer must never have to branch on null", env.Data)
	}
}

func TestTheEnvelopeIsFilledByTheEntrypointAndNotTheAgent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello", Model: "anthropic/claude-opus-5"})
	// The agent writes a payload that also contains envelope-shaped keys. They
	// are payload: every field of the envelope is the machine's, and a model
	// that says status: ok has said nothing.
	h.commander.handle = agentScript(h, "the answer", `{"status":"ok","runID":"NOTAREALRUNID","x":1}`)

	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("exit %d", code)
	}
	env := h.envelope()
	if env.SchemaVersion != runv1.OutputSchemaVersion {
		t.Errorf("schemaVersion %d, want %d", env.SchemaVersion, runv1.OutputSchemaVersion)
	}
	if env.RunID != h.prepared.RunID {
		t.Errorf("runID %q, want %q — the agent does not get to name the run", env.RunID, h.prepared.RunID)
	}
	if env.Status != runv1.OutputStatusOK {
		t.Errorf("status %q, want ok derived from the exit code", env.Status)
	}
	if env.Attempt != 1 || env.Agent != runv1.AgentClaudeCode || env.Model != "anthropic/claude-opus-5" {
		t.Errorf("the envelope's identity fields are wrong: %+v", env)
	}
	if env.ProducedAt == "" {
		t.Error("producedAt is empty")
	}
	if !strings.Contains(string(env.Data), `"x":1`) {
		t.Errorf("the payload did not survive into data: %s", env.Data)
	}
}

func TestATimeoutKeepsTheWorkAndExitsEleven(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "take too long", TimeoutSeconds: 1,
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc-slow",
	})
	h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			// Work done and left on disk, then a hang until the budget runs
			// out and the entrypoint stops it.
			h.writeOutput(`{"partial":true}`)
			c.Stdout.Write([]byte(claudeSummary("I got halfway")))
			h.commander.sleepUntilCancelled(c.Path, 30*time.Second)
		}
		return entrypoint.CommandResult{}, nil
	})

	run, code := h.executeRun()
	if code != runv1.ExitAgentTimeout {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitAgentTimeout, run.Failure())
	}
	// Forty minutes that did not fit into an hour cost the same as forty
	// minutes that did, and there is no reason to throw them away.
	for _, p := range []runv1.RuntimePhase{
		runv1.RuntimePhasePersist, runv1.RuntimePhaseCommit,
		runv1.RuntimePhasePush, runv1.RuntimePhasePR,
	} {
		if got := h.phase(run, p); got == runv1.PhaseOutcomeSkipped && p == runv1.RuntimePhasePersist {
			t.Errorf("phase %s was skipped after a timeout", p)
		}
	}
	if h.envelope().Status != runv1.OutputStatusPartial {
		t.Errorf("status %q, want partial: there was a timeout and there is work all the same",
			h.envelope().Status)
	}
}

func TestAnAgentThatSaidNothingStillProducesAResult(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "", "")

	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("exit %d", code)
	}
	// A missing object under a fixed key breaks the read path for the backend
	// and the UI, and "the agent said nothing" is information too.
	result := string(h.mustObject(runv1.StorageKeyResult))
	if !strings.Contains(result, "produced no textual result") {
		t.Fatalf("result.md is not the stub: %q", result)
	}
	if !strings.Contains(result, string(h.prepared.RunID)) {
		t.Errorf("the stub does not name its own run: %q", result)
	}
}

func TestUsageIsNormalisedForBothRuntimes(t *testing.T) {
	t.Parallel()

	t.Run("claude-code reports a cost", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, controlplane.RunRequest{Prompt: "hello", Agent: runv1.AgentClaudeCode})
		h.commander.handle = agentScript(h, "done", "")

		run, code := h.executeRun()
		if code != runv1.ExitSuccess {
			t.Fatalf("exit %d (%v)", code, run.Failure())
		}
		usage := run.Report().Usage
		if usage == nil {
			t.Fatal("no usage in the report")
		}
		if usage.TotalCostUSD != "0.423100" {
			t.Errorf("totalCostUSD %q; money is a decimal string, never a float on the wire",
				usage.TotalCostUSD)
		}
		if usage.InputTokens == 0 || usage.OutputTokens == 0 {
			t.Errorf("token counts are missing: %+v", usage)
		}
		if usage.DurationMs == 0 {
			t.Error("durationMs is zero; it is the one number the backend can cross-check")
		}
	})

	t.Run("codex reports tokens", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, controlplane.RunRequest{Prompt: "hello", Agent: runv1.AgentCodex})
		h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
			if c.Path == "codex" && len(c.Args) > 0 && c.Args[0] == "exec" {
				time.Sleep(2 * time.Millisecond)
				c.Stdout.Write([]byte(`{"type":"agent_message","session_id":"s1","message":"done"}` + "\n"))
				c.Stdout.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":900,"output_tokens":120,"cached_input_tokens":40}}` + "\n"))
			}
			return entrypoint.CommandResult{}, nil
		}

		run, code := h.executeRun()
		if code != runv1.ExitSuccess {
			t.Fatalf("exit %d (%v)", code, run.Failure())
		}
		usage := run.Report().Usage
		if usage == nil || usage.InputTokens != 900 || usage.OutputTokens != 120 {
			t.Fatalf("token counts are wrong: %+v", usage)
		}
		// The fields are present for both, which is the whole point of
		// normalising: a backend that branched on the runtime to read a usage
		// record would grow that branch in four places.
		if usage.DurationMs == 0 {
			t.Error("durationMs is zero for codex")
		}
	})
}

// --- git --------------------------------------------------------------------

func TestARunWithoutARepositorySkipsTheGitPhasesAndSucceeds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "analyse this for me"})
	h.commander.handle = agentScript(h, "the analysis", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d, want 0: a run without a repository is a legal case (%v)",
			code, run.Failure())
	}
	for _, p := range []runv1.RuntimePhase{
		runv1.RuntimePhaseClone, runv1.RuntimePhaseCommit,
		runv1.RuntimePhasePush, runv1.RuntimePhasePR,
	} {
		if got := h.phase(run, p); got != runv1.PhaseOutcomeSkipped {
			t.Errorf("phase %s is %s, want skipped", p, got)
		}
	}
	if h.commander.called("git") {
		t.Error("git was run for a run with no repository")
	}
	if run.Report().Repo != nil {
		t.Error("the report carries a repo result for a run that has no repository")
	}
}

func TestTheTokenDoesNotReachGitConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "change something", RepoURL: "https://forge.invalid/org/repo.git",
		GitProvider: runv1.GitProviderGitHub, BaseBranch: "main",
		TargetBranch: "haliphron/abc-change",
		Secrets:      map[string]string{runv1.SecretKeyGitToken: "ghs_supersecrettoken_1234567890"},
	})
	h.commander.handle = gitWithChanges(agentScript(h, "done", ""))

	if code := h.execute(); code != runv1.ExitSuccess {
		t.Fatalf("exit %d", code)
	}
	// Cloning through a URL with the token in it leaves it in the repository's
	// configuration, which the agent will read and may send wherever it likes.
	for _, call := range h.commander.invocations("git") {
		for _, arg := range call.Args {
			if strings.Contains(arg, "ghs_supersecrettoken_1234567890") {
				t.Fatalf("the token is on git's command line: %v", call.Args)
			}
		}
		for _, pair := range call.Env {
			if strings.HasPrefix(pair, "GIT_CONFIG") && strings.Contains(pair, "ghs_") {
				t.Fatalf("the token is in git's configuration environment: %s", pair)
			}
		}
	}
}

func TestAnExistingPullRequestIsUpdatedRatherThanRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt: "change something", RepoURL: "https://forge.invalid/org/repo.git",
		GitProvider: runv1.GitProviderGitHub, BaseBranch: "main",
		TargetBranch: "haliphron/abc-change", CreatePR: true,
	})
	h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "gh" && len(c.Args) > 1 && c.Args[1] == "list" {
			c.Stdout.Write([]byte(`[{"url":"https://forge.invalid/org/repo/pull/7","number":7}]`))
			return entrypoint.CommandResult{}, nil
		}
		if c.Path == "gh" && len(c.Args) > 1 && c.Args[1] == "create" {
			t.Error("a second pull request was opened for a branch that already has one")
		}
		return agentScript(h, "done", "")(c)
	})

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}
	repo := run.Report().Repo
	if repo == nil || repo.PRAction != runv1.PRActionUpdated {
		t.Fatalf("prAction is %+v, want updated: create-or-update is one of the "+
			"converging side effects that make a retry safe", repo)
	}
	if repo.PRNumber != 7 {
		t.Errorf("prNumber %d, want 7", repo.PRNumber)
	}
}

// --- redaction --------------------------------------------------------------

func TestSecretsDoNotReachStorage(t *testing.T) {
	t.Parallel()
	const token = "ghs_averyrecognisablesecret_9876543210"
	const key = "sk-ant-averyrecognisablemodelkey-123456"

	h := newHarness(t, controlplane.RunRequest{
		Prompt: "print your environment",
		Secrets: map[string]string{
			runv1.SecretKeyGitToken:  token,
			runv1.SecretKeyLLMAPIKey: key,
		},
		RepoURL: "https://forge.invalid/org/repo.git", GitProvider: runv1.GitProviderGitHub,
		BaseBranch: "main", TargetBranch: "haliphron/abc-change",
		LogChunkSeconds: 1,
	})
	// The normal way a secret reaches a log: not malice, but a model that
	// printed what it found.
	h.commander.handle = gitWithChanges(func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			c.Stdout.Write([]byte("here is the environment:\nGH_TOKEN=" + token + "\nANTHROPIC_API_KEY=" + key + "\n"))
			c.Stdout.Write([]byte(claudeSummary("I found these: " + token)))
			return entrypoint.CommandResult{}, nil
		}
		return entrypoint.CommandResult{}, nil
	})

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}

	for _, secret := range []string{token, key, h.prepared.CallbackToken} {
		for _, key := range h.cp.RunKeys(h.prepared.RunID) {
			body, _ := h.cp.Object(key)
			if bytes.Contains(body, []byte(secret)) {
				t.Errorf("%s contains a secret; it is now durable for thirty days", key)
			}
		}
		if strings.Contains(run.Report().Summary, secret) {
			t.Errorf("the report's summary contains a secret")
		}
	}
	if !containsAny(h.chunkBodies(), entrypoint.Mask) {
		t.Error("nothing was masked at all, which means the filter did not run")
	}
}

// --- phases and timings -----------------------------------------------------

func TestEveryPhaseIsAccountedForExactlyOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{Prompt: "hello"})
	h.commander.handle = agentScript(h, "done", "")

	run, code := h.executeRun()
	if code != runv1.ExitSuccess {
		t.Fatalf("exit %d (%v)", code, run.Failure())
	}

	// phaseTimings is the only way to see that the time went into cloning a
	// monorepo rather than into the model, and the UI groups a run's timeline
	// by these names.
	timings := run.Timings()
	if len(timings) != len(runv1.RuntimePhases) {
		t.Fatalf("%d timings for %d phases", len(timings), len(runv1.RuntimePhases))
	}
	for i, p := range runv1.RuntimePhases {
		if timings[i].Phase != p {
			t.Fatalf("timing %d is %s, want %s: the order is contract, because the "+
				"resume rule is meaningless against a sequence that can be reordered",
				i, timings[i].Phase, p)
		}
		if timings[i].Outcome == "" {
			t.Errorf("phase %s has no outcome; a phase missing from the timings and "+
				"one marked skipped are different messages", p)
		}
	}
}

func TestAConfigFailureStillReportsItselfToTheController(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt:       "hello",
		PromptSHA256: strings.Repeat("cd", 32),
	})
	h.commander.handle = agentScript(h, "", "")

	run, code := h.executeRun()
	if code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d", code, runv1.ExitConfig)
	}
	report, ok := h.cp.AcceptedReport(h.prepared.RunID)
	if !ok {
		t.Fatal("a run that failed at fetch told the controller nothing; the " +
			"explanation is the entire product of that run")
	}
	if report.FailedPhase != runv1.RuntimePhaseFetch {
		t.Errorf("failedPhase %q, want fetch", report.FailedPhase)
	}
	if report.Reason != "PromptDigestMismatch" {
		t.Errorf("reason %q", report.Reason)
	}
	if report.Status != runv1.CompletionFailure {
		t.Errorf("status %q, want failure", report.Status)
	}
	if report.FailureClass != runv1.FailureConfig {
		t.Errorf("failureClass %q, want config", report.FailureClass)
	}
	// The git phases never ran, so claiming they were skipped rather than
	// silently omitting them is what tells the reader the run stopped early.
	if got := h.phase(run, runv1.RuntimePhaseClone); got != runv1.PhaseOutcomeSkipped {
		t.Errorf("the clone phase is %s, want skipped", got)
	}
}

func TestMCPServersThatDidNotComeUpStopTheRunBeforeTheModel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt:  "use your tools",
		Secrets: map[string]string{runv1.SecretKeyMCPConfig: `{"mcpServers":{"jira":{"command":"jira-mcp"}}}`},
	})
	h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "mcp" {
			c.Stdout.Write([]byte("jira: failed to connect\n"))
			return entrypoint.CommandResult{ExitCode: 1}, nil
		}
		if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
			t.Error("the model was started without the tools it was promised")
		}
		return entrypoint.CommandResult{}, nil
	}

	run, code := h.executeRun()
	if code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitConfig, run.Failure())
	}
	if run.Failure().Reason != "MCPServersUnavailable" {
		t.Fatalf("reason %q, want MCPServersUnavailable", run.Failure().Reason)
	}
}

func TestAGitLabRunWithAGitHubMCPIsRefusedExplicitly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, controlplane.RunRequest{
		Prompt:      "open a merge request",
		RepoURL:     "https://gitlab.invalid/org/repo.git",
		GitProvider: runv1.GitProviderGitLab,
		BaseBranch:  "main", TargetBranch: "haliphron/abc-change",
		Secrets: map[string]string{
			runv1.SecretKeyMCPConfig: `{"mcpServers":{"github":{"command":"github-mcp-server"}}}`,
		},
	})

	run, code := h.executeRun()
	if code != runv1.ExitConfig {
		t.Fatalf("exit %d, want %d (%v)", code, runv1.ExitConfig, run.Failure())
	}
	// An incompatible combination is rejected explicitly rather than discovered
	// through a baffled agent.
	if !strings.Contains(run.Failure().Message(), "github") {
		t.Fatalf("the message does not name the offending server: %s", run.Failure().Message())
	}
}

// --- the agent as an adversary ----------------------------------------------

func TestTheAgentCannotExfiltrateSecretsThroughItsOwnOutput(t *testing.T) {
	t.Parallel()

	// The pod is the least trusted component in the system: it runs text that
	// came from outside, with tools that text selected. Two of the files it
	// writes are read by the entrypoint and uploaded, and both live in a
	// directory the agent controls — so both are one symlink away from being an
	// exfiltration channel. presigned.json is the sharpest case, because it is
	// a JSON object and would sail through every check that only asks whether
	// the output parses.
	tests := []struct {
		name string
		link func(h *harness) string
	}{
		{
			name: "output.json points at the secret mount",
			link: func(h *harness) string { return h.layout.Output },
		},
		{
			name: "an artifact points at the secret mount",
			link: func(h *harness) string {
				return filepath.Join(h.layout.Artifacts, "notes.json")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, controlplane.RunRequest{Prompt: "read your own secrets"})
			target := filepath.Join(h.layout.Secrets, runv1.SecretKeyPresigned)

			h.commander.handle = func(c entrypoint.Command) (entrypoint.CommandResult, error) {
				if c.Path == "claude" && len(c.Args) > 0 && c.Args[0] == "-p" {
					path := tc.link(h)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Errorf("preparing %s: %v", path, err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Errorf("linking %s: %v", path, err)
					}
					time.Sleep(2 * time.Millisecond)
					c.Stdout.Write([]byte(claudeSummary("have a look at my output")))
				}
				return entrypoint.CommandResult{}, nil
			}

			// The run succeeds: no node schema was declared, so an unreadable
			// output file is not an error. What must not happen is the link
			// being followed.
			run, code := h.executeRun()
			if code != runv1.ExitSuccess {
				t.Fatalf("exit %d (%v)", code, run.Failure())
			}

			// The signatures are in the redactor, so even a leak would be
			// masked — which is why the assertion is on the whole bundle's
			// distinctive structure rather than on one value.
			for _, key := range h.cp.RunKeys(h.prepared.RunID) {
				body, _ := h.cp.Object(key)
				if bytes.Contains(body, []byte(`"keyPrefix"`)) {
					t.Fatalf("%s carries the presigned bundle: the symlink was followed", key)
				}
			}
			if string(h.envelope().Data) != "{}" {
				t.Fatalf("data is %s, want {}", h.envelope().Data)
			}
		})
	}
}
