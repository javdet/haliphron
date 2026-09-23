-- expire_ack: the lease was handed out and never acknowledged.
--
-- $1 is the ceiling: how many unacknowledged expiries a run may accumulate.
-- The expiry that reaches it fails the run instead of requeueing it.
--
-- Before ack the work is guaranteed not to have started — that is the whole
-- reason this deadline is separate and short — so the run goes straight back
-- to Queued and can be handed to another cluster immediately.
--
-- The epoch rises here, in the same statement that revokes ownership. This is
-- the fence closing: from this moment the controller that was holding the run
-- reports under an epoch that is strictly smaller, so it gets abandon rather
-- than being able to write status into a run somebody else now owns. Raising
-- the epoch at the next lease instead would leave that window open for as long
-- as the run sat in the queue. It rises on the failing expiry too, for the same
-- reason: the controller still holding the materialised lease must be told to
-- abandon it rather than acknowledge a run that has ended.
--
-- cluster_id is kept, not cleared. Reassignment is placement's job and it has
-- an exclusion table to consult; clearing it here would send every ack timeout
-- — including the ordinary one where the controller was simply restarting —
-- through re-placement.
--
-- Keeping the assignment is also why the ceiling exists. A negative ack writes
-- an exclusion and so runs out of clusters to try; an ack timeout excludes
-- nothing, and without ack_expiries a controller that materialises the lease
-- and cannot deliver the ack is handed the same run every ackTimeout, forever,
-- with a per-run token minted for each lease. The class is infra: the path
-- between the controller and the backend failed, not the spec and not the
-- agent, which has not run.
UPDATE runs
SET status         = CASE WHEN ack_expiries + 1 >= $1::integer
                          THEN 'Failed' ELSE 'Queued' END,
    lease_epoch    = lease_epoch + 1,
    attempt        = 1,
    ack_expiries   = ack_expiries + 1,
    ack_deadline   = NULL,
    lease_deadline = NULL,
    queued_at      = CASE WHEN ack_expiries + 1 >= $1::integer
                          THEN queued_at ELSE now() END,
    finished_at    = CASE WHEN ack_expiries + 1 >= $1::integer
                          THEN now() ELSE finished_at END,
    failure_class  = CASE WHEN ack_expiries + 1 >= $1::integer
                          THEN 'infra' ELSE failure_class END,
    status_reason  = CASE WHEN ack_expiries + 1 >= $1::integer
                          THEN 'AckTimeoutExhausted' ELSE 'AckDeadlineExceeded' END,
    status_message = CASE WHEN ack_expiries + 1 >= $1::integer
                          THEN format('%s leases expired without an acknowledgement '
                                      'from the cluster', ack_expiries + 1)
                          ELSE status_message END
WHERE status = 'Leased'
  AND ack_deadline < now()
RETURNING id, cluster_id, lease_epoch, status, ack_expiries;
