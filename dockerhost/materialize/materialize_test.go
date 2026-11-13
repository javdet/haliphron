package materialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/dockerhost/state"
)

type gate struct{ err error }

func (g gate) Healthy() error { return g.err }

func setup(t *testing.T) (*Materializer, *state.Store) {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Materializer{Store: s, Network: gate{}, CallbackURL: "http://172.31.250.1:8083",
		Runtimes: []runv1.AgentType{"claude-code"}}, s
}

func lease(epoch int64) clusterv1.Lease {
	prompt := "fix it"
	sum := sha256.Sum256([]byte(prompt))
	digest := hex.EncodeToString(sum[:])
	return clusterv1.Lease{
		RunID: "01HZRUN0000000000000000000", Epoch: epoch, Attempt: 1,
		Spec: runv1.RenderedRunSpec{Agent: "claude-code", Image: "javdet/haliphron-agent:0.6.2",
			PromptSHA256: digest, Model: "m", Runtime: runv1.RuntimeSpec{TimeoutSeconds: 60}},
		Prompt: prompt, PromptSHA256: digest,
		Secrets:         map[string]string{"git-token": "x"},
		CompletedPhases: []runv1.RuntimePhase{runv1.RuntimePhaseRun},
	}
}

func TestALeaseBecomesARowAndRepeatingItReturnsTheSameRow(t *testing.T) {
	m, s := setup(t)
	ctx := context.Background()
	res, err := m.Apply(ctx, lease(1))
	if err != nil || res.Run == nil {
		t.Fatalf("%+v %v", res, err)
	}
	row, _ := s.Get(ctx, "01HZRUN0000000000000000000")
	if row.CallbackToken == "" || row.Materials.Prompt != "fix it" || row.AckedAt != nil ||
		len(row.Status.CompletedPhases) != 1 {
		t.Fatalf("row %+v", row)
	}
	again, err := m.Apply(ctx, lease(1))
	if err != nil || again.Run == nil || again.Run.CallbackToken != row.CallbackToken {
		t.Fatal("the restart path minted a new token or no row")
	}
}

func TestPreflightRefusesWithTheContractsCodes(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		mutate func(*Materializer, *clusterv1.Lease)
		code   clusterv1.RejectionCode
	}{
		"prompt digest mismatch": {func(_ *Materializer, l *clusterv1.Lease) { l.PromptSHA256 = "00" }, clusterv1.RejectInvalidSpec},
		"no prompt":              {func(_ *Materializer, l *clusterv1.Lease) { l.Prompt = "" }, clusterv1.RejectInvalidSpec},
		"bad quantity":           {func(_ *Materializer, l *clusterv1.Lease) { l.Spec.Runtime.Resources.CPU = "many" }, clusterv1.RejectInvalidSpec},
		"undeclared agent":       {func(_ *Materializer, l *clusterv1.Lease) { l.Spec.Agent = "codex" }, clusterv1.RejectInvalidSpec},
		"bad image":              {func(_ *Materializer, l *clusterv1.Lease) { l.Spec.Image = "Not An Image" }, clusterv1.RejectInvalidSpec},
		"secret key with a slash": {func(_ *Materializer, l *clusterv1.Lease) {
			l.Secrets = map[string]string{"../x": "y"}
		}, clusterv1.RejectInvalidSpec},
		"network check failing": {func(m *Materializer, _ *clusterv1.Lease) { m.Network = gate{errors.New("open")} },
			clusterv1.RejectMaterializationFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, s := setup(t)
			l := lease(1)
			tc.mutate(m, &l)
			res, err := m.Apply(ctx, l)
			if err != nil || res.Rejection == nil || res.Rejection.Code != tc.code {
				t.Fatalf("%+v %v", res, err)
			}
			if _, err := s.Get(ctx, l.RunID); !errors.Is(err, state.ErrNotFound) {
				t.Fatal("a refused lease left a row")
			}
		})
	}
}

func TestANewEpochWaitsForTheOldOneToBeRemoved(t *testing.T) {
	m, s := setup(t)
	ctx := context.Background()
	if _, err := m.Apply(ctx, lease(1)); err != nil {
		t.Fatal(err)
	}
	res, err := m.Apply(ctx, lease(2))
	if err != nil || !res.Superseded {
		t.Fatalf("%+v %v", res, err)
	}
	old, _ := s.Get(ctx, "01HZRUN0000000000000000000")
	if !old.IsAbandoned() || old.Epoch != 1 {
		t.Fatal("the old epoch was not marked for removal")
	}
	if err := s.Delete(ctx, old.RunID); err != nil {
		t.Fatal(err)
	}
	res, err = m.Apply(ctx, lease(2))
	if err != nil || res.Run == nil || res.Run.Epoch != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	stale, _ := m.Apply(ctx, lease(1))
	if stale.Rejection == nil {
		t.Fatal("an older epoch than the one held was accepted")
	}
}
