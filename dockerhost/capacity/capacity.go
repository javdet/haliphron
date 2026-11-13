// Package capacity answers the question a ResourceQuota answers on Kubernetes:
// does this run fit? On one host the answer is arithmetic over the host's
// totals, the limits of the runs it already holds, and the free disk.
//
// freeSlots = min(CAPACITY_SLOTS − active runs, the number of default-sized
// runs that still fit), and the same fit test is applied to each lease's own
// spec at preflight, so a large run is refused with QuotaExhausted rather than
// overcommitting the machine (docker-host.md §8).
package capacity

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/launcher"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// Config is the operator's share of the arithmetic.
type Config struct {
	Slots int32
	// Reserve is kept back for the OS, the daemon and the controller.
	ReserveCPU    string
	ReserveMemory string
	// Defaults size a run whose spec states no limit, and size the slot in
	// the free-slot count.
	DefaultCPU  string
	DefaultMem  string
	DefaultDisk string

	DiskPath     string
	DiskFloor    int64
	DiskCritical int64
}

// Checker computes snapshots.
type Checker struct {
	Engine docker.Engine
	Store  *state.Store
	Config Config
	// DiskFree is replaceable for tests. It defaults to statfs of DiskPath.
	DiskFree func(path string) (int64, error)

	mu     sync.Mutex
	info   docker.Info
	infoAt time.Time
}

// Snapshot is the state of the host at one moment.
type Snapshot struct {
	Slots  int32
	Active int32

	TotalNanoCPU  int64
	TotalMemory   int64
	UsedNanoCPU   int64
	UsedMemory    int64
	UsedDisk      int64
	DiskFree      int64
	DiskFloor     int64
	DiskCritical  int64
	slot          launcher.Limits
	reserveCPU    int64
	reserveMemory int64
}

