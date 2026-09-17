package entrypoint

import (
	"bytes"
	"sort"
	"sync"
)

// Redaction is mandatory on every byte this process uploads: the log chunks,
// agent.log, result.md and the summary in the report.
//
// The reason is not an attacker. A secret reaches a log through normal
// operation — a model debugging a failed request prints the headers, `set -x`
// in a repository script prints the arguments, an MCP server prints its
// configuration at startup — and once there it becomes durable and travels to
// storage with a thirty-day retention. This filter does not protect against an
// agent that wants to exfiltrate a secret; it protects against all the other
// ways, and those are the overwhelming majority.

// minRedactableLen is the shortest value worth replacing. Below it the false
// positives cost more than the protection is worth: a four-character token
// would blank every occurrence of those four characters in the diff the agent
// was explaining, and a log full of *** teaches the reader to stop reading.
const minRedactableLen = 8

// Mask is what replaces a secret. Fixed-width rather than proportional to the
// secret's length, which would leak that length.
const Mask = "***"

// Redactor replaces known secret values in a byte stream. Safe for concurrent
// use: the log uploader runs on its own goroutine while the main one writes the
// report.
type Redactor struct {
	mu sync.RWMutex
	// values are sorted longest first, so that a secret containing another
	// secret as a prefix is masked whole rather than leaving its tail exposed.
	values [][]byte
}

// NewRedactor builds a filter over the values it is worth hiding. Values below
// the length floor and duplicates are dropped.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add extends the filter. It is called again after the role and MCP phases,
// which can introduce header values that were not known at startup.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if len(v) < minRedactableLen {
			continue
		}
		b := []byte(v)
		if containsBytes(r.values, b) {
			continue
		}
		r.values = append(r.values, b)
	}
	sort.SliceStable(r.values, func(i, j int) bool {
		return len(r.values[i]) > len(r.values[j])
	})
}

// Bytes returns a copy with every known secret replaced.
func (r *Redactor) Bytes(b []byte) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := b
	copied := false
	for _, secret := range r.values {
		if !bytes.Contains(out, secret) {
			continue
		}
		if !copied {
			out = append([]byte(nil), out...)
			copied = true
		}
		out = bytes.ReplaceAll(out, secret, []byte(Mask))
	}
	if !copied {
		return append([]byte(nil), b...)
	}
	return out
}

// String is Bytes for text.
func (r *Redactor) String(s string) string { return string(r.Bytes([]byte(s))) }

func containsBytes(haystack [][]byte, needle []byte) bool {
	for _, v := range haystack {
		if bytes.Equal(v, needle) {
			return true
		}
	}
	return false
}
