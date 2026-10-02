package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Statistics over runs: what the UI's statistics page and the Prometheus
// endpoint both read.
//
// Every number is computed from the table, never counted in process. There are
// several replicas, split installs serve different listeners from different
// Deployments, and a counter held in memory would know only the share of the
// traffic its own process saw and forget it on restart. A query answers the
// same on every replica.
//
// The status is the reported status, spelled in SQL exactly as
// Run.ReportedStatus spells it in Go: a terminal phase whose completion has not
// arrived is CompletedWithoutResult. observed_rank 40 is the terminal rank, the
// one generated column that cannot drift from observed_phase.

// reportedStatusSQL is Run.ReportedStatus as an expression.
const reportedStatusSQL = `CASE WHEN r.observed_rank = 40 AND r.completion_received_at IS NULL
	THEN 'CompletedWithoutResult' ELSE r.status::text END`

// durationSQL is how long the agent ran: from the first Running observation to
// the terminal one. A run that never started has none.
const durationSQL = `EXTRACT(EPOCH FROM r.finished_at - r.started_at)::float8`

// queueWaitSQL is how long the run waited for a cluster to start it. From
// queued_at rather than created_at, because a retry queues the run again and
// the first wait has nothing to say about the second.
const queueWaitSQL = `EXTRACT(EPOCH FROM r.started_at - r.queued_at)::float8`

// Quantiles is a p50 and a p95, absent when nothing was measured.
type Quantiles struct {
	P50 *float64
	P95 *float64
}

// ClusterRunStats is one cluster's runs in a window. ClusterID is empty for
// runs no cluster has taken yet.
type ClusterRunStats struct {
	ClusterID   runv1.ULID
	ClusterName string
	Counts      map[string]int64
	Total       int64
	Duration    Quantiles
	QueueWait   Quantiles
}

