package reconcile

import (
	"path/filepath"
	"strings"
	"time"
)

func minPositive(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func atLeastSecond(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func equalTime(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

func equalInt(a, b *int32) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func lower(s string) string { return strings.ToLower(s) }

func parent(p string) string { return filepath.Dir(p) }
