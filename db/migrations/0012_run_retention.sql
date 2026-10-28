-- Retention for failed runs, kept as data so an operator can change it from
-- the UI.
--
-- HALIPHRON_RUN_RETENTION deletes every finished run after one age and is off
-- unless the deployment sets it. A failed run is worth less than the others:
-- once it has been looked at, what is left is a row in the list nobody will
-- open again. So failed runs have an age of their own, on by default, and the
-- shorter of the two applies to them.
--
-- One row per tenant, and no row means the default the backend states
-- (three days), the way chat_integrations reads before it is first saved.
-- failed_seconds = 0 keeps failed runs for ever. Only status 'Failed' is
-- covered: TimedOut and Cancelled are not failures of the agent, and keep
-- following the general retention alone.

-- +goose Up
CREATE TABLE run_retention (
  tenant_id      uuid        PRIMARY KEY DEFAULT '00000000-0000-0000-0000-000000000000',
  failed_seconds bigint      NOT NULL CHECK (failed_seconds >= 0),
  updated_by     text        NOT NULL,
  updated_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE run_retention;
