package clusterapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

type stubSigner struct{ id runv1.ULID }

func (s stubSigner) ClusterID() runv1.ULID           { return s.id }
func (s stubSigner) Token(time.Time) (string, error) { return "token", nil }

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *strings.Builder) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	var logged strings.Builder
	client, err := New(Options{
		BaseURL:           server.URL,
		ControllerVersion: "0.1.0",
		Signer:            stubSigner{id: "01J8X4K2ZQ7YB3M9F0R5W6T8CD"},
		Logger:            slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client, &logged
}

// TestAnUnknownFieldInALeaseIsIgnored. The control plane and the chart are
// upgraded independently, so a lease carrying a field this build has never
// heard of is a normal state. Both sides are obliged to ignore what they do not
// recognise — "obliged", not "may": the alternative is that rolling out a newer
// backend breaks every older controller in the fleet at once.
func TestAnUnknownFieldInALeaseIsIgnored(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"leases": [{
				"runID": "01J8X4K2ZQ7YB3M9F0R5W6T8CD",
				"epoch": 1, "attempt": 1,
				"sandboxProfile": "strict",
				"spec": {"agent": "claude-code", "unheardOf": 3},
				"secrets": {"git-token": "t"},
				"artifacts": {"bucket": "haliphron", "keyPrefix": "runs/x/"}
			}],
			"serverTime": "2026-09-17T10:00:00Z",
			"somethingElseEntirely": true
		}`))
	})

	resp, err := client.Lease(context.Background(), clusterv1.LeaseRequest{FreeSlots: 1})
	if err != nil {
		t.Fatalf("a lease with unknown fields was refused: %v", err)
	}
	if len(resp.Leases) != 1 || resp.Leases[0].Spec.Agent != runv1.AgentClaudeCode {
		t.Fatalf("the known fields did not survive: %+v", resp.Leases)
	}
}

// TestNoContentIsNotAnError. A 204 is the ordinary answer on an idle queue, and
// the caller re-polls at once: backing off here would turn a long poll back
// into polling.
func TestNoContentIsNotAnError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	resp, err := client.Lease(context.Background(), clusterv1.LeaseRequest{})
	if err != nil {
		t.Fatalf("204 came back as an error: %v", err)
	}
	if resp != nil {
		t.Fatalf("204 produced a response: %+v", resp)
	}
}

// TestBehaviourComesFromTheActionAndNeverFromTheCode. This is the single most
// load-bearing line of the error contract: two independently written sides
// always diverge on what a 409 means, and the action field is what stops them
// having to agree.
func TestBehaviourComesFromTheActionAndNeverFromTheCode(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"x","title":"stale epoch","status":409,
			"code":"EpochMismatch","action":"abandon","currentEpoch":4}`))
	})

	_, err := client.Ack(context.Background(), "01J8X4K2ZQ7YB3M9F0R5W6T8CD", clusterv1.AckRequest{Epoch: 3})
	if err == nil {
		t.Fatal("a 409 was not reported as a failure")
	}
	if got := ActionOf(err); got != clusterv1.ActionAbandon {
		t.Fatalf("want the action from the body, got %q", got)
	}
	problem, ok := ProblemOf(err)
	if !ok || problem.Code != clusterv1.CodeEpochMismatch || problem.CurrentEpoch != 4 {
		t.Fatalf("the problem document did not survive: %+v", problem)
	}
}

// TestAFailureWithoutAProblemDocumentIsFatal. A control plane answering 4xx in
// some other shape is not something a repeat fixes, and treating it as
// retriable is how a controller hammers a proxy that was never going to let it
// through.
func TestAFailureWithoutAProblemDocumentIsFatal(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>gateway</html>"))
	})
	_, err := client.Heartbeat(context.Background(), clusterv1.HeartbeatRequest{})
	if ActionOf(err) != clusterv1.ActionFatal {
		t.Fatalf("want fatal for an unparsable failure, got %q", ActionOf(err))
	}
}

