-- Chat triggers: a message in a chat platform that asked for a run, and the
-- reply the run owes it.
--
-- A row is claimed by the message's own identifier before anything else is
-- done about the message. That claim is the deduplication, and it is not a
-- corner case: every API replica holds its own connection to the chat server
-- as the bot, so every message arrives once per replica. The primary key is
-- what makes N deliveries one run.
--
-- The run is linked to its row inside the transaction that inserts the run.
-- Linked separately, a crash between the two statements is a run executing
-- that nobody will ever answer about — the one failure this table exists to
-- make impossible.
--
-- A row whose handler died between the claim and the link is an orphan:
-- neither run_id nor refusal is set once next_attempt_at, initially the claim
-- time plus a grace, has passed. The reply loop converts it into a refusal so
-- the person who asked is told to ask again, rather than left waiting for an
-- answer nothing is going to give. The conversion and the link both require
-- the other to be absent, so a slow admission and the loop cannot both win.
--
-- The table is keyed by platform so the Slack adapter the architecture plans
-- can share it. Today the only platform is Mattermost.
--
-- created_via is widened by the same drop-and-re-add 0006 uses for
-- runtime_phase, with the whole list written out. No column of an array type
-- uses created_via, so a plain re-validation would also work; NOT VALID is
-- kept anyway because the rows it skips satisfied a stricter list.

-- +goose Up
ALTER DOMAIN created_via DROP CONSTRAINT created_via_check;

ALTER DOMAIN created_via ADD CONSTRAINT created_via_check
  CHECK (VALUE IN ('api', 'mcp', 'ui', 'slack', 'mattermost',
                   'schedule', 'workflow', 'agent')) NOT VALID;

CREATE TABLE chat_triggers (
  tenant_id       uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  platform        text        NOT NULL CHECK (platform IN ('mattermost')),
  message_id      text        NOT NULL CHECK (length(message_id) BETWEEN 1 AND 64),

  -- Where the reply goes. thread_id is the root of the thread the reply is
  -- posted under: the message's own root when it was inside a thread, the
  -- message itself otherwise.
  channel_id      text        NOT NULL CHECK (length(channel_id) BETWEEN 1 AND 64),
  thread_id       text        NOT NULL CHECK (length(thread_id) BETWEEN 1 AND 64),
  user_id         text        NOT NULL CHECK (length(user_id) BETWEEN 1 AND 64),
  username        text        NOT NULL CHECK (length(username) <= 64),

  -- Exactly one of these is set once admission has answered, and neither while
  -- it is still thinking. A run deleted by an operator or by retention takes
  -- its row with it: there is nothing left to reply about.
  run_id          ulid        REFERENCES runs(id) ON DELETE CASCADE,
  refusal         text        CHECK (length(refusal) BETWEEN 1 AND 4096),

  claimed_at      timestamptz NOT NULL DEFAULT now(),

  -- The reply loop's lease. A row is due when this has passed; claiming it
  -- pushes it forward, so two replicas never post the same reply at once and
  -- no row lock is held across the call to the chat server.
  next_attempt_at timestamptz NOT NULL,
  attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  last_error      text,

  replied_at      timestamptz,
  reply_post_id   text,
  abandoned_at    timestamptz,

  PRIMARY KEY (tenant_id, platform, message_id),
  CHECK (run_id IS NULL OR refusal IS NULL),
  CHECK (replied_at IS NULL OR abandoned_at IS NULL)
);

CREATE UNIQUE INDEX chat_triggers_run ON chat_triggers (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX chat_triggers_due ON chat_triggers (next_attempt_at)
  WHERE replied_at IS NULL AND abandoned_at IS NULL;

-- +goose Down
DROP TABLE chat_triggers;

-- Narrowing. A run admitted from Mattermost would not satisfy the restored
-- constraint, so the rows that carry it are rewritten first. created_via is
-- informational and not frozen by runs_guard; 'api' is the nearest truth the
-- older schema can hold.
UPDATE runs SET created_via = 'api' WHERE created_via = 'mattermost';

ALTER DOMAIN created_via DROP CONSTRAINT created_via_check;

ALTER DOMAIN created_via ADD CONSTRAINT created_via_check
  CHECK (VALUE IN ('api', 'mcp', 'ui', 'slack', 'schedule', 'workflow', 'agent')) NOT VALID;
