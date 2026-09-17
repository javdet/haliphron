// Package config is the controller's configuration: what the chart sets, what
// the defaults are, and which of them the control plane overrides at runtime.
//
// The split matters. Everything about *this* cluster — where the agents run,
// what the pod may consume, how to reach the controller from inside — comes
// from the chart, because only the operator knows it. Everything about the
// protocol — the heartbeat interval, the lease TTL, how long a long poll may
// hang — comes from the control plane at registration, because a value that
// drifts between installations means no two clusters agree on what "stale"
// means.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Environment variables the chart sets.
const (
	EnvBackendURL     = "HALIPHRON_BACKEND_URL"
	EnvBootstrapToken = "HALIPHRON_BOOTSTRAP_TOKEN"
	// EnvBootstrapTokenFile is preferred over the value: a one-time token in a
	// variable is visible in `kubectl describe pod`, and it is the one credential
	// that can register a new cluster.
	EnvBootstrapTokenFile = "HALIPHRON_BOOTSTRAP_TOKEN_FILE"

	EnvClusterName      = "HALIPHRON_CLUSTER_NAME"
	EnvClusterLabels    = "HALIPHRON_CLUSTER_LABELS"
	EnvAgentNamespace   = "HALIPHRON_AGENT_NAMESPACE"
	EnvOwnNamespace     = "HALIPHRON_NAMESPACE"
	EnvIdentitySecret   = "HALIPHRON_IDENTITY_SECRET"
	EnvCapacitySlots    = "HALIPHRON_CAPACITY_SLOTS"
	EnvRuntimes         = "HALIPHRON_RUNTIMES"
	EnvServiceAccount   = "HALIPHRON_AGENT_SERVICE_ACCOUNT"
	EnvCallbackURL      = "HALIPHRON_CALLBACK_URL"
	EnvCallbackAddr     = "HALIPHRON_CALLBACK_ADDR"
	EnvRunURLTemplate   = "HALIPHRON_RUN_URL_TEMPLATE"
	EnvGraceSeconds     = "HALIPHRON_GRACE_SECONDS"
	EnvDeadlineSlack    = "HALIPHRON_DEADLINE_SLACK_SECONDS"
	EnvStartupDeadline  = "HALIPHRON_STARTUP_DEADLINE_SECONDS"
	EnvPreflightJob     = "HALIPHRON_PREFLIGHT_JOB"
	EnvMetricsAddr      = "HALIPHRON_METRICS_ADDR"
	EnvHealthAddr       = "HALIPHRON_HEALTH_ADDR"
	EnvLogLevel         = "HALIPHRON_LOG_LEVEL"
	EnvK8sVersionOnHost = "HALIPHRON_K8S_VERSION"
)

// Config is the controller's own settings.
type Config struct {
	BackendURL     string
	BootstrapToken string

	ClusterName    string
	ClusterLabels  map[string]string
	AgentNamespace string
	OwnNamespace   string
	IdentitySecret string
	CapacitySlots  int32
	Runtimes       []runv1.AgentType
	ServiceAccount string

	// CallbackURL is what the pod is told to post its report to. It is
	// cluster-local by nature — a Service in the controller's namespace — which
	// is why no part of it can come from the backend.
	CallbackURL  string
	CallbackAddr string

	RunURLTemplate string

	GraceSeconds    int64
	DeadlineSlack   int64
	StartupDeadline time.Duration
	PreflightJob    bool

	MetricsAddr string
	HealthAddr  string
	LogLevel    string
}

// Load reads the environment and applies the defaults. It fails on the four
// values that have no sensible default, because a controller that starts
// without them fails later and less clearly: a missing callback URL, for
// instance, surfaces as every run finishing without a result.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Config{
		BackendURL:      strings.TrimRight(getenv(EnvBackendURL), "/"),
		ClusterName:     getenv(EnvClusterName),
		ClusterLabels:   parseLabels(getenv(EnvClusterLabels)),
		AgentNamespace:  getenv(EnvAgentNamespace),
		OwnNamespace:    getenv(EnvOwnNamespace),
		IdentitySecret:  getenv(EnvIdentitySecret),
		ServiceAccount:  getenv(EnvServiceAccount),
		CallbackURL:     getenv(EnvCallbackURL),
		CallbackAddr:    orDefault(getenv(EnvCallbackAddr), ":8083"),
		RunURLTemplate:  getenv(EnvRunURLTemplate),
		MetricsAddr:     orDefault(getenv(EnvMetricsAddr), ":9090"),
		HealthAddr:      orDefault(getenv(EnvHealthAddr), ":8081"),
		LogLevel:        orDefault(getenv(EnvLogLevel), "info"),
		CapacitySlots:   int32(intOr(getenv(EnvCapacitySlots), 8)),
		GraceSeconds:    int64(intOr(getenv(EnvGraceSeconds), 60)),
		DeadlineSlack:   int64(intOr(getenv(EnvDeadlineSlack), 600)),
		StartupDeadline: time.Duration(intOr(getenv(EnvStartupDeadline), 600)) * time.Second,
		PreflightJob:    boolOr(getenv(EnvPreflightJob), true),
	}
	c.BootstrapToken = strings.TrimSpace(getenv(EnvBootstrapToken))
	if path := getenv(EnvBootstrapTokenFile); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		c.BootstrapToken = strings.TrimSpace(string(raw))
	}
	for _, name := range strings.Split(getenv(EnvRuntimes), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.Runtimes = append(c.Runtimes, runv1.AgentType(name))
		}
	}
	if c.OwnNamespace == "" {
		// The downward API is how a pod learns its own namespace, but a chart
		// that forgot to set it should not take the controller down: the
		// service account's namespace file says the same thing.
		if raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			c.OwnNamespace = strings.TrimSpace(string(raw))
		}
	}
	if c.AgentNamespace == "" {
		c.AgentNamespace = c.OwnNamespace
	}

	var missing []string
	for _, required := range []struct {
		name, value string
	}{
		{EnvBackendURL, c.BackendURL},
		{EnvClusterName, c.ClusterName},
		{EnvCallbackURL, c.CallbackURL},
		{EnvAgentNamespace, c.AgentNamespace},
	} {
		if required.value == "" {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: %s must be set", strings.Join(missing, ", "))
	}
	return c, nil
}

// Timings are the operational parameters the control plane issues. They are
// held behind a mutex because the heartbeat may replace them at any moment —
// that is the point of returning them on a heartbeat rather than only at
// registration — while the lease loop is reading them.
type Timings struct {
	mu sync.RWMutex
	v  clusterv1.Timings
}

// NewTimings starts from the contract's documented defaults, so that a
// controller which has not registered yet still behaves sanely.
func NewTimings() *Timings {
	return &Timings{v: clusterv1.DefaultTimings()}
}

// Get returns the current parameters.
func (t *Timings) Get() clusterv1.Timings {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.v
}

// Set replaces them, ignoring a nil or empty update: a control plane that
// omits the field is saying "unchanged", not "use zeroes".
func (t *Timings) Set(v *clusterv1.Timings) {
	if v == nil || v.HeartbeatIntervalSeconds == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.v = *v
}

func parseLabels(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if key = strings.TrimSpace(key); key != "" {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func intOr(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func boolOr(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		return fallback
	}
}
