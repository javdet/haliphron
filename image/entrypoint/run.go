package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Run is one attempt: everything the phases read from and write to.
//
// It is a struct rather than a set of globals because the contract tests run
// several attempts of several runs in one process, and because a phase that can
// reach a global is a phase whose inputs cannot be seen from its signature.
type Run struct {
	cfg     *Config
	secrets *Secrets
	layout  Layout

	// uploader is whichever half of the ArtifactStore port this run uses: the
	// relay through the controller, or presigned access to an object store.
	// Nothing below this line knows which, which is the point of the port.
	uploader  Uploader
	callback  *Callback
	redactor  *Redactor
	commander Commander
	clock     func() time.Time
	log       *LogSink
	http      *http.Client

	// checkpoint is what an earlier attempt got through and what this one has
	// reported. There is no "prior" any more: the checkpoint is not a document
	// this pod fetches and compares against, it is a list the controller handed
	// it in an environment variable.
	checkpoint *Checkpoint
	resumed    bool

	// outcomes is how each phase of *this* attempt ended, for the two phases
	// that ask about an earlier one — push asks whether commit was skipped. It
	// is separate from the checkpoint because the checkpoint answers "did any
	// attempt do this" and this answers "did this attempt do this", and
	// conflating them is how a resumed attempt decides it has nothing to push.
	outcomes map[runv1.RuntimePhase]runv1.PhaseOutcome

	prompt     []byte
	nodeSchema []byte
	// rolePrompt is the role's own system prompt, trimmed; empty when the role
	// has none. It follows the entrypoint's instruction and never replaces it.
	rolePrompt string
	// agentEnv is the environment handed to the agent's child process, built
	// rather than inherited. It carries the model key under the name that CLI
	// expects and the MCP header values, and nothing else: the git token, the
	// callback token and the presigned links are the entrypoint's business
	// before and after the run, and the agent needs none of them.
	agentEnv []string

	agent     AgentResult
	payload   *AgentPayload
	summary   string
	envelope  *runv1.OutputEnvelope
	artifacts []runv1.OutputArtifact

	resultRef *runv1.ObjectRef
	outputRef *runv1.ObjectRef
	logRef    *runv1.ObjectRef

	repo runv1.RepoResult

	timings   []runv1.PhaseTiming
	failure   *Failure
	cancelled bool

	report     *runv1.CompletionReport
	reportBody []byte
}

// Option adjusts a Run at construction. The three that exist are the three
// seams the contract tests need: the clock, the external commands and the
// filesystem layout.
type Option func(*Run)

// WithLayout puts the tree somewhere other than the image's root.
func WithLayout(l Layout) Option { return func(r *Run) { r.layout = l } }

// WithClock replaces time.Now.
func WithClock(clock func() time.Time) Option { return func(r *Run) { r.clock = clock } }

// WithCommander replaces the external-process runner, so that a test can drive
// the phases without git and without a model CLI on the machine.
func WithCommander(c Commander) Option { return func(r *Run) { r.commander = c } }

// New builds a run from the config and secrets the pod was given.
func New(cfg *Config, secrets *Secrets, opts ...Option) (*Run, error) {
	r := &Run{
		cfg:       cfg,
		secrets:   secrets,
		layout:    DefaultLayout(),
		clock:     time.Now,
		commander: ExecCommander{},
		http:      &http.Client{Timeout: 30 * time.Second},
		outcomes:  map[runv1.RuntimePhase]runv1.PhaseOutcome{},
	}
	for _, opt := range opts {
		opt(r)
	}

	// The redactor is built before anything can be written, from every secret
	// the pod holds. Adding values later is allowed — the MCP phase discovers
	// header values — but starting without it would leave the first phases
	// uncovered, and the first phases are the ones that print bundles.
	r.redactor = NewRedactor(secrets.Values()...)
	r.callback = NewCallback(cfg, secrets, r.redactor, r.logf)

	uploader, err := NewUploader(cfg, secrets, r.redactor, r.callback)
	if err != nil {
		return nil, err
	}
	r.uploader = uploader
	r.checkpoint = NewCheckpoint(cfg, r.callback, r.clock)
	return r, nil
}

