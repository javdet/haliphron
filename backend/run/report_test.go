package run

import (
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The decision table from section 5 of the Cluster API contract, row by row.
//
// It is unit-tested here as well as end to end against FakeController because
// the two ask different questions. The contract tests ask whether a controller
// driving a real backend over HTTP ends up in the right state; this asks
// whether every row of the table exists at all, including the ones a
// controller behaving correctly never provokes.
func TestApplyFollowsTheContractTable(t *testing.T) {
	const cluster = runv1.ULID("01JCLUSTER0000000000000001")

	running := State{
		RunID: "01JRUN000000000000000001", ClusterID: cluster,
		Epoch: 3, Attempt: 2, Status: clusterv1.StatusRunning,
		Phase: runv1.PhaseRunning, Rank: runv1.PhaseRunning.Rank(),
	}
	succeeded := State{
		RunID: running.RunID, ClusterID: cluster,
		Epoch: 3, Attempt: 2, Status: clusterv1.StatusSucceeded,
		Phase: runv1.PhaseSucceeded, Rank: runv1.PhaseSucceeded.Rank(),
	}

	cases := []struct {
		name    string
		state   State
		caller  runv1.ULID
		obs     clusterv1.RunObservation
		want    Verdict
		attempt bool
		code    clusterv1.ProblemCode
		action  clusterv1.Action
		audit   bool
	}{
		{
			name:  "a stale epoch is a zombie and is told to abandon",
			state: running, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 2, Attempt: 2, Phase: runv1.PhaseRunning},
			want: Reject, code: clusterv1.CodeEpochMismatch, action: clusterv1.ActionAbandon,
		},
		{
			name:  "an epoch the backend never issued is a defect, not a race",
			state: running, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 4, Attempt: 2, Phase: runv1.PhaseRunning},
			want: Reject, code: clusterv1.CodeInvalidRequest, action: clusterv1.ActionFatal,
		},
		{
			name:  "another cluster's report on this run is abandoned",
			state: running, caller: "01JCLUSTER0000000000000002",
			obs:  clusterv1.RunObservation{Epoch: 3, Attempt: 2, Phase: runv1.PhaseRunning},
			want: Reject, code: clusterv1.CodeRunLeasedByAnotherCluster, action: clusterv1.ActionAbandon,
		},
		{
			name:  "an attempt behind the current one lost its ownership",
			state: running, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 3, Attempt: 1, Phase: runv1.PhaseRunning},
			want: Reject, code: clusterv1.CodeAttemptRegression, action: clusterv1.ActionAbandon,
		},
		{
			name:  "a new attempt resets the rank and starts a ledger row",
			state: succeeded, caller: cluster,
			obs:     clusterv1.RunObservation{Epoch: 3, Attempt: 3, Phase: runv1.PhasePending},
			want:    Advance,
			attempt: true,
		},
		{
			name:  "forward within the attempt is applied",
			state: running, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 3, Attempt: 2, Phase: runv1.PhaseSucceeded},
			want: Advance,
		},
		{
			name:  "the same phase twice is an idempotent repeat",
			state: running, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 3, Attempt: 2, Phase: runv1.PhaseRunning},
			want: Repeat,
		},
		{
			name:  "a second, different ending is refused and audited",
			state: succeeded, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 3, Attempt: 2, Phase: runv1.PhaseFailed},
			want: Reject,
			code: clusterv1.CodeRunTerminal, action: clusterv1.ActionAbandon, audit: true,
		},
		{
			name:  "going backwards is reordering, so retry rather than abandon",
			state: running, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 3, Attempt: 2, Phase: runv1.PhaseStarting},
			want: Reject, code: clusterv1.CodePhaseRegression, action: clusterv1.ActionRetry,
		},
		{
			name:  "a phase this build does not know loses instead of crashing",
			state: running, caller: cluster,
			obs:  clusterv1.RunObservation{Epoch: 3, Attempt: 2, Phase: runv1.Phase("Levitating")},
			want: Reject, code: clusterv1.CodeInvalidRequest, action: clusterv1.ActionFatal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := tc.obs
			obs.RunID = tc.state.RunID
			got := Apply(tc.state, obs, tc.caller)

			if got.Verdict != tc.want {
				t.Errorf("verdict = %v, want %v", got.Verdict, tc.want)
			}
			if got.NewAttempt != tc.attempt {
				t.Errorf("newAttempt = %t, want %t", got.NewAttempt, tc.attempt)
			}
			if got.Code != tc.code {
				t.Errorf("code = %q, want %q", got.Code, tc.code)
			}
			if got.Action != tc.action {
				t.Errorf("action = %q, want %q", got.Action, tc.action)
			}
			if got.Audit != tc.audit {
				t.Errorf("audit = %t, want %t", got.Audit, tc.audit)
			}
		})
	}
}

