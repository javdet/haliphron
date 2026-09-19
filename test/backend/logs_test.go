package backend

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// A log sink the tests can read.
//
// One test exists because of it, and it is not a formality: the lease and the
// artifact bundle are the only messages in this system carrying secret
// material in the clear, and section 9 of the contract requires their bodies to
// be absent from the logs at any level. That is a property nobody notices
// breaking — a debug line added during an incident is how it breaks — so it is
// asserted rather than reviewed.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
	level slog.Level
}

func newLogRecorder() *logRecorder {
	// Debug, deliberately. The contract's rule is "at any level", so the test
	// has to be able to see everything the backend would ever emit.
	return &logRecorder{level: slog.LevelDebug}
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Level.String())
	b.WriteByte(' ')
	b.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, b.String())
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// Lines returns everything logged so far.
func (r *logRecorder) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// Contains reports whether any line holds the substring. Used to prove a
// secret is absent, so it looks at every level and every field.
func (r *logRecorder) Contains(needle string) bool {
	for _, line := range r.Lines() {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
