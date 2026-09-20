-- Runs: the work queue, the lease ledger and the result index, in one table.
--
-- Three groups of columns with three different owners, and the whole design
-- follows from keeping them apart:
--
--   admission   written once, at INSERT, never again. spec, prompt digest,
--               the promoted fields the UI filters on. Enforced immutable by
--               runs_guard below, for the same reason the CRD makes spec
--               immutable with CEL: re-rendering a spec between attempts would
--               silently pick up an edited role, and attempt 2 would stop
--               being a replay of attempt 1.
--
--   ownership   cluster_id, lease_epoch, attempt, the two deadlines. Written
--               by the lease, ack, heartbeat and expiry paths.
--
--   observation status, observed_phase, the result fields. Written by ingest.
--
-- What this table does not hold is unbounded content: logs, results and
-- artifacts live in the artifact store under a prefix derived from id, and the
-- exception is result_summary, duplicated here so that listing runs does not
-- mean one storage round trip per row.
--
-- The prompt is held here, and that is a deliberate reversal. It used to be an
-- object in a bucket, which made an object store a prerequisite for *starting*
-- a run rather than for finishing one — the one thing section 9.2 of the
-- architecture set out to remove. It is a column with a stated ceiling instead:
-- bounded content in the system of record, which already holds everything else
-- about a run.

-- +goose Up

