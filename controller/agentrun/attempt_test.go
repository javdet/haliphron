package agentrun

import (
	"testing"
	"time"
)

// TestTheRetryScheduleIsAFloorThatDoublesFromThirtySeconds. A pod that fails
// in seconds must not produce its next Job in seconds: the pause before retry n
// is never shorter than 30 s doubled n-1 times, and never more than a tenth
// longer.
func TestTheRetryScheduleIsAFloorThatDoublesFromThirtySeconds(t *testing.T) {
	for _, tc := range []struct {
		retry int32
		floor time.Duration
	}{
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 5 * time.Minute},
		{10, 5 * time.Minute},
	} {
		for range 1000 {
			d := retryDelay(tc.retry)
			if d < tc.floor || d > tc.floor+tc.floor/10 {
				t.Fatalf("retry %d waits %s, want within [%s, %s]",
					tc.retry, d, tc.floor, tc.floor+tc.floor/10)
			}
		}
	}
}
