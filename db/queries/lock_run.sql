-- lock_run: take the row a report is about, for the duration of applying it.
--
-- $1 run id.
--
-- Every path that applies a report — /ingest/status, /ingest/completion and
-- the runs[] of a heartbeat — starts here, inside one transaction, and then
-- evaluates the decision table in section 5 of the Cluster API contract in Go.
--
-- The decision stays in Go on purpose. It is domain logic: it decides what a
-- run means, and it is the thing the heartbeat path and the ingest path are
-- required to share. Expressing it as SQL would put the domain in the schema
-- and make every clarification of the rule a migration. What the database
-- contributes is the row lock that makes read-decide-write atomic, and the
-- runs_guard trigger that refuses the writes the rule should never produce.
--
-- The lock is per run, and a run is owned by one cluster at a time, so there
-- is no contention to speak of — only ordering between the low-latency ingest
-- path and the periodic heartbeat carrying the same observation.
SELECT id, cluster_id, lease_epoch, attempt, status, observed_phase, observed_rank,
       failure_class, cancel_requested_at, completion_received_at,
       ack_deadline, lease_deadline
FROM runs
WHERE id = $1
FOR UPDATE;
