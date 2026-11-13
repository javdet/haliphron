package reconcile

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/launcher"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// DefaultResync is how often every row is reconciled whatever the events say.
// Events are a hint; the resync is what guarantees convergence.
const DefaultResync = 30 * time.Second

// Driver feeds the reconciler: a keyed queue with one worker at a time per run,
// fed by the daemon's events, by row writes (Enqueue), by each reconcile's own
// requeue time, and by a full resync.
type Driver struct {
	Reconciler *Reconciler
	Engine     docker.Engine
	Store      *state.Store
	Workers    int
	Resync     time.Duration
	// OnDaemonReconnect runs after the events stream broke and came back —
	// usually a daemon restart, after which the network check must run again.
	OnDaemonReconnect func()
	Log               *slog.Logger

	once sync.Once
	q    *keyQueue
}

func (d *Driver) queue() *keyQueue {
	d.once.Do(func() { d.q = newKeyQueue() })
	return d.q
}

// Enqueue asks for a run to be reconciled now.
func (d *Driver) Enqueue(runID runv1.ULID) { d.queue().add(runID, time.Now()) }

// EnqueueAfter asks for a run to be reconciled later.
func (d *Driver) EnqueueAfter(runID runv1.ULID, after time.Duration) {
	d.queue().add(runID, time.Now().Add(after))
}

// Start runs until the context ends.
func (d *Driver) Start(ctx context.Context) error {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	workers := d.Workers
	if workers <= 0 {
		workers = 4
	}
	resync := d.Resync
	if resync <= 0 {
		resync = DefaultResync
	}
	q := d.queue()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				id, ok := q.get(ctx)
				if !ok {
					return
				}
				wait, err := d.Reconciler.Reconcile(ctx, id)
				if err != nil && ctx.Err() == nil {
					log.Error("reconciling a run", "runID", id, "error", err)
					wait = minPositive(wait, 5*time.Second)
				}
				q.done(id)
				if wait > 0 {
					q.add(id, time.Now().Add(wait))
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.resyncLoop(ctx, resync, log)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.eventsLoop(ctx, log)
	}()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// ResyncNow enqueues every row.
func (d *Driver) ResyncNow(ctx context.Context) error {
	rows, err := d.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		d.Enqueue(r.RunID)
	}
	return nil
}

func (d *Driver) resyncLoop(ctx context.Context, every time.Duration, log *slog.Logger) {
	for {
		if err := d.ResyncNow(ctx); err != nil && ctx.Err() == nil {
			log.Error("resync", "error", err)
		}
		if !clusterapi.Wait(ctx, every) {
			return
		}
	}
}

// eventsLoop turns container events into enqueues, and reconnects when the
// stream breaks. A reconnect is followed by a full resync: whatever happened
// while the stream was down produced events nobody heard.
func (d *Driver) eventsLoop(ctx context.Context, log *slog.Logger) {
	backoff := clusterapi.DefaultBackoff()
	first := true
	for ctx.Err() == nil {
		events, errs := d.Engine.Events(ctx, map[string]string{launcher.LabelManagedBy: launcher.ManagedBy})
		if !first {
			_ = d.ResyncNow(ctx)
			if d.OnDaemonReconnect != nil {
				d.OnDaemonReconnect()
			}
		}
		first = false
		received := false
		for ev := range events {
			received = true
			if id := ev.Actor.Attributes[launcher.LabelRunID]; id != "" {
				d.Enqueue(runv1.ULID(strings.ToUpper(id)))
			}
		}
		err := <-errs
		if ctx.Err() != nil {
			return
		}
		if received {
			backoff.Reset()
		}
		log.Warn("the docker events stream ended; reconnecting", "error", err)
		if !clusterapi.Wait(ctx, backoff.Next()) {
			return
		}
	}
}

// keyQueue holds at most one pending entry per run, with the earliest due
// time, and never hands the same run to two workers. A run enqueued while it is
// being reconciled is held back and released when that reconcile is done.
type keyQueue struct {
	mu     sync.Mutex
	due    map[runv1.ULID]time.Time
	busy   map[runv1.ULID]bool
	again  map[runv1.ULID]time.Time
	signal chan struct{}
}

func newKeyQueue() *keyQueue {
	return &keyQueue{
		due:    map[runv1.ULID]time.Time{},
		busy:   map[runv1.ULID]bool{},
		again:  map[runv1.ULID]time.Time{},
		signal: make(chan struct{}),
	}
}

// wakeLocked wakes every waiting worker by closing the current channel.
func (q *keyQueue) wakeLocked() {
	close(q.signal)
	q.signal = make(chan struct{})
}

func (q *keyQueue) add(id runv1.ULID, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	target := q.due
	if q.busy[id] {
		target = q.again
	}
	if cur, ok := target[id]; !ok || at.Before(cur) {
		target[id] = at
	}
	q.wakeLocked()
}

func (q *keyQueue) done(id runv1.ULID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.busy, id)
	if at, ok := q.again[id]; ok {
		delete(q.again, id)
		if cur, ok := q.due[id]; !ok || at.Before(cur) {
			q.due[id] = at
		}
	}
	q.wakeLocked()
}

func (q *keyQueue) get(ctx context.Context) (runv1.ULID, bool) {
	for {
		q.mu.Lock()
		now := time.Now()
		var (
			best    runv1.ULID
			bestAt  time.Time
			found   bool
			nextDue time.Time
		)
		for id, at := range q.due {
			if q.busy[id] {
				continue
			}
			if !at.After(now) {
				if !found || at.Before(bestAt) {
					best, bestAt, found = id, at, true
				}
			} else if nextDue.IsZero() || at.Before(nextDue) {
				nextDue = at
			}
		}
		if found {
			delete(q.due, best)
			q.busy[best] = true
			q.mu.Unlock()
			return best, true
		}
		wake := q.signal
		q.mu.Unlock()

		var (
			timer <-chan time.Time
			t     *time.Timer
		)
		if !nextDue.IsZero() {
			t = time.NewTimer(time.Until(nextDue))
			timer = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return "", false
		case <-wake:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
	}
}
