-- Schedules: a run template and a cron expression, and the backend firing it.
--
-- The scheduler is the backend's, not a custom resource's (ADR 2): the control
-- plane cannot create objects in a cluster, and cron wants queries and history
-- that etcd does not have. A schedule's fire is an ordinary admission with
-- created_via 'schedule', so a scheduled run is admitted by exactly the rules a
-- REST or MCP run is.
--
-- next_run_at is the fence. A fire advances it inside the transaction that
-- inserts the run, and only from the value it read: the UPDATE carries
-- next_run_at = <read value> AND revision = <read value> in its WHERE clause,
-- and a fire that changes no row rolls its run back. Two replicas due on the
-- same schedule therefore admit one run between them without a leader, and an
-- edit that lands while a fire is in flight wins over the fire — revision is
-- raised by every edit for exactly that. NULL means disabled, and the CHECK
-- keeps the two from disagreeing.
--
-- Missed fires collapse. next_run_at is always recomputed from the time of the
-- fire rather than from the slot that was due, so an outage of a day on an
-- hourly schedule is one catch-up run and not twenty-four.
-- starting_deadline_seconds drops even that one when it is late by more than
-- the deadline, which is batch/v1 CronJob's rule.
--
-- run is the template in the public API's own snake_case shape, validated by
-- admission's rules at save time. It is jsonb rather than columns for the same
-- reason roles.spec is: it is the run request, and the run request grows.
--
-- runs.schedule_id records which schedule a run came from, for the Forbid and
-- Replace policies and for the UI. Deleting a schedule keeps its runs and
-- forgets the link; run retention deleting a run clears last_run_id. Neither
-- column is frozen by runs_guard, which is what lets the SET NULL through on a
-- terminal run.

-- +goose Up
CREATE TABLE schedules (
  tenant_id                 uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  id                        ulid        PRIMARY KEY,
  name                      text        NOT NULL
                            CHECK (length(name) <= 253
                                   AND name ~ '^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$'),
  description               text        NOT NULL DEFAULT '' CHECK (length(description) <= 1024),

  cron                      text        NOT NULL CHECK (length(cron) BETWEEN 1 AND 256),
  timezone                  text        NOT NULL DEFAULT 'UTC' CHECK (length(timezone) BETWEEN 1 AND 64),
  enabled                   boolean     NOT NULL DEFAULT true,
  concurrency_policy        text        NOT NULL DEFAULT 'Forbid'
                            CHECK (concurrency_policy IN ('Allow', 'Forbid', 'Replace')),
  starting_deadline_seconds integer     CHECK (starting_deadline_seconds > 0),

  run                       jsonb       NOT NULL CHECK (jsonb_typeof(run) = 'object'),

  next_run_at               timestamptz,
  revision                  bigint      NOT NULL DEFAULT 1,

  last_fired_at             timestamptz,
  last_run_id               ulid,
  last_outcome              text        CHECK (last_outcome IN ('submitted', 'skipped', 'missed', 'refused')),
  last_error                text        CHECK (length(last_error) <= 4096),

  created_by                text        NOT NULL,
  created_at                timestamptz NOT NULL DEFAULT now(),
  updated_by                text        NOT NULL,
  updated_at                timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT schedules_name_key UNIQUE (tenant_id, name),
  CHECK ((next_run_at IS NULL) = (NOT enabled))
);

CREATE INDEX schedules_due ON schedules (next_run_at) WHERE enabled;

ALTER TABLE schedules
  ADD CONSTRAINT schedules_last_run_id_fkey
  FOREIGN KEY (last_run_id) REFERENCES runs(id) ON DELETE SET NULL;

ALTER TABLE runs
  ADD COLUMN schedule_id ulid REFERENCES schedules(id) ON DELETE SET NULL;

CREATE INDEX runs_schedule ON runs (schedule_id, created_at DESC) WHERE schedule_id IS NOT NULL;

-- +goose Down
DROP INDEX runs_schedule;
ALTER TABLE runs DROP COLUMN schedule_id;
DROP TABLE schedules;