// Take reads the host and the store.
func (c *Checker) Take(ctx context.Context) (Snapshot, error) {
	info, err := c.hostInfo(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	slot, err := c.defaults()
	if err != nil {
		return Snapshot{}, err
	}
	reserve, err := launcher.ParseLimits(runv1.Resources{CPU: c.Config.ReserveCPU, Memory: c.Config.ReserveMemory})
	if err != nil {
		return Snapshot{}, fmt.Errorf("capacity: reserve: %w", err)
	}
	s := Snapshot{
		Slots:         c.Config.Slots,
		TotalNanoCPU:  int64(info.NCPU) * 1_000_000_000,
		TotalMemory:   info.MemTotal,
		DiskFloor:     c.Config.DiskFloor,
		DiskCritical:  c.Config.DiskCritical,
		slot:          slot,
		reserveCPU:    reserve.NanoCPUs,
		reserveMemory: reserve.MemoryBytes,
	}
	rows, err := c.Store.List(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	for i := range rows {
		r := &rows[i]
		if r.Status.Phase.IsTerminal() && r.Status.Retry.NextAttemptAt == nil {
			continue
		}
		if r.IsAbandoned() {
			continue
		}
		s.Active++
		l := c.limitsOf(r.Spec.Runtime.Resources, slot)
		s.UsedNanoCPU += l.NanoCPUs
		s.UsedMemory += l.MemoryBytes
		s.UsedDisk += l.EphemeralStorage
	}
	free := c.DiskFree
	if free == nil {
		free = StatfsFree
	}
	if s.DiskFree, err = free(c.Config.DiskPath); err != nil {
		return Snapshot{}, fmt.Errorf("capacity: free disk on %s: %w", c.Config.DiskPath, err)
	}
	return s, nil
}

// FreeSlots is the lease's freeSlots.
func (s Snapshot) FreeSlots() int32 {
	free := s.Slots - s.Active
	if s.DiskLow() {
		return 0
	}
	byResources := int32(0)
	for n := int32(1); n <= s.Slots; n++ {
		if !s.fits(launcher.Limits{
			NanoCPUs:         s.slot.NanoCPUs * int64(n),
			MemoryBytes:      s.slot.MemoryBytes * int64(n),
			EphemeralStorage: s.slot.EphemeralStorage * int64(n),
		}) {
			break
		}
		byResources = n
	}
	free = min(free, byResources)
	if free < 0 {
		return 0
	}
	return free
}

// DiskLow reports the free disk under the floor: no new leases.
func (s Snapshot) DiskLow() bool { return s.DiskFloor > 0 && s.DiskFree < s.DiskFloor }

// DiskCriticalLow reports the free disk under the critical floor: a warning on
// every heartbeat. Running runs are not killed.
func (s Snapshot) DiskCriticalLow() bool { return s.DiskCritical > 0 && s.DiskFree < s.DiskCritical }

// Fits reports whether one more run with these resources fits, and why not.
func (c *Checker) Fits(s Snapshot, r runv1.Resources) (bool, string) {
	if s.Active >= s.Slots {
		return false, fmt.Sprintf("all %d slots on this host are in use", s.Slots)
	}
	if s.DiskLow() {
		return false, fmt.Sprintf("free disk is %d bytes, under the floor of %d", s.DiskFree, s.DiskFloor)
	}
	l := c.limitsOf(r, s.slot)
	if !s.fits(l) {
		return false, fmt.Sprintf("the run needs %d nanocpus and %d bytes of memory; "+
			"%d and %d are left after the reserve and the runs already here",
			l.NanoCPUs, l.MemoryBytes,
			s.TotalNanoCPU-s.reserveCPU-s.UsedNanoCPU, s.TotalMemory-s.reserveMemory-s.UsedMemory)
	}
	return true, ""
}

func (s Snapshot) fits(l launcher.Limits) bool {
	if s.TotalNanoCPU > 0 && s.UsedNanoCPU+l.NanoCPUs > s.TotalNanoCPU-s.reserveCPU {
		return false
	}
	if s.TotalMemory > 0 && s.UsedMemory+l.MemoryBytes > s.TotalMemory-s.reserveMemory {
		return false
	}
	// ephemeralStorage is not enforced per container — Docker has no
	// per-volume quota on the local driver — so it counts for admission only,
	// against what is free above the floor. The declared storage of the runs
	// already here is summed in although some of it is already spent and so
	// already missing from DiskFree: conservative on purpose, because a full
	// disk fails every run on the host at once.
	if s.DiskFree > 0 && s.UsedDisk+l.EphemeralStorage > s.DiskFree-s.DiskFloor {
		return false
	}
	return true
}

func (c *Checker) limitsOf(r runv1.Resources, slot launcher.Limits) launcher.Limits {
	l, err := launcher.ParseLimits(r)
	if err != nil {
		return slot
	}
	if l.NanoCPUs == 0 {
		l.NanoCPUs = slot.NanoCPUs
	}
	if l.MemoryBytes == 0 {
		l.MemoryBytes = slot.MemoryBytes
	}
	if l.EphemeralStorage == 0 {
		l.EphemeralStorage = slot.EphemeralStorage
	}
	return l
}

func (c *Checker) defaults() (launcher.Limits, error) {
	l, err := launcher.ParseLimits(runv1.Resources{
		CPU: c.Config.DefaultCPU, Memory: c.Config.DefaultMem, EphemeralStorage: c.Config.DefaultDisk,
	})
	if err != nil {
		return launcher.Limits{}, fmt.Errorf("capacity: default run size: %w", err)
	}
	return l, nil
}

// hostInfo caches docker info for a minute: the host's totals do not change
// between two heartbeats.
func (c *Checker) hostInfo(ctx context.Context) (docker.Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.infoAt.IsZero() && time.Since(c.infoAt) < time.Minute {
		return c.info, nil
	}
	info, err := c.Engine.Info(ctx)
	if err != nil {
		return docker.Info{}, fmt.Errorf("capacity: docker info: %w", err)
	}
	c.info, c.infoAt = info, time.Now()
	return info, nil
}

// StatfsFree is the space available to an unprivileged writer on the
// filesystem holding path.
func StatfsFree(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
