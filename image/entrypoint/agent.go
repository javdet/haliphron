package entrypoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Two runtimes, one image.
//
// Not two images: git, pull requests, uploads, notification, the checkpoint and
// log redaction are the same work either way. Four things differ —
// authentication, preparing the MCP configuration, the launch command and the
// output parser — and this file is those four things.
//
// The trap is asymmetry, and it is a security hole rather than a cosmetic
// discrepancy. codex has no analogue of --allowedTools for its built-in tools:
// file and shell access are set wholesale by the sandbox mode. A "read-only"
// role is plan mode plus a deny list on claude-code and -s read-only on codex,
// and if the translation is wrong the same role grants different powers on the
// two runtimes. The table in the contract is normative and the equivalence is
// a test, not an intention.

// Runtime is the per-CLI half of the work.
type Runtime interface {
	// Name is the agent type this runtime serves.
	Name() runv1.AgentType
	// Env is the environment for the agent's child process — built, not
	// inherited. The model key goes in under the name this CLI expects, and
	// the entrypoint's own credentials do not go in at all.
	Env(r *Run) []string
	// PrepareMCP renders the MCP configuration for this CLI, with header values
	// referenced rather than inlined, and returns the values that must reach
	// the child's environment for those references to resolve.
	PrepareMCP(r *Run) (env []string, err error)
	// VerifyMCP is the command that proves the servers came up, or false when
	// this run declared none.
	VerifyMCP(r *Run) (Command, bool)
	// Launch is the agent CLI itself.
	Launch(r *Run) Command
	// Parse normalises this CLI's own output format into text and usage.
	Parse(stdout []byte) AgentResult
}

// AgentResult is what the model run produced, normalised.
//
// Normalised is the point: claude-code reports a cost and codex reports token
// counts, and a backend that had to branch on the runtime to read a usage
// record would grow that branch in four places.
type AgentResult struct {
	Text      string
	SessionID string
	ExitCode  int32
	Usage     *runv1.Usage
}

// runtimeFor picks the implementation. The configuration has already been
// checked, so an unknown agent here is a bug rather than an input.
func runtimeFor(agent runv1.AgentType) (Runtime, error) {
	switch agent {
	case runv1.AgentClaudeCode:
		return claudeCode{}, nil
	case runv1.AgentCodex:
		return codex{}, nil
	default:
		return nil, fail(runv1.ExitConfig, "UnknownAgent", "no runtime for %q", agent)
	}
}

// phaseAuth wires the model credential and the git credential helper.
//
// The token never goes into .git/config. Cloning through a URL with the token
// in it leaves it in the repository's configuration, which the agent will read
// and may send wherever it sees fit; a credential helper bound to the host does
// the same job and leaves nothing behind.
func phaseAuth(_ context.Context, r *Run) error {
	rt, err := runtimeFor(r.cfg.Agent)
	if err != nil {
		return err
	}
	// What the agent's child process gets, and the whole of it. Not the git
	// token, not the callback token, not one presigned link: the entrypoint
	// handles those before and after the run, and the agent has a shell.
	r.agentEnv = rt.Env(r)

	if !r.cfg.HasRepo() {
		r.logf("model credential wired for %s; no repository, so no git credential", r.cfg.Agent)
		return nil
	}
	if err := r.writeGitCredentialHelper(); err != nil {
		return err
	}
	r.logf("model credential wired for %s; git credential helper at %s",
		r.cfg.Agent, r.gitCredentialHelperPath())
	return nil
}

func (r *Run) gitCredentialHelperPath() string {
	return filepath.Join(r.layout.RunPrivate, "git-credential-haliphron")
}

// writeGitCredentialHelper writes a helper that answers git's credential
// protocol from the mounted secret.
//
// It reads the token from the file at call time rather than embedding it: the
// script lives in the entrypoint's private directory, but a script with a token
// in it is one `cat` away from a log, and the file it reads is 0400 and mounted
// read-only anyway.
func (r *Run) writeGitCredentialHelper() error {
	tokenPath := filepath.Join(r.layout.Secrets, runv1.SecretKeyGitToken)
	script := "#!/bin/sh\n" +
		"# Answers git's credential protocol for haliphron runs. Only 'get' is\n" +
		"# implemented: there is nothing to store and nothing to erase, because the\n" +
		"# credential lives in a read-only mount for the lifetime of the pod.\n" +
		"[ \"$1\" = get ] || exit 0\n" +
		"printf 'username=x-access-token\\n'\n" +
		"printf 'password=%s\\n' \"$(cat '" + tokenPath + "')\"\n"

	path := r.gitCredentialHelperPath()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "writing %s", path)
	}
	return nil
}

