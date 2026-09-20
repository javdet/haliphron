package entrypoint

import (
	"context"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The attempt checkpoint: what a previous attempt of this run got through, and
// what this one is reporting as it goes.
//
// It used to be runs/{id}/state.json — an object this pod wrote and the next
// attempt read back. It is now two environment variables' worth of input and a
// stream of small reports out, and the change is not only about where the fact
// is stored.
//
// The object was written when the pod chose to save it, at the end of persist
// and again at finalize, so a pod killed anywhere between those two points
// recorded nothing about the phases in between. A report at the moment a phase
// completes is held by something that outlives the pod, so an OOM between two
// phases still leaves the one that finished on the record. And the record is
// visible: "how far did this get before it died" is a SELECT on the control
// plane rather than an object fetched out of a bucket.
//
// Exactly one phase is worth resuming — run. It is the only one whose
// repetition costs money, and the only one whose product is already durable by
// the time the checkpoint is read, because persist runs before the git phases
// rather than after them. Everything else is cheaper to replay than to trust a
// record of: a clone from a previous attempt went with its pod, and a branch
// marked pushed may since have been force-pushed over by a human.

// Checkpoint is what this attempt knows about what came before, and what it has
// reported about itself.
//
// It is deliberately not a document any more. Nothing serialises it, nothing
// uploads it, and nothing outside this process reads it: the controller holds
// the durable copy on the CR's status and the backend holds the system of
// record in run_attempts.completed_phases.
type Checkpoint struct {
	RunID   runv1.ULID
	Attempt int32

	// done is what earlier attempts of this run completed, as
	// HALIPHRON_COMPLETED_PHASES stated it.
	done map[runv1.RuntimePhase]bool
	// reported is what this attempt has told the controller about, so that a
	// repeat is not sent.
	reported map[runv1.RuntimePhase]bool

	// nextChunkSeq numbers log chunks. It is seeded past everything a previous
	// attempt can have written rather than from zero: numbering from zero again
	// would overwrite the earlier attempt's chunks, and a reader following them
	// in order would see two runs spliced into one without a seam.
	nextChunkSeq int

	callback *Callback
	clock    func() time.Time
}

// NewCheckpoint reads what the controller handed this attempt.
func NewCheckpoint(cfg *Config, callback *Callback, clock func() time.Time) *Checkpoint {
	cp := &Checkpoint{
		RunID:    cfg.RunID,
		Attempt:  cfg.Attempt,
		done:     map[runv1.RuntimePhase]bool{},
		reported: map[runv1.RuntimePhase]bool{},
		callback: callback,
		clock:    clock,
	}
	for _, phase := range cfg.CompletedPhases {
		cp.done[phase] = true
	}
	// Each attempt gets a block of a thousand chunk numbers. Cruder than a
	// carried-over counter and strictly more robust: the counter used to live
	// in the object this package no longer writes, and asking the controller
	// for it would put a round trip in front of the first line of log. A run
	// that produces a thousand chunks in one attempt has bigger problems than
	// an overlap, and the ordering within an attempt — which is what a reader
	// follows — is exact.
	cp.nextChunkSeq = int(cfg.Attempt-1) * chunksPerAttempt
	return cp
}

// chunksPerAttempt is the block each attempt numbers within. Six digits of
// zero-padded key leaves room for far more than this; the block is what keeps
// attempt 2's first chunk sorting after attempt 1's last.
const chunksPerAttempt = 1000

// Resume decides whether the expensive phase may be skipped.
//
// Every condition is mandatory and any doubt is resolved in favour of a full
// run. An extra bill for the model is money; a wrongly resumed attempt is an
// incorrect result reported as correct, and nobody notices that on the day it
// happens.
//
// The returned string is why, in both directions: a resumption that cannot
// explain itself is indistinguishable from a bug, and so is a refusal to
// resume.
func (cp *Checkpoint) Resume(c *Config) (bool, string) {
	switch {
	case len(cp.done) == 0:
		return false, "no checkpoint: this is the first attempt of this run under this lease"
	case c.Attempt <= 1:
		// A first attempt carrying a checkpoint means the backend re-issued
		// work whose previous controller had reported progress. Legitimate,
		// and still refused: this pod has nothing of that attempt's — no
		// result object it wrote, no usage it recorded — so skipping the model
		// would leave it with nothing to report.
		return false, "this is attempt 1; a checkpoint from a previous owner is not this attempt's to resume"
	case !cp.done[runv1.RuntimePhaseRun]:
		return false, "the previous attempt did not finish the run phase"
	case !cp.done[runv1.RuntimePhasePersist]:
		// The run phase alone is not enough. persist is what made the model's
		// product durable, and a checkpoint that says the model ran and does
		// not say the result was stored describes an attempt whose output went
		// with its pod. Resuming it would skip the paid phase and then have
		// nothing to report, which is worse than paying twice.
		return false, "the previous attempt ran the model and did not persist its result"
	}
	return true, "an earlier attempt completed the run and persist phases"
}

// Done reports whether an earlier attempt finished a phase. Only
// RuntimePhase.Resumable() phases are consulted for skipping; the rest are on
// the record so that "how far did this get" has an answer.
func (cp *Checkpoint) Done(phase runv1.RuntimePhase) bool { return cp.done[phase] }

// Completed is everything known to be done — what an earlier attempt reported
// plus what this one has — in the contract's execution order.
//
// It rides in the completion report as a summary. The per-phase reports are the
// primary channel, and this is the copy that survives a pod whose last few
// reports did not get through.
func (cp *Checkpoint) Completed() []runv1.RuntimePhase {
	out := make([]runv1.RuntimePhase, 0, len(cp.done))
	for _, phase := range runv1.RuntimePhases {
		if cp.done[phase] {
			out = append(out, phase)
		}
	}
	return out
}

// Record notes how a phase ended and, when it ended cleanly, tells the
// controller.
//
// Only an ok outcome is reported. A failed or skipped phase is not something a
// later attempt may assume was done, and the one phase whose replay costs money
// is exactly the one where getting this wrong would skip a model call that
// never happened.
func (cp *Checkpoint) Record(ctx context.Context, phase runv1.RuntimePhase,
	outcome runv1.PhaseOutcome, elapsed time.Duration, reason string) {

	if outcome != runv1.PhaseOutcomeOK {
		return
	}
	cp.done[phase] = true
	if cp.callback == nil || cp.reported[phase] {
		return
	}
	cp.reported[phase] = true
	cp.callback.Phase(ctx, runv1.PhaseReport{
		RunID:      cp.RunID,
		Attempt:    cp.Attempt,
		Phase:      phase,
		Outcome:    outcome,
		DurationMs: elapsed.Milliseconds(),
		Reason:     truncate(reason, 128),
	})
}

// nextChunk hands out the next log chunk number and advances the cursor.
func (cp *Checkpoint) nextChunk() int {
	seq := cp.nextChunkSeq
	cp.nextChunkSeq++
	return seq
}

// parseCompletedPhases reads HALIPHRON_COMPLETED_PHASES.
//
// A name this build does not recognise is dropped rather than carried. The list
// is "phases you may skip", and skipping one whose name means nothing here is
// the single way this variable could cost money — an image a version behind
// must not be talked into believing it has already run the model.
func parseCompletedPhases(raw string) []runv1.RuntimePhase {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			seen[name] = true
		}
	}
	var out []runv1.RuntimePhase
	for _, phase := range runv1.RuntimePhases {
		if seen[string(phase)] {
			out = append(out, phase)
		}
	}
	return out
}
