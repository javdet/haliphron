-- Roles, secrets, tokens, request idempotency and the audit log: everything
-- the run_agent path needs that is not a run and not a cluster.
--
-- What is deliberately absent, and why, is in docs/contracts/run-store.md.

-- +goose Up

-- A role as edited in the UI. The spec is stored whole: it is a product object
-- whose shape changes with the product, and phase 2 adds the repository
-- resolution chain to it without touching this table.
CREATE TABLE roles (
  id         ulid        PRIMARY KEY,
  tenant_id  uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  name       text        NOT NULL CHECK (name ~ '^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$'
                                         AND length(name) <= 253),
  spec       jsonb       NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
  created_by text        NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz
);
CREATE UNIQUE INDEX roles_name ON roles (tenant_id, name) WHERE deleted_at IS NULL;

CREATE TRIGGER roles_touch BEFORE UPDATE ON roles
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- There is no foreign key from runs.role_name to this table, on purpose. A run
-- records which role it was admitted under, and that record has to survive the
-- role being renamed or deleted — the run's spec is frozen and its role is part
-- of what it froze. A foreign key would make deleting a role either impossible
-- or destructive of history.

-- Secrets, in the two modes the SecretResolver port has.
--
-- managed: the value is encrypted by the application with a per-row DEK, and
-- the DEK is stored wrapped by a KEK that lives in the environment, a file or a
-- KMS — never in this database. The property being bought is narrow and worth
-- stating plainly: a database dump is not a credential leak.
--
-- referenced: the value is in the customer's Vault or External Secrets, and
-- this row holds only the pointer. The backend never sees it.
CREATE TABLE secrets (
  id           ulid        PRIMARY KEY,
  tenant_id    uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  name         text        NOT NULL CHECK (length(name) BETWEEN 1 AND 253),
  kind         secret_kind NOT NULL,

  kek_id       text,
  dek_wrapped  bytea,
  nonce        bytea,
  ciphertext   bytea,

  ref_uri      text,

  created_by   text        NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  rotated_at   timestamptz,

  UNIQUE (tenant_id, name),

  -- The two modes are exclusive, and each is complete or absent. A half-filled
  -- managed secret is a run that fails at auth inside the pod, five minutes and
  -- one image pull after the mistake was made.
  CHECK ((kind = 'managed') = (ciphertext IS NOT NULL
                               AND dek_wrapped IS NOT NULL
                               AND nonce IS NOT NULL
                               AND kek_id IS NOT NULL)),
  CHECK ((kind = 'referenced') = (ref_uri IS NOT NULL))
);

CREATE TRIGGER secrets_touch BEFORE UPDATE ON secrets
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- API, MCP and per-run tokens. Only digests are stored.
--
-- run-mcp is the class that makes child runs accountable: the token handed to a
-- pod carries its run_id and dies with the run, so a run_agent call arriving
-- from inside an agent can be attributed to a parent, checked against the depth
-- limit and charged to the right budget. A global MCP token would make all
-- three impossible at once.
CREATE TABLE api_tokens (
  id           ulid        PRIMARY KEY,
  tenant_id    uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  name         text        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
  kind         token_kind  NOT NULL,
  token_sha256 sha256      NOT NULL UNIQUE,
  scopes       text[]      NOT NULL DEFAULT '{}',
  subject      text,
  run_id       ulid        REFERENCES runs(id) ON DELETE CASCADE,
  expires_at   timestamptz,
  last_used_at timestamptz,
  revoked_at   timestamptz,
  created_by   text        NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),

  CHECK ((kind = 'run-mcp') = (run_id IS NOT NULL)),
  CHECK (kind <> 'run-mcp' OR expires_at IS NOT NULL)
);
CREATE INDEX api_tokens_run ON api_tokens (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX api_tokens_expiry ON api_tokens (expires_at) WHERE revoked_at IS NULL;

-- last_used_at is written on authentication, which makes every read of this
-- table a potential write. Update it at most once a minute
-- (WHERE last_used_at IS NULL OR last_used_at < now() - interval '1 minute'):
-- the column exists so an operator can spot a token nobody uses, and minute
-- resolution answers that question exactly as well as microsecond resolution
-- does. Nothing indexed here changes on that write, so it stays HOT.

-- Idempotency-Key on the mutating REST and MCP calls. The retry that Slack and
-- n8n perform on a timeout must return the first run, not create a second one.
CREATE TABLE idempotency_keys (
  tenant_id       uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  scope           text        NOT NULL CHECK (length(scope) BETWEEN 1 AND 64),
  key             text        NOT NULL CHECK (length(key) BETWEEN 1 AND 255),

  -- Digest of the request body. The same key with a different body is a client
  -- bug, and answering it with the first run's id would hand back a result for
  -- work that was never requested. It is a 422, and this column is how that is
  -- detected.
  request_sha256  sha256      NOT NULL,

  run_id          ulid        REFERENCES runs(id) ON DELETE CASCADE,
  response_status smallint    CHECK (response_status BETWEEN 100 AND 599),
  response_body   jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz NOT NULL,

  PRIMARY KEY (tenant_id, scope, key)
);
CREATE INDEX idempotency_keys_expiry ON idempotency_keys (expires_at);

-- The audit log. Phase 1 fills it from four places, all of them cases where
-- something was silently discarded and the silence is the problem: a second,
-- different terminal phase for the same attempt; a report rejected under a
-- stale epoch; an ack that refused a lease; and a self-declared duration that
-- does not match the Job duration the controller observed.
CREATE TABLE audit_log (
  id           ulid        PRIMARY KEY,
  at           timestamptz NOT NULL DEFAULT now(),
  tenant_id    uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  actor        text        NOT NULL,
  actor_kind   actor_kind  NOT NULL,
  action       text        NOT NULL CHECK (length(action) BETWEEN 1 AND 128),
  subject_kind text        NOT NULL CHECK (length(subject_kind) BETWEEN 1 AND 64),
  subject_id   text,
  run_id       ulid,
  cluster_id   ulid,
  payload      jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(payload) = 'object')
);
CREATE INDEX audit_log_at ON audit_log (at DESC);
CREATE INDEX audit_log_run ON audit_log (run_id, at DESC) WHERE run_id IS NOT NULL;

-- No foreign keys from audit_log. The record of what happened to a run has to
-- outlive the run: retention here is measured in years and retention on runs is
-- not, and a cascade would delete the evidence along with the subject.

-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_log is append-only'
    USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

-- UPDATE only. DELETE is left open because retention is a real operation with
-- a boundary the chart configures, whereas an audit record that can be edited
-- in place is not an audit record.
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

-- +goose Down
DROP TABLE audit_log;
DROP FUNCTION audit_log_append_only();
DROP TABLE idempotency_keys;
DROP TABLE api_tokens;
DROP TABLE secrets;
DROP TABLE roles;
