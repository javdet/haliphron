package app

import (
	"context"
	"time"

	"github.com/automagicops/haliphron/backend/store"
)

// Statistics, read by the REST endpoint and the Prometheus collector alike, so
// that the page and the dashboard cannot disagree about what a number means.

// RunStats counts runs per cluster and reported status since a point in time,
// or over every run when since is nil.
func (s *Service) RunStats(ctx context.Context, since *time.Time) ([]store.ClusterRunStats, error) {
	return s.store.RunStats(ctx, since)
}

// RunHistograms buckets run duration and queue wait per cluster over every run.
func (s *Service) RunHistograms(ctx context.Context, durationBounds, waitBounds []float64) ([]store.ClusterHistograms, error) {
	return s.store.RunHistograms(ctx, durationBounds, waitBounds)
}

// ClusterHealth is one cluster's capacity and liveness, as Prometheus sees it.
type ClusterHealth struct {
	Cluster    store.Cluster
	ActiveRuns int64
}

// ClusterHealth lists every cluster with the runs the backend believes it is
// running.
func (s *Service) ClusterHealth(ctx context.Context) ([]ClusterHealth, error) {
	clusters, err := s.store.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	active, err := s.store.ActiveRunsByCluster(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ClusterHealth, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, ClusterHealth{Cluster: c, ActiveRuns: active[c.ID]})
	}
	return out, nil
}
