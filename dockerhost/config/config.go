// Package config is the Docker host controller's configuration: what the
// install script writes into .env, what the defaults are, and which values the
// control plane overrides at runtime.
//
// The split is the Kubernetes controller's. Everything about this host — the
// agents network, the resolvers, how much of the machine agents may use — comes
// from the operator, because only the operator knows it. Everything about the
// protocol — the heartbeat interval, the lease TTL, the long poll — comes from
// the control plane at registration, through controller/config.Timings.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Environment variables. The names the Kubernetes controller also reads keep
// the same meaning here, so an operator who knows one runtime knows the other.
const (
	EnvBackendURL         = "HALIPHRON_BACKEND_URL"
	EnvBootstrapToken     = "HALIPHRON_BOOTSTRAP_TOKEN"
	EnvBootstrapTokenFile = "HALIPHRON_BOOTSTRAP_TOKEN_FILE"
	EnvClusterName        = "HALIPHRON_CLUSTER_NAME"
	EnvClusterLabels      = "HALIPHRON_CLUSTER_LABELS"
	EnvCapacitySlots      = "HALIPHRON_CAPACITY_SLOTS"
	EnvRuntimes           = "HALIPHRON_RUNTIMES"
	EnvCallbackURL        = "HALIPHRON_CALLBACK_URL"
	EnvCallbackAddr       = "HALIPHRON_CALLBACK_ADDR"
	EnvMaxArtifactBytes   = "HALIPHRON_ARTIFACT_MAX_BYTES_PER_RUN"
	EnvGraceSeconds       = "HALIPHRON_GRACE_SECONDS"
	EnvDeadlineSlack      = "HALIPHRON_DEADLINE_SLACK_SECONDS"
	EnvStartupDeadline    = "HALIPHRON_STARTUP_DEADLINE_SECONDS"
	EnvMetricsAddr        = "HALIPHRON_METRICS_ADDR"
	EnvHealthAddr         = "HALIPHRON_HEALTH_ADDR"
	EnvLogLevel           = "HALIPHRON_LOG_LEVEL"

	// Host layout. Both directories are bind-mounted into the controller's
	// container at the same path as on the host: the controller writes files
	// there and asks the daemon to mount them into agent containers, and the
	// daemon resolves paths on the host.
	EnvStateDir  = "HALIPHRON_STATE_DIR"
	EnvRunDir    = "HALIPHRON_RUN_DIR"
	EnvSpoolPath = "HALIPHRON_SPOOL_PATH"
	EnvDiskPath  = "HALIPHRON_DISK_PATH"

	EnvDockerHost = "HALIPHRON_DOCKER_HOST"

	// The agents network (section 7.2 of the plan, contract docker-host.md).
	EnvNetwork = "HALIPHRON_AGENT_NETWORK"
	EnvSubnet  = "HALIPHRON_AGENT_SUBNET"
	EnvBridge  = "HALIPHRON_AGENT_BRIDGE"
	EnvDNS     = "HALIPHRON_AGENT_DNS"

	// Container limits the spec does not carry.
	EnvPidsLimit   = "HALIPHRON_AGENT_PIDS_LIMIT"
	EnvNofile      = "HALIPHRON_AGENT_NOFILE"
	EnvLogDriver   = "HALIPHRON_AGENT_LOG_DRIVER"
	EnvLogMaxSize  = "HALIPHRON_AGENT_LOG_MAX_SIZE"
	EnvLogMaxFile  = "HALIPHRON_AGENT_LOG_MAX_FILE"
	EnvDefaultCPU  = "HALIPHRON_AGENT_DEFAULT_CPU"
	EnvDefaultMem  = "HALIPHRON_AGENT_DEFAULT_MEMORY"
	EnvDefaultDisk = "HALIPHRON_AGENT_DEFAULT_EPHEMERAL_STORAGE"

	// Capacity and disk (section 8).
	EnvReserveCPU    = "HALIPHRON_RESERVE_CPU"
	EnvReserveMemory = "HALIPHRON_RESERVE_MEMORY"
	EnvDiskFloor     = "HALIPHRON_DISK_FLOOR"
	EnvDiskCritical  = "HALIPHRON_DISK_CRITICAL"
	EnvImageKeep     = "HALIPHRON_IMAGE_KEEP"

	// The network check (section 7.4).
	EnvNetcheckImage        = "HALIPHRON_NETCHECK_IMAGE"
	EnvNetcheckPublicURL    = "HALIPHRON_NETCHECK_PUBLIC_URL"
	EnvNetcheckBlocked      = "HALIPHRON_NETCHECK_BLOCKED"
	EnvNetcheckInterval     = "HALIPHRON_NETCHECK_INTERVAL_SECONDS"
	EnvControlPlaneMCP      = "HALIPHRON_CONTROL_PLANE_MCP"
	EnvControlPlaneHost     = "HALIPHRON_CONTROL_PLANE_HOST"
	EnvControlPlanePorts    = "HALIPHRON_CONTROL_PLANE_PORTS"
	EnvInsecureSkipNetcheck = "HALIPHRON_INSECURE_SKIP_NETCHECK"
)

