-- expire_lease: the cluster stopped reporting on work it had already started.
--
-- Unknown, never Queued. After ack a Job may be executing this second, and
-- handing the same run to a second cluster would run the agent twice, pay for
-- the model twice and push two branches. Recovery is deliberately manual or
-- by the cluster coming back.
--
-- The epoch does NOT rise here, and that asymmetry is the point. The lease
-- deadline is authoritative for the backend and merely informative for the
-- controller: a controller that lost the network keeps playing the work out
-- and sends its reports when the link returns. Those reports carry the epoch
-- it was given, and they are accepted, and the run recovers by itself. Raising
-- the epoch would turn a ten-second network blip into an hour of agent work
-- thrown away.
--
-- observed_phase is left where it was, so the returning controller's Running
-- report compares equal to the rank already stored and is applied as the
-- idempotent repeat it is. Unknown is a statement about the backend's sight of
-- the run, not about the run's progress.
UPDATE runs
SET status        = 'Unknown',
    status_reason = 'LeaseDeadlineExceeded'
WHERE status IN ('Dispatched', 'Starting', 'Running')
  AND lease_deadline < now()
RETURNING id, cluster_id, lease_epoch, attempt, observed_phase;
