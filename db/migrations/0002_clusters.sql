-- Clusters and the bootstrap tokens they register with.
--
-- The property this table exists to preserve is the one ADR 1 is built on: the
-- control plane holds no credential that lets it act as a cluster. There is no
-- private key column here and there will not be one. What is stored is the
-- public half of a keypair the controller generated inside its own cluster,
-- and revocation is a status change on this row, which takes effect within the
-- five-minute life of a token without any revocation list.

-- +goose Up

-- A bootstrap token is single-use by default and is the registration record
-- afterwards: which token admitted which cluster, issued by whom, and when. It
-- is therefore never deleted while a cluster references it, hence RESTRICT.
CREATE TABLE cluster_bootstrap_tokens (
  id            ulid        PRIMARY KEY,
  tenant_id     uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  name          text        NOT NULL CHECK (length(name) BETWEEN 1 AND 128),

  -- Only the digest. A bootstrap token grants the right to become a cluster;
  -- a database dump that contains one is a database dump that can register a
  -- rogue controller.
  token_sha256  sha256      NOT NULL UNIQUE,

  expires_at    timestamptz NOT NULL,
  max_uses      integer     NOT NULL DEFAULT 1 CHECK (max_uses >= 1),
  uses          integer     NOT NULL DEFAULT 0 CHECK (uses >= 0),
  created_by    text        NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz,

  CHECK (uses <= max_uses)
);

CREATE TABLE clusters (
  id                 ulid           PRIMARY KEY,
  tenant_id          uuid           NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',

  -- Participates in role clusterSelector and in every UI listing, so it carries
  -- the RFC 1123 label shape the Cluster API already demands of it.
  name               text           NOT NULL UNIQUE
                       CHECK (name ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' AND length(name) <= 63),
  labels             jsonb          NOT NULL DEFAULT '{}'
                       CHECK (jsonb_typeof(labels) = 'object'),

  status             cluster_status NOT NULL DEFAULT 'Registering',

  -- The JWT header's kid is what a request is authenticated by, so it is the
  -- lookup key and is unique on its own. The key itself is unique too: two
  -- clusters sharing a keypair would make the sub check in section 2 of the
  -- contract the only thing separating them.
  key_id             text           NOT NULL UNIQUE CHECK (length(key_id) BETWEEN 1 AND 64),
  public_key         bytea          NOT NULL UNIQUE CHECK (octet_length(public_key) = 32),
  key_alg            text           NOT NULL DEFAULT 'Ed25519' CHECK (key_alg = 'Ed25519'),

  bootstrap_token_id ulid           NOT NULL
                       REFERENCES cluster_bootstrap_tokens(id) ON DELETE RESTRICT,

  agent_namespace    text           NOT NULL CHECK (length(agent_namespace) BETWEEN 1 AND 63),
  controller_version text           NOT NULL,
  k8s_version        text,
  node_count         integer        CHECK (node_count >= 0),
  runtimes           agent_type[]   NOT NULL DEFAULT '{}',
  crd_versions       text[]         NOT NULL DEFAULT '{}',

  -- Declared by the controller, not computed here. The real ceiling is the
  -- ResourceQuota of the agent namespace, which this side cannot see.
  capacity_slots     integer        NOT NULL DEFAULT 0 CHECK (capacity_slots >= 0),
  free_slots         integer        NOT NULL DEFAULT 0 CHECK (free_slots >= 0),

  -- Set from ClusterFacts. Placement stops choosing this cluster while it is
  -- true, instead of discovering the same fact as a run of Failed/config with
  -- "exceeded quota" in the message.
  quota_exhausted    boolean        NOT NULL DEFAULT false,

  registered_at      timestamptz    NOT NULL DEFAULT now(),
  last_heartbeat_at  timestamptz,
  last_lease_at      timestamptz,
  revoked_at         timestamptz,
  revoked_reason     text,
  updated_at         timestamptz    NOT NULL DEFAULT now(),

  -- Idempotency of POST /register, keyed exactly as section 6 of the contract
  -- specifies. Without it a controller that received its clusterID and died
  -- before writing it to its Secret is unrecoverable: the token is spent and
  -- the identity is lost. With it, the retry after restart returns the same row.
  UNIQUE (bootstrap_token_id, public_key),

  CHECK ((status = 'Revoked') = (revoked_at IS NOT NULL))
);

-- No index beyond the keys above, deliberately. This table holds tens of rows
-- in the installation this system is built for, and free_slots and
-- last_heartbeat_at are rewritten by every heartbeat — once per cluster every
-- ten seconds. An index on a column with that write rate costs more than the
-- sequential scan it would save, and keeping those columns unindexed is also
-- what keeps the update HOT.

CREATE TRIGGER clusters_touch BEFORE UPDATE ON clusters
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- +goose Down
DROP TABLE clusters;
DROP TABLE cluster_bootstrap_tokens;