// LabelInsecureNetwork is added to the registration when the network check is
// disabled, so the cluster is marked in the UI rather than only in a log
// nobody reads.
const LabelInsecureNetwork = "haliphron.io/insecure-network"

// Config is the controller's own settings.
type Config struct {
	BackendURL     string
	BootstrapToken string
	ClusterName    string
	ClusterLabels  map[string]string
	CapacitySlots  int32
	Runtimes       []runv1.AgentType

	// CallbackURL is what agent containers are told to post to. It names the
	// agents bridge's gateway address, the one host address the firewall lets
	// an agent reach.
	CallbackURL  string
	CallbackAddr string

	MaxArtifactBytesPerRun int64
	GraceSeconds           int64
	DeadlineSlack          int64
	StartupDeadline        time.Duration

	MetricsAddr string
	HealthAddr  string
	LogLevel    string

	StateDir  string
	RunDir    string
	SpoolPath string
	DiskPath  string

	DockerHost string

	Network string
	Subnet  netip.Prefix
	Gateway netip.Addr
	Bridge  string
	DNS     []netip.Addr

	PidsLimit   int64
	Nofile      int64
	LogDriver   string
	LogMaxSize  string
	LogMaxFile  string
	DefaultCPU  string
	DefaultMem  string
	DefaultDisk string

	ReserveCPU    string
	ReserveMemory string
	DiskFloor     int64
	DiskCritical  int64
	ImageKeep     int

	NetcheckImage    string
	NetcheckPublic   string
	NetcheckBlocked  []string
	NetcheckInterval time.Duration
	// ControlPlaneMCP is host:port of the control plane's MCP endpoint as an
	// agent reaches it, when the control plane shares the host. Empty when it
	// does not, or when MCP is reached at a public address.
	ControlPlaneMCP string
	// ControlPlaneHost is the address the control plane's published ports
	// answer on, as seen from the agents network. Empty means the control
	// plane is on another machine and its ports are not checked.
	ControlPlaneHost  string
	ControlPlanePorts []int
	InsecureNetwork   bool
}

