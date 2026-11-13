// Package netcheck verifies the agents network behaves as docker-host.md §6
// requires, without the controller holding a single firewall privilege.
//
// The controller has no NET_ADMIN and cannot read iptables. So it checks the
// result rather than the rules: the network's configuration through the Docker
// API, then the behaviour, from a short-lived probe container on the agents
// network, hardened exactly as an agent is. While the check fails the host
// takes no new work; runs already executing continue, because killing them
// would not undo whatever they could reach.
//
// "Blocked" means a connection attempt timed out. The firewall DROPs; it never
// REJECTs. That is what makes the probe able to tell the rules apart from the
// absence of rules: a port nobody listens on answers a connection with a reset
// when nothing filters it, and with silence when a DROP rule does.
package netcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/launcher"
)

// Expect is what one check must observe.
type Expect string

const (
	// ExpectReach is a completed HTTP exchange, whatever the status.
	ExpectReach Expect = "reach"
	// ExpectBlocked is a connect timeout: a DROP rule on the path.
	ExpectBlocked Expect = "blocked"
	// ExpectZero is a shell command that exits 0.
	ExpectZero Expect = "zero"
)

// curl's exit codes the verdict reads.
const (
	curlOK        = 0
	curlRefused   = 7
	curlTimeout   = 28
	curlNoResolve = 6
)

// Check is one assertion.
type Check struct {
	Name   string
	URL    string // for reach and blocked
	Shell  string // for zero
	Expect Expect
}

// Settings is what the check needs to know about the host.
type Settings struct {
	Network string
	Subnet  netip.Prefix
	Gateway netip.Addr
	Bridge  string
	DNS     []string

	CallbackPort int
	HealthPort   int
	MetricsPort  int

	PublicURL        string
	Blocked          []string
	ControlPlaneMCP  string
	ControlPlaneHost string
	ControlPlanePort []int

	Image string
	// MountDir is a directory the controller writes and agents get bind
	// mounts from (the run directory). The probe mounts it to prove the
	// controller and the daemon see the same path.
	MountDir string
}

// Plan is the list of checks for these settings, in a fixed order.
func Plan(s Settings, peerIP string, marker string) []Check {
	gw := s.Gateway.String()
	checks := []Check{
		{Name: "callback", URL: fmt.Sprintf("http://%s/healthz", net.JoinHostPort(gw, strconv.Itoa(s.CallbackPort))), Expect: ExpectReach},
		// Port 9 is discard: nobody listens on it, so without the INPUT rules
		// the host's kernel answers with a reset.
		{Name: "host-other-port", URL: "http://" + net.JoinHostPort(gw, "9") + "/", Expect: ExpectBlocked},
		{Name: "host-health-port", URL: "http://" + net.JoinHostPort(gw, strconv.Itoa(s.HealthPort)) + "/", Expect: ExpectBlocked},
		{Name: "host-metrics-port", URL: "http://" + net.JoinHostPort(gw, strconv.Itoa(s.MetricsPort)) + "/", Expect: ExpectBlocked},
		{Name: "cloud-metadata", URL: "http://169.254.169.254/", Expect: ExpectBlocked},
	}
	if peerIP != "" {
		checks = append(checks, Check{Name: "other-agent", URL: "http://" + net.JoinHostPort(peerIP, "9") + "/", Expect: ExpectBlocked})
	}
	for _, target := range s.Blocked {
		checks = append(checks, Check{Name: "blocked " + target, URL: "http://" + target + "/", Expect: ExpectBlocked})
	}
	if s.ControlPlaneHost != "" {
		ports := append([]int(nil), s.ControlPlanePort...)
		sort.Ints(ports)
		for _, p := range ports {
			checks = append(checks, Check{
				Name:   "control-plane-port " + strconv.Itoa(p),
				URL:    "http://" + net.JoinHostPort(s.ControlPlaneHost, strconv.Itoa(p)) + "/",
				Expect: ExpectBlocked,
			})
		}
	}
	if s.ControlPlaneMCP != "" {
		checks = append(checks, Check{Name: "control-plane-mcp", URL: "http://" + s.ControlPlaneMCP + "/", Expect: ExpectReach})
	}
	if s.PublicURL != "" {
		checks = append(checks, Check{Name: "public-https", URL: s.PublicURL, Expect: ExpectReach})
	}
	if marker != "" {
		checks = append(checks, Check{Name: "same-path-mounts", Shell: "test -f /haliphron-mountcheck/" + marker, Expect: ExpectZero})
	}
	return checks
}

