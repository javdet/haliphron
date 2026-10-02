package backend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/metrics"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Run statistics: the UI's statistics page and the Prometheus endpoint.
//
// Both read the same use case, and the property that matters is that they
// agree with each other and with the run list: a number on a dashboard that
// differs from the number of rows a user can click through to is a number
// nobody trusts again.

type statsBody struct {
	Since    *time.Time `json:"since"`
	Clusters []struct {
		ClusterID   *string          `json:"cluster_id"`
		ClusterName string           `json:"cluster_name"`
		Counts      map[string]int64 `json:"counts"`
		Total       int64            `json:"total"`
		Duration    struct {
			P50 *float64 `json:"p50"`
			P95 *float64 `json:"p95"`
		} `json:"duration_seconds"`
		QueueWait struct {
			P50 *float64 `json:"p50"`
			P95 *float64 `json:"p95"`
		} `json:"queue_wait_seconds"`
	} `json:"clusters"`
}

// aSpreadOfRuns puts four runs in four places: one that succeeded with its
// completion, one whose terminal report arrived without one, one still
// running, all on cluster east, and one no cluster can take — a codex run,
// which east does not offer.
func aSpreadOfRuns(t *testing.T, h *harness) *probe {
	t.Helper()
	p := newProbe(t, h, "east")

	drive := func(phases ...runv1.Phase) clusterv1.Lease {
		h.Submit()
		lease := p.LeaseOne()
		if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
			t.Fatalf("ack: %+v", problem)
		}
		for _, phase := range phases {
			_, problem := p.Phase(lease.RunID, lease.Epoch, 1, phase)
			fatalIfProblem(t, "report "+string(phase), problem)
		}
		return lease
	}

	done := drive(runv1.PhaseRunning, runv1.PhaseSucceeded)
	_, problem := p.Complete(done.RunID, done.Epoch, 1, completionFor(done.RunID, 1, "0.100000", ""))
	fatalIfProblem(t, "completion", problem)
	drive(runv1.PhaseRunning, runv1.PhaseFailed)
	drive(runv1.PhaseRunning)
	h.Submit(func(r *run.SubmitRequest) { r.Agent = runv1.AgentCodex })
	return p
}

func TestTheRunStatisticsCountEachClusterByReportedStatus(t *testing.T) {
	h := newHarness(t)
	aSpreadOfRuns(t, h)
	reader := h.Token(store.ScopeRunsRead)

	var body statsBody
	if status := h.call(t, reader, http.MethodGet, "/api/v1/stats/runs", nil, &body); status != http.StatusOK {
		t.Fatalf("stats: %d", status)
	}
	if body.Since != nil {
		t.Errorf("since = %v, want null without a window", body.Since)
	}
	if len(body.Clusters) != 2 {
		t.Fatalf("clusters = %+v, want east and the unplaced row", body.Clusters)
	}

	var east, unplaced = body.Clusters[0], body.Clusters[1]
	if east.ClusterName != "east" {
		east, unplaced = unplaced, east
	}
	if east.ClusterID == nil || east.ClusterName != "east" {
		t.Fatalf("no row for east: %+v", body.Clusters)
	}
	want := map[string]int64{
		clusterv1.StatusSucceeded:              1,
		clusterv1.StatusCompletedWithoutResult: 1,
		clusterv1.StatusRunning:                1,
	}
	for status, n := range want {
		if east.Counts[status] != n {
			t.Errorf("east %s = %d, want %d (counts %v)", status, east.Counts[status], n, east.Counts)
		}
	}
	if east.Total != 3 {
		t.Errorf("east total %d, want 3", east.Total)
	}
	if east.Duration.P50 == nil || east.QueueWait.P50 == nil {
		t.Errorf("east has no duration or queue wait: %+v %+v", east.Duration, east.QueueWait)
	}

	if unplaced.ClusterID != nil || unplaced.ClusterName != "" {
		t.Errorf("the unplaced row names a cluster: %+v", unplaced)
	}
	if unplaced.Counts[clusterv1.StatusQueued] != 1 || unplaced.Total != 1 {
		t.Errorf("unplaced counts = %v, want one Queued", unplaced.Counts)
	}
	if unplaced.Duration.P50 != nil {
		t.Errorf("a run that never started has a duration: %v", *unplaced.Duration.P50)
	}
}

