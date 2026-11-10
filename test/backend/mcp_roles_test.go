package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/automagicops/haliphron/backend/store"
)

// The role tools: an operator's own agent, connecting from outside, configures
// roles over MCP by the rules the UI's REST calls are held to. An agent this
// platform started never gets them — a role decides which tools, MCP servers
// and secrets an agent has, and an agent that could edit roles could widen
// its children's permissions on a prompt injection's say-so.

var roleTools = []string{"get_role", "put_role", "delete_role", "inspect_marketplace"}

func TestAnOperatorsAgentManagesRolesOverMCP(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	offered := h.toolNames(t, admin)
	for _, want := range roleTools {
		if !offered[want] {
			t.Errorf("an admin token is not offered %s", want)
		}
	}

	text, saved, isError := h.toolText(t, admin, "put_role", map[string]any{
		"name": "infra-coder",
		"spec": map[string]any{
			"description":  "Changes the infrastructure repository.",
			"model":        "anthropic/claude-sonnet-5",
			"systemPrompt": "Plan before you apply.",
			"toolPolicy":   map[string]any{"deny": []string{"Bash(rm:*)"}},
		},
	})
	if isError {
		t.Fatalf("put_role refused a valid role: %s", text)
	}
	if saved["name"] != "infra-coder" {
		t.Errorf("put_role answered %v, want the saved role", saved)
	}

	stored, err := h.Store.RoleByName(context.Background(), "infra-coder")
	if err != nil {
		t.Fatalf("the role was not stored: %v", err)
	}
	if !strings.HasPrefix(stored.CreatedBy, "token:") {
		t.Errorf("created_by = %q, want the operator's token", stored.CreatedBy)
	}

	text, got, isError := h.toolText(t, admin, "get_role", map[string]any{"name": "infra-coder"})
	if isError {
		t.Fatalf("get_role: %s", text)
	}
	spec, _ := got["spec"].(map[string]any)
	if spec["systemPrompt"] != "Plan before you apply." || spec["model"] != "anthropic/claude-sonnet-5" {
		t.Errorf("get_role spec = %v, want the whole spec as saved", spec)
	}
	if !strings.Contains(text, `"systemPrompt": "Plan before you apply."`) {
		t.Errorf("the text block does not carry the spec: %q", text)
	}

	// put_role replaces the whole spec, as REST's PUT does: nothing merges.
	if text, _, isError := h.toolText(t, admin, "put_role", map[string]any{
		"name": "infra-coder", "spec": map[string]any{"model": "anthropic/claude-opus-5"},
	}); isError {
		t.Fatalf("put_role refused a replacement: %s", text)
	}
	_, got, _ = h.toolText(t, admin, "get_role", map[string]any{"name": "infra-coder"})
	if spec, _ := got["spec"].(map[string]any); len(spec) != 1 || spec["model"] != "anthropic/claude-opus-5" {
		t.Errorf("after a replacement the spec is %v, want the new spec alone", spec)
	}

	text, _, isError = h.toolText(t, admin, "delete_role", map[string]any{"name": "infra-coder"})
	if isError {
		t.Fatalf("delete_role: %s", text)
	}
	if _, err := h.Store.RoleByName(context.Background(), "infra-coder"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the role outlived delete_role: %v", err)
	}
	text, _, isError = h.toolText(t, admin, "get_role", map[string]any{"name": "infra-coder"})
	if !isError || !strings.Contains(text, `no role named "infra-coder"`) {
		t.Errorf("get_role of a deleted role answered %q (error %v)", text, isError)
	}
	if _, _, isError := h.toolText(t, admin, "delete_role", map[string]any{"name": "infra-coder"}); !isError {
		t.Error("deleting a role that is gone was not reported")
	}
}

// A role saved over MCP is admitted by app.PutRole, the same as one saved from
// the UI, and a refusal names the field so the agent can correct it.
func TestPutRoleRefusesWhatTheUIWouldRefuseAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	for _, tc := range []struct {
		name  string
		args  map[string]any
		field string
	}{
		{"a bad name", map[string]any{"name": "Infra Coder", "spec": map[string]any{}}, "name:"},
		{"no name", map[string]any{"spec": map[string]any{}}, "name:"},
		{"no spec", map[string]any{"name": "x"}, "spec:"},
		{"a spec that is not an object", map[string]any{"name": "x", "spec": []string{"a"}}, "spec:"},
		{"a nested config file", map[string]any{"name": "x",
			"spec": map[string]any{"configFiles": map[string]string{".claude/settings.json": "{}"}}}, "configFiles:"},
		{"a secret that does not exist", map[string]any{"name": "x",
			"spec": map[string]any{"secretEnv": []map[string]string{{"name": "TOKEN", "secret": "nowhere"}}}},
			"secretEnv[0].secret:"},
		{"a description that is an essay", map[string]any{"name": "x",
			"spec": map[string]any{"description": strings.Repeat("a", 2048)}}, "description:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, _, isError := h.toolText(t, admin, "put_role", tc.args)
			if !isError {
				t.Fatalf("put_role accepted %s", tc.name)
			}
			if !strings.HasPrefix(text, tc.field) {
				t.Errorf("refusal %q does not name %s", text, tc.field)
			}
		})
	}
	if roles, _ := h.Store.ListRoles(context.Background()); len(roles) != 0 {
		t.Errorf("a refused role was stored: %+v", roles)
	}

	// A model that passes the object as its JSON text gets the object saved.
	if text, _, isError := h.toolText(t, admin, "put_role", map[string]any{
		"name": "as-text", "spec": `{"model": "anthropic/claude-sonnet-5"}`,
	}); isError {
		t.Fatalf("a spec given as JSON text was refused: %s", text)
	}
	stored, err := h.Store.RoleByName(context.Background(), "as-text")
	if err != nil {
		t.Fatalf("read the role: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(stored.Spec, &spec); err != nil || spec["model"] != "anthropic/claude-sonnet-5" {
		t.Errorf("stored spec = %s (%v), want the object", stored.Spec, err)
	}
}

