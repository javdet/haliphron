package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// The role tools: get_role, put_role, delete_role and inspect_marketplace.
//
// They are the configuration tools of the "without a UI" scenario: an operator
// pointing their own agent at this endpoint and having it set roles up. They
// are the REST role endpoints under other names, by the same scopes, and every
// rule is app's (PutRole) so a role saved here and one saved from the UI are
// admitted alike.
//
// They are never offered to a per-run token, whatever its scopes. A role is
// what decides which tools, MCP servers, secrets and system prompt an agent
// gets; an agent that could edit roles could widen its own children's
// permissions, and the agent may be acting on a prompt injection from the
// repository it is working in. The spec also holds what list_roles keeps from
// an agent on purpose — the system prompt, env values, MCP server addresses —
// so get_role is withheld from it too.

const roleSpecHint = `The spec is the role spec object, camelCase, every field optional: ` +
	`description, agent ("claude-code" or "codex"), model, image, permissionMode ("default", ` +
	`"acceptEdits", "bypassPermissions", "plan"), maxTurns, systemPrompt (appended to the agent's own), ` +
	`env [{name, value}], secretEnv [{name, secret}] (secret names a managed secret), resources {cpu, ` +
	`memory, ephemeralStorage}, mcpServers [{name, transport ("stdio", "http", "sse"), url, command, ` +
	`args}], nodeSelector, tolerations, toolPolicy {allow, deny}, plugins {marketplaces [{name, url, ` +
	`ref}], enabled ["plugin@marketplace"]}, configFiles {flat file name: contents}, clusterSelector. ` +
	`Unknown fields are kept and ignored.`

// roleToolDefinitions are offered to an operator's token, never to a run's.
func roleToolDefinitions(c Caller) []tool {
	if c.Parent != nil {
		return nil
	}
	var tools []tool
	if c.Token.Allows(store.ScopeRunsRead) {
		tools = append(tools, tool{
			Name:  "get_role",
			Title: "Read a role",
			Description: "One role's whole spec: its model, tool policy, MCP servers, plugins, files and system " +
				"prompt. Read it before put_role: put_role replaces the whole spec, so an edit is get_role, " +
				"change, put_role.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "The role's name, as list_roles gives it."},
				},
				"required": []string{"name"},
			},
		})
	}
	if c.Token.Allows(store.ScopeAdmin) {
		tools = append(tools,
			tool{
				Name:  "put_role",
				Title: "Create or replace a role",
				Description: "Create a role, or replace an existing one's spec whole — nothing is merged, so a " +
					"field left out is a field removed. To edit, get_role first and send back the changed spec. " +
					"Runs already admitted keep the spec they were admitted with. " + roleSpecHint,
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "maxLength": 253,
							"pattern":     "^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$",
							"description": "Lower-case letters, digits, '-', '_' and '.'."},
						"spec": map[string]any{"type": "object", "description": "The whole role spec."},
					},
					"required": []string{"name", "spec"},
				},
			},
			tool{
				Name:  "delete_role",
				Title: "Delete a role",
				Description: "Delete a role. Runs already admitted under it keep running and keep naming it; a " +
					"new run naming it is refused. The name can be used again.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
					},
					"required": []string{"name"},
				},
			},
			tool{
				Name:  "inspect_marketplace",
				Title: "Inspect a plugin marketplace",
				Description: "Read a claude-code plugin marketplace's manifest: its name and the plugins it " +
					"offers. Use it to fill a role's plugins before put_role. A marketplace that cannot be read " +
					"is not an error: found is false and problem says why.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{"type": "string",
							"description": "The marketplace source, as plugins.marketplaces[].url takes it."},
						"ref": map[string]any{"type": "string", "description": "A branch or tag. Omit for the default."},
					},
					"required": []string{"url"},
				},
			},
		)
	}
	return tools
}

// operatorOnly refuses a role tool to a run's token. tools/list does not
// offer them to one; this is for the agent that calls them anyway.
func operatorOnly(c Caller, scope string) *toolResult {
	if c.Parent != nil {
		refused := failed("role management is not offered to an agent's token")
		return &refused
	}
	if !c.Token.Allows(scope) {
		refused := failed("this token does not carry the " + scope + " scope")
		return &refused
	}
	return nil
}

type roleNameArgs struct {
	Name string `json:"name"`
}

type putRoleArgs struct {
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec"`
}

func (s *Server) getRole(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	if refused := operatorOnly(c, store.ScopeRunsRead); refused != nil {
		return *refused, nil
	}
	var args roleNameArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}
	if args.Name == "" {
		return failed("name: a role needs a name"), nil
	}
	role, err := s.app.Store().RoleByName(r.Context(), args.Name)
	if err != nil {
		return s.roleError(args.Name, err), nil
	}
	return roleResult(role, "role "+role.Name), nil
}