// TestALeaseBodyIsNeverLogged. The lease is the one message in the system
// carrying secret material in the clear: an hour-long git token, a model key
// and bearer capabilities on a bucket prefix. The cost of a logging mistake is
// bounded by that hour, and the mistake is only ever noticed afterwards.
func TestALeaseBodyIsNeverLogged(t *testing.T) {
	const secret = "ghp_thisisthetokenthatmustnotappear"
	client, logged := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"leases":[{"runID":"01J8X4K2ZQ7YB3M9F0R5W6T8CD","epoch":1,
			"attempt":1,"spec":{"agent":"claude-code"},
			"secrets":{"git-token":"` + secret + `"},
			"artifacts":{"bucket":"b","keyPrefix":"runs/x/","put":{"result.md":
			{"url":"https://storage/b/runs/x/result.md?sig=deadbeef","method":"PUT"}}}}],
			"serverTime":"2026-09-17T10:00:00Z"}`))
	})

	if _, err := client.Lease(context.Background(), clusterv1.LeaseRequest{FreeSlots: 1}); err != nil {
		t.Fatalf("lease: %v", err)
	}
	for _, forbidden := range []string{secret, "sig=deadbeef", "storage/b/runs"} {
		if strings.Contains(logged.String(), forbidden) {
			t.Fatalf("the log contains %q", forbidden)
		}
	}
	if !strings.Contains(logged.String(), "leases received") {
		t.Fatalf("the lease was not logged at all; the count is the part worth keeping: %s", logged.String())
	}
}

// TestADroppedLongPollIsRepeatedNoMoreThanOncePerSecond. Proxies close idle
// connections and backends restart; both look identical from here. Repeating at
// once is right, and repeating at once *without a bound* is a denial of service
// mounted by a cluster that believes it is idle.
func TestADroppedLongPollIsRepeatedNoMoreThanOncePerSecond(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	pacer := &Pacer{MinInterval: time.Second}

	// The first call is free.
	if !pacer.Wait(context.Background(), clock) {
		t.Fatal("the first wait was cancelled")
	}

	start := time.Now()
	// The second, at the same instant on the caller's clock, must actually
	// sleep: the pacer waits in real time and only stamps the fake clock.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if pacer.Wait(ctx, clock) {
		t.Fatal("a second poll in the same instant was allowed through")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("the pacer did not wait: %s", elapsed)
	}
}

// TestAPollThatHungIsRepeatedImmediately is the other half of the same
// mechanism: the pacer stamps the start of a poll, so a long poll that held for
// its full thirty seconds has already served its own interval.
func TestAPollThatHungIsRepeatedImmediately(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	pacer := &Pacer{MinInterval: time.Second}
	pacer.Wait(context.Background(), clock)

	now = now.Add(30 * time.Second)
	start := time.Now()
	if !pacer.Wait(context.Background(), clock) {
		t.Fatal("the wait was cancelled")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("a poll that had already hung for thirty seconds was delayed again: %s", elapsed)
	}
}

// TestBackoffGrowsAndIsBounded. Exponential with jitter, base one second,
// ceiling sixty, and never zero: full jitter can draw a near-zero wait, and a
// hundred clusters drawing it at once is the herd the jitter exists to prevent.
func TestBackoffGrowsAndIsBounded(t *testing.T) {
	b := DefaultBackoff()
	var last time.Duration
	for i := 0; i < 20; i++ {
		d := b.Next()
		if d <= 0 {
			t.Fatalf("attempt %d waited %s", i, d)
		}
		if d > 60*time.Second {
			t.Fatalf("attempt %d exceeded the ceiling: %s", i, d)
		}
		last = d
	}
	if last < 30*time.Second {
		t.Fatalf("twenty failures in a row still wait only %s", last)
	}
	b.Reset()
	if d := b.Next(); d > time.Second {
		t.Fatalf("the sequence was not reset: %s", d)
	}
}
