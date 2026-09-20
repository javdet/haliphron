package image_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/fake/controlplane"
	"github.com/automagicops/haliphron/image/entrypoint"
)

// The harness for the image's contract tests: FakeControlPlane on a loopback
// listener, the pod's two mounts on disk, and a scripted stand-in for the
// external commands.
//
// The contract's checklist verifies the image "without a cluster and without a
// backend". This is that, one level in: `docker run` exercises the same code
// with a real agent CLI and real git, and these tests exercise every phase
// boundary, every exit code and every branch of the resume rule in under a
// second. The two are complements — the checklist rows about read-only roots
// and non-root users belong to `docker run` and are not reachable from here.

// harness is one prepared run and everything needed to execute it.
type harness struct {
	t         *testing.T
	cp        *controlplane.ControlPlane
	prepared  *controlplane.Prepared
	layout    entrypoint.Layout
	commander *scriptedCommander
	clock     func() time.Time
}

// harnessOption adjusts the harness before the run is built.
type harnessOption func(*harness)

// withCommands replaces the default script for external commands.
func withCommands(handle func(entrypoint.Command) (entrypoint.CommandResult, error)) harnessOption {
	return func(h *harness) { h.commander.handle = handle }
}

// artifactMode is the package-level switch the whole suite runs under, set by
// TestMain. It is a variable rather than a harness option because the point is
// to run *every* row against both halves of the port: an option would mean
// choosing, per test, which mode the row belongs to, and the rows do not divide
// that way — "a refused upload is infra, not config" is true in both and worth
// checking in both.
var artifactMode = runv1.ArtifactModeRelay

// TestMain runs the checklist twice, once per artifact mode.
//
// The image implements both and an installation runs one of them. Relay is the
// default and is what an installation gets with nothing configured, so a suite
// that exercised only the optimisation would let the default path ship
// untested — and object-store mode has failure modes relay does not, which is
// the other half of the argument.
func TestMain(m *testing.M) {
	for _, mode := range []runv1.ArtifactMode{
		runv1.ArtifactModeRelay, runv1.ArtifactModeObjectStore,
	} {
		artifactMode = mode
		fmt.Fprintf(os.Stderr, "=== artifact mode: %s ===\n", mode)
		if code := m.Run(); code != 0 {
			os.Exit(code)
		}
	}
	os.Exit(0)
}

func newHarness(t *testing.T, req controlplane.RunRequest, opts ...harnessOption) *harness {
	t.Helper()

	cp, srv := controlplane.NewServer(controlplane.WithArtifactMode(artifactMode))
	t.Cleanup(srv.Close)

	h := &harness{
		t:         t,
		cp:        cp,
		prepared:  cp.Prepare(req),
		commander: &scriptedCommander{},
		clock:     time.Now,
	}
	for _, opt := range opts {
		opt(h)
	}
	h.stage(h.prepared)
	return h
}

// stage writes the run's two mounts to disk and builds the layout that points
// at them. Called again for a second attempt, which re-materialises the
// reissued bundle over the first one's.
func (h *harness) stage(p *controlplane.Prepared) {
	h.t.Helper()
	root := h.t.TempDir()
	mounts, err := p.Materialize(filepath.Join(root, "mounts"))
	// Registered before the error check and after t.TempDir, so that a partial
	// layout is still cleaned and the removal runs before t.TempDir's own: the
	// secrets directory is 0500 and RemoveAll cannot get into it as a normal
	// user, which is every user outside the CI container.
	h.t.Cleanup(func() {
		if err := mounts.Remove(); err != nil {
			h.t.Errorf("removing the mounts: %v", err)
		}
	})
	if err != nil {
		h.t.Fatalf("materialize: %v", err)
	}
	h.prepared = p
	h.layout = entrypoint.LayoutUnder(root)
	h.layout.Secrets = mounts.SecretsDir
	h.layout.RoleConfig = mounts.RoleDir
}

// execute runs the whole pipeline and returns the exit code.
func (h *harness) execute() int32 {
	h.t.Helper()
	run := h.build()
	return run.Execute(context.Background())
}

// executeRun runs the pipeline and hands back the Run, for a test that wants to
// look at the report or the timings rather than only the code.
func (h *harness) executeRun() (*entrypoint.Run, int32) {
	h.t.Helper()
	run := h.build()
	return run, run.Execute(context.Background())
}

func (h *harness) build() *entrypoint.Run {
	h.t.Helper()
	cfg, err := entrypoint.LoadConfig(h.env)
	if err != nil {
		h.t.Fatalf("the harness produced a configuration the entrypoint rejects: %v", err)
	}
	secrets, err := entrypoint.LoadSecrets(h.layout.Secrets, cfg)
	if err != nil {
		h.t.Fatalf("the harness produced secrets the entrypoint rejects: %v", err)
	}
	run, err := entrypoint.New(cfg, secrets,
		entrypoint.WithLayout(h.layout),
		entrypoint.WithCommander(h.commander),
		entrypoint.WithClock(h.clock))
	if err != nil {
		h.t.Fatalf("building the run: %v", err)
	}
	return run
}