func (s *Server) putRole(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	if refused := operatorOnly(c, store.ScopeAdmin); refused != nil {
		return *refused, nil
	}
	var args putRoleArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}
	spec := bytes.TrimSpace(args.Spec)
	if len(spec) > 0 && spec[0] == '"' {
		// A model sometimes passes an object argument as its JSON text. The
		// text of an object is the object; anything else is still refused by
		// PutRole, with the same message as a spec that was never a string.
		var text string
		if err := json.Unmarshal(spec, &text); err == nil {
			spec = bytes.TrimSpace([]byte(text))
		}
	}
	if len(spec) > 0 && !json.Valid(spec) {
		return failed("spec: not valid JSON"), nil
	}

	role, err := s.app.PutRole(r.Context(), args.Name, json.RawMessage(spec), c.Name())
	if err != nil {
		return s.roleError(args.Name, err), nil
	}
	return roleResult(role, "role "+role.Name+" saved"), nil
}

func (s *Server) deleteRole(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	if refused := operatorOnly(c, store.ScopeAdmin); refused != nil {
		return *refused, nil
	}
	var args roleNameArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}
	if args.Name == "" {
		return failed("name: a role needs a name"), nil
	}
	if err := s.app.Store().DeleteRole(r.Context(), args.Name); err != nil {
		return s.roleError(args.Name, err), nil
	}
	return ok("role "+args.Name+" deleted", map[string]any{"name": args.Name, "deleted": true}), nil
}

type inspectMarketplaceArgs struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

func (s *Server) inspectMarketplace(r *http.Request, c Caller, raw json.RawMessage) (any, *rpcError) {
	// Admin, like the REST endpoint: it spends the installation's git token on
	// a repository the caller names.
	if refused := operatorOnly(c, store.ScopeAdmin); refused != nil {
		return *refused, nil
	}
	var args inspectMarketplaceArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "arguments could not be read"}
	}
	got, err := s.app.InspectMarketplace(r.Context(), args.URL, args.Ref)
	if err != nil {
		var invalid *run.InvalidRequestError
		if errorsAs(err, &invalid) {
			return failed(invalid.Field + ": " + invalid.Detail), nil
		}
		s.log.Error("MCP inspect_marketplace failed", "error", err)
		return failed("the marketplace could not be inspected"), nil
	}

	plugins := make([]map[string]any, 0, len(got.Plugins))
	lines := make([]string, 0, len(got.Plugins)+1)
	if got.Found {
		lines = append(lines, fmt.Sprintf("marketplace %s (%s), %d plugin(s); enable them as \"<plugin>@%s\":",
			got.Name, got.URL, len(got.Plugins), got.Name))
	} else {
		lines = append(lines, fmt.Sprintf("no usable marketplace manifest at %s: %s", got.URL, got.Problem))
	}
	for _, p := range got.Plugins {
		item := map[string]any{"name": p.Name}
		line := p.Name
		if p.Version != "" {
			item["version"] = p.Version
			line += " " + p.Version
		}
		if p.Description != "" {
			item["description"] = p.Description
			line += " — " + p.Description
		}
		plugins = append(plugins, item)
		lines = append(lines, line)
	}
	body := map[string]any{"url": got.URL, "found": got.Found, "plugins": plugins}
	for k, v := range map[string]string{
		"ref": got.Ref, "name": got.Name, "description": got.Description,
		"manifest_path": got.ManifestPath, "problem": got.Problem,
	} {
		if v != "" {
			body[k] = v
		}
	}
	return ok(joinLines(lines), body), nil
}

// roleResult is a role, rendered: the spec indented for the model, and the
// same object REST's GET /roles/{name} returns for a program.
func roleResult(role store.Role, heading string) toolResult {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, role.Spec, "", "  "); err != nil {
		pretty.Reset()
		pretty.Write(role.Spec)
	}
	return ok(heading+"\n\n"+pretty.String(), map[string]any{
		"name":       role.Name,
		"spec":       json.RawMessage(role.Spec),
		"created_by": role.CreatedBy,
		"updated_at": role.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) roleError(name string, err error) toolResult {
	var invalid *run.InvalidRequestError
	if errorsAs(err, &invalid) {
		return failed(invalid.Field + ": " + invalid.Detail)
	}
	if errorsIs(err, store.ErrNotFound) {
		return failed(fmt.Sprintf("no role named %q", name))
	}
	s.log.Error("MCP role tool failed", "role", name, "error", err)
	return failed("the request could not be completed")
}
