package netcheck

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/automagicops/haliphron/dockerhost/docker"
)

func settings() Settings {
	return Settings{
		Network: "haliphron-agents", Subnet: netip.MustParsePrefix("172.31.250.0/24"),
		Gateway: netip.MustParseAddr("172.31.250.1"), Bridge: "br-haliphron",
		CallbackPort: 8083, HealthPort: 8084, MetricsPort: 9091,
		PublicURL: "https://api.example.com", ControlPlaneHost: "172.31.250.1",
		ControlPlanePort: []int{8082, 5432}, ControlPlaneMCP: "172.31.250.1:8081",
	}
}

func TestTheVerdictTellsADropFromNoRuleAtAll(t *testing.T) {
	checks := Plan(settings(), "172.31.250.7", "m")
	var out strings.Builder
	for _, c := range checks {
		code := 0
		if c.Expect == ExpectBlocked {
			code = 28
		}
		out.WriteString("check|" + c.Name + "|" + itoa(code) + "\n")
	}
	if err := Failures(Judge(checks, out.String())); err != nil {
		t.Fatalf("a correct firewall failed: %v", err)
	}

	// No firewall: the host answers resets on closed ports, peers are
	// reachable, the control plane's ports answer.
	out.Reset()
	for _, c := range checks {
		code := 0
		if c.Expect == ExpectBlocked {
			code = 7
		}
		out.WriteString("check|" + c.Name + "|" + itoa(code) + "\n")
	}
	err := Failures(Judge(checks, out.String()))
	if err == nil || !strings.Contains(err.Error(), "no DROP rule") {
		t.Fatalf("an open host passed: %v", err)
	}
}

func TestACheckWithNoOutputFails(t *testing.T) {
	checks := Plan(settings(), "", "")
	results := Judge(checks, "check|callback|0\n")
	if Failures(results) == nil {
		t.Fatal("a probe that died after one line passed")
	}
}

func TestThePlanCoversTheControlPlaneAndKeepsMCPReachable(t *testing.T) {
	names := map[string]Expect{}
	for _, c := range Plan(settings(), "10.0.0.2", "") {
		names[c.Name] = c.Expect
	}
	for name, want := range map[string]Expect{
		"callback": ExpectReach, "control-plane-port 8082": ExpectBlocked, "control-plane-port 5432": ExpectBlocked,
		"control-plane-mcp": ExpectReach, "other-agent": ExpectBlocked, "cloud-metadata": ExpectBlocked,
		"public-https": ExpectReach,
	} {
		if names[name] != want {
			t.Errorf("%s: %q, want %q", name, names[name], want)
		}
	}
}

func TestTheNetworkMustBeAsInstallShCreatesIt(t *testing.T) {
	n := docker.Network{Driver: "bridge", Options: map[string]string{
		"com.docker.network.bridge.enable_icc": "false", "com.docker.network.bridge.name": "br-haliphron"}}
	n.IPAM.Config = append(n.IPAM.Config, struct {
		Subnet  string `json:"Subnet"`
		Gateway string `json:"Gateway"`
	}{"172.31.250.0/24", "172.31.250.1"})
	if err := CheckNetwork(n, settings()); err != nil {
		t.Fatal(err)
	}
	n.Options["com.docker.network.bridge.enable_icc"] = "true"
	n.EnableIPv6 = true
	err := CheckNetwork(n, settings())
	if err == nil || !strings.Contains(err.Error(), "inter-container") || !strings.Contains(err.Error(), "IPv6") {
		t.Fatalf("got %v", err)
	}
}

func TestTheScriptQuotesItsArguments(t *testing.T) {
	s := Script([]Check{{Name: "x'y", URL: "http://a/", Expect: ExpectReach}})
	if !strings.Contains(s, `'x'\''y'`) {
		t.Fatal(s)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
