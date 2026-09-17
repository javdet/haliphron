package backend

import (
	"strings"
	"sync"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Crockford base32, the ULID alphabet: no I, L, O or U, so a transcribed
// identifier cannot turn into a different valid one.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidGen produces identifiers that sort by creation order, which is what makes
// the queue's "oldest first" a plain ORDER BY in the real backend and a stable
// expectation in tests here.
//
// It is not a full ULID implementation: the entropy is a counter rather than
// random bytes. That is deliberate. A test that fails on the eleventh run
// should print the same identifier when it is re-run, and randomness buys a
// fake nothing — there is exactly one writer.
type ulidGen struct {
	mu      sync.Mutex
	counter uint64
}

func (g *ulidGen) next(now time.Time) runv1.ULID {
	g.mu.Lock()
	g.counter++
	n := g.counter
	g.mu.Unlock()

	var b strings.Builder
	b.Grow(26)
	ms := uint64(now.UnixMilli())
	// 48 bits of timestamp, 10 characters.
	for shift := 45; shift >= 0; shift -= 5 {
		b.WriteByte(crockford[(ms>>uint(shift))&0x1f])
	}
	// 80 bits of "entropy", 16 characters, holding the counter in the low bits.
	// The top three characters are always zero: a uint64 counter does not
	// reach them, and padding them keeps the length at the 26 the pattern
	// requires.
	for shift := 75; shift >= 0; shift -= 5 {
		var v uint64
		if shift < 64 {
			v = (n >> uint(shift)) & 0x1f
		}
		b.WriteByte(crockford[v])
	}
	return runv1.ULID(b.String())
}
