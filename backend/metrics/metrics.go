// Package metrics is the backend's Prometheus endpoint, served on the health
// listener at /metrics.
//
// Every series about runs and clusters is read from the database at scrape
// time, not counted in process. Replicas share the work, split installs serve
// different listeners from different Deployments, and a counter in memory
// would hold only the share its own process saw and lose it on restart. The
// consequence is that every replica reports the same values: aggregate with
// max without(instance, pod), never sum.
//
// A scrape costs a handful of aggregate queries, so the result is cached for a
// short while: several replicas each scraped every thirty seconds must not
// become load on the database the runs themselves depend on. A query that fails
// does not fail the scrape; haliphron_stats_up says the numbers are missing.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/automagicops/haliphron/backend/app"
)

// Bounds, in seconds. Runs last minutes to hours; queue waits are seconds when
// a cluster has room and minutes to an hour when it does not.
var (
	DurationBounds  = []float64{30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}
	QueueWaitBounds = []float64{1, 5, 15, 30, 60, 120, 300, 900, 3600}
)

// ClusterStatuses is the cluster_status domain, so that the status gauge has a
// zero for every state a cluster is not in and an alert can match on == 1.
var ClusterStatuses = []string{"Registering", "Active", "Unreachable", "Revoked"}

const (
	cacheFor     = 15 * time.Second
	queryTimeout = 5 * time.Second
)

var (
	descRuns = prometheus.NewDesc("haliphron_runs",
		"Runs in the store, by cluster and reported status. A gauge: deleting a run lowers it.",
		[]string{"cluster", "status"}, nil)
	descDuration = prometheus.NewDesc("haliphron_run_duration_seconds",
		"How long finished runs ran, from first Running to terminal, by cluster.",
		[]string{"cluster"}, nil)
	descQueueWait = prometheus.NewDesc("haliphron_run_queue_wait_seconds",
		"How long started runs waited from queued to Running, by cluster.",
		[]string{"cluster"}, nil)
	descActive = prometheus.NewDesc("haliphron_cluster_active_runs",
		"Runs the backend believes are on the cluster (Leased through Running, and Unknown).",
		[]string{"cluster"}, nil)
	descCapacity = prometheus.NewDesc("haliphron_cluster_capacity_slots",
		"Agent slots the cluster last reported.", []string{"cluster"}, nil)
	descFree = prometheus.NewDesc("haliphron_cluster_free_slots",
		"Free agent slots the cluster last reported.", []string{"cluster"}, nil)
	descHeartbeat = prometheus.NewDesc("haliphron_cluster_last_heartbeat_timestamp_seconds",
		"When the cluster's controller last heartbeated, as a Unix time.", []string{"cluster"}, nil)
	descClusterStatus = prometheus.NewDesc("haliphron_cluster_status",
		"1 for the status the cluster is in, 0 for the others.", []string{"cluster", "status"}, nil)
	descUp = prometheus.NewDesc("haliphron_stats_up",
		"1 if the run and cluster statistics were read from the database, 0 if not.", nil, nil)
)

// Handler is /metrics over a registry of its own: the run and cluster series,
// plus the Go runtime and process collectors.
func Handler(service *app.Service, logger *slog.Logger) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		NewCollector(service, logger),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: slogErrorLog{logger}})
}

// Collector reads run and cluster statistics through the app layer.
type Collector struct {
	service *app.Service
	log     *slog.Logger
	now     func() time.Time

	mu     sync.Mutex
	at     time.Time
	cached []prometheus.Metric
}

// NewCollector builds it.
func NewCollector(service *app.Service, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.Default()
	}
	return &Collector{service: service, log: logger, now: time.Now}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descRuns, descDuration, descQueueWait, descActive,
		descCapacity, descFree, descHeartbeat, descClusterStatus, descUp} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached == nil || c.now().Sub(c.at) >= cacheFor {
		c.cached = c.read()
		c.at = c.now()
	}
	for _, m := range c.cached {
		ch <- m
	}
}

// read queries everything once. A failure is logged and reported as
// haliphron_stats_up 0 with no other series: partial numbers that look whole
// are worse than none.
func (c *Collector) read() []prometheus.Metric {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	out, err := c.series(ctx)
	if err != nil {
		c.log.Warn("metrics: reading statistics failed", "error", err)
		return []prometheus.Metric{prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)}
	}
	return append(out, prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 1))
}

func (c *Collector) series(ctx context.Context) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	stats, err := c.service.RunStats(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, s := range stats {
		for status, n := range s.Counts {
			out = append(out, prometheus.MustNewConstMetric(descRuns, prometheus.GaugeValue,
				float64(n), s.ClusterName, status))
		}
	}

	hists, err := c.service.RunHistograms(ctx, DurationBounds, QueueWaitBounds)
	if err != nil {
		return nil, err
	}
	for _, h := range hists {
		out = append(out,
			prometheus.MustNewConstHistogram(descDuration, h.Duration.Count, h.Duration.Sum,
				buckets(DurationBounds, h.Duration.Buckets), h.ClusterName),
			prometheus.MustNewConstHistogram(descQueueWait, h.QueueWait.Count, h.QueueWait.Sum,
				buckets(QueueWaitBounds, h.QueueWait.Buckets), h.ClusterName))
	}

	health, err := c.service.ClusterHealth(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range health {
		name := h.Cluster.Name
		out = append(out,
			prometheus.MustNewConstMetric(descActive, prometheus.GaugeValue, float64(h.ActiveRuns), name),
			prometheus.MustNewConstMetric(descCapacity, prometheus.GaugeValue, float64(h.Cluster.CapacitySlots), name),
			prometheus.MustNewConstMetric(descFree, prometheus.GaugeValue, float64(h.Cluster.FreeSlots), name))
		if h.Cluster.LastHeartbeatAt != nil {
			out = append(out, prometheus.MustNewConstMetric(descHeartbeat, prometheus.GaugeValue,
				float64(h.Cluster.LastHeartbeatAt.UnixNano())/1e9, name))
		}
		for _, status := range ClusterStatuses {
			v := 0.0
			if h.Cluster.Status == status {
				v = 1
			}
			out = append(out, prometheus.MustNewConstMetric(descClusterStatus, prometheus.GaugeValue, v, name, status))
		}
	}
	return out, nil
}

func buckets(bounds []float64, counts []uint64) map[float64]uint64 {
	out := make(map[float64]uint64, len(bounds))
	for i, b := range bounds {
		out[b] = counts[i]
	}
	return out
}

// slogErrorLog adapts promhttp's error log to slog.
type slogErrorLog struct{ log *slog.Logger }

func (l slogErrorLog) Println(v ...any) { l.log.Error("metrics: serving /metrics failed", "error", v) }