// The live count is not windowed: a run started yesterday and still running is
// on the cluster now, whatever window the statistics page shows.
func TestTheClusterListCarriesTheRunsActiveOnEachCluster(t *testing.T) {
	h := newHarness(t)
	aSpreadOfRuns(t, h)

	var body struct {
		Clusters []struct {
			Name       string `json:"name"`
			ActiveRuns int64  `json:"active_runs"`
		} `json:"clusters"`
	}
	if status := h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/clusters", nil, &body); status != http.StatusOK {
		t.Fatalf("list clusters: %d", status)
	}
	if len(body.Clusters) != 1 || body.Clusters[0].Name != "east" || body.Clusters[0].ActiveRuns != 1 {
		t.Fatalf("clusters = %+v, want east with one active run", body.Clusters)
	}
}

func TestTheRunStatisticsHonourTheSinceWindow(t *testing.T) {
	h := newHarness(t)
	aSpreadOfRuns(t, h)
	reader := h.Token(store.ScopeRunsRead)

	path := func(since time.Time) string {
		return "/api/v1/stats/runs?since=" + url.QueryEscape(since.Format(time.RFC3339))
	}

	var past statsBody
	if status := h.call(t, reader, http.MethodGet, path(time.Now().Add(-time.Hour)), nil, &past); status != http.StatusOK {
		t.Fatalf("stats since an hour ago: %d", status)
	}
	if past.Since == nil || len(past.Clusters) != 2 {
		t.Errorf("an hour's window = %+v, want every run", past)
	}

	var future statsBody
	if status := h.call(t, reader, http.MethodGet, path(time.Now().Add(time.Hour)), nil, &future); status != http.StatusOK {
		t.Fatalf("stats since an hour from now: %d", status)
	}
	if len(future.Clusters) != 0 {
		t.Errorf("a window that starts in the future counted %+v", future.Clusters)
	}

	var bad errorBody
	if status := h.call(t, reader, http.MethodGet, "/api/v1/stats/runs?since=yesterday", nil, &bad); status != http.StatusBadRequest {
		t.Fatalf("a malformed since: %d, want 400", status)
	}
	if bad.Error.Field != "since" {
		t.Errorf("field = %q, want since", bad.Error.Field)
	}
}

func TestTheStatisticsEndpointNeedsRunsRead(t *testing.T) {
	h := newHarness(t)
	if status := h.call(t, "", http.MethodGet, "/api/v1/stats/runs", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", status)
	}
	if status := h.call(t, h.Token(store.ScopeRunsRead), http.MethodGet, "/api/v1/stats/runs", nil, nil); status != http.StatusOK {
		t.Fatalf("runs:read: %d, want 200", status)
	}
}

// The scrape and the API read the same use case, so they must give the same
// numbers; and every cluster gets its capacity and its status series.
func TestTheMetricsEndpointExposesTheSameCountsAsTheAPI(t *testing.T) {
	h := newHarness(t)
	p := aSpreadOfRuns(t, h)
	// Partial, so that the run east is not listing is not taken as gone.
	p.Heartbeat(false)

	srv := httptest.NewServer(metrics.Handler(h.App, nil))
	t.Cleanup(srv.Close)
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read scrape: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape: %d\n%s", resp.StatusCode, raw)
	}
	text := string(raw)

	for _, line := range []string{
		`haliphron_stats_up 1`,
		`haliphron_runs{cluster="east",status="Succeeded"} 1`,
		`haliphron_runs{cluster="east",status="CompletedWithoutResult"} 1`,
		`haliphron_runs{cluster="east",status="Running"} 1`,
		`haliphron_runs{cluster="",status="Queued"} 1`,
		`haliphron_cluster_active_runs{cluster="east"} 1`,
		`haliphron_cluster_capacity_slots{cluster="east"} 10`,
		`haliphron_cluster_status{cluster="east",status="Active"} 1`,
		`haliphron_cluster_status{cluster="east",status="Revoked"} 0`,
		`haliphron_run_duration_seconds_count{cluster="east"} 2`,
		`haliphron_run_queue_wait_seconds_count{cluster="east"} 3`,
		`haliphron_run_duration_seconds_bucket{cluster="east",le="+Inf"} 2`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("scrape lacks %q", line)
		}
	}
	if !strings.Contains(text, `haliphron_cluster_last_heartbeat_timestamp_seconds{cluster="east"}`) {
		t.Error("scrape has no heartbeat time for east")
	}
}