// Load reads the environment and applies the defaults. It fails on the values
// that have no sensible default and on values that do not parse: a resolver
// list with a typo is a firewall that allows one address and containers that
// ask another.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Config{
		BackendURL:    strings.TrimRight(getenv(EnvBackendURL), "/"),
		ClusterName:   getenv(EnvClusterName),
		ClusterLabels: parseLabels(getenv(EnvClusterLabels)),
		CallbackURL:   getenv(EnvCallbackURL),
		CallbackAddr:  getenv(EnvCallbackAddr),
		MaxArtifactBytesPerRun: int64(intOr(getenv(EnvMaxArtifactBytes),
			clusterv1.DefaultMaxBytesPerRun)),
		GraceSeconds:    int64(intOr(getenv(EnvGraceSeconds), 60)),
		DeadlineSlack:   int64(intOr(getenv(EnvDeadlineSlack), 600)),
		StartupDeadline: time.Duration(intOr(getenv(EnvStartupDeadline), 600)) * time.Second,
		MetricsAddr:     orDefault(getenv(EnvMetricsAddr), "127.0.0.1:9091"),
		HealthAddr:      orDefault(getenv(EnvHealthAddr), "127.0.0.1:8084"),
		LogLevel:        orDefault(getenv(EnvLogLevel), "info"),

		StateDir:  orDefault(getenv(EnvStateDir), "/var/lib/haliphron"),
		RunDir:    orDefault(getenv(EnvRunDir), "/run/haliphron"),
		SpoolPath: getenv(EnvSpoolPath),
		DiskPath:  getenv(EnvDiskPath),

		DockerHost: orDefault(getenv(EnvDockerHost), "unix:///var/run/docker.sock"),

		Network: orDefault(getenv(EnvNetwork), "haliphron-agents"),
		Bridge:  orDefault(getenv(EnvBridge), "br-haliphron"),

		PidsLimit:   int64(intOr(getenv(EnvPidsLimit), 4096)),
		Nofile:      int64(intOr(getenv(EnvNofile), 65536)),
		LogDriver:   orDefault(getenv(EnvLogDriver), "local"),
		LogMaxSize:  orDefault(getenv(EnvLogMaxSize), "10m"),
		LogMaxFile:  orDefault(getenv(EnvLogMaxFile), "2"),
		DefaultCPU:  orDefault(getenv(EnvDefaultCPU), "2"),
		DefaultMem:  orDefault(getenv(EnvDefaultMem), "4Gi"),
		DefaultDisk: orDefault(getenv(EnvDefaultDisk), "10Gi"),

		ReserveCPU:    orDefault(getenv(EnvReserveCPU), "1"),
		ReserveMemory: orDefault(getenv(EnvReserveMemory), "2Gi"),
		ImageKeep:     intOr(getenv(EnvImageKeep), 3),

		NetcheckImage:    getenv(EnvNetcheckImage),
		NetcheckPublic:   getenv(EnvNetcheckPublicURL),
		NetcheckBlocked:  splitList(getenv(EnvNetcheckBlocked)),
		NetcheckInterval: time.Duration(intOr(getenv(EnvNetcheckInterval), 600)) * time.Second,
		ControlPlaneMCP:  strings.TrimSpace(getenv(EnvControlPlaneMCP)),
		ControlPlaneHost: strings.TrimSpace(getenv(EnvControlPlaneHost)),
		InsecureNetwork:  boolOr(getenv(EnvInsecureSkipNetcheck), false),
	}
	if c.SpoolPath == "" {
		c.SpoolPath = filepath.Join(c.StateDir, "spool")
	}
	if c.DiskPath == "" {
		c.DiskPath = c.StateDir
	}

	var err error
	if c.DiskFloor, err = bytesOr(getenv(EnvDiskFloor), 20<<30); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", EnvDiskFloor, err)
	}
	if c.DiskCritical, err = bytesOr(getenv(EnvDiskCritical), 5<<30); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", EnvDiskCritical, err)
	}

	capacity := intOr(getenv(EnvCapacitySlots), 4)
	if capacity > clusterv1.MaxCapacitySlots {
		return Config{}, fmt.Errorf("config: %s is %d, the maximum is %d",
			EnvCapacitySlots, capacity, clusterv1.MaxCapacitySlots)
	}
	c.CapacitySlots = int32(capacity)

	c.BootstrapToken = strings.TrimSpace(getenv(EnvBootstrapToken))
	if path := getenv(EnvBootstrapTokenFile); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		c.BootstrapToken = strings.TrimSpace(string(raw))
	}
	for _, name := range splitList(getenv(EnvRuntimes)) {
		c.Runtimes = append(c.Runtimes, runv1.AgentType(name))
	}

	subnet := orDefault(getenv(EnvSubnet), "172.31.250.0/24")
	if c.Subnet, err = netip.ParsePrefix(subnet); err != nil || !c.Subnet.Addr().Is4() {
		return Config{}, fmt.Errorf("config: %s=%q is not an IPv4 prefix", EnvSubnet, subnet)
	}
	c.Subnet = c.Subnet.Masked()
	// Docker gives a user-defined bridge the first host address of its subnet
	// as the gateway unless told otherwise, and install.sh never tells it
	// otherwise. The callback listens there.
	c.Gateway = c.Subnet.Addr().Next()

	for _, raw := range splitList(getenv(EnvDNS)) {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return Config{}, fmt.Errorf("config: %s: %q is not an IP address", EnvDNS, raw)
		}
		c.DNS = append(c.DNS, addr)
	}

	ports := getenv(EnvControlPlanePorts)
	if strings.TrimSpace(ports) == "" {
		ports = "8080,8081,8082,9090,5432"
	}
	for _, raw := range splitList(ports) {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			return Config{}, fmt.Errorf("config: %s: %q is not a port", EnvControlPlanePorts, raw)
		}
		c.ControlPlanePorts = append(c.ControlPlanePorts, n)
	}
	// The MCP port is the one control-plane port agents may reach, so it is
	// never in the list the check expects to be closed.
	if _, port, err := net.SplitHostPort(c.ControlPlaneMCP); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			c.ControlPlanePorts = without(c.ControlPlanePorts, n)
		}
	} else if c.ControlPlaneMCP != "" {
		return Config{}, fmt.Errorf("config: %s=%q is not host:port", EnvControlPlaneMCP, c.ControlPlaneMCP)
	}

	if c.CallbackAddr == "" {
		c.CallbackAddr = net.JoinHostPort(c.Gateway.String(), "8083")
	}
	if c.CallbackURL == "" {
		c.CallbackURL = "http://" + c.CallbackAddr
	}

	var missing []string
	for _, required := range []struct{ name, value string }{
		{EnvBackendURL, c.BackendURL},
		{EnvClusterName, c.ClusterName},
	} {
		if required.value == "" {
			missing = append(missing, required.name)
		}
	}
	if len(c.DNS) == 0 {
		// Not defaulted. Docker's embedded resolver forwards from inside the
		// container's namespace, and with a local stub resolver on the host it
		// silently falls back to public servers the firewall then has to allow
		// or block. A resolver list is a decision the operator makes once.
		missing = append(missing, EnvDNS)
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: %s must be set", strings.Join(missing, ", "))
	}
	return c, nil
}