CREATE TABLE runs (
  id                  ulid          PRIMARY KEY,
  tenant_id           uuid          NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',

  -- A run started by another run through its per-run MCP token. depth is
  -- checked at admission; it is bounded here as well because an unbounded
  -- chain is the failure mode that costs money rather than correctness.
  parent_run_id       ulid          REFERENCES runs(id) ON DELETE SET NULL,
  depth               smallint      NOT NULL DEFAULT 0 CHECK (depth BETWEEN 0 AND 8),

  created_by          text          NOT NULL,
  created_via         created_via   NOT NULL,
  priority            integer       NOT NULL DEFAULT 0,

  -- ---- admission: written once ------------------------------------------

  -- The RenderedRunSpec exactly as it will be handed out, every time it is
  -- handed out. Stored whole rather than decomposed into columns: it is
  -- already a versioned, additively-evolving contract with its own schema and
  -- its own drift test, and shredding it into forty columns would mean a
  -- migration every time that contract gains an optional field.
  --
  -- jsonb, not json: key order is not part of the contract — the CRD compares
  -- the parsed object, not the bytes — and jsonb is the smaller, queryable one.
  spec                jsonb         NOT NULL CHECK (jsonb_typeof(spec) = 'object'),

  -- The task. Written at admission and frozen with the rest of the group.
  --
  -- The ceiling is real, is stated here rather than discovered, and is checked
  -- again at admission so that the caller gets a 413 naming the limit instead
  -- of a constraint violation. 512 KiB, because the value has to fit in the
  -- per-run Secret beside the git token, the model key and mcp.json, and a
  -- Secret is hard-capped at 1 MiB across all of its keys together. A workflow
  -- step whose accumulated context outgrows that is a real case, and the answer
  -- is to summarise upstream output into the step's input rather than to grow
  -- the envelope.
  --
  -- text and not bytea: it is UTF-8 that a human reads in the UI and greps in
  -- psql. Large enough to TOAST, which is what keeps it off the run list's
  -- pages provided the run list does not select it — the same rule as
  -- result_summary, and a rule about the query rather than about the schema.
  prompt              text          NOT NULL
                        CHECK (octet_length(prompt) <= 524288),

  -- Digest of prompt above, as the backend computed it at admission. The pod
  -- verifies the value it receives against this and refuses to run on a
  -- mismatch, so the column is what "the run executed what was admitted" is
  -- provable from after the fact. It is stored rather than derived because a
  -- derived digest proves only that the row is self-consistent.
  prompt_sha256       sha256        NOT NULL,

  -- Promoted out of spec because the UI filters and sorts on them and because
  -- placement needs agent without parsing jsonb. Redundancy with spec is the
  -- point; runs_guard freezes both together so they cannot disagree.
  agent               agent_type    NOT NULL,
  model               text          NOT NULL,
  role_name           text,
  repo_url            text,
  repo_provider       git_provider  NOT NULL DEFAULT 'none',
  base_branch         text,
  target_branch       text,
  timeout_seconds     integer       NOT NULL CHECK (timeout_seconds BETWEEN 60 AND 86400),
  max_cost_usd        money_usd,

  -- ---- ownership ---------------------------------------------------------

  -- The assigned cluster. NULL means the run still needs placement, which is
  -- also the state a run returns to when its cluster rejects it and there is
  -- another one to try. RESTRICT, not CASCADE: a cluster with runs pointing at
  -- it is revoked, never deleted, and losing the record of where a run ran is
  -- not a thing an operator should be able to do with one DELETE.
  cluster_id          ulid          REFERENCES clusters(id) ON DELETE RESTRICT,

  -- Fencing token. Raised by the events that revoke ownership — ack timeout,
  -- negative ack, operator retry, reassignment — and NOT by handing out a
  -- lease.
  --
  -- This is a deliberate correction to the sequence diagram in section 12.1 of
  -- architecture.md, which raises it at lease time. If the raise happened at
  -- lease time, a run sitting in Queued after a requeue would still carry the
  -- epoch its abandoned controller holds, and a report from that controller
  -- would compare equal and be accepted — the exact zombie the fence exists to
  -- stop, in the window between requeue and re-lease. Raising it at revocation
  -- closes the window, and it also makes the table in section 3 of the Cluster
  -- API contract literally true: the first lease hands out epoch 1.
  lease_epoch         epoch_no      NOT NULL DEFAULT 1,

  -- Raised only by the controller, reset to 1 whenever lease_epoch rises.
  attempt             attempt_no    NOT NULL DEFAULT 1,

  -- Two deadlines because the risk before ack and after ack are opposite. Both
  -- are computed from the server clock: cluster clocks are not synchronised,
  -- and nothing in this schema is ordered by a timestamp a cluster produced.
  ack_deadline        timestamptz,
  lease_deadline      timestamptz,

  -- ---- observation -------------------------------------------------------

  status              run_status    NOT NULL DEFAULT 'Queued',
  status_reason       text          CHECK (length(status_reason) <= 256),
  status_message      text          CHECK (length(status_message) <= 1024),
  failure_class       failure_class NOT NULL DEFAULT 'none',
  exit_code           integer,

  -- The last phase the controller reported, kept separate from status.
  --
  -- Separating them is what makes Unknown work. Unknown is not a phase and has
  -- no rank: it is the backend saying it has lost sight of a run, which can
  -- happen while the run is Running and must be undoable when the cluster comes
  -- back. Storing it in status and leaving observed_phase at Running means the
  -- returning controller's Running report compares equal — an idempotent repeat
  -- that clears Unknown — instead of being rejected as a phase regression,
  -- which is what would happen if Unknown occupied a rank of its own.
  observed_phase      run_phase,

  -- The monotonic half of the ordering key from section 5 of the contract.
  -- Generated, so that it cannot drift from observed_phase the way a column
  -- the application maintains eventually does. The ELSE 0 mirrors Phase.Rank()
  -- in api/run/v1: a phase this build has never heard of loses every comparison
  -- rather than moving a run backwards.
  observed_rank       smallint      NOT NULL GENERATED ALWAYS AS (
                        CASE observed_phase
                          WHEN 'Pending'   THEN 10
                          WHEN 'Starting'  THEN 20
                          WHEN 'Running'   THEN 30
                          WHEN 'Succeeded' THEN 40
                          WHEN 'Failed'    THEN 40
                          WHEN 'TimedOut'  THEN 40
                          WHEN 'Cancelled' THEN 40
                          ELSE 0
                        END) STORED,

  -- Cancellation is state, not a message. Both commands the backend sends down
  -- are functions of current state — cancel while this is set and the run is
  -- not terminal, abandon when a report arrives under a stale epoch — which is
  -- precisely why the contract gives commands no acknowledgement and why there
  -- is no commands table here to keep in sync.
  cancel_requested_at   timestamptz,
  cancel_requested_by   text,
  cancel_reason         text CHECK (length(cancel_reason) <= 1024),
  cancel_grace_seconds  integer CHECK (cancel_grace_seconds BETWEEN 0 AND 3600),

  -- ---- result, promoted from the attempt that finished --------------------

  -- Up to 64 KiB, the cap the Cluster API puts on summary. TOAST moves it out
  -- of line past ~2 KiB, so a wide column here costs the run list nothing —
  -- provided the run list does not select it. The rule is about the query.
  result_summary      text          CHECK (octet_length(result_summary) <= 65536),

  -- Where the full result actually is, as a URI carrying its scheme:
  -- file://runs/01J8.../result.md or s3://haliphron/runs/01J8.../result.md.
  --
  -- The scheme is the point. An installation that migrates between artifact
  -- modes keeps its old runs readable instead of orphaning them, because a
  -- stored row says which store wrote it rather than leaving the reader to
  -- assume the mode that is configured today.
  result_ref          text          CHECK (length(result_ref) <= 2048),
  pr_url              text          CHECK (length(pr_url) <= 512),
  pr_number           integer,
  pr_action           pr_action,
  commit_sha          text          CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
  pushed              boolean,

  -- Self-declared by the pod and aggregated from run_attempts. Not authority:
  -- a compromised agent understates it. The backend audits the gap between
  -- declared duration and the Job duration the controller observed.
  cost_usd            money_usd     NOT NULL DEFAULT 0,
  input_tokens        token_count   NOT NULL DEFAULT 0,
  output_tokens       token_count   NOT NULL DEFAULT 0,
  cache_read_tokens   token_count   NOT NULL DEFAULT 0,
  cache_write_tokens  token_count   NOT NULL DEFAULT 0,
  num_turns           integer       NOT NULL DEFAULT 0 CHECK (num_turns >= 0),

  completion_received_at timestamptz,

  created_at          timestamptz   NOT NULL DEFAULT now(),
  queued_at           timestamptz   NOT NULL DEFAULT now(),
  leased_at           timestamptz,
  dispatched_at       timestamptz,
  started_at          timestamptz,
  finished_at         timestamptz,
  updated_at          timestamptz   NOT NULL DEFAULT now(),

  -- A leased run has a cluster and an ack deadline; a queued one has neither.
  CHECK (status <> 'Leased' OR (cluster_id IS NOT NULL AND ack_deadline IS NOT NULL)),
  CHECK (status <> 'Queued' OR (ack_deadline IS NULL AND lease_deadline IS NULL))
);

