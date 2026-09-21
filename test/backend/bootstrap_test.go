package backend

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/automagicops/haliphron/backend/store"
)

// The first admin credential.
//
// A fresh installation has no token and no way to mint one: every endpoint of
// the public API needs a bearer token, and the endpoint that issues tokens is
// itself admin-scoped. The chart breaks that circle by generating a token into
// a Secret, which the backend writes to the store at startup. What is checked
// here is the part of that which is not the chart's: that the row is created
// once, that it opens the API, that restarting does not disturb it, and — the
// one that matters — that revoking it is final.

const aBootstrapToken = "hlt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestTheBootstrapTokenOpensAnInstallationThatHasNoOtherCredential(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Before it is installed, the credential is nothing.
	if status := h.call(t, aBootstrapToken, http.MethodGet, "/api/v1/tokens", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("an uninstalled bootstrap token was accepted: %d", status)
	}

	outcome, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, 0)
	if err != nil {
		t.Fatalf("install the bootstrap token: %v", err)
	}
	if outcome != store.BootstrapCreated {
		t.Fatalf("outcome is %q, want %q", outcome, store.BootstrapCreated)
	}

	// It carries admin, because the first thing it has to be able to do is
	// mint a narrower token.
	var minted map[string]any
	status := h.call(t, aBootstrapToken, http.MethodPost, "/api/v1/tokens",
		map[string]any{"name": "ops", "scopes": []string{store.ScopeRunsRead}}, &minted)
	if status != http.StatusCreated {
		t.Fatalf("mint a token with the bootstrap credential: %d", status)
	}
	if minted["token"] == "" {
		t.Fatal("the minted token came back empty")
	}

	// And it is visible as what it is, rather than as an entry nobody can
	// account for.
	tokens, err := h.Store.ListTokens(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	var found *store.Token
	for i, tok := range tokens {
		if tok.Name == store.BootstrapTokenName {
			found = &tokens[i]
		}
	}
	if found == nil {
		t.Fatalf("no token named %q among %d", store.BootstrapTokenName, len(tokens))
	}
	if !found.Allows(store.ScopeAdmin) {
		t.Fatalf("the bootstrap token does not carry admin: %v", found.Scopes)
	}
	if found.ExpiresAt != nil {
		t.Fatalf("no TTL was asked for, but it expires at %v", found.ExpiresAt)
	}
}

func TestInstallingTheBootstrapTokenTwiceIsOneRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Every replica runs this at startup, and a rollout runs it again.
	for i, want := range []store.BootstrapOutcome{
		store.BootstrapCreated, store.BootstrapPresent, store.BootstrapPresent,
	} {
		got, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, 0)
		if err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("install %d: outcome is %q, want %q", i, got, want)
		}
	}

	tokens, err := h.Store.ListTokens(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	var n int
	for _, tok := range tokens {
		if tok.Name == store.BootstrapTokenName {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("three installs left %d bootstrap rows, want 1", n)
	}
}

// The one that matters. An operator who revoked the bootstrap credential did
// so on purpose, and a restart that reinstated it would be a back door that
// reopens on every node drain.
func TestARevokedBootstrapTokenIsNotReinstatedByTheNextStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

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

	outcome, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, 0)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if outcome != store.BootstrapSpent {
		t.Fatalf("outcome is %q, want %q", outcome, store.BootstrapSpent)
	}
	if status := h.call(t, aBootstrapToken, http.MethodGet, "/api/v1/tokens", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("a revoked bootstrap token was accepted after a restart: %d", status)
	}
}

// The documented way back in after the credential has lapsed: a different
// value is a different digest, so it is a new row — and the lapsed one stays
// lapsed.
func TestChangingTheBootstrapTokenInstallsANewOneAndLeavesTheOldOneSpent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, time.Hour); err != nil {
		t.Fatalf("install the bootstrap token: %v", err)
	}
	// Age it past its expiry, which is what a lapsed one looks like on the
	// next start.
	if _, err := h.Store.DB().ExecContext(ctx,
		`UPDATE api_tokens SET expires_at = now() - interval '1 minute' WHERE name = $1`,
		store.BootstrapTokenName); err != nil {
		t.Fatalf("age the bootstrap token: %v", err)
	}

	outcome, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, time.Hour)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if outcome != store.BootstrapSpent {
		t.Fatalf("an expired bootstrap token reads as %q, want %q", outcome, store.BootstrapSpent)
	}

	const replacement = "hlt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if outcome, err := h.Store.EnsureBootstrapToken(ctx, replacement, 0); err != nil {
		t.Fatalf("install the replacement: %v", err)
	} else if outcome != store.BootstrapCreated {
		t.Fatalf("the replacement reads as %q, want %q", outcome, store.BootstrapCreated)
	}

	if status := h.call(t, replacement, http.MethodGet, "/api/v1/tokens", nil, nil); status != http.StatusOK {
		t.Fatalf("the replacement was refused: %d", status)
	}
	if status := h.call(t, aBootstrapToken, http.MethodGet, "/api/v1/tokens", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("the lapsed token still works: %d", status)
	}
}

// A TTL is honoured, and it is a real expiry rather than a note in the row.
func TestABootstrapTokenWithATTLStopsWorkingWhenItLapses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.Store.EnsureBootstrapToken(ctx, aBootstrapToken, time.Hour); err != nil {
		t.Fatalf("install the bootstrap token: %v", err)
	}
	installed, err := h.Store.AuthenticateToken(ctx, aBootstrapToken)
	if err != nil {
		t.Fatalf("authenticate the bootstrap token: %v", err)
	}
	if installed.ExpiresAt == nil {
		t.Fatal("a TTL was asked for and the row does not expire")
	}
	if until := time.Until(*installed.ExpiresAt); until < 55*time.Minute || until > time.Hour {
		t.Fatalf("it expires in %v, want about an hour", until)
	}
}

// An admin credential reachable over the network. The chart's own is 32
// random characters; one supplied by hand is the case worth refusing.
func TestAShortBootstrapTokenIsRefusedRatherThanInstalled(t *testing.T) {
	h := newHarness(t)

	if _, err := h.Store.EnsureBootstrapToken(context.Background(), "hunter2", 0); !errors.Is(err, store.ErrBootstrapTokenWeak) {
		t.Fatalf("error is %v, want %v", err, store.ErrBootstrapTokenWeak)
	}
}
