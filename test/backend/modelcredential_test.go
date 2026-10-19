package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// The model credential is the one secret every run needs, and the one whose
// absence is only discovered in a pod. Its endpoint says whether it is there
// and what kind it is, and still never says what it is.

func TestTheModelCredentialSaysWhatItIsAndNeverWhatItHolds(t *testing.T) {
	h := newHarness(t)
	reader := h.Token(store.ScopeRunsRead)
	admin := h.Token(store.ScopeAdmin)

	var got map[string]any
	if status := h.call(t, reader, http.MethodGet, "/api/v1/model-credential", nil, &got); status != http.StatusOK {
		t.Fatalf("get: status %d; a caller who submits runs is owed the reason they fail", status)
	}
	if got["secret_name"] != "llm-api-key" || got["configured"] != true || got["type"] != "api_key" {
		t.Errorf("the harness's stored key reads back as %v", got)
	}

	const oauth = "sk-ant-oat01-a-subscription-token-for-this-test"
	if status := h.call(t, admin, http.MethodPut, "/api/v1/model-credential",
		map[string]any{"type": "oauth_token", "value": oauth + "\n"}, &got); status != http.StatusOK {
		t.Fatalf("put: status %d", status)
	}
	if got["type"] != "oauth_token" || got["kind"] != "managed" {
		t.Errorf("after storing an OAuth token: %v", got)
	}
	raw, _ := json.Marshal(got)
	if contains(string(raw), oauth) {
		t.Fatal("the credential's value came back from the API")
	}

	// Stored trimmed, under the configured name, so the lease path reads it.
	value, err := h.Store.ResolveSecret(context.Background(), "llm-api-key")
	if err != nil || string(value) != oauth {
		t.Errorf("stored %q (%v), want the token without its newline", value, err)
	}
}

// The pod decides by prefix whatever is declared, so a declared type the value
// contradicts is a paste into the wrong box. It is refused now rather than
// becoming a 401 in a pod.
func TestAModelCredentialOfTheWrongTypeIsRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"an OAuth token declared as an API key",
			map[string]any{"type": "api_key", "value": "sk-ant-oat01-abc"}},
		{"an API key declared as an OAuth token",
			map[string]any{"type": "oauth_token", "value": "sk-ant-api03-abc"}},
		{"no type", map[string]any{"value": "sk-ant-api03-abc"}},
		{"an unknown type", map[string]any{"type": "password", "value": "sk-ant-api03-abc"}},
		{"neither value nor ref", map[string]any{"type": "api_key"}},
		{"both value and ref",
			map[string]any{"type": "api_key", "value": "sk-x", "ref": "vault://kv#llm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status := h.call(t, admin, http.MethodPut, "/api/v1/model-credential", tc.body, nil); status != http.StatusUnprocessableEntity {
				t.Errorf("status %d, want 422", status)
			}
		})
	}

	value, err := h.Store.ResolveSecret(context.Background(), "llm-api-key")
	if err != nil || string(value) != "sk-test-model-key" {
		t.Errorf("a refused request replaced the stored credential: %q (%v)", value, err)
	}
}

func TestOnlyAnAdminSetsTheModelCredential(t *testing.T) {
	h := newHarness(t)
	token := h.Token(store.ScopeRunsRead, store.ScopeRunsWrite)
	if status := h.call(t, token, http.MethodPut, "/api/v1/model-credential",
		map[string]any{"type": "api_key", "value": "sk-ant-api03-abc"}, nil); status != http.StatusForbidden {
		t.Errorf("status %d, want 403", status)
	}
}

// Absent is a state the UI warns on before anybody submits a run.
func TestAnAbsentModelCredentialIsReportedAsAbsent(t *testing.T) {
	h := newHarness(t)
	service := app.New(app.Options{Store: h.Store, Limits: app.Limits{LLMAPIKeySecret: "never-stored"}})

	cred, err := service.ModelCredential(context.Background())
	if err != nil {
		t.Fatalf("model credential: %v", err)
	}
	if cred.Configured || cred.SecretName != "never-stored" || cred.Type != "" {
		t.Errorf("an unstored credential reads as %+v", cred)
	}
}