-- One attempt, keyed by the ownership it happened under.
--
-- The key is (run_id, lease_epoch, attempt), not (run_id, attempt) as sketched
-- in section 9.1 of architecture.md. attempt resets to 1 every time the epoch
-- rises, so the second owner's first attempt collides with the first owner's
-- first attempt under the shorter key — and the row it would collide with is
-- the accounting record of an attempt that already spent money.
CREATE TABLE run_attempts (
  run_id               ulid          NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  lease_epoch          epoch_no      NOT NULL,
  attempt              attempt_no    NOT NULL,
  cluster_id           ulid          NOT NULL REFERENCES clusters(id) ON DELETE RESTRICT,

  phase                run_phase,
  phase_rank           smallint      NOT NULL GENERATED ALWAYS AS (
                         CASE phase
                           WHEN 'Pending'   THEN 10
                           WHEN 'Starting'  THEN 20
                           WHEN 'Running'   THEN 30
                           WHEN 'Succeeded' THEN 40
                           WHEN 'Failed'    THEN 40
                           WHEN 'TimedOut'  THEN 40
                           WHEN 'Cancelled' THEN 40
                           ELSE 0
                         END) STORED,
  reason               text          CHECK (length(reason) <= 256),
  message              text          CHECK (length(message) <= 1024),

  job_name             text,
  pod_name             text,
  node_name            text,

  exit_code            integer,
  failure_class        failure_class NOT NULL DEFAULT 'none',

  -- The idempotent-retry checkpoint, and what used to be runs/{id}/state.json.
  --
  -- It moved here because it is a fact about the run's progress and the run's
  -- progress already lives in this schema — observed_phase, observed_rank, the
  -- phase column above. Keeping a second copy in an object store meant two
  -- writers for one piece of state, and it put that store on the critical path
  -- of *starting* an attempt rather than finishing one.
  --
  -- The controller accumulates it from the pod's phase reports and hands it to
  -- the next attempt's Job as HALIPHRON_COMPLETED_PHASES; the entrypoint skips
  -- what is already done, and if 'run' is in the list the retry does not call
  -- the model. Two properties the object never had fall out: it survives the
  -- pod's prefix being unreadable, because it never lived there, and it is
  -- visible — "how far did this get before it died" is a SELECT.
  --
  -- Unioned on write, never replaced. Reports arrive reordered, and a heartbeat
  -- carrying an earlier snapshot must not shorten a list the ingest path
  -- already grew.
  completed_phases     runtime_phase[] NOT NULL DEFAULT '{}',

  started_at           timestamptz,
  finished_at          timestamptz,

  -- The cluster's own clock. Diagnostic only — named so that using it as an
  -- ordering key looks wrong in the query that does it. Order comes from
  -- (lease_epoch, attempt, phase_rank) and from nothing else.
  cluster_observed_at  timestamptz,

  -- The CompletionReport verbatim, as the pod produced it and the controller
  -- forwarded it unedited. Stored whole for the same reason spec is: it is a
  -- contract-owned, additively-evolving shape with its own schema, and the
  -- copy in storage, the copy in the controller's memory and this one are
  -- meant to stay byte-comparable.
  completion           jsonb         CHECK (completion IS NULL OR jsonb_typeof(completion) = 'object'),
  completion_received_at timestamptz,

  -- The accounting unit. runs.cost_usd is the sum over these rows; this is
  -- where "charged once" is enforced, by the row existing at most once per
  -- (run, epoch, attempt) and a duplicate /ingest/completion being a no-op.
  cost_usd             money_usd     NOT NULL DEFAULT 0,
  input_tokens         token_count   NOT NULL DEFAULT 0,
  output_tokens        token_count   NOT NULL DEFAULT 0,
  cache_read_tokens    token_count   NOT NULL DEFAULT 0,
  cache_write_tokens   token_count   NOT NULL DEFAULT 0,

  -- Declared by the pod against measured by the controller. Kept side by side
  -- because the check the contract asks for is a comparison, and a comparison
  -- whose two halves live in different systems does not get made.
  declared_duration_ms bigint        CHECK (declared_duration_ms >= 0),
  observed_duration_ms bigint        CHECK (observed_duration_ms >= 0),

  created_at           timestamptz   NOT NULL DEFAULT now(),
  updated_at           timestamptz   NOT NULL DEFAULT now(),

  PRIMARY KEY (run_id, lease_epoch, attempt)
);