// Unknown is a statement about the backend's sight of a run, not about the
// run's progress. It lives in status while observed_phase keeps the rank, so
// the report that arrives when the cluster comes back compares equal and is
// applied as the repeat it is — which is what clears it. If Unknown had a rank
// of its own, this observation would be a phase regression and the run would
// stay Unknown forever.
func TestUnknownIsClearedByTheReportThatComesBack(t *testing.T) {
	state := State{
		RunID: "01JRUN000000000000000001", ClusterID: "01JCLUSTER0000000000000001",
		Epoch: 1, Attempt: 1,
		Status: clusterv1.StatusUnknown,
		Phase:  runv1.PhaseRunning, Rank: runv1.PhaseRunning.Rank(),
	}
	obs := clusterv1.RunObservation{
		RunID: state.RunID, Epoch: 1, Attempt: 1, Phase: runv1.PhaseRunning,
	}

	got := Apply(state, obs, state.ClusterID)
	if got.Verdict != Repeat {
		t.Fatalf("verdict = %v, want Repeat", got.Verdict)
	}
	if !got.Accepted() {
		t.Fatal("the report that clears Unknown must be accepted")
	}
	if want := clusterv1.StatusRunning; StoredStatus(obs.Phase) != want {
		t.Errorf("stored status = %q, want %q", StoredStatus(obs.Phase), want)
	}
}

// A terminal phase whose report has not arrived is not a failure: the result
// was in storage before the callback was made, so this is a read the backend
// owes itself. The state is reported as CompletedWithoutResult and stored as
// the terminal status — the run_status domain has no value for the former.
func TestTerminalWithoutACompletionIsReportedAsCompletedWithoutResult(t *testing.T) {
	if got := StatusFor(runv1.PhaseSucceeded, false); got != clusterv1.StatusCompletedWithoutResult {
		t.Errorf("status = %q, want %q", got, clusterv1.StatusCompletedWithoutResult)
	}
	if got := StatusFor(runv1.PhaseSucceeded, true); got != clusterv1.StatusSucceeded {
		t.Errorf("status = %q, want %q", got, clusterv1.StatusSucceeded)
	}
	if got := StatusFor(runv1.PhaseRunning, false); got != clusterv1.StatusRunning {
		t.Errorf("a non-terminal phase must not be affected: %q", got)
	}
	if got := StoredStatus(runv1.PhaseSucceeded); got != clusterv1.StatusSucceeded {
		t.Errorf("stored status = %q, want a value the run_status domain holds", got)
	}
}

// Identifiers cross five boundaries in this system and are compared as strings
// at every one of them.
func TestULIDsSortByMintTimeAndSurviveValidation(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	first := MustULID(base)
	second := MustULID(base.Add(time.Millisecond))

	if !ValidULID(first) || !ValidULID(second) {
		t.Fatalf("minted identifiers must pass validation: %s %s", first, second)
	}
	if !(first < second) {
		t.Errorf("%s should sort before %s", first, second)
	}
	if len(first) != 26 {
		t.Errorf("length = %d, want 26", len(first))
	}
	// I, L, O and U are not in the alphabet, so an identifier read off a
	// screen cannot turn into a different valid one.
	for _, bad := range []runv1.ULID{"01JRUN00000000000000000I01", "short", ""} {
		if ValidULID(bad) {
			t.Errorf("%q must not validate", bad)
		}
	}
}