// phaseMCPPrepare renders the MCP configuration for the chosen runtime.
//
// Header values are not written into the file. They are exported into the child
// process's environment and referenced from the configuration, because the file
// outlives the pod in log chunks and in dumps while the child's environment
// does not.
func phaseMCPPrepare(_ context.Context, r *Run) error {
	if !r.hasMCPServers() {
		return skip("this run declared no MCP servers")
	}
	rt, err := runtimeFor(r.cfg.Agent)
	if err != nil {
		return err
	}
	env, err := rt.PrepareMCP(r)
	if err != nil {
		return err
	}
	r.agentEnv = append(r.agentEnv, env...)

	// Values discovered here were not known when the redactor was built.
	for _, pair := range env {
		if _, value, ok := strings.Cut(pair, "="); ok {
			r.redactor.Add(value)
		}
	}
	r.logf("MCP configuration rendered for %s with %d server(s)", r.cfg.Agent, r.mcpServerCount())
	return nil
}

// phaseMCPVerify proves the servers came up before the model is started.
//
// This is the row of the security table that deserves to be said plainly: an
// agent that did not get the tools it was promised does not fail. It cheerfully
// does something else, and produces a plausible invented result — which costs
// more than an explicit refusal, because the failure is visible immediately and
// the invention only at review, if you are lucky.
func phaseMCPVerify(ctx context.Context, r *Run) error {
	if !r.hasMCPServers() {
		return skip("this run declared no MCP servers")
	}
	rt, err := runtimeFor(r.cfg.Agent)
	if err != nil {
		return err
	}
	cmd, ok := rt.VerifyMCP(r)
	if !ok {
		return skip("this runtime offers no way to check the servers")
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	result, err := r.commander.Run(ctx, cmd)
	if err != nil {
		return failWrap(runv1.ExitConfig, "MCPServersUnavailable", err,
			"could not check the MCP servers")
	}
	r.logf("mcp check: %s", strings.TrimSpace(r.redactor.String(out.String())))
	if result.ExitCode != 0 {
		return fail(runv1.ExitConfig, "MCPServersUnavailable",
			"the MCP servers did not come up (%s exited %d); refusing to start an agent "+
				"that would silently do something else", cmd.Path, result.ExitCode)
	}
	return nil
}

func (r *Run) mcpServers() map[string]json.RawMessage {
	if len(r.secrets.MCPConfig) == 0 {
		return nil
	}
	var cfg struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(r.secrets.MCPConfig, &cfg); err != nil {
		return nil
	}
	return cfg.Servers
}

func (r *Run) hasMCPServers() bool { return len(r.mcpServers()) > 0 }
func (r *Run) mcpServerCount() int { return len(r.mcpServers()) }
func (r *Run) mcpConfigPath() string {
	return filepath.Join(r.layout.RunPrivate, "mcp.json")
}

// agentGraceOnTimeout is how long the agent gets between SIGTERM and SIGKILL
// when its budget runs out. The run is not cut off dry: forty minutes of work
// that did not fit into an hour cost the same as work that did, and there is no
// reason to throw them away.
const agentGraceOnTimeout = 30 * time.Second

// phaseRun is the agent CLI, under its own budget.
//
// HALIPHRON_TIMEOUT_SECONDS is the budget for this phase and not for the pod:
// cloning a monorepo and uploading a two-gigabyte log must not eat into the
// time allotted to the model. The Job's activeDeadlineSeconds is deliberately
// larger, so that the normal timeout still has time to upload what was made.
func phaseRun(ctx context.Context, r *Run) error {
	if r.resumed {
		return skip("attempt %d already completed this phase; the model is not called twice",
			r.cfg.Attempt-1)
	}
	rt, err := runtimeFor(r.cfg.Agent)
	if err != nil {
		return err
	}

	cmd := rt.Launch(r)
	// Tee'd: the log gets everything as it happens and the parser gets the same
	// bytes at the end. Two readers, one stream, no second chance to collect it.
	//
	// The capture is synchronised even though Commander promises that nothing
	// is still writing when Run returns. The promise is easy to keep for a real
	// process and easy to break for anything else, and the cost of breaking it
	// here is a torn read of the one buffer holding a paid-for result.
	captured := &syncBuffer{}
	cmd.Stdout = multiWriter(captured, r.log)
	cmd.Stderr = r.log
	cmd.GraceOnCancel = agentGraceOnTimeout

	budget, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	started := r.clock()
	r.logf("starting %s with a budget of %s", cmd.Path, r.cfg.Timeout)
	result, runErr := r.commander.Run(budget, cmd)
	elapsed := r.clock().Sub(started)

	r.agent = rt.Parse(captured.Bytes())
	r.agent.ExitCode = result.ExitCode
	if r.agent.Usage == nil {
		r.agent.Usage = &runv1.Usage{}
	}
	// Measured by the entrypoint rather than taken from the CLI. It is the one
	// number in the usage record the backend can cross-check against the Job
	// duration the controller observed, and a self-reported one would be worth
	// nothing for exactly that purpose.
	r.agent.Usage.DurationMs = elapsed.Milliseconds()
	if r.agent.SessionID != "" {
		r.agent.Usage.SessionID = r.agent.SessionID
	}
	switch {
	case errors.Is(budget.Err(), context.DeadlineExceeded):
		// Exit 11, and the pipeline goes on. parse, output, persist, commit,
		// push and pr all still run; what the agent managed in its budget is
		// worth exactly as much as what it would have managed in twice it.
		return fail(runv1.ExitAgentTimeout, "AgentTimeout",
			"the agent did not finish within %s and was stopped; the partial result is kept",
			r.cfg.Timeout)
	case runErr != nil:
		return failWrap(runv1.ExitAgentError, "AgentNotStarted", runErr,
			"could not start %s", cmd.Path)
	case result.ExitCode != 0:
		return fail(runv1.ExitAgentError, "AgentFailed",
			"%s exited %d after %s", cmd.Path, result.ExitCode, elapsed.Round(time.Second))
	}

	r.logf("agent finished in %s", elapsed.Round(time.Second))
	return nil
}

// phaseParse normalises what the runtime produced into the result text and a
// usage record. The work happens inside phaseRun, where the bytes are; this
// phase exists in its own right because the contract names it, the checkpoint
// is keyed by it, and a run whose time went into parsing a gigabyte of JSON
// should say so in its timings.
func phaseParse(_ context.Context, r *Run) error {
	if r.resumed {
		return skip("an earlier attempt already produced and stored the result")
	}
	r.summary = ResultMarkdown(r.cfg, r.agent.Text, r.exitCodeSoFar(), r.failure, r.timings)
	if r.agent.Usage != nil {
		r.logf("usage: %s turns, cost %s, tokens in %d out %d",
			strconv.Itoa(int(r.agent.Usage.NumTurns)), orDash(string(r.agent.Usage.TotalCostUSD)),
			r.agent.Usage.InputTokens, r.agent.Usage.OutputTokens)
	}
	return nil
}

// exitCodeSoFar is the code the run would exit with if it stopped here. The
// envelope's status derives from it, and the status must say partial for a
// timeout that nevertheless produced work.
func (r *Run) exitCodeSoFar() int32 {
	if r.failure != nil {
		return r.failure.Code
	}
	return runv1.ExitSuccess
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// syncBuffer is a bytes.Buffer that survives being written from one goroutine
// and read from another.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// multiWriter is io.MultiWriter without the import, and with one behavioural
// difference that matters here: a failing writer does not stop the others. The
// log sink never fails, but if it did, losing the log must not also lose the
// bytes the parser needs.
func multiWriter(writers ...interface{ Write([]byte) (int, error) }) *teeWriter {
	return &teeWriter{writers: writers}
}

type teeWriter struct {
	writers []interface{ Write([]byte) (int, error) }
}

func (t *teeWriter) Write(p []byte) (int, error) {
	for _, w := range t.writers {
		_, _ = w.Write(p)
	}
	return len(p), nil
}