CREATE TRIGGER run_attempts_touch BEFORE UPDATE ON run_attempts
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- A cluster that answered ack with accepted:false is excluded from placement
-- for this run. The rejection is kept, not just the fact of it: when this is
-- the only cluster, the run fails with class config, and the message a user
-- reads is this one.
CREATE TABLE run_cluster_exclusions (
  run_id      ulid           NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  cluster_id  ulid           NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
  lease_epoch epoch_no       NOT NULL,
  code        rejection_code NOT NULL,
  message     text           CHECK (length(message) <= 1024),
  fields      text[]         NOT NULL DEFAULT '{}',
  at          timestamptz    NOT NULL DEFAULT now(),

  PRIMARY KEY (run_id, cluster_id)
);
CREATE INDEX run_cluster_exclusions_cluster ON run_cluster_exclusions (cluster_id);

-- ---------------------------------------------------------------------------
-- Indexes. Every one of them is partial, because the selective thing about
-- every access path in this table is a status that all but a handful of rows
-- have left behind. A year of runs is millions of terminal rows; the queue and
-- the two expiry scanners want to look at dozens.
-- ---------------------------------------------------------------------------

-- The lease. Ordered exactly as the lease statement orders, so the FOR UPDATE
-- SKIP LOCKED scan reads the index and stops at LIMIT.
CREATE INDEX runs_lease_queue ON runs (cluster_id, priority DESC, created_at)
  WHERE status = 'Queued';

