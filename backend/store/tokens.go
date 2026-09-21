package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// API, MCP and per-run tokens. Only digests are stored.
//
// run-mcp is the class that makes child runs accountable. The token handed to
// a pod carries its run_id and dies with the run, so a run_agent call arriving
// from inside an agent can be attributed to a parent, checked against the depth
// limit and charged to the right budget. A global MCP token makes all three
// impossible at once, which is the detail the original specification missed.

// Token kinds, as the token_kind domain spells them.
const (
	TokenKindUser    = "user"
	TokenKindService = "service"
	TokenKindRunMCP  = "run-mcp"
)

// Scopes. Three of them, matching the three platform roles in v1; a
// fine-grained model is phase 4 and gets a table rather than more strings.
const (
	ScopeRunsRead  = "runs:read"
	ScopeRunsWrite = "runs:write"
	ScopeAdmin     = "admin"
)

// TokenPrefix tells a platform token from a cluster JWT at a glance, in a log
// line and in a paste into an issue.
const TokenPrefix = "hlt_"

// Token is an issued credential. Secret is set only by Create, and only once.
type Token struct {
	ID        runv1.ULID
	Name      string
	Kind      string
	Scopes    []string
	Subject   string
	RunID     runv1.ULID
	ExpiresAt *time.Time
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time

	Secret string
}

// Allows reports whether the token carries a scope. admin implies the rest:
// the alternative is an admin token that cannot read a run, which is a rule
// nobody remembers and everybody works around by granting all three.
func (t Token) Allows(scope string) bool {
	return slices.Contains(t.Scopes, ScopeAdmin) || slices.Contains(t.Scopes, scope)
}

// ErrTokenInvalid is an unknown, revoked or expired credential. The three are
// one error on purpose: telling a caller which of them it was is telling an
// attacker which guess was closer.
var ErrTokenInvalid = errors.New("store: token is unknown, revoked or expired")

// CreateToken mints a credential and returns it once.
func (s *Store) CreateToken(ctx context.Context, t Token, ttl time.Duration) (Token, error) {
	secret, err := randomToken()
	if err != nil {
		return Token{}, err
	}
	t.ID = newID()
	t.Secret = TokenPrefix + secret
	digest := sha256.Sum256([]byte(t.Secret))

	if ttl > 0 {
		expires := time.Now().Add(ttl)
		t.ExpiresAt = &expires
	}
	if t.Kind == TokenKindRunMCP && t.ExpiresAt == nil {
		return Token{}, fmt.Errorf("store: a run-mcp token must expire")
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO api_tokens (id, name, kind, token_sha256, scopes, subject, run_id,
		                        expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5::text[], $6, $7, $8, $9)
		RETURNING created_at`,
		t.ID, t.Name, t.Kind, digest[:], textArray(t.Scopes), nullString(t.Subject),
		nullString(string(t.RunID)), nullTime(t.ExpiresAt), t.CreatedBy).Scan(&t.CreatedAt)
	if err != nil {
		return Token{}, fmt.Errorf("store: create token %s: %w", t.Name, err)
	}
	return t, nil
}

// AuthenticateToken resolves a presented credential.
//
// last_used_at is written at most once a minute. The column exists so an
// operator can spot a token nobody uses, and minute resolution answers that
// question exactly as well as microsecond resolution does — while keeping
// every authentication from being a write, and keeping the write HOT, since
// nothing indexed changes.
func (s *Store) AuthenticateToken(ctx context.Context, presented string) (Token, error) {
	digest := sha256.Sum256([]byte(presented))

	var (
		t       Token
		subject sql.NullString
		runID   sql.NullString
		expires sql.NullTime
		revoked sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, scopes, subject, run_id, expires_at, created_by, created_at, revoked_at
		FROM api_tokens WHERE token_sha256 = $1`, digest[:]).Scan(
		&t.ID, &t.Name, &t.Kind, pgArray(&t.Scopes), &subject, &runID,
		&expires, &t.CreatedBy, &t.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrTokenInvalid
	}
	if err != nil {
		return Token{}, fmt.Errorf("store: authenticate token: %w", err)
	}

	t.Subject = subject.String
	t.RunID = runv1.ULID(runID.String)
	t.ExpiresAt = timePtr(expires)
	t.RevokedAt = timePtr(revoked)

	if t.RevokedAt != nil || (t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now())) {
		return Token{}, ErrTokenInvalid
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE api_tokens SET last_used_at = now()
		WHERE id = $1
		  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, t.ID); err != nil {
		return Token{}, fmt.Errorf("store: record token use: %w", err)
	}
	return t, nil
}

