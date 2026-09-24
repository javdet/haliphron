package entrypoint

import (
	"context"
	"errors"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The nineteen phases, in the order the contract fixes.
//
// The order is contract rather than implementation detail, for a reason that
// only shows up on a retry: the checkpoint's resume rule is "every phase before
// the first unfinished one is done", which is meaningless against a sequence
// that can be reordered. The names are contract too — run_attempts.completed_phases
// holds them, phaseTimings enumerates them, and the UI groups a run's timeline
// by them.

// step is one phase and what it costs when it fails without saying so.
type step struct {
	phase runv1.RuntimePhase
	fn    func(context.Context, *Run) error
	// defaultCode classifies an error that arrived unclassified. Every phase
	// has one, so that a bug in this package produces a wrong-but-honest exit
	// code rather than a zero that the controller reads as success.
	defaultCode int32
	// epilogue marks a phase that runs even after the work has been abandoned.
	// Between them they produce the three things a failed run still owes
	// somebody: a result object under the fixed key, a durable copy of the
	// report, and the report to the controller.
	epilogue bool
}

// errSkip is how a phase says it had nothing to do. Distinct from success,
// because "the four git phases are skipped" and "the four git phases succeeded"
// are different messages and the second one is a lie.
var errSkip = errors.New("phase skipped")

// skipf records why a phase was skipped. The reason reaches the checkpoint and
// the timings; a skip nobody can explain is indistinguishable from a bug.
type skipped struct{ reason string }

func (s *skipped) Error() string { return "skipped: " + s.reason }
func (s *skipped) Unwrap() error { return errSkip }

func skip(format string, args ...any) error {
	return &skipped{reason: sprintf(format, args...)}
}

// steps is the pipeline.
func steps() []step {
	return []step{
		{runv1.RuntimePhaseInit, phaseInit, runv1.ExitConfig, false},
		{runv1.RuntimePhaseValidate, phaseValidate, runv1.ExitConfig, false},
		{runv1.RuntimePhaseFetch, phaseFetch, runv1.ExitStorage, false},
		{runv1.RuntimePhaseCheckpoint, phaseCheckpoint, runv1.ExitStorage, false},
		{runv1.RuntimePhaseAuth, phaseAuth, runv1.ExitConfig, false},
		{runv1.RuntimePhaseClone, phaseClone, runv1.ExitGit, false},
		{runv1.RuntimePhaseRole, phaseRole, runv1.ExitConfig, false},
		{runv1.RuntimePhasePlugins, phasePlugins, runv1.ExitConfig, false},
		{runv1.RuntimePhaseMCPPrepare, phaseMCPPrepare, runv1.ExitConfig, false},
		{runv1.RuntimePhaseMCPVerify, phaseMCPVerify, runv1.ExitConfig, false},
		{runv1.RuntimePhaseRun, phaseRun, runv1.ExitAgentError, false},
		{runv1.RuntimePhaseParse, phaseParse, runv1.ExitAgentError, true},
		{runv1.RuntimePhaseOutput, phaseOutput, runv1.ExitOutputInvalid, true},
		{runv1.RuntimePhasePersist, phasePersist, runv1.ExitStorage, true},
		{runv1.RuntimePhaseCommit, phaseCommit, runv1.ExitGit, false},
		{runv1.RuntimePhasePush, phasePush, runv1.ExitGit, false},
		{runv1.RuntimePhasePR, phasePR, runv1.ExitGit, false},
		{runv1.RuntimePhaseFinalize, phaseFinalize, runv1.ExitStorage, true},
		{runv1.RuntimePhaseNotify, phaseNotify, runv1.ExitStorage, true},
	}
}

// Execute walks the pipeline and returns the process's exit code.
//
// Two rules decide what happens after something goes wrong, and both come
// straight from the contract:
//
//   - A failure before the agent has run abandons the work. Nothing has been
//     produced, so there is nothing to commit and nothing to push, and
//     attempting the git phases against a repository that was never cloned
//     produces a second, misleading failure on top of the real one.
//   - A failure from the run phase onwards does not. This is the whole of the
//     persist-before-git ordering: the agent worked for forty minutes and
//     twenty dollars, and every phase after it exists to make sure that is not
//     thrown away. A timeout is the clearest case — SIGTERM, thirty seconds,
//     SIGKILL, and then parse, output, persist, commit, push and pr, exiting 11
//     at the end of it.
//
// The epilogue runs either way. A run that failed at mcp-verify still owes the
// controller an explanation, and the explanation is the whole product of that
// run.
func (r *Run) Execute(ctx context.Context) int32 {
	abandoned := false

	for _, s := range steps() {
		if abandoned && !s.epilogue {
			r.record(ctx, s.phase, runv1.PhaseOutcomeSkipped, 0,
				"the run was abandoned at "+string(r.failure.Phase))
			continue
		}

		started := r.clock()
		err := s.fn(ctx, r)
		elapsed := r.clock().Sub(started)

		switch {
		case err == nil:
			r.record(ctx, s.phase, runv1.PhaseOutcomeOK, elapsed, "")

		case errors.Is(err, errSkip):
			var sk *skipped
			reason := err.Error()
			if errors.As(err, &sk) {
				reason = sk.reason
			}
			r.record(ctx, s.phase, runv1.PhaseOutcomeSkipped, elapsed, reason)

		default:
			f := classify(err, s.defaultCode)
			if f.Phase == "" {
				f.Phase = s.phase
			}
			r.record(ctx, s.phase, runv1.PhaseOutcomeFailed, elapsed, f.Reason)
			r.logf("phase %s failed: %s (%s, exit %d)", s.phase, f.Message(), f.Reason, f.Code)

			// The first failure is the one that gets reported. A later phase
			// failing because of the first would otherwise overwrite the cause
			// with one of its symptoms.
			if r.failure == nil {
				r.failure = f
				if !s.epilogue && s.phase != runv1.RuntimePhaseRun {
					abandoned = true
				}
			}
		}
	}

	return r.exitCode()
}

// record notes a phase's outcome in the three places that need it: this
// attempt's own map, which two later phases consult; the checkpoint, which
// reports an ok outcome onward to the controller and so to the next attempt;
// and the timings, which the report carries.
//
// The report to the controller happens here rather than at the end, and that is
// the whole improvement over the object this replaced. A checkpoint saved at
// two points in the pipeline records nothing about a pod killed between them; a
// report at the moment a phase completes is held by something that outlives the
// pod.
//
// phaseTimings is a cheap substitute for tracing when OTLP is not configured,
// and the only way to see that forty of the run's forty-five minutes went into
// cloning a monorepo rather than into the model.
func (r *Run) record(ctx context.Context, phase runv1.RuntimePhase,
	outcome runv1.PhaseOutcome, elapsed time.Duration, reason string) {

	r.outcomes[phase] = outcome
	r.checkpoint.Record(ctx, phase, outcome, elapsed, reason)
	r.timings = append(r.timings, runv1.PhaseTiming{
		Phase:      phase,
		DurationMs: elapsed.Milliseconds(),
		Outcome:    outcome,
	})
	if outcome == runv1.PhaseOutcomeSkipped {
		r.logf("phase %s skipped: %s", phase, reason)
		return
	}
	r.logf("phase %s %s in %dms", phase, outcome, elapsed.Milliseconds())
}

// exitCode is the one channel that survives everything: the controller reads it
// from the pod's status even when this process never managed to say a word.
//
// Notification cannot influence it (R10). By the time notify runs the result is
// already in storage, the outcome is visible through the Job's exit code, and
// the contents will be lifted from the run's prefix regardless. Failing a
// successful run because the controller happened to be restarting would be
// substituting the means for the end.
func (r *Run) exitCode() int32 {
	if r.cancelled {
		// Indistinguishable from an eviction by code — both are signal 143 —
		// and distinguished in the report by status, which is why the report
		// carries a status that is otherwise derivable.
		return exitSIGTERM
	}
	if r.failure != nil && r.failure.Phase == runv1.RuntimePhaseNotify {
		// The single case where a phase's failure is deliberately dropped.
		return runv1.ExitSuccess
	}
	if r.failure != nil {
		return r.failure.Code
	}
	return runv1.ExitSuccess
}

// exitSIGTERM is 128 plus SIGTERM, the code a shell reports for a process the
// platform stopped. The controller classes everything above 128 as infra and
// retryable.
const exitSIGTERM = 143
