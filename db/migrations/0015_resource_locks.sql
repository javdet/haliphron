-- Resource locks and deferred runs: agents telling each other what shared
-- infrastructure they are working on, and handing the rest of a task to a run
-- that starts once that infrastructure is free.
--
-- A lock is a registration, not a lock manager. Its name and description are
-- free-form, written by an agent, and the backend never compares two locks,
-- never refuses one because of another and never holds a run back because of
-- one: what conflicts with what is the agents' judgement. What the backend
-- does guarantee is housekeeping, and ordering.
--
-- Housekeeping. Every lock is held by a run and dies with it: the trigger
-- below deletes a run's locks in the same transaction in which its status
-- becomes terminal, so there is no window in which a finished run still holds
-- something and no call site that can forget. Every lock also has an expiry,
-- by default as long as its run can live; an expired lock is invisible to
-- every read at once (expires_at > now()) and deleted by the sweeper later.
-- A lock belongs to the run and not to the epoch: when a run is reassigned,
-- the old pod on a partitioned cluster may still be changing things.
--
-- Ordering. "List, see nothing, set" is a check-then-act race, so the rule is
-- write first, then judge: every lock gets a strictly increasing seq, and the
-- statement that sets one returns, in the same transaction, every live lock
-- of another run with a lower seq. Lock writes take one transaction-level
-- advisory lock (backend/store/locks.go), which makes seq order and commit
-- order the same: for any two locks, the later one's answer contains the
-- earlier one. Without it, a lower seq can commit after a higher one has
-- already read, and two agents each miss the other.
--
-- A deferred run is a run request waiting for locks to go, for a time, or
-- both. It is a table of its own rather than a run status because a run in the
-- runs table is work a cluster may lease, and nothing here is that yet; no
-- contract — the Cluster API, the CRD, lease.sql — learns anything new. It
-- becomes a run only inside InsertRun's transaction, by an UPDATE fenced on
-- state = 'waiting' that rolls the run back when it changes no row, which is
-- what makes one waiting row one run across replicas without a leader.
--
-- wait_for_locks holds lock ids, not names: names are free-form and not unique
-- across runs. A waited id with no live row — released, expired, or its run
-- ended — counts as gone.
--
-- template is the run request in the public API's snake_case shape, the way
-- schedules.run is, validated by admission's rules when the row is saved.

-- +goose Up
CREATE TABLE resource_locks (
  tenant_id    uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  id           ulid        PRIMARY KEY,
  seq          bigint      GENERATED ALWAYS AS IDENTITY,
  run_id       ulid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  -- The epoch the lock was last set under. Audit only: the lock is the run's,
  -- and an epoch rise does not release it.
  lease_epoch  bigint      NOT NULL,
  name         text        NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128),
  description  text        NOT NULL DEFAULT '' CHECK (octet_length(description) <= 1024),
  acquired_at  timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  CONSTRAINT resource_locks_run_name UNIQUE (run_id, name),
  CONSTRAINT resource_locks_seq UNIQUE (seq),
  CHECK (expires_at > acquired_at)
);

CREATE INDEX resource_locks_live ON resource_locks (tenant_id, seq);
CREATE INDEX resource_locks_expiry ON resource_locks (expires_at);

-- runs.status becomes terminal => the run's locks go, in the same transaction.
--
-- A requested cancellation releases nothing: cancel_requested_at is set, the
-- status is not, and the pod is alive until the next heartbeat delivers the
-- cancel. A run cancelled before it was leased does go terminal here, and has
-- no pod to protect.
--
-- A run can leave a terminal status again under a higher attempt or epoch
-- (runs_guard allows it). Its locks are gone from the first terminal status
-- on, which is acceptable: the attempt that ended has no pod any more, and the
-- one that replaces it redoes its work and sets its locks again.
-- +goose StatementBegin
CREATE FUNCTION release_locks_on_terminal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM resource_locks WHERE run_id = NEW.id;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runs_release_locks AFTER UPDATE OF status ON runs
  FOR EACH ROW
  WHEN (NEW.status IN ('Succeeded', 'Failed', 'TimedOut', 'Cancelled')
        AND OLD.status IS DISTINCT FROM NEW.status)
  EXECUTE FUNCTION release_locks_on_terminal();

CREATE TABLE deferred_runs (
  tenant_id       uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  id              ulid        PRIMARY KEY,
  -- The run that deferred the work. The run this row becomes is its child,
  -- so it inherits the chat thread and is counted against the parent's
  -- fan-out and depth, as any child is.
  parent_run_id   ulid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  template        jsonb       NOT NULL CHECK (jsonb_typeof(template) = 'object'),
  wait_for_locks  ulid[]      NOT NULL DEFAULT '{}',
  not_before      timestamptz,
  give_up_at      timestamptz NOT NULL,
  state           text        NOT NULL DEFAULT 'waiting'
                  CHECK (state IN ('waiting', 'submitted', 'expired', 'cancelled')),
  run_id          ulid        REFERENCES runs(id) ON DELETE SET NULL,
  reason          text        NOT NULL DEFAULT '' CHECK (octet_length(reason) <= 1024),
  created_by      text        NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  finished_at     timestamptz,
  CHECK (cardinality(wait_for_locks) > 0 OR not_before IS NOT NULL),
  CHECK ((state = 'waiting') = (finished_at IS NULL))
);

CREATE INDEX deferred_runs_waiting ON deferred_runs (created_at) WHERE state = 'waiting';
CREATE INDEX deferred_runs_parent ON deferred_runs (parent_run_id);
CREATE INDEX deferred_runs_recent ON deferred_runs (created_at DESC);

-- +goose Down
DROP TABLE deferred_runs;
DROP TRIGGER runs_release_locks ON runs;
DROP FUNCTION release_locks_on_terminal();
DROP TABLE resource_locks;
