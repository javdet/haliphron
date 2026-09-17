-- Domains: every bounded value in the wire contracts, stated once.
--
-- The enum-shaped domains are the store's copy of enums that already exist in
-- api/cluster/v1/openapi.yaml. A copy is a drift risk, so it is a tested one:
-- TestDomainsMatchOpenAPIEnums in test/store reads the CHECK constraint back
-- out of pg_constraint and compares the sets. A value accepted by one side and
-- refused by the other is the failure this pins down — it surfaces as a lease
-- the backend hands out and its own store cannot record.
--
-- Domains rather than CREATE TYPE ... AS ENUM, for three reasons: ALTER TYPE
-- ADD VALUE cannot run in the same transaction that uses the new value, which
-- makes a data migration a two-release affair; enum values cannot be removed at
-- all; and the ordering an enum type carries is a trap here, because the only
-- ordering that matters in this schema (phase rank) is not alphabetical and is
-- defined explicitly below.
--
-- No extensions. On-prem installations are not guaranteed a role that may
-- CREATE EXTENSION, and nothing here needs one: gen_random_uuid() has been in
-- core since 13, and ULIDs are minted by the backend, not by the database.

-- +goose Up

-- ULID, the identifier shared with runs.id in the backend, the AgentRun object
-- name in the cluster and the runs/{id}/ prefix in storage. Stored as text in
-- its wire form, not as uuid/bytea: it crosses four boundaries in this system
-- and converting at each one is where the identifier in a log line stops
-- matching the identifier in a bucket. The 26 bytes it costs over a uuid buy
-- that, and Crockford base32 sorts lexicographically by mint time, so the
-- index locality of a uuidv4 is not what is being given up.
CREATE DOMAIN ulid AS text
  CHECK (VALUE ~ '^[0-9A-HJKMNP-TV-Z]{26}$');

-- Fencing token. Raised only by the backend, only when ownership of a run is
-- revoked. See 0003 for why it is not raised when a lease is handed out.
CREATE DOMAIN epoch_no AS bigint
  CHECK (VALUE >= 1);

-- Attempt number within one ownership. Raised only by the controller.
CREATE DOMAIN attempt_no AS integer
  CHECK (VALUE >= 1);

-- MoneyUSD is '^-?[0-9]{1,12}(\.[0-9]{1,6})?$' on the wire: twelve digits
-- before the point and six after is exactly numeric(18,6), so the store neither
-- widens nor truncates what the contract allows. Never float, and never
-- rounded to cents: a model bills 0.4231 and a bill nobody can reproduce is
-- worse than one extra decimal place.
--
-- The wire permits a leading minus and the store does not. A negative
-- self-declared cost is not a number this system has any use for; it is a
-- broken or hostile pod, and it should fail loudly at the write rather than
-- quietly offset another run's spend in a SUM.
CREATE DOMAIN money_usd AS numeric(18,6)
  CHECK (VALUE >= 0);

-- Token counts and durations are self-declared by the pod too, and bounded the
-- same way for the same reason.
CREATE DOMAIN token_count AS bigint
  CHECK (VALUE >= 0);

CREATE DOMAIN sha256 AS bytea
  CHECK (octet_length(VALUE) = 32);

-- ---------------------------------------------------------------------------
-- Enums mirrored from api/cluster/v1/openapi.yaml.
-- ---------------------------------------------------------------------------

CREATE DOMAIN agent_type AS text
  CHECK (VALUE IN ('claude-code', 'codex'));

CREATE DOMAIN git_provider AS text
  CHECK (VALUE IN ('github', 'gitlab', 'none'));

-- Phase: what the controller observes in the cluster. Distinct from
-- run_status below, which is the backend's own state machine — the controller
-- never names those, and this schema keeps the two in separate columns so that
-- it cannot start to.
CREATE DOMAIN run_phase AS text
  CHECK (VALUE IN ('Pending', 'Starting', 'Running',
                   'Succeeded', 'Failed', 'TimedOut', 'Cancelled'));

CREATE DOMAIN failure_class AS text
  CHECK (VALUE IN ('none', 'infra', 'agent', 'git', 'config', 'budget'));

CREATE DOMAIN completion_status AS text
  CHECK (VALUE IN ('success', 'failure', 'timeout', 'cancelled'));

CREATE DOMAIN pr_action AS text
  CHECK (VALUE IN ('created', 'updated', 'none'));

-- Why a controller refused to materialise a lease. Drives both the exclusion
-- of that cluster from re-placement and the reason a user reads in the UI when
-- there is no other cluster to try.
CREATE DOMAIN rejection_code AS text
  CHECK (VALUE IN ('InvalidSpec', 'SpecFieldsPruned', 'QuotaExhausted',
                   'ImageNotAllowed', 'MaterializationFailed'));

-- ---------------------------------------------------------------------------
-- Enums the store owns. These have no counterpart on the Cluster API, on
-- purpose: AckResponse.status and StatusIngestResult.appliedStatus are
-- free-form strings there because they are informational for the controller.
-- The set is still a contract — the REST API and the UI are built on it — and
-- this is the one place it is written down.
-- ---------------------------------------------------------------------------

-- The backend's state machine. Queued, Leased, Dispatched and Unknown are
-- backend-owned and unobservable from a cluster; the rest are named identically
-- to the phases they are set from.
CREATE DOMAIN run_status AS text
  CHECK (VALUE IN ('Queued', 'Leased', 'Dispatched',
                   'Starting', 'Running', 'Unknown',
                   'Succeeded', 'Failed', 'TimedOut', 'Cancelled'));

CREATE DOMAIN cluster_status AS text
  CHECK (VALUE IN ('Registering', 'Active', 'Unreachable', 'Revoked'));

-- How a run entered the system. 'agent' is a child run started through a
-- per-run MCP token and is the value depth and parent accounting hang off.
CREATE DOMAIN created_via AS text
  CHECK (VALUE IN ('api', 'mcp', 'ui', 'slack', 'schedule', 'workflow', 'agent'));

CREATE DOMAIN actor_kind AS text
  CHECK (VALUE IN ('user', 'token', 'cluster', 'system', 'agent'));

CREATE DOMAIN token_kind AS text
  CHECK (VALUE IN ('user', 'service', 'run-mcp'));

CREATE DOMAIN secret_kind AS text
  CHECK (VALUE IN ('managed', 'referenced'));

-- ---------------------------------------------------------------------------
-- Shared trigger helpers.
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION touch_updated_at();
DROP DOMAIN secret_kind, token_kind, actor_kind, created_via, cluster_status, run_status;
DROP DOMAIN rejection_code, pr_action, completion_status, failure_class, run_phase,
            git_provider, agent_type;
DROP DOMAIN sha256, token_count, money_usd, attempt_no, epoch_no, ulid;