// RunStats counts runs per cluster and reported status, created at or after
// since (every run when since is nil), with the duration and queue-wait
// quantiles of the same set.
func (s *Store) RunStats(ctx context.Context, since *time.Time) ([]ClusterRunStats, error) {
	var bound any
	if since != nil {
		bound = *since
	}

	byCluster := map[runv1.ULID]*ClusterRunStats{}
	var order []runv1.ULID
	get := func(id runv1.ULID, name string) *ClusterRunStats {
		c, ok := byCluster[id]
		if !ok {
			c = &ClusterRunStats{ClusterID: id, ClusterName: name, Counts: map[string]int64{}}
			byCluster[id] = c
			order = append(order, id)
		}
		return c
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.cluster_id, COALESCE(c.name, ''), `+reportedStatusSQL+`, count(*)
		FROM runs r LEFT JOIN clusters c ON c.id = r.cluster_id
		WHERE $1::timestamptz IS NULL OR r.created_at >= $1::timestamptz
		GROUP BY 1, 2, 3
		ORDER BY 2, 1, 3`, bound)
	if err != nil {
		return nil, fmt.Errorf("store: run stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     sql.NullString
			name   string
			status string
			n      int64
		)
		if err := rows.Scan(&id, &name, &status, &n); err != nil {
			return nil, fmt.Errorf("store: run stats: %w", err)
		}
		c := get(runv1.ULID(id.String), name)
		c.Counts[status] = n
		c.Total += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: run stats: %w", err)
	}

	rows, err = s.db.QueryContext(ctx, `
		SELECT r.cluster_id,
		       percentile_cont(0.5)  WITHIN GROUP (ORDER BY `+durationSQL+`),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY `+durationSQL+`),
		       percentile_cont(0.5)  WITHIN GROUP (ORDER BY `+queueWaitSQL+`),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY `+queueWaitSQL+`)
		FROM runs r
		WHERE ($1::timestamptz IS NULL OR r.created_at >= $1::timestamptz)
		  AND r.started_at IS NOT NULL
		GROUP BY r.cluster_id`, bound)
	if err != nil {
		return nil, fmt.Errorf("store: run quantiles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id                 sql.NullString
			d50, d95, w50, w95 sql.NullFloat64
		)
		if err := rows.Scan(&id, &d50, &d95, &w50, &w95); err != nil {
			return nil, fmt.Errorf("store: run quantiles: %w", err)
		}
		c, ok := byCluster[runv1.ULID(id.String)]
		if !ok {
			continue
		}
		c.Duration = Quantiles{P50: nullFloat(d50), P95: nullFloat(d95)}
		c.QueueWait = Quantiles{P50: nullFloat(w50), P95: nullFloat(w95)}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: run quantiles: %w", err)
	}

	out := make([]ClusterRunStats, 0, len(order))
	for _, id := range order {
		out = append(out, *byCluster[id])
	}
	return out, nil
}

// Histogram is a cumulative histogram as Prometheus exposes one: Buckets[i] is
// the number of observations at or below the i-th upper bound.
type Histogram struct {
	Count   uint64
	Sum     float64
	Buckets []uint64
}

// ClusterHistograms is one cluster's duration and queue-wait histograms over
// every run the table holds.
type ClusterHistograms struct {
	ClusterID   runv1.ULID
	ClusterName string
	Duration    Histogram
	QueueWait   Histogram
}

// RunHistograms buckets duration and queue wait per cluster, over all runs.
// Duration covers finished runs, queue wait every run that started.
//
// One row per cluster, series and bound rather than an array per cluster: the
// package reads no arrays but text[], and the cost is rows × bounds, which a
// dozen bounds keeps small.
func (s *Store) RunHistograms(ctx context.Context, durationBounds, waitBounds []float64) ([]ClusterHistograms, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH m AS (
			SELECT r.cluster_id, c.name,
			       CASE WHEN r.finished_at IS NOT NULL THEN `+durationSQL+` END AS d,
			       `+queueWaitSQL+` AS w
			FROM runs r JOIN clusters c ON c.id = r.cluster_id
			WHERE r.started_at IS NOT NULL
		)
		SELECT cluster_id, name, 'd', 0, count(d), COALESCE(sum(d), 0) FROM m GROUP BY 1, 2
		UNION ALL
		SELECT cluster_id, name, 'w', 0, count(w), COALESCE(sum(w), 0) FROM m GROUP BY 1, 2
		UNION ALL
		SELECT m.cluster_id, m.name, 'd', u.i, count(*) FILTER (WHERE m.d <= u.b), 0
		FROM m CROSS JOIN unnest($1::float8[]) WITH ORDINALITY AS u(b, i) GROUP BY 1, 2, 4
		UNION ALL
		SELECT m.cluster_id, m.name, 'w', u.i, count(*) FILTER (WHERE m.w <= u.b), 0
		FROM m CROSS JOIN unnest($2::float8[]) WITH ORDINALITY AS u(b, i) GROUP BY 1, 2, 4
		ORDER BY 2, 1, 3, 4`, floatArray(durationBounds), floatArray(waitBounds))
	if err != nil {
		return nil, fmt.Errorf("store: run histograms: %w", err)
	}
	defer rows.Close()

	var (
		out   []ClusterHistograms
		index = map[runv1.ULID]int{}
	)
	for rows.Next() {
		var (
			id, name, series string
			bound            int
			count            int64
			sum              float64
		)
		if err := rows.Scan(&id, &name, &series, &bound, &count, &sum); err != nil {
			return nil, fmt.Errorf("store: run histograms: %w", err)
		}
		i, ok := index[runv1.ULID(id)]
		if !ok {
			i = len(out)
			index[runv1.ULID(id)] = i
			out = append(out, ClusterHistograms{
				ClusterID: runv1.ULID(id), ClusterName: name,
				Duration:  Histogram{Buckets: make([]uint64, len(durationBounds))},
				QueueWait: Histogram{Buckets: make([]uint64, len(waitBounds))},
			})
		}
		h := &out[i].Duration
		if series == "w" {
			h = &out[i].QueueWait
		}
		if bound == 0 {
			h.Count, h.Sum = uint64(count), sum
			continue
		}
		// WITH ORDINALITY counts from 1.
		h.Buckets[bound-1] = uint64(count)
	}
	return out, rows.Err()
}

// ActiveRunsByCluster counts the runs the backend believes are on each
// cluster: the set runs_active_by_cluster indexes.
func (s *Store) ActiveRunsByCluster(ctx context.Context) (map[runv1.ULID]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cluster_id, count(*) FROM runs
		WHERE status IN ('Leased', 'Dispatched', 'Starting', 'Running', 'Unknown')
		  AND cluster_id IS NOT NULL
		GROUP BY cluster_id`)
	if err != nil {
		return nil, fmt.Errorf("store: active runs: %w", err)
	}
	defer rows.Close()
	out := map[runv1.ULID]int64{}
	for rows.Next() {
		var (
			id string
			n  int64
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: active runs: %w", err)
		}
		out[runv1.ULID(id)] = n
	}
	return out, rows.Err()
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// floatArray renders bounds as a float8[] literal, the way textArray renders
// text.
func floatArray(in []float64) string {
	parts := make([]string, len(in))
	for i, v := range in {
		parts[i] = strconv.FormatFloat(v, 'g', -1, 64)
	}
	return "{" + strings.Join(parts, ",") + "}"
}