-- Placement: queued and not yet assigned.
CREATE INDEX runs_needs_placement ON runs (created_at)
  WHERE status = 'Queued' AND cluster_id IS NULL;

-- Two deadlines, two scanners, two indexes. Their consequences are opposite —
-- ack expiry requeues, lease expiry does not — so they are never one query.
CREATE INDEX runs_ack_expiry ON runs (ack_deadline)
  WHERE status = 'Leased';
CREATE INDEX runs_lease_expiry ON runs (lease_deadline)
  WHERE status IN ('Dispatched', 'Starting', 'Running');

-- The heartbeat reconciliation in section 8: what the backend believes this
-- cluster is running, to compare against what the cluster reported.
CREATE INDEX runs_active_by_cluster ON runs (cluster_id)
  WHERE status IN ('Leased', 'Dispatched', 'Starting', 'Running', 'Unknown');

-- Cancellation is delivered by repetition until the phase goes terminal.
CREATE INDEX runs_cancel_pending ON runs (cluster_id)
  WHERE cancel_requested_at IS NOT NULL
    AND status NOT IN ('Succeeded', 'Failed', 'TimedOut', 'Cancelled');

-- The UI list, and the child-run rollup.
CREATE INDEX runs_list ON runs (status, created_at DESC);
CREATE INDEX runs_recent ON runs (created_at DESC);
CREATE INDEX runs_parent ON runs (parent_run_id) WHERE parent_run_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- runs_guard: assertions, not logic.
--
-- It never repairs anything and never fills anything in except updated_at. It
-- refuses writes that no correct path produces, so that a bug in the ordering
-- code fails at the statement that has it rather than a week later as a run
-- whose cost is wrong and whose PR link points at someone else's branch.
--
-- Each rule is one line of the epoch/attempt table in section 3 of the Cluster
-- API contract, restated as something the database can check.
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION runs_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- Admission is written once. Re-rendering a spec between attempts would let
  -- an edited role change what attempt 2 executes.
  IF NEW.spec IS DISTINCT FROM OLD.spec THEN
    RAISE EXCEPTION 'run %: spec is immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.prompt IS DISTINCT FROM OLD.prompt
     OR NEW.prompt_sha256 IS DISTINCT FROM OLD.prompt_sha256
     OR NEW.agent IS DISTINCT FROM OLD.agent
     OR NEW.model IS DISTINCT FROM OLD.model
     OR NEW.timeout_seconds IS DISTINCT FROM OLD.timeout_seconds THEN
    RAISE EXCEPTION 'run %: admission fields are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  -- Ownership only ever moves forward. epoch is the backend's, attempt is the
  -- controller's, and attempt resets only when epoch rises.
  IF NEW.lease_epoch < OLD.lease_epoch THEN
    RAISE EXCEPTION 'run %: epoch regression % -> %', OLD.id, OLD.lease_epoch, NEW.lease_epoch
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.lease_epoch = OLD.lease_epoch AND NEW.attempt < OLD.attempt THEN
    RAISE EXCEPTION 'run %: attempt regression % -> % within epoch %',
      OLD.id, OLD.attempt, NEW.attempt, OLD.lease_epoch
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  -- The first terminal phase wins. A run leaves a terminal status only when a
  -- new owner takes it (epoch rises, operator retry) or the controller starts
  -- a further attempt of its own (attempt rises) — which is exactly the pair of
  -- conditions under which section 5 says a report may reset the phase rank.
  IF OLD.status IN ('Succeeded', 'Failed', 'TimedOut', 'Cancelled')
     AND NEW.status IS DISTINCT FROM OLD.status
     AND NEW.lease_epoch = OLD.lease_epoch
     AND NEW.attempt <= OLD.attempt THEN
    RAISE EXCEPTION 'run %: terminal status % cannot become % without a new epoch or attempt',
      OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  NEW.updated_at := now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runs_guard BEFORE UPDATE ON runs
  FOR EACH ROW EXECUTE FUNCTION runs_guard();

