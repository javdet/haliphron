package backend

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// Retention for per-run tokens: every run mints one, and an installation that
// keeps its runs must not keep one dead token per run in the list for ever.

func (h *harness) aRunToken(runID runv1.ULID) store.Token {
	h.t.Helper()
	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: "run " + string(runID), Kind: store.TokenKindRunMCP,
		Scopes: []string{store.ScopeRunsWrite, store.ScopeRunsRead},
		RunID:  runID, CreatedBy: "test",
	}, time.Hour)
	if err != nil {
		h.t.Fatalf("mint a per-run token: %v", err)
	}
	return token
}

func (h *harness) aUserToken(name string) store.Token {
	h.t.Helper()
	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: name, Kind: store.TokenKindUser, Scopes: []string{store.ScopeRunsRead}, CreatedBy: "test",
	}, 0)
	if err != nil {
		h.t.Fatalf("mint a user token: %v", err)
	}
	return token
}

func (h *harness) tokenCreatedAgo(id runv1.ULID, ago time.Duration) {
	h.t.Helper()
	if _, err := h.Store.DB().ExecContext(context.Background(),
		`UPDATE api_tokens SET created_at = now() - make_interval(secs => $2) WHERE id = $1`,
		string(id), ago.Seconds()); err != nil {
		h.t.Fatalf("backdate token %s: %v", id, err)
	}
}