// outcome is how a phase of this attempt ended.
func (r *Run) outcome(phase runv1.RuntimePhase) runv1.PhaseOutcome { return r.outcomes[phase] }

// Uploader is the artifact path this run took, for a test that wants to assert
// on which one it was.
func (r *Run) Uploader() Uploader { return r.uploader }

// Layout is where this run's files are.
func (r *Run) Layout() Layout { return r.layout }

// Checkpoint is the state this attempt has accumulated.
func (r *Run) Checkpoint() *Checkpoint { return r.checkpoint }

// Report is the completion report, available after the pipeline has run.
func (r *Run) Report() *runv1.CompletionReport { return r.report }

// Failure is what went wrong, or nil.
func (r *Run) Failure() *Failure { return r.failure }

// Timings are the phase durations and outcomes, in execution order.
func (r *Run) Timings() []runv1.PhaseTiming { return r.timings }

// Log is the rolling log, for a test that wants to read what the run said.
func (r *Run) Log() *LogSink { return r.log }

// logf narrates. Before the log sink exists — which is the first half of the
// init phase — it goes to stderr, which is where a pod that dies in its first
// second leaves its only evidence.
func (r *Run) logf(format string, args ...any) {
	if r.log != nil {
		r.log.Printf(format, args...)
		return
	}
	_, _ = os.Stderr.WriteString(r.redactor.String(sprintf(format, args...)) + "\n")
}

// Cancel marks the run as stopped from outside — a cancellation, an eviction, a
// drain. It is what makes the report say cancelled when the exit code says only
// "signal 143", which every one of those produces.
func (r *Run) Cancel() { r.cancelled = true }

// Command is one external program, with its environment built rather than
// inherited.
type Command struct {
	Path   string
	Args   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// GraceOnCancel is how long the process gets between SIGTERM and SIGKILL
	// when its context is cancelled. Zero means kill at once, which is right
	// for a version check and wrong for an agent that has spent forty minutes
	// and still has a partial result in its buffers.
	GraceOnCancel time.Duration
}

// CommandResult is what it exited with. A non-zero code is not an error: git
// answers 1 to "nothing to commit", and a caller that had to inspect an error
// string to learn that would get it wrong.
type CommandResult struct {
	ExitCode int32
}

// Commander runs external programs. An interface, because the git phases and
// the agent phase are the two places this package cannot test against the real
// thing in a unit test, and because "what exactly did the entrypoint ask git to
// do" is a question a test should be able to answer.
//
// One obligation, and it is not obvious from the signature: when Run returns,
// nothing may still be writing to c.Stdout or c.Stderr. The caller reads those
// writers immediately, including on the cancellation path, and an
// implementation that abandons a still-running child on ctx.Done hands the
// parser a buffer that is being written underneath it. exec.Cmd.Run keeps this
// promise; anything hand-rolled has to be made to.
type Commander interface {
	Run(ctx context.Context, c Command) (CommandResult, error)
}

// ExecCommander is the real one.
type ExecCommander struct{}

// Run executes the command. Only a failure to start is an error; a non-zero
// exit is a result.
func (ExecCommander) Run(ctx context.Context, c Command) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr

	if c.GraceOnCancel > 0 {
		// The default for CommandContext is SIGKILL on cancellation, which
		// throws away whatever the agent had not yet flushed. SIGTERM first,
		// then the grace period, then Go sends the kill itself.
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = c.GraceOnCancel
	}

	err := cmd.Run()
	if err == nil {
		return CommandResult{}, nil
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		return CommandResult{ExitCode: int32(exitErr.ExitCode())}, nil
	}
	return CommandResult{ExitCode: -1}, err
}

// asExitError is errors.As specialised to the one type this file cares about.
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

// sprintf is fmt.Sprintf under a shorter name, used by logf and by the skip
// helper in runner.go.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
