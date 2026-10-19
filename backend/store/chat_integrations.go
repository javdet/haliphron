package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Chat integrations: a bot's configuration kept as data, so an operator can
// change it from the UI. The semantics are at the top of
// db/migrations/0009_chat_integrations.sql. Credentials are not here; they are
// managed secrets under names the app layer fixes.

// Chat transports, as the chat_integrations.transport domain spells them.
const (
	ChatTransportWebSocket = "websocket"
	ChatTransportWebhook   = "webhook"
	ChatTransportBoth      = "both"
)

// ChatIntegration is a row.
type ChatIntegration struct {
	Platform       string
	Enabled        bool
	Transport      string
	URL            string
	Role           string
	RepoURL        string
	BaseBranch     string
	RunURL         string
	ReplyPolicy    string
	MaxPostRunes   int
	MaxThreadBytes int
	UpdatedBy      string
	UpdatedAt      time.Time
}

// ChatIntegration reads a platform's configuration, or ErrNotFound when none
// was ever saved.
func (s *Store) ChatIntegration(ctx context.Context, platform string) (ChatIntegration, error) {
	var c ChatIntegration
	err := s.db.QueryRowContext(ctx, `
		SELECT platform, enabled, transport, url, role, repo_url, base_branch, run_url,
		       reply_policy, max_post_runes, max_thread_bytes, updated_by, updated_at
		  FROM chat_integrations WHERE platform = $1`, platform).Scan(
		&c.Platform, &c.Enabled, &c.Transport, &c.URL, &c.Role, &c.RepoURL, &c.BaseBranch,
		&c.RunURL, &c.ReplyPolicy, &c.MaxPostRunes, &c.MaxThreadBytes, &c.UpdatedBy, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatIntegration{}, ErrNotFound
	}
	if err != nil {
		return ChatIntegration{}, fmt.Errorf("store: read chat integration %s: %w", platform, err)
	}
	return c, nil
}

// PutChatIntegration saves a platform's configuration whole and returns the
// stored row. Every save moves updated_at, which is what tells the other
// replicas to reconnect.
func (s *Store) PutChatIntegration(ctx context.Context, c ChatIntegration) (ChatIntegration, error) {
	var out ChatIntegration
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO chat_integrations (platform, enabled, transport, url, role, repo_url, base_branch,
		                               run_url, reply_policy, max_post_runes, max_thread_bytes,
		                               updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, clock_timestamp())
		ON CONFLICT (tenant_id, platform) DO UPDATE SET
		       enabled = EXCLUDED.enabled, transport = EXCLUDED.transport, url = EXCLUDED.url,
		       role = EXCLUDED.role, repo_url = EXCLUDED.repo_url,
		       base_branch = EXCLUDED.base_branch, run_url = EXCLUDED.run_url,
		       reply_policy = EXCLUDED.reply_policy, max_post_runes = EXCLUDED.max_post_runes,
		       max_thread_bytes = EXCLUDED.max_thread_bytes, updated_by = EXCLUDED.updated_by,
		       updated_at = EXCLUDED.updated_at
		RETURNING platform, enabled, transport, url, role, repo_url, base_branch, run_url,
		          reply_policy, max_post_runes, max_thread_bytes, updated_by, updated_at`,
		c.Platform, c.Enabled, c.Transport, c.URL, c.Role, c.RepoURL, c.BaseBranch, c.RunURL,
		c.ReplyPolicy, c.MaxPostRunes, c.MaxThreadBytes, c.UpdatedBy).Scan(
		&out.Platform, &out.Enabled, &out.Transport, &out.URL, &out.Role, &out.RepoURL,
		&out.BaseBranch, &out.RunURL, &out.ReplyPolicy, &out.MaxPostRunes, &out.MaxThreadBytes,
		&out.UpdatedBy, &out.UpdatedAt)
	if err != nil {
		return ChatIntegration{}, fmt.Errorf("store: save chat integration %s: %w", c.Platform, err)
	}
	return out, nil
}

// ChatIntegrationVersion is the platform's updated_at, zero when nothing was
// ever saved. It is the cheap read each replica polls.
func (s *Store) ChatIntegrationVersion(ctx context.Context, platform string) (time.Time, error) {
	var at time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT updated_at FROM chat_integrations WHERE platform = $1`, platform).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: read chat integration version %s: %w", platform, err)
	}
	return at, nil
}
