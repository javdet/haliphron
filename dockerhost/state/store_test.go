package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func row() Run {
	return Run{
		RunID: "01HZRUN0000000000000000000", Epoch: 2,
		Spec:          runv1.RenderedRunSpec{Agent: "claude-code", Image: "img", Model: "m"},
		CallbackToken: "tok",
		Materials:     Materials{Prompt: "p", Secrets: map[string]string{"git-token": "x"}},
	}
}

func TestARowRoundTripsWithItsMaterials(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.Insert(ctx, row()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "01HZRUN0000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != 2 || got.Materials.Secrets["git-token"] != "x" || got.Materials.Prompt != "p" ||
		got.Version != 1 || got.Materials.Artifacts.Mode != runv1.ArtifactModeRelay {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.Insert(ctx, row()); !errors.Is(err, ErrExists) {
		t.Fatalf("a second insert for the same run: %v", err)
	}
}

func TestAWriteFromAStaleReadIsAConflict(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	first, _ := s.Insert(ctx, row())
	second := first

	first.Status.Phase = runv1.PhaseRunning
	if _, err := s.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	second.CancelReason = "user"
	if _, err := s.Update(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write from a stale read was accepted: %v", err)
	}

	got, err := s.Mutate(ctx, first.RunID, func(r *Run) bool { r.CancelReason = "user"; return true })
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != runv1.PhaseRunning || got.CancelReason != "user" {
		t.Fatalf("Mutate lost a field: %+v", got)
	}
}

func TestATerminalPhaseStampsTheTerminalTime(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	r, _ := s.Insert(ctx, row())
	r.Status.Phase = runv1.PhaseSucceeded
	r, err := s.Update(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if r.TerminalAt == nil {
		t.Fatal("no terminal time")
	}
}

func TestTheFileIsPrivateAndReopeningDoesNotMigrateTwice(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	_, _ = s.Insert(ctx, row())
	_ = s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the store holds secrets and is mode %v", info.Mode().Perm())
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if rows, err := s2.List(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if _, err := ro.Get(ctx, "01HZRUN0000000000000000000"); err != nil {
		t.Fatal(err)
	}
}

func TestImagesAreListedMostRecentFirst(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	_ = s.TouchImage(ctx, "a")
	now = now.Add(time.Minute)
	_ = s.TouchImage(ctx, "b")
	imgs, err := s.Images(ctx)
	if err != nil || len(imgs) != 2 || imgs[0].Ref != "b" {
		t.Fatalf("%v %v", imgs, err)
	}
}