// loadFails executes only as far as configuration and secrets, and returns the
// failure. It is how the rows that must fail "before any network call" are
// checked: a run that never gets built cannot have made one.
func (h *harness) loadFails() *entrypoint.Failure {
	h.t.Helper()
	cfg, err := entrypoint.LoadConfig(h.env)
	if err == nil {
		_, err = entrypoint.LoadSecrets(h.layout.Secrets, cfg)
	}
	if err == nil {
		h.t.Fatal("expected loading to fail and it did not")
	}
	var f *entrypoint.Failure
	if !asFailure(err, &f) {
		h.t.Fatalf("the failure is not classified: %v", err)
	}
	return f
}

func (h *harness) env(name string) string { return h.prepared.Env[name] }

// runID is the prefix everything in storage hangs under.
func (h *harness) prefix() string { return h.prepared.Prefix }

func (h *harness) object(key string) ([]byte, bool) {
	return h.cp.Object(h.prefix() + key)
}

func (h *harness) mustObject(key string) []byte {
	h.t.Helper()
	body, ok := h.object(key)
	if !ok {
		h.t.Fatalf("%s is not in storage; the keys under this run are %v",
			key, h.cp.RunKeys(h.prepared.RunID))
	}
	return body
}

// envelope reads the structured output back.
func (h *harness) envelope() runv1.OutputEnvelope {
	h.t.Helper()
	var out runv1.OutputEnvelope
	if err := json.Unmarshal(h.mustObject(runv1.StorageKeyOutput), &out); err != nil {
		h.t.Fatalf("output.json does not parse: %v", err)
	}
	return out
}

// phase returns how one phase ended, from the timings the run recorded.
//
// The timings rather than the checkpoint, now that the checkpoint holds only
// what completed: "skipped" and "failed" are outcomes a test asks about and the
// checkpoint deliberately does not record, because a later attempt may not
// assume a skipped phase was done.
func (h *harness) phase(run *entrypoint.Run, p runv1.RuntimePhase) runv1.PhaseOutcome {
	h.t.Helper()
	for _, t := range run.Timings() {
		if t.Phase == p {
			return t.Outcome
		}
	}
	h.t.Fatalf("phase %s is missing from the timings entirely", p)
	return ""
}

// writeOutput puts a payload where the agent would have left it.
func (h *harness) writeOutput(body string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.layout.Output), 0o755); err != nil {
		h.t.Fatalf("preparing the run-io directory: %v", err)
	}
	if err := os.WriteFile(h.layout.Output, []byte(body), 0o644); err != nil {
		h.t.Fatalf("writing the agent's output: %v", err)
	}
}

// ---------------------------------------------------------------------------

// scriptedCommander stands in for git, the forge CLI and the agent CLI. Every
// call is recorded, because "the model was never called" is an assertion these
// tests make constantly and it is not checkable any other way.
type scriptedCommander struct {
	mu     sync.Mutex
	calls  []entrypoint.Command
	stops  map[string]chan struct{}
	handle func(entrypoint.Command) (entrypoint.CommandResult, error)
}

func (s *scriptedCommander) Run(ctx context.Context, c entrypoint.Command) (entrypoint.CommandResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	handle := s.handle
	s.mu.Unlock()

	if handle == nil {
		return entrypoint.CommandResult{}, nil
	}
	// The agent phase runs under its own deadline, and a scripted agent that
	// ignores the context cannot be made to time out — so the handler runs on
	// its own goroutine and the deadline is observed here.
	//
	// On cancellation this waits for the handler rather than abandoning it,
	// because Commander promises the caller that nothing is writing to Stdout
	// when Run returns. A real process is stopped by SIGTERM and then waited
	// for; a scripted one has no signal, so it is simply waited for.
	type outcome struct {
		result entrypoint.CommandResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := handle(c)
		done <- outcome{result, err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			return entrypoint.CommandResult{ExitCode: -1}, o.err
		}
		return o.result, nil
	case <-ctx.Done():
		s.cancel(c)
		<-done
		return entrypoint.CommandResult{ExitCode: 143}, nil
	}
}

// cancel asks a scripted command to stop. Handlers that can block register a
// stop channel through sleepUntilCancelled; the rest simply run to completion.
func (s *scriptedCommander) cancel(c entrypoint.Command) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.stops[c.Path]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
		delete(s.stops, c.Path)
	}
}

