-- What executes agents behind a cluster's controller.
--
-- A controller is no longer always a Kubernetes one: a single Docker host runs
-- the same lease, ack and report protocol with containers in place of Jobs.
-- Nothing on this side treats the two differently — placement, leasing,
-- epochs and reports read none of this — so the kind is recorded for the
-- operator and nothing else. It is a column with a CHECK rather than an enum
-- type because adding a third runtime later is then one constraint swap in a
-- transaction, where an enum value can be added but never taken away.
--
-- The default is what makes the change additive: every row registered before
-- the column existed is a Kubernetes cluster, and so is a registration that
-- does not name a kind.
--
-- agent_namespace keeps its meaning of "where agents run": for a Docker host
-- it holds the agents network name, and its NOT NULL stays.
--
-- docker_version is the counterpart of k8s_version and is stored the same way:
-- nullable, NULL when the controller never said, and overwritten by a
-- heartbeat's facts when the daemon is upgraded.

-- +goose Up
ALTER TABLE clusters
  ADD COLUMN runtime_kind text NOT NULL DEFAULT 'kubernetes'
    CHECK (runtime_kind IN ('kubernetes', 'docker')),
  ADD COLUMN docker_version text;

-- +goose Down
ALTER TABLE clusters
  DROP COLUMN docker_version,
  DROP COLUMN runtime_kind;
