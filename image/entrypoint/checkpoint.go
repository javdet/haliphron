package entrypoint

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// runs/{runID}/state.json is the only object this process both writes and
// reads. Neither the controller nor the backend parses it: it is runtime
// state, moved outside the pod only because a pod's filesystem dies with the
// pod.
//
// Exactly one phase is resumed — run. It is the only one whose repetition costs
// money, and the only one whose result is already durable by the time the
// checkpoint is read, because persist runs before the git phases rather than
// after them. Everything else is cheaper to replay than to trust a record of: a
// clone from a previous attempt went with its pod, and a branch marked pushed
// may since have been force-pushed over by a human.

// CheckpointSchemaVersion matches state.schema.json.
const CheckpointSchemaVersion = 1

// Checkpoint is runs/{runID}/state.json.
type Checkpoint struct {
	SchemaVersion   int        `json:"schemaVersion"`
	RunID           runv1.ULID `json:"runID"`
	Attempt         int32      `json:"attempt"`
	ContractVersion string     `json:"contractVersion"`
	StartedAt       string     `json:"startedAt,omitempty"`
	UpdatedAt       string     `json:"updatedAt"`

	Phases map[runv1.RuntimePhase]PhaseRecord `json:"phases"`

	Agent     *CheckpointAgent     `json:"agent,omitempty"`
	Artifacts *CheckpointArtifacts `json:"artifacts,omitempty"`
	Repo      *CheckpointRepo      `json:"repo,omitempty"`
	Log       *CheckpointLog       `json:"log,omitempty"`
}

// PhaseRecord is how one phase ended.
type PhaseRecord struct {
	Outcome    runv1.PhaseOutcome `json:"outcome"`
	StartedAt  string             `json:"startedAt,omitempty"`
	DurationMs int64              `json:"durationMs,omitempty"`
	Reason     string             `json:"reason,omitempty"`
}

// CheckpointAgent is what the model run returned. Without it, resuming is
// pointless: to skip run and not know its usage is to lose the cost of the run
// — which is the one number the whole exercise was about.
type CheckpointAgent struct {
	SessionID string       `json:"sessionID,omitempty"`
	ExitCode  int32        `json:"exitCode"`
	Usage     *runv1.Usage `json:"usage,omitempty"`
}

// CheckpointArtifacts is where the run phase's product ended up.
type CheckpointArtifacts struct {
	Result *runv1.ObjectRef `json:"result,omitempty"`
	Output *runv1.ObjectRef `json:"output,omitempty"`
	Log    *runv1.ObjectRef `json:"log,omitempty"`
}

// CheckpointRepo is what happened to the repository.
type CheckpointRepo struct {
	CommitSHA string `json:"commitSHA,omitempty"`
	Pushed    bool   `json:"pushed,omitempty"`
	PRURL     string `json:"prURL,omitempty"`
	PRNumber  int    `json:"prNumber,omitempty"`
}

// CheckpointLog is the upload cursor. NextChunk is deliberately not reset
// between attempts: numbering from zero would overwrite the previous attempt's
// chunks, and the reader would see two runs spliced together as one, seamlessly
// and wrongly.
type CheckpointLog struct {
	NextChunk     int   `json:"nextChunk"`
	BytesUploaded int64 `json:"bytesUploaded,omitempty"`
}

// NewCheckpoint starts a fresh one for this attempt.
func NewCheckpoint(c *Config, now time.Time) *Checkpoint {
	return &Checkpoint{
		SchemaVersion:   CheckpointSchemaVersion,
		RunID:           c.RunID,
		Attempt:         c.Attempt,
		ContractVersion: runv1.ContractVersion,
		StartedAt:       now.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       now.UTC().Format(time.RFC3339Nano),
		Phases:          map[runv1.RuntimePhase]PhaseRecord{},
		Log:             &CheckpointLog{},
	}
}

// LoadCheckpoint fetches the previous attempt's state. A 404 is the normal
// answer on a first attempt and returns (nil, nil): a caller that had to
// distinguish "absent" from "broken" by inspecting an error string would get it
// wrong eventually, and the cost of getting it wrong is a run that refuses to
// start.
//
// A checkpoint that does not parse is also (nil, nil), with a note. The
// alternative — failing the run — hands a corrupt object in a bucket the power
// to stop every subsequent attempt, and the worst a full re-run costs is money
// that was going to be spent anyway.
func LoadCheckpoint(ctx context.Context, s *Storage) (*Checkpoint, string, error) {
	body, err := s.Get(ctx, runv1.StorageKeyState)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, "no checkpoint: this is the first attempt", nil
	case err != nil:
		return nil, "", err
	}
	var cp Checkpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return nil, "the checkpoint does not parse and is ignored: " + err.Error(), nil
	}
	return &cp, "", nil
}

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
	case cp == nil:
		return false, "no checkpoint"
	case cp.RunID != c.RunID:
		// Somebody else's state. It should be unreachable — the bundle is
		// scoped to this run's prefix — and it is checked anyway, because the
		// cost of the check is nothing and the cost of being wrong is reporting
		// another run's work as this one's.
		return false, "the checkpoint belongs to run " + string(cp.RunID)
	case cp.Attempt >= c.Attempt:
		return false, "the checkpoint is from attempt " +
			strconv.Itoa(int(cp.Attempt)) + ", which is not earlier than this one"
	case majorOf(cp.ContractVersion) != runv1.ContractMajor:
		return false, "the checkpoint was written under contract " + cp.ContractVersion
	case cp.Phases[runv1.RuntimePhaseRun].Outcome != runv1.PhaseOutcomeOK:
		return false, "the previous attempt did not finish the run phase"
	case cp.Artifacts == nil || cp.Artifacts.Result == nil || cp.Artifacts.Output == nil:
		// The checkpoint claims the model ran and does not say where its
		// product went. Resuming would skip the paid phase and then have
		// nothing to report, which is worse than paying twice.
		return false, "the checkpoint records no result and output in storage"
	}
	return true, "attempt " + strconv.Itoa(int(cp.Attempt)) + " already completed the run phase"
}

// Record notes how a phase ended.
func (cp *Checkpoint) Record(phase runv1.RuntimePhase, rec PhaseRecord) {
	if cp.Phases == nil {
		cp.Phases = map[runv1.RuntimePhase]PhaseRecord{}
	}
	cp.Phases[phase] = rec
}

// Save uploads the checkpoint. Called at the end of persist and again at
// finalize; in between are the phases entitled to fail, and what they would
// lose is already durable.
func (cp *Checkpoint) Save(ctx context.Context, s *Storage, now time.Time) (*runv1.ObjectRef, error) {
	cp.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(cp)
	if err != nil {
		return nil, failWrap(runv1.ExitStorage, "StateUnserialisable", err, "marshalling the checkpoint")
	}
	return s.Put(ctx, runv1.StorageKeyState, body, "application/json")
}

// nextChunk hands out the next log chunk number and advances the cursor. The
// counter is carried across attempts, so the second attempt of a run continues
// where the first stopped instead of overwriting it.
func (cp *Checkpoint) nextChunk() int {
	if cp.Log == nil {
		cp.Log = &CheckpointLog{}
	}
	seq := cp.Log.NextChunk
	cp.Log.NextChunk++
	return seq
}

// majorOf reads the leading integer of a version string. An unparseable version
// yields -1, which matches no major and therefore forces a full run — the safe
// direction, and the one a corrupt or future checkpoint should take.
func majorOf(version string) int {
	head, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(head)
	if err != nil {
		return -1
	}
	return n
}
