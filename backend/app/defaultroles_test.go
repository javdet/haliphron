package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/automagicops/haliphron/backend/store"
)

func TestEveryDefaultRoleWouldBeAcceptedFromTheUI(t *testing.T) {
	entries, err := defaultRoles.ReadDir("defaults")
	if err != nil || len(entries) == 0 {
		t.Fatalf("no default roles: %v", err)
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		spec, err := defaultRoles.ReadFile("defaults/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := validateDefaultRole(name, spec); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheClassificatorRunsWithoutARepositoryAndReadsTheBotToken(t *testing.T) {
	spec, err := defaultRoles.ReadFile("defaults/" + DefaultChatRole + ".json")
	if err != nil {
		t.Fatalf("the default chat role has no spec: %v", err)
	}
	role, err := parseRole(store.Role{Name: DefaultChatRole, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if role.SystemPrompt == "" || !strings.Contains(role.SystemPrompt, "run_agent") ||
		!strings.Contains(role.SystemPrompt, "list_roles") {
		t.Error("the classificator's prompt does not say how to route")
	}
	if len(role.SecretEnv) != 1 || role.SecretEnv[0].Name != "MATTERMOST_TOKEN" {
		t.Errorf("secretEnv is %+v", role.SecretEnv)
	}
	var raw map[string]any
	if err := json.Unmarshal(spec, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["repo"]; ok {
		t.Error("the classificator names a repository")
	}
	if _, ok := role.ConfigFiles["settings.json"]; !ok {
		t.Error("the classificator has no settings.json for the operator to fill in")
	}
}

func TestARolesSettingsJSONBecomesItsPluginsWhenPluginsIsUnset(t *testing.T) {
	spec := `{"configFiles": {"settings.json": "{\"extraKnownMarketplaces\": {\"acme\": {\"source\": {\"source\": \"github\", \"repo\": \"acme/plugins\"}}}, \"enabledPlugins\": {\"mattermost@acme\": true, \"off@acme\": false}}"}}`
	role, err := parseRole(store.Role{Name: "r", Spec: json.RawMessage(spec)})
	if err != nil {
		t.Fatal(err)
	}
	if role.Plugins == nil || len(role.Plugins.Marketplaces) != 1 ||
		role.Plugins.Marketplaces[0].URL != "acme/plugins" || len(role.Plugins.Enabled) != 1 ||
		role.Plugins.Enabled[0] != "mattermost@acme" {
		t.Errorf("plugins are %+v", role.Plugins)
	}
}

func TestARolesSettingsJSONIsReadStrictlyWhenSaved(t *testing.T) {
	var parsed roleSpec
	_ = json.Unmarshal([]byte(`{"configFiles": {"settings.json": "{\"extraKnownMarketplaces\": {\"local\": {\"source\": {\"source\": \"directory\", \"path\": \"/home/me/plugins\"}}}}"}}`), &parsed)
	if err := validateRoleSettings("r", parsed); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("a marketplace the pod cannot fetch was accepted: %v", err)
	}
}

func TestSecretEnvMayNotReplaceWhatTheEntrypointSets(t *testing.T) {
	var parsed roleSpec
	for _, name := range []string{"ANTHROPIC_BASE_URL", "PATH", "HALIPHRON_ROLE", "GH_TOKEN", "1BAD"} {
		_ = json.Unmarshal([]byte(`{"secretEnv": [{"name": "`+name+`", "secret": "s"}]}`), &parsed)
		if err := validateSecretEnv(parsed.SecretEnv); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	_ = json.Unmarshal([]byte(`{"secretEnv": [{"name": "MATTERMOST_TOKEN", "secret": "s"}, {"name": "MATTERMOST_TOKEN", "secret": "t"}]}`), &parsed)
	if err := validateSecretEnv(parsed.SecretEnv); err == nil {
		t.Error("a name given twice was accepted")
	}
}
