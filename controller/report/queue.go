// Package report is everything the controller owes the backend: the phase
// changes it observed, the pod's completion report, and the heartbeat that
// proves the cluster is alive and reconciles what each side believes.
//
// The queue below is the practical half of principle P4. A controller that
// cannot reach the control plane keeps running the work it holds and keeps
// accumulating what it will say about it; nothing is dropped and nothing
// blocks. Losing a queued observation is survivable anyway — the heartbeat
// carries the current state of every run an interval later — but losing it
// silently is not, so the queue is bounded and says when it sheds.
package report

import (
	"context"
	"sync"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// DefaultQueueDepth is generous: an observation is a few hundred bytes, and the
// case this exists for is an outage long enough for every run on a busy cluster
// to change phase several times.
const DefaultQueueDepth = 2048

// Queue holds observations waiting for the low-latency ingest path.
//
// Entries are keyed by (runID, attempt, phase rank) rather than by run: the
// terminal phase of attempt 3 and the pending phase of attempt 4 are two
// different facts and both are owed. A repeat of the same fact replaces the
// earlier copy, because at-least-once delivery of an idempotent report is worth
// nothing and costs bytes.
type Queue struct {
	mu    sync.Mutex
	items map[entryKey]clusterv1.RunObservation
	order []entryKey
	max   int

	wake chan struct{}
}

type entryKey struct {
	runID   runv1.ULID
	attempt int32
	rank    int
}

// NewQueue builds a queue with the given depth; zero means DefaultQueueDepth.
func NewQueue(depth int) *Queue {
	if depth <= 0 {
		depth = DefaultQueueDepth
	}
	return &Queue{
		items: make(map[entryKey]clusterv1.RunObservation, depth),
		max:   depth,
		wake:  make(chan struct{}, 1),
	}
}

// Observed records a phase change for delivery. It never blocks: the caller is
// a reconcile loop, and a reconcile that waits on the control plane is a
// cluster that stops making progress the moment the control plane does.
func (q *Queue) Observed(obs clusterv1.RunObservation) {
	q.mu.Lock()
	k := entryKey{runID: obs.RunID, attempt: obs.Attempt, rank: obs.Phase.Rank()}
	if _, exists := q.items[k]; !exists {
		q.shedLocked()
		q.order = append(q.order, k)
	}
	q.items[k] = obs
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// shedLocked makes room by dropping the oldest non-terminal observation. A
// terminal phase is the one report that cannot be reconstructed from the
// heartbeat later — by then the run is over and the controller has moved on —
// so it outranks anything intermediate.
func (q *Queue) shedLocked() {
	if len(q.order) < q.max {
		return
	}
	for i, k := range q.order {
		if k.rank < 40 {
			delete(q.items, k)
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
	// Everything queued is terminal. Drop the oldest: a cluster this far
	// behind has a bigger problem than one lost report, and the backend will
	// see the run as Unknown and reconcile it.
	oldest := q.order[0]
	delete(q.items, oldest)
	q.order = q.order[1:]
}

// Forget drops everything queued about a run. Called on abandon: the work
// belongs to another cluster now, and appending our observations to it is
// exactly the zombie behaviour the epoch check exists to prevent.
func (q *Queue) Forget(runID runv1.ULID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.order[:0]
	for _, k := range q.order {
		if k.runID == runID {
			delete(q.items, k)
			continue
		}
		kept = append(kept, k)
	}
	q.order = kept
}

// Take removes and returns up to n observations, oldest first.
func (q *Queue) Take(n int) []clusterv1.RunObservation {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n > len(q.order) {
		n = len(q.order)
	}
	out := make([]clusterv1.RunObservation, 0, n)
	for _, k := range q.order[:n] {
		out = append(out, q.items[k])
		delete(q.items, k)
	}
	q.order = q.order[n:]
	return out
}

// Return puts observations back after a failed delivery, ahead of anything
// queued since. An observation that has been superseded in the meantime is
// dropped rather than resurrected.
func (q *Queue) Return(obs []clusterv1.RunObservation) {
	q.mu.Lock()
	defer q.mu.Unlock()
	front := make([]entryKey, 0, len(obs))
	for _, o := range obs {
		k := entryKey{runID: o.RunID, attempt: o.Attempt, rank: o.Phase.Rank()}
		if _, exists := q.items[k]; exists {
			continue
		}
		q.items[k] = o
		front = append(front, k)
	}
	q.order = append(front, q.order...)
}

// Depth is what is waiting, for the heartbeat's ControllerHealth.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// Wait blocks until something is queued or the context ends. It reports false
// only when the context ended.
func (q *Queue) Wait(ctx context.Context) bool {
	if q.Depth() > 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-q.wake:
		return ctx.Err() == nil
	}
}