// Labels is what the controller registers with: the operator's labels, plus
// the mark of a host whose network isolation is not being checked.
func (c Config) Labels() map[string]string {
	out := make(map[string]string, len(c.ClusterLabels)+1)
	for k, v := range c.ClusterLabels {
		out[k] = v
	}
	if c.InsecureNetwork {
		out[LabelInsecureNetwork] = "true"
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AttemptDir is where one attempt's secret files and role files live on the
// host. It is under RunDir, which install.sh requires to be a tmpfs, so the
// git token never touches a disk.
func (c Config) AttemptDir(runID runv1.ULID, epoch int64, attempt int32) string {
	return filepath.Join(c.RunDir, "runs", strings.ToLower(string(runID)),
		strconv.FormatInt(epoch, 10), "a"+strconv.Itoa(int(attempt)))
}

// RunPath is the directory holding every attempt of one run.
func (c Config) RunPath(runID runv1.ULID) string {
	return filepath.Join(c.RunDir, "runs", strings.ToLower(string(runID)))
}

// StatePath is the SQLite file.
func (c Config) StatePath() string { return filepath.Join(c.StateDir, "state.db") }

// IdentityPath is the cluster's key file.
func (c Config) IdentityPath() string { return filepath.Join(c.StateDir, "identity.json") }

// LockPath is the file the single-instance lock is taken on.
func (c Config) LockPath() string { return filepath.Join(c.StateDir, "lock") }

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

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func without(list []int, n int) []int {
	out := list[:0]
	for _, v := range list {
		if v != n {
			out = append(out, v)
		}
	}
	return out
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
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

// bytesOr parses a size with an optional binary suffix (Ki, Mi, Gi, Ti) or a
// plain number of bytes.
func bytesOr(raw string, fallback int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	return ParseBytes(raw)
}

// ParseBytes reads "20Gi", "512Mi", "1073741824".
func ParseBytes(raw string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}}
	for _, u := range units {
		if strings.HasSuffix(raw, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(raw, u.suffix), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("%q is not a size", raw)
			}
			return n * u.mult, nil
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a size", raw)
	}
	return n, nil
}
