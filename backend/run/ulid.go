// Package run is the backend's domain: the rules that decide what a run means,
// independent of how it is stored or how it arrived.
//
// Two things live here rather than in the store or the handlers. The report
// decision table, because it is the one piece of logic /ingest/status, the
// heartbeat and the completion path are obliged to share — three copies of it
// would drift, and the way that drift presents is a periodic heartbeat rolling
// back state the low-latency path delivered. And admission rendering, because
// a RenderedRunSpec is written once and handed out byte-for-byte on every
// lease afterwards, so what goes into it is a decision about the run rather
// than about the request that created it.
package run

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// crockford is the ULID alphabet: I, L, O and U are left out so that an
// identifier read off a screen and typed into a shell cannot become a
// different valid one.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID mints an identifier: 48 bits of millisecond timestamp and 80 bits of
// randomness, Crockford base32, 26 characters.
//
// The identifier is minted here rather than by the database because it is the
// same string in five places — the runs row, the AgentRun object name, the S3
// prefix, the JWT subject and every log line about the run — and a value the
// database assigns cannot be used to build the storage key the same
// transaction writes.
//
// Sorting lexicographically by mint time is not decoration either: it is what
// makes the queue's "oldest first" an ordinary index scan and keeps the
// primary key's inserts local, which a random uuid does not.
func NewULID(now time.Time) (runv1.ULID, error) {
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("mint ulid: %w", err)
	}
	return encodeULID(now, entropy), nil
}

// MustULID is NewULID for callers that have nothing to do with a failure of the
// system random source. A process that cannot read randomness cannot mint
// tokens either, and continuing would produce predictable identifiers.
func MustULID(now time.Time) runv1.ULID {
	id, err := NewULID(now)
	if err != nil {
		panic(err)
	}
	return id
}

func encodeULID(now time.Time, entropy [10]byte) runv1.ULID {
	var b strings.Builder
	b.Grow(26)

	ms := uint64(now.UnixMilli())
	for shift := 45; shift >= 0; shift -= 5 {
		b.WriteByte(crockford[(ms>>uint(shift))&0x1f])
	}

	// The 80 bits of entropy are read as two integers so the shifting stays
	// plain: 16 bits then 64, emitted most significant first.
	hi := uint64(binary.BigEndian.Uint16(entropy[0:2]))
	lo := binary.BigEndian.Uint64(entropy[2:10])
	for shift := 11; shift >= 0; shift -= 5 {
		b.WriteByte(crockford[(hi>>uint(shift))&0x1f])
	}
	// 16 bits do not divide into 5, so the third character of the entropy
	// carries one bit from hi and four from lo.
	b.WriteByte(crockford[((hi&0x1)<<4)|(lo>>60)&0xf])
	for shift := 55; shift >= 0; shift -= 5 {
		b.WriteByte(crockford[(lo>>uint(shift))&0x1f])
	}
	return runv1.ULID(b.String())
}

// ValidULID reports whether a string is one. The check is the same regex the
// wire contract and the ulid domain in the schema state, written out: a value
// that fails here would be refused by the database anyway, and refusing it at
// the edge is the difference between a 400 and a 500.
func ValidULID(s runv1.ULID) bool {
	if len(s) != 26 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(crockford, rune(s[i])) {
			return false
		}
	}
	return true
}
