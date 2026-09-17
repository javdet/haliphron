-- expire_ack: the lease was handed out and never acknowledged.
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
-- as the run sat in the queue.
--
-- cluster_id is kept, not cleared. Reassignment is placement's job and it has
-- an exclusion table to consult; clearing it here would send every ack timeout
-- — including the ordinary one where the controller was simply restarting —
-- through re-placement.
UPDATE runs
SET status         = 'Queued',
    lease_epoch    = lease_epoch + 1,
    attempt        = 1,
    ack_deadline   = NULL,
    lease_deadline = NULL,
    queued_at      = now(),
    status_reason  = 'AckDeadlineExceeded'
WHERE status = 'Leased'
  AND ack_deadline < now()
RETURNING id, cluster_id, lease_epoch;
