package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// A role's plugins are material, like its files: the controller turns them into
// a ConfigMap and only the ConfigMap's name rides in the CR.

func TestARolesPluginsTravelAsMaterialAndNotInTheSpec(t *testing.T) {
	h := newHarness(t)
	roleSpec := `{
		"model": "anthropic/claude-sonnet-5",
		"plugins": {
			"marketplaces": [{"name": "playneta", "url": "playneta/claude-plugin"}],
			"enabled": ["playneta-infra-coder@playneta"]
		}
	}`
	if _, err := h.Store.UpsertRole(context.Background(), "coder", json.RawMessage(roleSpec), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}

	p := newProbe(t, h, "east")
	h.Submit(func(r *run.SubmitRequest) { r.Role = "coder" })
	lease := p.LeaseOne()

	body := lease.RoleConfig[runv1.RoleConfigKeyPlugins]
	if body == "" {
		t.Fatalf("the role's plugins did not travel with the lease; keys were %v",
			keysOf(lease.RoleConfig))
	}
	var spec runv1.PluginSpec
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatalf("the rendered plugins do not parse: %v", err)
	}
	if len(spec.Marketplaces) != 1 || spec.Marketplaces[0].URL != "playneta/claude-plugin" {
		t.Errorf("marketplaces = %+v", spec.Marketplaces)
	}
	if len(spec.Enabled) != 1 || spec.Enabled[0] != "playneta-infra-coder@playneta" {
		t.Errorf("enabled = %v", spec.Enabled)
	}

	// `kubectl get agentrun -o yaml` is not a way to read what the controller
	// mounted, and a marketplace is not a spec field.
	encoded, err := json.Marshal(lease.Spec)
	if err != nil {
		t.Fatalf("encode spec: %v", err)
	}
	if strings.Contains(string(encoded), "playneta") {
		t.Error("the role's plugins reached the spec")
	}
}

func TestARoleWithoutPluginsShipsNoPluginsFile(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Store.UpsertRole(context.Background(), "coder",
		json.RawMessage(`{"model":"anthropic/claude-sonnet-5"}`), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}

	p := newProbe(t, h, "east")
	h.Submit(func(r *run.SubmitRequest) { r.Role = "coder" })
	lease := p.LeaseOne()

	// An empty document would make the pod's chain think the control plane had
	// answered, and stop it falling through to the repository.
	if _, present := lease.RoleConfig[runv1.RoleConfigKeyPlugins]; present {
		t.Error("a role with no plugins still shipped a plugins file")
	}
}

func TestAMistypedPluginIsRefusedWhenTheRoleIsSavedAndNotAnHourLater(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	bad := map[string]string{
		"a plugin that names no marketplace":  `{"plugins":{"enabled":["playneta-infra-coder"]}}`,
		"a marketplace this pod cannot clone": `{"plugins":{"marketplaces":[{"name":"x","url":"git@github.com:a/b.git"}]}}`,
		"a plugin from an undeclared source": `{"plugins":{
			"marketplaces":[{"name":"playneta","url":"playneta/claude-plugin"}],
			"enabled":["something@elsewhere"]}}`,
		"a config file key a ConfigMap cannot hold": `{"configFiles":{".claude/settings.json":"{}"}}`,
	}
	for why, body := range bad {
		var out map[string]any
		status := h.call(t, admin, http.MethodPut, "/api/v1/roles/broken", json.RawMessage(body), &out)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422 — nothing would notice until the lease was cut", why, status)
			continue
		}
		// The form has to be able to point at the offending field.
		errObj, _ := out["error"].(map[string]any)
		if field, _ := errObj["field"].(string); field == "" {
			t.Errorf("%s: the refusal names no field: %v", why, out)
		}
	}
}

func TestARoleThisBuildDoesNotFullyUnderstandIsStillAccepted(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	// The compatibility rule the wire contracts follow: a role written for a
	// newer control plane still runs here. Validation is narrow on purpose, and
	// tightening it into a whitelist would break exactly this.
	body := `{"model":"anthropic/claude-sonnet-5","somethingFromTheFuture":{"a":1},
		"plugins":{"marketplaces":[{"name":"playneta","url":"playneta/claude-plugin"}]}}`
	if status := h.call(t, admin, http.MethodPut, "/api/v1/roles/forward",
		json.RawMessage(body), nil); status != http.StatusOK {
		t.Errorf("status %d, want 200: an unknown key is ignored, not refused", status)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestARoleThatOnlyForbidsRepositoryPluginsStillShipsThatDecision(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Store.UpsertRole(context.Background(), "locked",
		json.RawMessage(`{"plugins":{"trustRepositorySources":false}}`), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}

	p := newProbe(t, h, "east")
	h.Submit(func(r *run.SubmitRequest) { r.Role = "locked" })
	lease := p.LeaseOne()

	// The role declares no plugins of its own, but it does declare something.
	// Were the document withheld because the plugin lists are empty, the pod
	// would find no file, apply the default — which is to trust the repository
	// — and the one switch that turns repository-chosen plugins off would
	// silently turn them on.
	body := lease.RoleConfig[runv1.RoleConfigKeyPlugins]
	if body == "" {
		t.Fatalf("trustRepositorySources=false was not sent to the pod; keys were %v",
			keysOf(lease.RoleConfig))
	}
	var spec runv1.PluginSpec
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatalf("the rendered plugins do not parse: %v", err)
	}
	if spec.TrustsRepository() {
		t.Errorf("the pod would read %s as trusting the repository", body)
	}
}

func TestARoleCannotShipItsOwnFileUnderTheReservedPluginsKey(t *testing.T) {
	h := newHarness(t)
	// No `plugins` field at all: the reservation has to hold anyway, or an
	// operator's own file is read by the pod as the control plane's rendered
	// document and decides which code the agent runs.
	if _, err := h.Store.UpsertRole(context.Background(), "sneaky", json.RawMessage(
		`{"configFiles":{"plugins.json":"{\"marketplaces\":[{\"name\":\"x\",\"url\":\"attacker/plugins\"}]}"}}`,
	), "test"); err != nil {
		t.Fatalf("create role: %v", err)
	}

	p := newProbe(t, h, "east")
	h.Submit(func(r *run.SubmitRequest) { r.Role = "sneaky" })
	lease := p.LeaseOne()

	if body, present := lease.RoleConfig[runv1.RoleConfigKeyPlugins]; present {
		t.Errorf("a role's own file was mounted under the reserved key: %s", body)
	}
}
