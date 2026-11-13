-- Alert receivers: Prometheus Alertmanager notifications that start agent runs.
--
-- A receiver is the backend's object, like a schedule: a run template checked
-- by admission's rules when it is saved, a throttle window, and the digest of
-- a credential Alertmanager presents as a bearer token. A notification that
-- arrives is one run, carrying the notification's firing alerts that are not
-- suppressed; created_via 'alertmanager', so it is admitted by exactly the
-- rules a REST run is.
--
-- alert_fingerprints is the throttle, and suppressed_until is its fence. A
-- run claims its alerts inside the transaction that inserts it, with
--
--   INSERT ... ON CONFLICT (receiver_id, fingerprint) DO UPDATE ...
--     WHERE suppressed_until <= now()
--
-- and an alert whose row the statement neither inserts nor updates was claimed
-- by someone else since it was read: another replica, or one of Alertmanager's
-- HA peers delivering the same group. The run is then rolled back. In READ
-- COMMITTED a conflicting statement waits for the other transaction and
-- re-evaluates the WHERE against what it committed, so N replicas admit one
-- run per alert per window without a leader. Rows are claimed in fingerprint
-- order, so two deliveries with overlapping sets cannot deadlock.
--
-- suppressed_until is written by that statement and nowhere else. An
-- operator clearing a suppression deletes the row, which can cause an extra
-- run and never a lost one, and retention deletes rows only long after they
-- stopped suppressing anything.
--
-- alert_deliveries is what arrived and what was done about it, for "why did
-- this alert not start anything". It is append-only and bounded by age.
--
-- runs.alert_receiver_id records which receiver a run came from. Deleting a
-- receiver keeps its runs and forgets the link; run retention deleting a run
-- clears every reference to it. None of these columns is frozen by
-- runs_guard, which is what lets the SET NULL through on a terminal run. The
-- run_id columns are indexed because run retention deletes runs in batches,
-- and an unindexed foreign key would be a scan of the table per run.
--
-- created_via is widened by the drop-and-re-add 0007 uses, with the whole
-- list written out.

-- +goose Up
ALTER DOMAIN created_via DROP CONSTRAINT created_via_check;

ALTER DOMAIN created_via ADD CONSTRAINT created_via_check
  CHECK (VALUE IN ('api', 'mcp', 'ui', 'slack', 'mattermost',
                   'schedule', 'workflow', 'agent', 'alertmanager')) NOT VALID;

CREATE TABLE alert_receivers (
  tenant_id         uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  id                ulid        PRIMARY KEY,
  name              text        NOT NULL
                    CHECK (length(name) <= 253
                           AND name ~ '^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$'),
  description       text        NOT NULL DEFAULT '' CHECK (length(description) <= 1024),
  enabled           boolean     NOT NULL DEFAULT true,

  run               jsonb       NOT NULL CHECK (jsonb_typeof(run) = 'object'),

  throttle_seconds  integer     NOT NULL DEFAULT 3600
                    CHECK (throttle_seconds BETWEEN 0 AND 604800),

  token_sha256      bytea       NOT NULL CHECK (length(token_sha256) = 32),

  last_received_at  timestamptz,
  last_outcome      text        CHECK (last_outcome IN ('submitted', 'throttled', 'resolved', 'refused')),
  last_run_id       ulid,
  last_error        text        CHECK (length(last_error) <= 4096),

  created_by        text        NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_by        text        NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT alert_receivers_name_key UNIQUE (tenant_id, name),
  CONSTRAINT alert_receivers_token_key UNIQUE (token_sha256)
);

ALTER TABLE alert_receivers
  ADD CONSTRAINT alert_receivers_last_run_id_fkey
  FOREIGN KEY (last_run_id) REFERENCES runs(id) ON DELETE SET NULL;

CREATE TABLE alert_fingerprints (
  receiver_id       ulid        NOT NULL REFERENCES alert_receivers(id) ON DELETE CASCADE,
  fingerprint       text        NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{1,64}$'),
  alertname         text        NOT NULL DEFAULT '' CHECK (length(alertname) <= 256),
  suppressed_until  timestamptz NOT NULL,
  run_id            ulid        REFERENCES runs(id) ON DELETE SET NULL,
  first_run_at      timestamptz NOT NULL,
  last_run_at       timestamptz NOT NULL,
  runs              integer     NOT NULL DEFAULT 1 CHECK (runs >= 1),
  suppressed        bigint      NOT NULL DEFAULT 0 CHECK (suppressed >= 0),
  PRIMARY KEY (receiver_id, fingerprint)
);

CREATE INDEX alert_fingerprints_expiry ON alert_fingerprints (suppressed_until);
CREATE INDEX alert_fingerprints_run ON alert_fingerprints (run_id) WHERE run_id IS NOT NULL;

CREATE TABLE alert_deliveries (
  id                ulid        PRIMARY KEY,
  receiver_id       ulid        NOT NULL REFERENCES alert_receivers(id) ON DELETE CASCADE,
  received_at       timestamptz NOT NULL DEFAULT now(),
  group_key         text        NOT NULL DEFAULT '' CHECK (length(group_key) <= 1024),
  status            text        NOT NULL CHECK (status IN ('firing', 'resolved')),
  firing            integer     NOT NULL CHECK (firing >= 0),
  resolved          integer     NOT NULL CHECK (resolved >= 0),
  -- The fingerprints this delivery left out of its run because an earlier
  -- run still suppressed them; with outcome 'throttled', all of them.
  throttled         text[]      NOT NULL DEFAULT '{}',
  outcome           text        NOT NULL
                    CHECK (outcome IN ('submitted', 'throttled', 'resolved', 'refused')),
  run_id            ulid        REFERENCES runs(id) ON DELETE SET NULL,
  error             text        CHECK (length(error) <= 4096),
  -- Only a submitted delivery has a run, and keeps it until run retention
  -- deletes the run.
  CHECK (outcome = 'submitted' OR run_id IS NULL)
);

CREATE INDEX alert_deliveries_receiver ON alert_deliveries (receiver_id, received_at DESC, id DESC);
CREATE INDEX alert_deliveries_age ON alert_deliveries (received_at);
CREATE INDEX alert_deliveries_run ON alert_deliveries (run_id) WHERE run_id IS NOT NULL;

ALTER TABLE runs
  ADD COLUMN alert_receiver_id ulid REFERENCES alert_receivers(id) ON DELETE SET NULL;

CREATE INDEX runs_alert_receiver ON runs (alert_receiver_id, created_at DESC)
  WHERE alert_receiver_id IS NOT NULL;

-- +goose Down
DROP INDEX runs_alert_receiver;
ALTER TABLE runs DROP COLUMN alert_receiver_id;
DROP TABLE alert_deliveries;
DROP TABLE alert_fingerprints;
DROP TABLE alert_receivers;

-- Narrowing. A run admitted from Alertmanager would not satisfy the restored
-- constraint, so the rows that carry it are rewritten first, as 0007's Down
-- does for 'mattermost'.
UPDATE runs SET created_via = 'api' WHERE created_via = 'alertmanager';

ALTER DOMAIN created_via DROP CONSTRAINT created_via_check;

ALTER DOMAIN created_via ADD CONSTRAINT created_via_check
  CHECK (VALUE IN ('api', 'mcp', 'ui', 'slack', 'mattermost',
                   'schedule', 'workflow', 'agent')) NOT VALID;