// sleepUntilCancelled blocks a scripted command the way a real agent blocks:
// until it is told to stop, or until a ceiling that keeps a broken test from
// hanging the suite.
func (s *scriptedCommander) sleepUntilCancelled(path string, ceiling time.Duration) {
	stop := make(chan struct{})
	s.mu.Lock()
	if s.stops == nil {
		s.stops = map[string]chan struct{}{}
	}
	s.stops[path] = stop
	s.mu.Unlock()

	select {
	case <-stop:
	case <-time.After(ceiling):
	}
}

// called reports whether a program was ever started.
func (s *scriptedCommander) called(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c.Path == path {
			return true
		}
	}
	return false
}

// invocations returns every call to a program.
func (s *scriptedCommander) invocations(path string) []entrypoint.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []entrypoint.Command
	for _, c := range s.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------

// agentScript builds a handler that behaves like a successful claude-code run:
// it prints the CLI's JSON summary and leaves a payload where the contract says
// the agent leaves one.
func agentScript(h *harness, text, payload string) func(entrypoint.Command) (entrypoint.CommandResult, error) {
	return func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		switch c.Path {
		case "claude", "codex":
			if len(c.Args) > 0 && (c.Args[0] == "mcp" || c.Args[0] == "--version") {
				fmt.Fprintln(c.Stdout, "1.2.3")
				return entrypoint.CommandResult{}, nil
			}
			if payload != "" {
				h.writeOutput(payload)
			}
			// The entrypoint measures the run rather than believing the CLI's
			// own figure, so a scripted agent that returns instantly reports a
			// duration of zero — correctly. Taking a moment is what makes the
			// measurement observable.
			time.Sleep(2 * time.Millisecond)
			fmt.Fprintln(c.Stdout, claudeSummary(text))
			return entrypoint.CommandResult{}, nil
		}
		return entrypoint.CommandResult{}, nil
	}
}

// claudeSummary is what `claude -p --output-format json` prints.
func claudeSummary(text string) string {
	body, _ := json.Marshal(map[string]any{
		"result":         text,
		"session_id":     "sess_01HZX",
		"num_turns":      7,
		"total_cost_usd": 0.4231,
		"duration_ms":    12000,
		"usage": map[string]any{
			"input_tokens":                120000,
			"output_tokens":               4300,
			"cache_read_input_tokens":     98000,
			"cache_creation_input_tokens": 1200,
		},
	})
	return string(body)
}

// gitWithChanges makes the git phases behave as though the agent edited files:
// a dirty work tree, a commit, and a diff against the base.
func gitWithChanges(inner func(entrypoint.Command) (entrypoint.CommandResult, error)) func(entrypoint.Command) (entrypoint.CommandResult, error) {
	return func(c entrypoint.Command) (entrypoint.CommandResult, error) {
		if c.Path != "git" {
			if inner != nil {
				return inner(c)
			}
			return entrypoint.CommandResult{}, nil
		}
		switch sub := gitSubcommand(c.Args); sub {
		case "status":
			fmt.Fprintln(c.Stdout, " M src/main.go")
		case "rev-parse":
			fmt.Fprintln(c.Stdout, "7c3f1a9b2d4e5f60718293a4b5c6d7e8f9012345")
		case "diff":
			// Non-zero from --quiet means "there are differences", which is the
			// answer that lets the push phase run.
			return entrypoint.CommandResult{ExitCode: 1}, nil
		}
		if inner != nil {
			return inner(c)
		}
		return entrypoint.CommandResult{}, nil
	}
}

// gitSubcommand skips the -c options the entrypoint always passes.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}

func asFailure(err error, target **entrypoint.Failure) bool {
	if f, ok := err.(*entrypoint.Failure); ok {
		*target = f
		return true
	}
	return false
}

// chunkBodies returns the log chunks in key order.
func (h *harness) chunkBodies() []string {
	var out []string
	for _, key := range h.cp.Keys(h.prefix() + runv1.StoragePrefixChunks) {
		body, _ := h.cp.Object(key)
		out = append(out, string(body))
	}
	return out
}

func containsAny(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// removeSecret deletes one key from the materialised secret mount, as a Secret
// created without that key would.
func removeSecret(t *testing.T, h *harness, key string) {
	t.Helper()
	dir := h.layout.Secrets
	if err := chmodDir(dir, 0o700); err != nil {
		t.Fatalf("opening the secret mount: %v", err)
	}
	if err := removeFile(dir, key); err != nil {
		t.Fatalf("removing %s: %v", key, err)
	}
	if err := chmodDir(dir, 0o500); err != nil {
		t.Fatalf("closing the secret mount: %v", err)
	}
}

func chmodDir(dir string, mode os.FileMode) error { return os.Chmod(dir, mode) }

func removeFile(dir, name string) error { return os.Remove(filepath.Join(dir, name)) }
