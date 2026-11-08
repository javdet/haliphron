package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Retention for per-run tokens.
//
// Every run that reaches the MCP endpoint mints a run-mcp token, revoked when
// the run ends, and the row stays. An installation that keeps its runs would
// otherwise grow its tokens list by one dead entry per run for ever. So an
// ended run-mcp token is deleted once it is older than an age the operator
// sets from the UI, two weeks unless changed. User and service tokens are not
// touched: they are minted by hand and removed by hand.

// runTokenRetentionBatch bounds one DELETE, for the same reason
// runRetentionBatch does: the first pass on an old installation finds every
// run it ever had.
const runTokenRetentionBatch = 1000

const (
	// DefaultRunTokenRetention is how long an ended per-run token is kept
	// until an operator saves another number.
	DefaultRunTokenRetention = 14 * 24 * time.Hour

	// MinRunTokenRetention is the shortest age that may be saved, zero aside.
	// An ended token is no use to anyone, but it is what says which run a
	// call came from while somebody is still looking at that run.
	MinRunTokenRetention = time.Hour

	// MaxRunTokenRetention bounds the number so it stays a duration.
	MaxRunTokenRetention = 10 * 365 * 24 * time.Hour
)

// Where the per-run token age comes from.
const (
	TokenRetentionDefault  = "default"
	TokenRetentionDatabase = "database"
)

// TokenRetention is what is in force.
type TokenRetention struct {
	// RunTokens is the age of an ended per-run token; zero keeps them for ever.
	RunTokens time.Duration
	// Source says whether RunTokens was saved or is the default.
	Source    string
	UpdatedBy string
	UpdatedAt *time.Time
}

// TokenRetention reads the retention in force.
func (s *Service) TokenRetention(ctx context.Context) (TokenRetention, error) {
	out := TokenRetention{RunTokens: DefaultRunTokenRetention, Source: TokenRetentionDefault}
	stored, err := s.store.TokenRetention(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return TokenRetention{}, err
	}
	out.RunTokens, out.Source = stored.RunTokens, TokenRetentionDatabase
	out.UpdatedBy, out.UpdatedAt = stored.UpdatedBy, &stored.UpdatedAt
	return out, nil
}

// PutRunTokenRetention saves the age of an ended per-run token. Zero keeps
// them for ever; anything else must be whole seconds between the bounds above.
func (s *Service) PutRunTokenRetention(ctx context.Context, keep time.Duration, by string) (TokenRetention, error) {
	switch {
	case keep < 0 || keep%time.Second != 0:
		return TokenRetention{}, &run.InvalidRequestError{Field: "run_token_retention_seconds",
			Detail: "must be a whole number of seconds, 0 to keep per-run tokens for ever"}
	case keep != 0 && keep < MinRunTokenRetention:
		return TokenRetention{}, &run.InvalidRequestError{Field: "run_token_retention_seconds",
			Detail: fmt.Sprintf("must be at least %d (an hour), or 0 to keep per-run tokens for ever",
				int64(MinRunTokenRetention/time.Second))}
	case keep > MaxRunTokenRetention:
		return TokenRetention{}, &run.InvalidRequestError{Field: "run_token_retention_seconds",
			Detail: fmt.Sprintf("must be at most %d (ten years)", int64(MaxRunTokenRetention/time.Second))}
	}
	if _, err := s.store.PutTokenRetention(ctx, keep, by); err != nil {
		return TokenRetention{}, err
	}
	return s.TokenRetention(ctx)
}

// reapRunTokensOnce is one pass, run by ReapRuns after the runs. The age is
// read again on every pass, so a change saved on one replica is applied by
// whichever replica runs the next one.
func (s *Service) reapRunTokensOnce(ctx context.Context) {
	keep, err := s.TokenRetention(ctx)
	if err != nil {
		// Wait for the next pass rather than fall back to a default the
		// operator may have replaced with "for ever".
		if !ctxDone(ctx) {
			s.log.Error("could not read the per-run token retention", "error", err)
		}
		return
	}
	deleted, err := s.ExpireRunTokens(ctx, keep.RunTokens)
	if err != nil && !ctxDone(ctx) {
		s.log.Error("per-run token retention failed", "error", err)
	}
	if deleted > 0 {
		s.log.Info("ended per-run tokens deleted", "tokens", deleted, "retention", keep.RunTokens)
	}
}

// ExpireRunTokens is one retention pass: it deletes every ended per-run token
// created more than keep ago, batch by batch, and returns how many went. A
// zero keep deletes nothing.
func (s *Service) ExpireRunTokens(ctx context.Context, keep time.Duration) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	cutoff := s.now().Add(-keep)

	var total int
	for {
		n, err := s.store.DeleteEndedRunTokens(ctx, cutoff, runTokenRetentionBatch)
		total += n
		if err != nil {
			return total, err
		}
		if n < runTokenRetentionBatch || ctxDone(ctx) {
			return total, nil
		}
	}
}