// A gateway's key is useless without the gateway, so the two are stored as one
// fact and leased as one: the endpoint reads back normalised beside the type,
// and the lease carries it next to the key.
func TestAGatewayEndpointIsStoredWithTheKeyAndLeasedBesideIt(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	var got map[string]any
	if status := h.call(t, admin, http.MethodPut, "/api/v1/model-credential", map[string]any{
		"type": "api_key", "value": "sk-or-v1-an-openrouter-key", "base_url": " https://openrouter.ai/api/ ",
	}, &got); status != http.StatusOK {
		t.Fatalf("put: status %d", status)
	}
	if got["base_url"] != "https://openrouter.ai/api" || got["type"] != "api_key" {
		t.Errorf("after storing a gateway key: %v", got)
	}

	p := newProbe(t, h, "east")
	h.Submit()
	lease := p.LeaseOne()
	if lease.Secrets[runv1.SecretKeyLLMAPIKey] != "sk-or-v1-an-openrouter-key" {
		t.Error("the lease does not carry the gateway's key")
	}
	if got := lease.Secrets[runv1.SecretKeyLLMBaseURL]; got != "https://openrouter.ai/api" {
		t.Errorf("the lease's %s = %q, want the gateway", runv1.SecretKeyLLMBaseURL, got)
	}
}

// A key replaced without restating its endpoint must not go where the previous
// key went: an Anthropic key sent to a gateway is a key handed to a third
// party. Both writers clear it — this endpoint and the generic secrets one.
func TestReplacingTheKeyWithoutAnEndpointClearsTheOldOne(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		body       map[string]any
	}{
		{"model-credential", "/api/v1/model-credential",
			map[string]any{"type": "api_key", "value": "sk-ant-api03-a-direct-key"}},
		{"secrets", "/api/v1/secrets/llm-api-key",
			map[string]any{"value": "sk-ant-api03-a-direct-key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			admin := h.Token(store.ScopeAdmin)
			if status := h.call(t, admin, http.MethodPut, "/api/v1/model-credential", map[string]any{
				"type": "api_key", "value": "sk-or-v1-key", "base_url": "https://openrouter.ai/api",
			}, nil); status != http.StatusOK {
				t.Fatalf("put gateway: status %d", status)
			}
			if status := h.call(t, admin, http.MethodPut, tc.path, tc.body, nil); status != http.StatusOK {
				t.Fatalf("replace: status %d", status)
			}

			value, endpoint, err := h.Store.ResolveCredential(context.Background(), "llm-api-key")
			if err != nil || string(value) != "sk-ant-api03-a-direct-key" || endpoint != "" {
				t.Errorf("after replacing: value %q endpoint %q (%v), want the new key and no endpoint",
					value, endpoint, err)
			}
		})
	}
}

func TestAnUnusableGatewayEndpointIsRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"not a URL", map[string]any{"type": "api_key", "value": "sk-x", "base_url": "openrouter.ai/api"}},
		{"an API path rather than the root",
			map[string]any{"type": "api_key", "value": "sk-x", "base_url": "https://openrouter.ai/api/v1"}},
		{"credentials in the URL",
			map[string]any{"type": "api_key", "value": "sk-x", "base_url": "https://u:p@openrouter.ai/api"}},
		{"a query", map[string]any{"type": "api_key", "value": "sk-x", "base_url": "https://openrouter.ai/api?x=1"}},
		// A subscription token is good at Anthropic only; sent to a gateway
		// it is handed to whoever runs the gateway.
		{"an OAuth token with a gateway", map[string]any{
			"type": "oauth_token", "value": "sk-ant-oat01-abc", "base_url": "https://openrouter.ai/api"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var problem map[string]any
			if status := h.call(t, admin, http.MethodPut, "/api/v1/model-credential", tc.body, &problem); status != http.StatusUnprocessableEntity {
				t.Errorf("status %d, want 422", status)
			}
		})
	}

	value, endpoint, err := h.Store.ResolveCredential(context.Background(), "llm-api-key")
	if err != nil || string(value) != "sk-test-model-key" || endpoint != "" {
		t.Errorf("a refused request changed the stored credential: %q at %q (%v)", value, endpoint, err)
	}
}
