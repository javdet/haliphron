package backend

import (
	"context"
	"net/http"
	"testing"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/store"
)

// Removing a token, as distinct from revoking it.
//
// Revocation ends a credential and keeps its row; removal deletes the row. The
// rules checked here are the ones that keep removal from being a second, quieter
// way to end a credential: only a token that has already ended can be removed,
// the removal is audited because nothing else will remember it, and the
// bootstrap row is never removed because it is what keeps that token revoked.

func (h *harness) aServiceToken(t *testing.T, name string, ttl time.Duration) store.Token {
	t.Helper()
	token, err := h.Store.CreateToken(context.Background(), store.Token{
		Name: name, Kind: store.TokenKindService, Scopes: []string{store.ScopeRunsRead},
		CreatedBy: "test",
	}, ttl)
	if err != nil {
		t.Fatalf("create token %s: %v", name, err)
	}
	return token
}

func (h *harness) listsToken(t *testing.T, id runv1.ULID) bool {
	t.Helper()
	tokens, err := h.Store.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	for _, tok := range tokens {
		if tok.ID == id {
			return true
		}
	}
	return false
}

func removePath(id runv1.ULID) string { return "/api/v1/tokens/" + string(id) + "/remove" }

func TestARevokedTokenCanBeRemovedAndTheRemovalIsAudited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.Token(store.ScopeAdmin)
	victim := h.aServiceToken(t, "old-laptop", 0)

	if status := h.call(t, admin, http.MethodDelete, "/api/v1/tokens/"+string(victim.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke: %d", status)
	}
	if status := h.call(t, admin, http.MethodPost, removePath(victim.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("remove a revoked token: %d", status)
	}
	if h.listsToken(t, victim.ID) {
		t.Fatal("a removed token is still listed")
	}

	var action, actor, name string
	if err := h.Store.DB().QueryRowContext(ctx, `
		SELECT action, actor, payload->>'name' FROM audit_log
		WHERE subject_kind = 'token' AND subject_id = $1`, string(victim.ID)).Scan(&action, &actor, &name); err != nil {
		t.Fatalf("read the audit record of the removal: %v", err)
	}
	if action != store.AuditTokenRemoved || name != "old-laptop" || actor != "token:test" {
		t.Fatalf("audit record is %q by %q naming %q", action, actor, name)
	}

	if status := h.call(t, admin, http.MethodPost, removePath(victim.ID), nil, nil); status != http.StatusNotFound {
		t.Fatalf("removing it twice: %d, want 404", status)
	}
}

// Revoke first, then remove. A live credential that simply vanished would
// leave no row saying it was ever ended.
func TestALiveTokenIsNotRemovedUntilItIsRevoked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.Token(store.ScopeAdmin)
	live := h.aServiceToken(t, "ci-pipeline", time.Hour)

	if status := h.call(t, admin, http.MethodPost, removePath(live.ID), nil, nil); status != http.StatusConflict {
		t.Fatalf("remove a live token: %d, want 409", status)
	}
	if _, err := h.Store.AuthenticateToken(ctx, live.Secret); err != nil {
		t.Fatalf("a refused removal left the token unusable: %v", err)
	}
	if !h.listsToken(t, live.ID) {
		t.Fatal("a refused removal removed the token")
	}
}

// Expiry ends a token as surely as revocation does.
func TestAnExpiredTokenCanBeRemovedWithoutBeingRevoked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.Token(store.ScopeAdmin)
	lapsed := h.aServiceToken(t, "last-quarter", time.Hour)

	if _, err := h.Store.DB().ExecContext(ctx,
		`UPDATE api_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		string(lapsed.ID)); err != nil {
		t.Fatalf("age the token: %v", err)
	}
	if status := h.call(t, admin, http.MethodPost, removePath(lapsed.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("remove an expired token: %d", status)
	}
	if h.listsToken(t, lapsed.ID) {
		t.Fatal("a removed token is still listed")
	}
}

// DELETE on a token keeps meaning what it meant before removal existed.
func TestDeletingATokenStillRevokesItAndKeepsTheRow(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)
	victim := h.aServiceToken(t, "old-laptop", 0)

	if status := h.call(t, admin, http.MethodDelete, "/api/v1/tokens/"+string(victim.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke: %d", status)
	}
	if !h.listsToken(t, victim.ID) {
		t.Fatal("DELETE removed the row instead of revoking the token")
	}
}

// The row is the tombstone. Without it the next start finds no digest for the
// value still in the Secret and installs it again as a fresh admin token.
func TestTheBootstrapRowIsNeverRemovedBecauseItIsWhatKeepsItRevoked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.Token(store.ScopeAdmin)

	if _, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, 0); err != nil {
		t.Fatalf("install the bootstrap token: %v", err)
	}
	installed, err := h.Store.AuthenticateToken(ctx, aBootstrapToken)
	if err != nil {
		t.Fatalf("authenticate the bootstrap token: %v", err)
	}
	if err := h.Store.RevokeToken(ctx, installed.ID); err != nil {
		t.Fatalf("revoke the bootstrap token: %v", err)
	}

	if status := h.call(t, admin, http.MethodPost, removePath(installed.ID), nil, nil); status != http.StatusConflict {
		t.Fatalf("remove the bootstrap row: %d, want 409", status)
	}

	outcome, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, 0)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if outcome != store.BootstrapSpent {
		t.Fatalf("after a refused removal the next start reads %q, want %q", outcome, store.BootstrapSpent)
	}
}

func TestRemovingATokenNeedsAdmin(t *testing.T) {
	h := newHarness(t)
	reader := h.Token(store.ScopeRunsRead, store.ScopeRunsWrite)
	victim := h.aServiceToken(t, "old-laptop", 0)
	if err := h.Store.RevokeToken(context.Background(), victim.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if status := h.call(t, reader, http.MethodPost, removePath(victim.ID), nil, nil); status != http.StatusForbidden {
		t.Fatalf("remove without admin: %d, want 403", status)
	}
	if !h.listsToken(t, victim.ID) {
		t.Fatal("a refused removal removed the token")
	}
}
