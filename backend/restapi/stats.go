package restapi

import (
	"net/http"
	"time"

	"github.com/automagicops/haliphron/backend/store"
)

// ---------------------------------------------------------------------------
// statistics
// ---------------------------------------------------------------------------

type quantilesResponse struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
}

type clusterStatsResponse struct {
	// Null for runs no cluster has taken yet.
	ClusterID   *string           `json:"cluster_id"`
	ClusterName string            `json:"cluster_name"`
	Counts      map[string]int64  `json:"counts"`
	Total       int64             `json:"total"`
	Duration    quantilesResponse `json:"duration_seconds"`
	QueueWait   quantilesResponse `json:"queue_wait_seconds"`
}

type runStatsResponse struct {
	Since    *time.Time             `json:"since"`
	Clusters []clusterStatsResponse `json:"clusters"`
}

// runStats counts runs per cluster and reported status. since is optional and
// RFC 3339; without it every run is counted.
func (s *Server) runStats(w http.ResponseWriter, r *http.Request, _ caller) {
	var since *time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			s.fail(w, http.StatusBadRequest, "invalid_request",
				"since must be an RFC 3339 timestamp", "since")
			return
		}
		since = &t
	}

	stats, err := s.app.RunStats(r.Context(), since)
	if err != nil {
		s.failFor(w, r, err)
		return
	}

	items := make([]clusterStatsResponse, 0, len(stats))
	for _, c := range stats {
		items = append(items, clusterStatsView(c))
	}
	s.write(w, http.StatusOK, runStatsResponse{Since: since, Clusters: items})
}

func clusterStatsView(c store.ClusterRunStats) clusterStatsResponse {
	var id *string
	if c.ClusterID != "" {
		v := string(c.ClusterID)
		id = &v
	}
	return clusterStatsResponse{
		ClusterID: id, ClusterName: c.ClusterName,
		Counts: c.Counts, Total: c.Total,
		Duration:  quantilesResponse{P50: c.Duration.P50, P95: c.Duration.P95},
		QueueWait: quantilesResponse{P50: c.QueueWait.P50, P95: c.QueueWait.P95},
	}
}
