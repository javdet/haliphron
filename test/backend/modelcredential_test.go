package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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