// ListTokens returns the issued credentials, without a secret among them.
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, scopes, subject, run_id, expires_at, created_by, created_at, revoked_at
		FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var (
			t       Token
			subject sql.NullString
			runID   sql.NullString
			expires sql.NullTime
			revoked sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, pgArray(&t.Scopes), &subject, &runID,
			&expires, &t.CreatedBy, &t.CreatedAt, &revoked); err != nil {
			return nil, fmt.Errorf("store: scan token: %w", err)
		}
		t.Subject = subject.String
		t.RunID = runv1.ULID(runID.String)
		t.ExpiresAt = timePtr(expires)
		t.RevokedAt = timePtr(revoked)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken ends a credential.
func (s *Store) RevokeToken(ctx context.Context, id runv1.ULID) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("store: revoke token %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeRunTokens ends every per-run token of a run. Called when the run ends:
// a token that outlives its run is a token that can start work charged to a
// budget nobody is watching any more.
func (s *Store) RevokeRunTokens(ctx context.Context, runID runv1.ULID) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		WHERE run_id = $1 AND revoked_at IS NULL`, runID); err != nil {
		return fmt.Errorf("store: revoke tokens of %s: %w", runID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// the first one
// ---------------------------------------------------------------------------

// The bootstrap credential.
//
// Every endpoint of the public API needs a bearer token, and the endpoint that
// issues tokens is itself admin-scoped. A fresh installation therefore has no
// way in: the first credential cannot come from the API that the first
// credential is needed to call. It has to be installed from outside, and the
// chart is what installs it — a random token generated into a Secret, mounted
// as a file, and written here at startup.

// BootstrapTokenName is what the row is called, so that the credential an
// installation started with is obvious in the tokens list rather than being
// one more entry nobody can account for.
const BootstrapTokenName = "bootstrap"

// MinBootstrapTokenLength is the floor. This is an admin credential reachable
// over the network, and one supplied by hand is the case worth checking: the
// chart's own is 32 random characters.
const MinBootstrapTokenLength = 16

// ErrBootstrapTokenWeak is a bootstrap credential too short to be one.
var ErrBootstrapTokenWeak = errors.New("store: the bootstrap token is too short")

// BootstrapOutcome is what EnsureBootstrapToken found. It is returned rather
// than logged here, because what an operator needs to be told differs by case
// and the store is not where that is decided.
type BootstrapOutcome string

const (
	// BootstrapCreated: there was no row for this credential, and one was
	// written. A first install says this once.
	BootstrapCreated BootstrapOutcome = "created"

	// BootstrapPresent: the row is already there and usable. Every restart
	// after the first says this.
	BootstrapPresent BootstrapOutcome = "present"

	// BootstrapSpent: the row is there and revoked or expired, and is left
	// exactly as it is. An operator who revoked the bootstrap credential did
	// so on purpose — normally because a token of their own now exists — and
	// a restart that quietly reinstated it would be a back door that reopens
	// on every node drain.
	BootstrapSpent BootstrapOutcome = "spent"
)

// EnsureBootstrapToken installs the admin credential an installation starts
// with, once.
//
// It is keyed on the digest and not on the name, which is what makes the
// recovery path work: a bootstrap token that was revoked or allowed to expire
// is never resurrected, and changing the value in the Secret is a different
// digest and therefore a new row. That is the documented way back into an
// installation whose first credential has lapsed, and it leaves the lapsed one
// lapsed.
func (s *Store) EnsureBootstrapToken(ctx context.Context, secret string, ttl time.Duration) (BootstrapOutcome, error) {
	if len(secret) < MinBootstrapTokenLength {
		return "", fmt.Errorf("%w: %d characters, and %d is the floor",
			ErrBootstrapTokenWeak, len(secret), MinBootstrapTokenLength)
	}
	digest := sha256.Sum256([]byte(secret))

	var expires, revoked sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at, revoked_at FROM api_tokens WHERE token_sha256 = $1`,
		digest[:]).Scan(&expires, &revoked)
	switch {
	case err == nil:
		if revoked.Valid || (expires.Valid && !expires.Time.After(time.Now())) {
			return BootstrapSpent, nil
		}
		return BootstrapPresent, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("store: look up the bootstrap token: %w", err)
	}

	var expiresAt *time.Time
	if ttl > 0 {
		at := time.Now().Add(ttl)
		expiresAt = &at
	}

	// admin and nothing else. The scope exists so that the first thing this
	// credential can do is mint a narrower one.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO api_tokens (id, name, kind, token_sha256, scopes, subject,
		                        expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5::text[], $2, $6, $2)
		ON CONFLICT (token_sha256) DO NOTHING`,
		newID(), BootstrapTokenName, TokenKindUser, digest[:],
		textArray([]string{ScopeAdmin}), nullTime(expiresAt))
	if err != nil {
		return "", fmt.Errorf("store: create the bootstrap token: %w", err)
	}
	// DO NOTHING rather than a unique-violation: every replica runs this at
	// startup, and the one that loses the race wanted the row that is now
	// there.
	if n, _ := res.RowsAffected(); n == 0 {
		return BootstrapPresent, nil
	}
	return BootstrapCreated, nil
}