func (h *harness) tokenExists(id runv1.ULID) bool {
	h.t.Helper()
	var n int
	if err := h.Store.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM api_tokens WHERE id = $1`, string(id)).Scan(&n); err != nil {
		h.t.Fatalf("look up token %s: %v", id, err)
	}
	return n == 1
}

type tokenRetentionBody struct {
	RunTokenRetentionSeconds int64  `json:"run_token_retention_seconds"`
	Source                   string `json:"source"`
	UpdatedBy                string `json:"updated_by"`
}

func TestAnEndedRunTokenIsDeletedAfterTwoWeeksByDefaultAndNoOtherTokenIs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	oldRun := h.aFinishedRun()
	recentRun := h.aFinishedRun()
	liveRun := h.Submit()

	old := h.aRunToken(oldRun.ID)
	recent := h.aRunToken(recentRun.ID)
	live := h.aRunToken(liveRun.ID)
	for _, id := range []runv1.ULID{oldRun.ID, recentRun.ID} {
		if err := h.Store.RevokeRunTokens(ctx, id); err != nil {
			t.Fatalf("revoke the tokens of %s: %v", id, err)
		}
	}
	revokedUser := h.aUserToken("by-hand")
	if err := h.Store.RevokeToken(ctx, revokedUser.ID); err != nil {
		t.Fatalf("revoke a user token: %v", err)
	}

	h.tokenCreatedAgo(old.ID, 14*24*time.Hour+time.Minute)
	h.tokenCreatedAgo(recent.ID, 14*24*time.Hour-time.Hour)
	h.tokenCreatedAgo(live.ID, 40*24*time.Hour)
	h.tokenCreatedAgo(revokedUser.ID, 40*24*time.Hour)

	keep, err := h.App.TokenRetention(ctx)
	if err != nil {
		t.Fatalf("read token retention: %v", err)
	}
	if keep.RunTokens != 14*24*time.Hour || keep.Source != app.TokenRetentionDefault {
		t.Fatalf("retention = %s from %s, want 336h from the default", keep.RunTokens, keep.Source)
	}

	n, err := h.App.ExpireRunTokens(ctx, keep.RunTokens)
	if err != nil {
		t.Fatalf("expire run tokens: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d tokens, want 1", n)
	}
	if h.tokenExists(old.ID) {
		t.Error("an ended per-run token older than two weeks is still there")
	}
	if !h.tokenExists(recent.ID) {
		t.Error("an ended per-run token younger than two weeks was deleted")
	}
	if !h.tokenExists(live.ID) {
		t.Error("a per-run token that still works was deleted")
	}
	if !h.tokenExists(revokedUser.ID) {
		t.Error("a user token was deleted by the per-run token retention")
	}
	if !h.runExists(oldRun.ID) {
		t.Error("deleting a run's token deleted the run")
	}
}

func TestAnOperatorChangesTheRunTokenRetentionFromTheAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.Token(store.ScopeAdmin)
	writer := h.Token(store.ScopeRunsWrite, store.ScopeRunsRead)

	var got tokenRetentionBody
	if status := h.call(t, writer, http.MethodGet, "/api/v1/token-retention", nil, &got); status != http.StatusOK {
		t.Fatalf("read token retention with runs:read: %d", status)
	}
	if got.RunTokenRetentionSeconds != 1209600 || got.Source != "default" {
		t.Errorf("retention = %+v, want 1209600 s by default", got)
	}

	if status := h.call(t, writer, http.MethodPut, "/api/v1/token-retention",
		map[string]any{"run_token_retention_seconds": 0}, nil); status != http.StatusForbidden {
		t.Errorf("runs:write changed the token retention: %d, want 403", status)
	}
	for _, body := range []map[string]any{
		{},
		{"run_token_retention_seconds": -1},
		{"run_token_retention_seconds": 60},
	} {
		if status := h.call(t, admin, http.MethodPut, "/api/v1/token-retention", body, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("PUT %v = %d, want 422", body, status)
		}
	}

	if status := h.call(t, admin, http.MethodPut, "/api/v1/token-retention",
		map[string]any{"run_token_retention_seconds": 30 * 86400}, &got); status != http.StatusOK {
		t.Fatalf("save token retention: %d", status)
	}
	if got.RunTokenRetentionSeconds != 30*86400 || got.Source != "database" || got.UpdatedBy == "" {
		t.Errorf("saved retention = %+v, want 30 days from the database with who saved it", got)
	}
	var saved int
	if err := h.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE action = $1`, store.AuditTokenRetentionSaved).Scan(&saved); err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	if saved != 1 {
		t.Errorf("saving the token retention left %d audit records, want 1", saved)
	}

	finished := h.aFinishedRun()
	token := h.aRunToken(finished.ID)
	if err := h.Store.RevokeRunTokens(ctx, finished.ID); err != nil {
		t.Fatalf("revoke the run's token: %v", err)
	}
	h.tokenCreatedAgo(token.ID, 20*24*time.Hour)
	keep, err := h.App.TokenRetention(ctx)
	if err != nil {
		t.Fatalf("read token retention: %v", err)
	}
	if n, err := h.App.ExpireRunTokens(ctx, keep.RunTokens); err != nil || n != 0 {
		t.Errorf("a 30-day retention deleted %d tokens 20 days old (err %v)", n, err)
	}

	// Zero keeps per-run tokens for ever.
	if status := h.call(t, admin, http.MethodPut, "/api/v1/token-retention",
		map[string]any{"run_token_retention_seconds": 0}, &got); status != http.StatusOK || got.RunTokenRetentionSeconds != 0 {
		t.Fatalf("save a zero token retention: %d, %+v", status, got)
	}
	h.tokenCreatedAgo(token.ID, 400*24*time.Hour)
	if keep, err = h.App.TokenRetention(ctx); err != nil {
		t.Fatalf("read token retention: %v", err)
	}
	if n, err := h.App.ExpireRunTokens(ctx, keep.RunTokens); err != nil || n != 0 || !h.tokenExists(token.ID) {
		t.Errorf("a zero retention deleted %d tokens (err %v); zero means keep forever", n, err)
	}

	// Kept for ever is kept as a row, not as a credential.
	if _, err := h.Store.AuthenticateToken(ctx, token.Secret); !errors.Is(err, store.ErrTokenInvalid) {
		t.Errorf("an ended per-run token authenticated: %v", err)
	}
}
