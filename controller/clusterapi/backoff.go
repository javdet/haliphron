package clusterapi

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff is what section 7 of the contract prescribes for the retry and
// backoff actions: exponential, with jitter, base one second, ceiling sixty,
// indefinitely. Indefinitely is the important word — a controller that gives up
// on an unreachable control plane stops reporting work it has already finished.
//
// The jitter is half-range rather than full: the delay is never less than half
// the current step, so a fleet of controllers reconnecting to a restarted
// backend spreads out without any of them spinning. Full jitter can draw a
// near-zero wait, and a hundred clusters drawing it at once is the thundering
// herd the jitter exists to prevent.
type Backoff struct {
	Base time.Duration
	Max  time.Duration

	step time.Duration
}

// DefaultBackoff is the contract's own parameters.
func DefaultBackoff() *Backoff {
	return &Backoff{Base: time.Second, Max: 60 * time.Second}
}

// Next advances the sequence and returns the delay to wait.
func (b *Backoff) Next() time.Duration {
	if b.Base <= 0 {
		b.Base = time.Second
	}
	if b.Max <= 0 {
		b.Max = 60 * time.Second
	}
	if b.step <= 0 {
		b.step = b.Base
	} else if b.step < b.Max {
		b.step *= 2
		if b.step > b.Max {
			b.step = b.Max
		}
	}
	half := b.step / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// Reset returns the sequence to the base. Called on every success: a run of
// failures an hour apart is not evidence that the next call needs a minute of
// patience.
func (b *Backoff) Reset() { b.step = 0 }

// Wait sleeps for at least d, and reports false if the context ended first.
func Wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Pacer bounds how often an operation may be repeated. It exists for one line
// of the contract: a dropped long poll is repeated at once, but no more than
// once per second. Without it a proxy that closes connections instantly turns
// the lease loop into a busy loop against the control plane, and the control
// plane sees a denial of service from a cluster that believes it is idle.
type Pacer struct {
	MinInterval time.Duration

	last time.Time
}

// Wait blocks until MinInterval has passed since the previous call, and
// reports false if the context ended first.
func (p *Pacer) Wait(ctx context.Context, now func() time.Time) bool {
	t := now()
	if !p.last.IsZero() {
		if elapsed := t.Sub(p.last); elapsed < p.MinInterval {
			if !Wait(ctx, p.MinInterval-elapsed) {
				return false
			}
			t = now()
		}
	}
	p.last = t
	return ctx.Err() == nil
}