-- ---------------------------------------------------------------------------
-- The duplicate guard: at most one live attempt per run.
--
-- Preventing two live attempts is a constraint, not a convention. Kubernetes
-- already contributes restartPolicy: Never and backoffLimit: 0, so every
-- attempt is a Job some controller created deliberately — but backoffLimit
-- constrains one Job, not two controllers, and a rule that lives only in the
-- controller's code is a rule the second controller does not know about. A
-- constraint in the system of record is the only place it cannot be forgotten.
--
-- The controller is the monitor in the sense that it is the side that finds
-- out: the row is opened by the lease and by the first observation of a new
-- attempt, and a conflict comes back as a refusal it can act on. A conflict
-- means somebody else's attempt is still open, which is either a zombie
-- controller — answered by the epoch — or its own duplicate reconcile, which
-- is answered by doing nothing.
--
-- This bounds duplicate *attempts*. It does not and cannot bound duplicate
-- *execution* across a fencing boundary; that is what the epoch is for, and
-- the honest formulation still stands: at-least-once execution with converging
-- side effects.
CREATE UNIQUE INDEX run_attempts_one_live ON run_attempts (run_id)
  WHERE finished_at IS NULL;

-- ---------------------------------------------------------------------------
-- Raising the epoch closes the open attempt.
--
-- Without this the index above is a trap. A controller that vanished mid-run
-- leaves finished_at IS NULL behind, and the cluster the work is reassigned to
-- can never take the row — the run becomes permanently unrunnable by the very
-- mechanism meant to keep it from running twice.
--
-- So every event that raises lease_epoch — ack expiry, negative ack,
-- reassignment, operator retry — finishes the outstanding attempt. As a trigger
-- rather than as a line in each of those statements, for the same reason the
-- rule above is a constraint: there are four call sites today and the fifth is
-- the one that forgets.
--
-- The accounting record survives, which is the point of keeping the epoch in
-- the attempt key: what is released is the claim, not the history of what was
-- spent. failure_class is 'infra' because the attempt was ended by the
-- platform's fence and not by anything the agent or the user did, and the
-- reason names the fence so that "why does this attempt say Failed when the
-- run succeeded on the next cluster" has an answer in the row itself.
-- +goose StatementBegin
CREATE FUNCTION close_attempt_on_new_epoch() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  UPDATE run_attempts
  SET finished_at   = now(),
      failure_class = CASE WHEN failure_class = 'none' THEN 'infra'
                           ELSE failure_class END,
      reason        = COALESCE(reason, 'OwnershipRevoked'),
      message       = COALESCE(message,
                        format('lease_epoch rose from %s to %s; the claim was released',
                               OLD.lease_epoch, NEW.lease_epoch))
  WHERE run_id = NEW.id
    AND finished_at IS NULL;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runs_close_attempt AFTER UPDATE OF lease_epoch ON runs
  FOR EACH ROW WHEN (NEW.lease_epoch > OLD.lease_epoch)
  EXECUTE FUNCTION close_attempt_on_new_epoch();

-- +goose Down
DROP TABLE run_cluster_exclusions;
DROP TABLE run_attempts;
DROP TABLE runs;
DROP FUNCTION runs_guard();
DROP FUNCTION close_attempt_on_new_epoch();