// Script is the probe's shell program: one line of output per check.
func Script(checks []Check) string {
	var b strings.Builder
	b.WriteString("probe() { curl -s -o /dev/null --connect-timeout 4 --max-time 8 \"$2\"; echo \"check|$1|$?\"; }\n")
	b.WriteString("shellcheck() { sh -c \"$2\" >/dev/null 2>&1; echo \"check|$1|$?\"; }\n")
	for _, c := range checks {
		switch c.Expect {
		case ExpectZero:
			fmt.Fprintf(&b, "shellcheck %s %s\n", quote(c.Name), quote(c.Shell))
		default:
			fmt.Fprintf(&b, "probe %s %s\n", quote(c.Name), quote(c.URL))
		}
	}
	return b.String()
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Result is one check's outcome.
type Result struct {
	Check
	Exit int
	OK   bool
	Why  string
}

// Judge reads the probe's output against the plan. A check with no line of
// output failed: the probe died before it.
func Judge(checks []Check, output string) []Result {
	codes := map[string]int{}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 3 || parts[0] != "check" {
			continue
		}
		if n, err := strconv.Atoi(parts[2]); err == nil {
			codes[parts[1]] = n
		}
	}
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		code, seen := codes[c.Name]
		r := Result{Check: c, Exit: code}
		switch {
		case !seen:
			r.Why = "the probe produced no result for this check"
		case c.Expect == ExpectZero:
			r.OK = code == 0
			if !r.OK {
				r.Why = "the controller and the daemon do not see the same files at the run directory; " +
					"mount it into the controller at the same path as on the host"
			}
		case c.Expect == ExpectReach:
			r.OK = code == curlOK
			if !r.OK {
				r.Why = reachWhy(code)
			}
		case c.Expect == ExpectBlocked:
			r.OK = code == curlTimeout
			if !r.OK {
				r.Why = blockedWhy(code)
			}
		}
		out = append(out, r)
	}
	return out
}

func reachWhy(code int) string {
	switch code {
	case curlTimeout:
		return "timed out: a rule drops traffic agents must be able to send"
	case curlRefused:
		return "connection refused: nothing listens there, or a rule rejects it"
	case curlNoResolve:
		return "the name did not resolve through the configured resolvers"
	default:
		return fmt.Sprintf("curl exited %d", code)
	}
}

func blockedWhy(code int) string {
	switch code {
	case curlOK:
		return "reachable: an agent can connect here and must not"
	case curlRefused:
		return "connection refused: the packets reached the destination, so no DROP rule is in the path " +
			"(is the firewall unit installed and running?)"
	default:
		return fmt.Sprintf("curl exited %d where a DROP rule should have made it time out", code)
	}
}

