-- lease: hand work to a cluster that is asking for it.
--
-- $1 cluster_id, $2 agent types the cluster will accept, $3 how many,
-- $4 ack timeout seconds, $5 lease TTL seconds.
--
-- FOR UPDATE SKIP LOCKED is what makes two concurrent polls from the same
-- cluster — which happen, because a controller reconnects before the previous
-- long poll has finished unwinding — hand out disjoint sets instead of the
-- same run twice. The ORDER BY matches runs_lease_queue exactly, so the scan
-- reads the index and stops at LIMIT rather than sorting the queue.
--
-- lease_epoch is read, not raised. It rises when ownership is revoked, which
-- has already happened by the time a run is back in Queued; raising it here
-- too would leave a requeued run carrying the epoch its abandoned controller
-- still holds, and that controller's next report would compare equal.
--
-- A run with cancel_requested_at set is not handed out: cancelling work that
-- has not started is a state change here, not a Job created in a cluster so
-- that a command can go down and kill it.
--
-- prompt is returned because the lease is where it travels. It used to be an
-- object the pod fetched with a presigned GET, which put the artifact store on
-- the path to *starting* a run; it is bounded at admission, so returning it
-- here adds at most 512 KiB to a message that already carries a git token, a
-- model key and mcp.json, and it removes the store from that path entirely.
WITH picked AS (
  SELECT id
  FROM runs
  WHERE status = 'Queued'
    AND cluster_id = $1
    AND agent = ANY ($2::agent_type[])
    AND cancel_requested_at IS NULL
  ORDER BY priority DESC, created_at
  FOR UPDATE SKIP LOCKED
  LIMIT $3
)
UPDATE runs r
SET status         = 'Leased',
    attempt        = 1,
    leased_at      = now(),
    ack_deadline   = now() + make_interval(secs => $4),
    lease_deadline = now() + make_interval(secs => $5),
    status_reason  = NULL,
    status_message = NULL
FROM picked
WHERE r.id = picked.id
RETURNING r.id, r.lease_epoch, r.attempt, r.priority, r.spec,
          r.prompt, r.prompt_sha256,
          r.ack_deadline, r.lease_deadline, r.timeout_seconds;