// Reading a role is runs:read, as GET /roles/{name} is; writing one is admin.
func TestARunsReadTokenReadsARoleButCannotWriteOne(t *testing.T) {
	h := newHarness(t)
	reader := h.Token(store.ScopeRunsRead, store.ScopeRunsWrite)
	if _, err := h.Store.UpsertRole(context.Background(), "coder", json.RawMessage(`{"model":"m"}`), "test"); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	offered := h.toolNames(t, reader)
	if !offered["get_role"] {
		t.Error("a runs:read token is not offered get_role")
	}
	for _, name := range []string{"put_role", "delete_role", "inspect_marketplace"} {
		if offered[name] {
			t.Errorf("a token without admin is offered %s", name)
		}
	}

	if _, _, isError := h.toolText(t, reader, "get_role", map[string]any{"name": "coder"}); isError {
		t.Error("a runs:read token could not read a role")
	}
	if _, _, isError := h.toolText(t, reader, "put_role", map[string]any{
		"name": "coder", "spec": map[string]any{"model": "other"},
	}); !isError {
		t.Error("a token without admin replaced a role")
	}
	if _, _, isError := h.toolText(t, reader, "delete_role", map[string]any{"name": "coder"}); !isError {
		t.Error("a token without admin deleted a role")
	}
	stored, err := h.Store.RoleByName(context.Background(), "coder")
	var spec map[string]any
	if err == nil {
		err = json.Unmarshal(stored.Spec, &spec)
	}
	if err != nil || len(spec) != 1 || spec["model"] != "m" {
		t.Errorf("the role changed: %s (%v)", stored.Spec, err)
	}
}

// The property the tools are built around: an agent this platform started is
// never offered them and is refused when it calls them anyway — even with a
// token that carries admin, which no per-run token is minted with today, so
// that a future change to the per-run scopes cannot open this by accident.
func TestARunsTokenIsNeverGivenTheRoleTools(t *testing.T) {
	h := newHarness(t)
	parent := h.Submit()
	if _, err := h.Store.UpsertRole(context.Background(), "coder",
		json.RawMessage(`{"systemPrompt":"the confidential part"}`), "test"); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	for name, token := range map[string]string{
		"as minted": h.aRunToken(parent.ID).Secret,
		"with admin": func() string {
			tok, err := h.Store.CreateToken(context.Background(), store.Token{
				Name: "run " + string(parent.ID), Kind: store.TokenKindRunMCP,
				Scopes: []string{store.ScopeAdmin}, RunID: parent.ID, CreatedBy: "test",
			}, time.Hour)
			if err != nil {
				t.Fatalf("mint a per-run token: %v", err)
			}
			return tok.Secret
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			offered := h.toolNames(t, token)
			for _, tool := range roleTools {
				if offered[tool] {
					t.Errorf("a run's token is offered %s", tool)
				}
			}
			if !offered["list_roles"] {
				t.Error("a run's token lost list_roles, which it needs to choose a child's role")
			}

			calls := map[string]map[string]any{
				"get_role":            {"name": "coder"},
				"put_role":            {"name": "coder", "spec": map[string]any{"permissionMode": "bypassPermissions"}},
				"delete_role":         {"name": "coder"},
				"inspect_marketplace": {"url": "owner/repo"},
			}
			for tool, args := range calls {
				text, _, isError := h.toolText(t, token, tool, args)
				if !isError {
					t.Errorf("a run's token called %s", tool)
				}
				if strings.Contains(text, "the confidential part") {
					t.Errorf("%s showed a run's token the role's spec: %q", tool, text)
				}
			}
		})
	}

	stored, err := h.Store.RoleByName(context.Background(), "coder")
	if err != nil || !strings.Contains(string(stored.Spec), "the confidential part") {
		t.Errorf("a run's token changed the role: %s (%v)", stored.Spec, err)
	}
}