// Failures renders the failed results, or nil.
func Failures(results []Result) error {
	var msgs []string
	for _, r := range results {
		if !r.OK {
			msgs = append(msgs, fmt.Sprintf("%s (%s): %s", r.Name, describe(r.Check), r.Why))
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New("netcheck: " + strings.Join(msgs, "; "))
}

func describe(c Check) string {
	if c.URL != "" {
		return c.URL
	}
	return c.Shell
}

// CheckNetwork is the configuration half: the network exists as install.sh
// creates it.
func CheckNetwork(n docker.Network, s Settings) error {
	var problems []string
	if n.Driver != "bridge" {
		problems = append(problems, fmt.Sprintf("driver is %q, want bridge", n.Driver))
	}
	if n.Internal {
		problems = append(problems, "the network is internal, so agents have no egress at all")
	}
	if n.EnableIPv6 {
		problems = append(problems, "IPv6 is enabled; the firewall rules are IPv4 only")
	}
	if n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		problems = append(problems, "inter-container communication is not disabled")
	}
	if got := n.Options["com.docker.network.bridge.name"]; got != s.Bridge {
		problems = append(problems, fmt.Sprintf("the bridge interface is %q, the firewall rules match %q", got, s.Bridge))
	}
	matched := false
	for _, c := range n.IPAM.Config {
		if c.Subnet == s.Subnet.String() {
			matched = true
			if c.Gateway != "" && c.Gateway != s.Gateway.String() {
				problems = append(problems, fmt.Sprintf("the gateway is %s, the callback listens on %s", c.Gateway, s.Gateway))
			}
		}
	}
	if !matched {
		problems = append(problems, fmt.Sprintf("no IPAM config for subnet %s", s.Subnet))
	}
	if len(problems) > 0 {
		return fmt.Errorf("netcheck: network %s: %s", s.Network, strings.Join(problems, "; "))
	}
	return nil
}

// Checker runs the check and holds its verdict.
type Checker struct {
	Engine   docker.Engine
	Settings Settings
	Auths    *docker.Auths
	// Disabled is HALIPHRON_INSECURE_SKIP_NETCHECK: development only.
	Disabled bool
	Interval time.Duration
	Log      *slog.Logger

	mu      sync.Mutex
	err     error
	checked bool
	at      time.Time
	trigger chan struct{}
}

// Healthy is nil when the last check passed. Before the first check it is an
// error: a host whose network has not been checked takes no work.
func (c *Checker) Healthy() error {
	if c.Disabled {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checked {
		return errors.New("netcheck: the network has not been checked yet")
	}
	return c.err
}

// Trigger asks for a check as soon as possible, after a daemon restart.
func (c *Checker) Trigger() {
	c.mu.Lock()
	if c.trigger == nil {
		c.trigger = make(chan struct{}, 1)
	}
	ch := c.trigger
	c.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Start runs the check now, then every interval and on every trigger.
func (c *Checker) Start(ctx context.Context) error {
	if c.Disabled {
		c.log().Warn("the network check is disabled (HALIPHRON_INSECURE_SKIP_NETCHECK); " +
			"agents on this host are not verified to be isolated. Development only.")
		<-ctx.Done()
		return nil
	}
	c.mu.Lock()
	if c.trigger == nil {
		c.trigger = make(chan struct{}, 1)
	}
	trigger := c.trigger
	c.mu.Unlock()
	interval := c.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	for {
		err := c.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		c.mu.Lock()
		was := c.err
		c.err, c.checked, c.at = err, true, time.Now()
		c.mu.Unlock()
		switch {
		case err != nil:
			c.log().Error("the network check failed; this host takes no new work until it passes", "error", err)
		case was != nil:
			c.log().Info("the network check passes again")
		default:
			c.log().Debug("the network check passed")
		}
		wait := interval
		if err != nil {
			wait = min(interval, time.Minute)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-trigger:
			t.Stop()
		case <-t.C:
		}
	}
}

func (c *Checker) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// RunOnce runs both halves of the check and returns the first failure.
func (c *Checker) RunOnce(ctx context.Context) error {
	s := c.Settings
	n, err := c.Engine.NetworkInspect(ctx, s.Network)
	if errors.Is(err, docker.ErrNotFound) {
		return fmt.Errorf("netcheck: network %s does not exist; run install.sh", s.Network)
	}
	if err != nil {
		return fmt.Errorf("netcheck: inspect network %s: %w", s.Network, err)
	}
	if err := CheckNetwork(n, s); err != nil {
		return err
	}
	results, err := c.Probe(ctx)
	if err != nil {
		return err
	}
	return Failures(results)
}

// Probe runs the behavioural half and returns every result.
func (c *Checker) Probe(ctx context.Context) ([]Result, error) {
	s := c.Settings
	if s.Image == "" {
		return nil, errors.New("netcheck: no probe image configured (HALIPHRON_NETCHECK_IMAGE)")
	}
	if err := c.ensureImage(ctx); err != nil {
		return nil, err
	}

	marker := ""
	if s.MountDir != "" {
		marker = fmt.Sprintf(".netcheck-%d", time.Now().UnixNano())
		if err := os.MkdirAll(s.MountDir, 0o711); err != nil {
			return nil, fmt.Errorf("netcheck: create %s: %w", s.MountDir, err)
		}
		path := filepath.Join(s.MountDir, marker)
		if err := os.WriteFile(path, []byte("ok"), 0o644); err != nil {
			return nil, fmt.Errorf("netcheck: write the mount marker: %w", err)
		}
		defer func() { _ = os.Remove(path) }()
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	peerName := "haliphron-netcheck-peer-" + suffix
	peerID, err := c.Engine.ContainerCreate(ctx, peerName, c.probeSpec([]string{"sleep", "120"}, ""))
	if err != nil {
		return nil, fmt.Errorf("netcheck: create the peer probe: %w", err)
	}
	defer c.remove(peerID)
	if err := c.Engine.ContainerStart(ctx, peerID); err != nil {
		return nil, fmt.Errorf("netcheck: start the peer probe: %w", err)
	}
	peer, err := c.Engine.ContainerInspect(ctx, peerID)
	if err != nil {
		return nil, fmt.Errorf("netcheck: inspect the peer probe: %w", err)
	}
	peerIP := peer.NetworkSettings.Networks[s.Network].IPAddress

	checks := Plan(s, peerIP, marker)
	probeID, err := c.Engine.ContainerCreate(ctx, "haliphron-netcheck-"+suffix,
		c.probeSpec([]string{"/bin/sh", "-c", Script(checks)}, marker))
	if err != nil {
		return nil, fmt.Errorf("netcheck: create the probe: %w", err)
	}
	defer c.remove(probeID)
	if err := c.Engine.ContainerStart(ctx, probeID); err != nil {
		return nil, fmt.Errorf("netcheck: start the probe: %w", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		ctr, err := c.Engine.ContainerInspect(ctx, probeID)
		if err != nil {
			return nil, fmt.Errorf("netcheck: inspect the probe: %w", err)
		}
		if ctr.State.Finished() {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("netcheck: the probe did not finish in three minutes")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	out, err := c.Engine.ContainerLogs(ctx, probeID)
	if err != nil {
		return nil, fmt.Errorf("netcheck: read the probe's output: %w", err)
	}
	return Judge(checks, out), nil
}

// probeSpec is an agent's hardening with a different command: what the probe
// can reach is what an agent can reach.
func (c *Checker) probeSpec(cmd []string, marker string) docker.ContainerSpec {
	s := c.Settings
	pids := int64(64)
	spec := docker.ContainerSpec{
		Image:      s.Image,
		User:       "1000:1000",
		Entrypoint: cmd[:1],
		Cmd:        cmd[1:],
		Labels: map[string]string{
			launcher.LabelManagedBy: launcher.ManagedBy,
			launcher.LabelRole:      launcher.RoleProbe,
		},
		AttachStdout: true,
		AttachStderr: true,
		HostConfig: docker.HostConfig{
			NetworkMode:    s.Network,
			DNS:            s.DNS,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			RestartPolicy:  docker.RestartPolicy{Name: "no"},
			NanoCPUs:       500_000_000,
			Memory:         256 << 20,
			MemorySwap:     256 << 20,
			PidsLimit:      &pids,
			LogConfig:      docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m"}},
		},
		NetworkingConfig: &docker.NetworkingConfig{
			EndpointsConfig: map[string]docker.EndpointSettings{s.Network: {}},
		},
	}
	if marker != "" {
		spec.HostConfig.Mounts = []docker.Mount{{
			Type: "bind", Source: s.MountDir, Target: "/haliphron-mountcheck", ReadOnly: true,
		}}
	}
	return spec
}

func (c *Checker) remove(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Engine.ContainerRemove(ctx, id, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
		c.log().Warn("removing a netcheck probe", "container", id, "error", err)
	}
}

func (c *Checker) ensureImage(ctx context.Context) error {
	if _, err := c.Engine.ImageInspect(ctx, c.Settings.Image); err == nil {
		return nil
	} else if !errors.Is(err, docker.ErrNotFound) {
		return fmt.Errorf("netcheck: inspect %s: %w", c.Settings.Image, err)
	}
	pullCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := c.Engine.ImagePull(pullCtx, c.Settings.Image, c.Auths.For(c.Settings.Image)); err != nil {
		return fmt.Errorf("netcheck: pull %s: %w", c.Settings.Image, err)
	}
	return nil
}
