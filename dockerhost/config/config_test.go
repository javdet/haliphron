package config

import (
	"slices"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{
		EnvBackendURL: "https://cp.example.com/", EnvClusterName: "box", EnvDNS: "10.0.0.53, 10.0.0.54",
	}
}

func TestTheDefaultsDeriveTheCallbackFromTheAgentsSubnet(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.String() != "172.31.250.1" || c.CallbackAddr != "172.31.250.1:8083" ||
		c.CallbackURL != "http://172.31.250.1:8083" {
		t.Fatalf("gateway %s addr %s url %s", c.Gateway, c.CallbackAddr, c.CallbackURL)
	}
	if c.BackendURL != "https://cp.example.com" || c.HealthAddr != "127.0.0.1:8084" || c.MetricsAddr != "127.0.0.1:9091" {
		t.Fatalf("%+v", c)
	}
	if len(c.DNS) != 2 || c.SpoolPath != "/var/lib/haliphron/spool" {
		t.Fatalf("%+v", c)
	}
}

func TestResolversAreRequired(t *testing.T) {
	m := base()
	delete(m, EnvDNS)
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), EnvDNS) {
		t.Fatalf("got %v", err)
	}
	m[EnvDNS] = "not-an-ip"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("a bad resolver was accepted")
	}
}

func TestTheMCPPortIsNeverExpectedClosed(t *testing.T) {
	m := base()
	m[EnvControlPlaneMCP] = "172.31.250.1:8081"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(c.ControlPlanePorts, 8081) || !slices.Contains(c.ControlPlanePorts, 8082) {
		t.Fatalf("ports %v", c.ControlPlanePorts)
	}
}

func TestAnInsecureHostIsLabelled(t *testing.T) {
	m := base()
	m[EnvInsecureSkipNetcheck] = "true"
	c, _ := Load(env(m))
	if c.Labels()[LabelInsecureNetwork] != "true" {
		t.Fatal("no insecure label")
	}
}

func TestAttemptDirectoriesAreUnderTheRunDirectory(t *testing.T) {
	c, _ := Load(env(base()))
	if got := c.AttemptDir("01HZABC", 3, 2); got != "/run/haliphron/runs/01hzabc/3/a2" {
		t.Fatal(got)
	}
}
