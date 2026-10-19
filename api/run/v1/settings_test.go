package v1

import (
	"strings"
	"testing"
)

const settingsDoc = `{
  "permissions": {"allow": ["Bash(*)"]},
  "extraKnownMarketplaces": {
    "acme": {"source": {"source": "github", "repo": "acme/plugins", "ref": "v1"}},
    "local": {"source": {"source": "directory", "path": "/home/me/plugins"}}
  },
  "enabledPlugins": {"zeta@acme": true, "alpha@acme": true, "off@acme": false}
}`

func TestASettingsFileReadLenientlyDropsWhatAPodCannotFetch(t *testing.T) {
	spec, dropped, err := PluginSpecFromSettings([]byte(settingsDoc), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Marketplaces) != 1 || spec.Marketplaces[0].Name != "acme" ||
		spec.Marketplaces[0].URL != "acme/plugins" || spec.Marketplaces[0].Ref != "v1" {
		t.Errorf("marketplaces are %+v", spec.Marketplaces)
	}
	if strings.Join(spec.Enabled, ",") != "alpha@acme,zeta@acme" {
		t.Errorf("enabled is %v, sorted and without the disabled one", spec.Enabled)
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "local") {
		t.Errorf("dropped is %q", dropped)
	}
}

func TestASettingsFileReadStrictlyRefusesWhatAPodCannotFetch(t *testing.T) {
	if _, _, err := PluginSpecFromSettings([]byte(settingsDoc), true); err == nil ||
		!strings.Contains(err.Error(), "local") {
		t.Errorf("got %v", err)
	}
}

func TestAnythingButPluginsInASettingsFileIsIgnored(t *testing.T) {
	spec, _, err := PluginSpecFromSettings([]byte(`{"permissions": {"allow": ["Bash(*)"]}, "env": {"X": "1"}}`), true)
	if err != nil || !spec.IsEmpty() {
		t.Errorf("spec %+v, err %v", spec, err)
	}
}

func TestTheAgentEnvironmentRefusesTheEntrypointsOwnNames(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "claude_config_dir", "HALIPHRON_X", "PATH",
		"GH_TOKEN", "GIT_ASKPASS", "LD_PRELOAD", "", "1X", "A-B"} {
		if AgentEnvName(name) == nil {
			t.Errorf("%q was accepted", name)
		}
	}
	for _, name := range []string{"MATTERMOST_TOKEN", "MM_URL", "_PRIVATE"} {
		if err := AgentEnvName(name); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}
