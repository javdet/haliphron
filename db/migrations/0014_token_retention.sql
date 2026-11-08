-- Retention for per-run tokens, kept as data so an operator can change it
-- from the UI.
--
-- Every run that reaches the MCP endpoint mints a run-mcp token, and the token
-- is revoked when the run ends. The row stays, so an installation that keeps
-- its runs collects one dead token per run in the tokens list, where nobody
-- looks at them again. A run-mcp token is therefore deleted once it has ended
-- (revoked or expired) and was created longer ago than this age. User and
-- service tokens are not covered: they are minted by hand and removed by hand.
--
-- One row per tenant, and no row means the default the backend states (two
-- weeks), the way run_retention reads before it is first saved.
-- run_token_seconds = 0 keeps per-run tokens for ever. A token whose run is
-- deleted goes with it whatever this says (api_tokens.run_id cascades).

-- +goose Up
CREATE TABLE token_retention (
  tenant_id         uuid        PRIMARY KEY DEFAULT '00000000-0000-0000-0000-000000000000',
  run_token_seconds bigint      NOT NULL CHECK (run_token_seconds >= 0),
  updated_by        text        NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now()
);

-- What the retention pass selects on. Partial, so the user and service tokens
-- an operator manages by hand are not in it.
CREATE INDEX api_tokens_run_created ON api_tokens (created_at) WHERE kind = 'run-mcp';

-- +goose Down
DROP INDEX api_tokens_run_created;
DROP TABLE token_retention;
