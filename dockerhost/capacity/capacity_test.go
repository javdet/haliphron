package capacity

import (
	"context"
	"path/filepath"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/state"
)

type fakeEngine struct {
	docker.Engine
	info docker.Info
}

func (f fakeEngine) Info(context.Context) (docker.Info, error) { return f.info, nil }

func checker(t *testing.T, free int64) (*Checker, *state.Store) {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Checker{
		Engine: fakeEngine{info: docker.Info{NCPU: 8, MemTotal: 32 << 30}},
		Store:  s,
		Config: Config{Slots: 4, ReserveCPU: "1", ReserveMemory: "2Gi", DefaultCPU: "2", DefaultMem: "4Gi",
			DefaultDisk: "10Gi", DiskFloor: 20 << 30, DiskCritical: 5 << 30},
		DiskFree: func(string) (int64, error) { return free, nil },
	}, s
}

func TestFreeSlotsIsTheSmallerOfTheSlotsAndWhatFits(t *testing.T) {
	c, _ := checker(t, 500<<30)
	snap, err := c.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 7 usable CPUs at 2 each is 3 runs; the slots allow 4.
	if got := snap.FreeSlots(); got != 3 {
		t.Fatalf("free slots %d, want 3", got)
	}
}

func TestARunTooLargeForTheHostDoesNotFit(t *testing.T) {
	c, _ := checker(t, 500<<30)
	snap, _ := c.Take(context.Background())
	if ok, _ := c.Fits(snap, runv1.Resources{CPU: "4", Memory: "8Gi"}); !ok {
		t.Fatal("a run that fits was refused")
	}
	if ok, why := c.Fits(snap, runv1.Resources{CPU: "16"}); ok || why == "" {
		t.Fatal("a run larger than the host was admitted")
	}
}

func TestDiskUnderTheFloorStopsNewWork(t *testing.T) {
	c, _ := checker(t, 10<<30)
	snap, _ := c.Take(context.Background())
	if !snap.DiskLow() || snap.FreeSlots() != 0 {
		t.Fatalf("disk low %v free %d", snap.DiskLow(), snap.FreeSlots())
	}
	if ok, _ := c.Fits(snap, runv1.Resources{}); ok {
		t.Fatal("admitted under the disk floor")
	}
}

func TestActiveRunsCountAgainstTheHost(t *testing.T) {
	c, s := checker(t, 500<<30)
	ctx := context.Background()
	for _, id := range []runv1.ULID{"A", "B", "C"} {
		if _, err := s.Insert(ctx, state.Run{RunID: id, Epoch: 1, CallbackToken: "t",
			Spec: runv1.RenderedRunSpec{Runtime: runv1.RuntimeSpec{Resources: runv1.Resources{CPU: "2"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := c.Take(ctx)
	if snap.Active != 3 || snap.FreeSlots() != 0 {
		t.Fatalf("active %d free %d", snap.Active, snap.FreeSlots())
	}
}
