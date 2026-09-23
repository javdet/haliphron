-- The brake on ack-timeout churn.
--
-- A negative ack terminates by construction: it writes a run_cluster_exclusions
-- row, and once every cluster has refused, placement finds nobody and the run
-- fails. An ack timeout had no equivalent. It requeues at epoch + 1 on the same
-- cluster and excludes nothing, so a controller that materialises the lease and
-- cannot deliver the ack — a flaky proxy, a load balancer cutting the long poll
-- — was handed the same run every ackTimeout, indefinitely, with a fresh
-- per-run token minted each time.
--
-- A counter of its own rather than a ceiling on lease_epoch. The epoch also
-- rises on a negative ack and on an operator's retry; a ceiling on it would
-- count those too, and a run an operator retried a few times would fail on its
-- next restart-induced timeout for reasons that have nothing to do with acks.
--
-- expire_ack.sql raises it and fails the run when it reaches the ceiling. An
-- operator's retry resets it: that is a person deciding to try again, not the
-- scanner repeating itself. Nothing else needs to: once a lease is
-- acknowledged the run never returns to Leased except through that retry.

-- +goose Up
ALTER TABLE runs
  ADD COLUMN ack_expiries integer NOT NULL DEFAULT 0 CHECK (ack_expiries >= 0);

-- +goose Down
ALTER TABLE runs DROP COLUMN ack_expiries;
