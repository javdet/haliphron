package config

import (
	"strconv"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
)

func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		EnvBackendURL:     "https://haliphron.example",
		EnvClusterName:    "east",
		EnvCallbackURL:    "http://haliphron-controller:8083",
		EnvAgentNamespace: "haliphron-agents",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(key string) string { return base[key] }
}

// The backend refuses a declaration above the ceiling, fatally, at
// registration. Refusing it here names the variable instead; and the check
// runs before the int32 conversion, which would otherwise wrap an oversized
// value into a plausible-looking one.
func TestACapacityAboveTheMaximumStopsTheControllerAtStartup(t *testing.T) {
	for _, raw := range []string{
		strconv.Itoa(clusterv1.MaxCapacitySlots + 1),
		"100000",
		"4294967304", // 2^32 + 8: wraps to 8 as an int32
	} {
		if _, err := Load(env(map[string]string{EnvCapacitySlots: raw})); err == nil {
			t.Errorf("%s=%s was accepted", EnvCapacitySlots, raw)
		}
	}
}

func TestACapacityAtTheMaximumIsAccepted(t *testing.T) {
	c, err := Load(env(map[string]string{EnvCapacitySlots: strconv.Itoa(clusterv1.MaxCapacitySlots)}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.CapacitySlots != clusterv1.MaxCapacitySlots {
		t.Errorf("capacity = %d, want %d", c.CapacitySlots, clusterv1.MaxCapacitySlots)
	}
}
