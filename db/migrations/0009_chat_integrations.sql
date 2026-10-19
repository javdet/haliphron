-- The chat integration as data, and the reply a chat-started run owes as a
-- policy rather than a constant.
--
-- chat_integrations is the bot's configuration when an operator keeps it in
-- the UI rather than in the deployment's environment. One row per platform. It
-- holds no credential: the bot's token and the outgoing-webhook tokens are
-- managed secrets under fixed names, encrypted like every other, and this row
-- says only how the bot behaves. The environment wins when it names a server,
-- so there is never a question of which of two sources is in force.
--
-- updated_at is what every API replica polls to notice a change; it is written
-- by the statement that saves the row, never by a trigger, so a no-op save is
-- still a restart an operator can ask for.
--
-- chat_triggers gains three things.
--
-- reply_policy is frozen on the row when the message is claimed. 'always' is
-- what 0007 did: an acknowledgement, then the run's result. 'if_unanswered' is
-- for an agent that answers in the thread itself: the backend reacts, and when
-- the run ends it posts only if nothing answered the thread after the claim —
-- because Succeeded means the agent exited 0, and that is not the same as the
-- person having been answered. Frozen, because an operator changing the policy
-- while a run is in flight must not change what that run owes.
--
-- channel_name and channel_type are what a prompt says about where a message
-- came from, kept so a child run started by the agent can be told the same.
--
-- origin_message_id marks a follow-up: a row inserted for a child run, in the
-- child's own insert, copying where its parent's reply goes. Its message_id is
-- 'run:' and the child's identifier — not a chat message, and never claimed —
-- and origin_message_id is the message the whole tree answers. A follow-up is
-- what makes a child that fails, or ends without answering, still owe the
-- person a reply.

-- +goose Up
CREATE TABLE chat_integrations (
  tenant_id        uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
  platform         text        NOT NULL CHECK (platform IN ('mattermost')),
  enabled          boolean     NOT NULL DEFAULT false,
  transport        text        NOT NULL DEFAULT 'websocket'
                               CHECK (transport IN ('websocket', 'webhook', 'both')),
  url              text        NOT NULL DEFAULT '' CHECK (length(url) <= 2048),
  role             text        NOT NULL DEFAULT '' CHECK (length(role) <= 128),
  repo_url         text        NOT NULL DEFAULT '' CHECK (length(repo_url) <= 2048),
  base_branch      text        NOT NULL DEFAULT '' CHECK (length(base_branch) <= 255),
  run_url          text        NOT NULL DEFAULT '' CHECK (length(run_url) <= 2048),
  reply_policy     text        NOT NULL DEFAULT 'if_unanswered'
                               CHECK (reply_policy IN ('always', 'if_unanswered')),
  max_post_runes   integer     NOT NULL DEFAULT 16383
                               CHECK (max_post_runes BETWEEN 1000 AND 65535),
  max_thread_bytes integer     NOT NULL DEFAULT 65536 CHECK (max_thread_bytes >= 0),
  updated_by       text        NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, platform)
);

ALTER TABLE chat_triggers
  ADD COLUMN reply_policy text NOT NULL DEFAULT 'always'
    CHECK (reply_policy IN ('always', 'if_unanswered')),
  ADD COLUMN channel_name text NOT NULL DEFAULT '' CHECK (length(channel_name) <= 256),
  ADD COLUMN channel_type text NOT NULL DEFAULT '' CHECK (length(channel_type) <= 8),
  ADD COLUMN origin_message_id text
    CHECK (origin_message_id IS NULL OR length(origin_message_id) BETWEEN 1 AND 64);

-- +goose Down
-- Follow-ups go first: under 0007's rules a row with a run was claimed from a
-- message, and these were not.
DELETE FROM chat_triggers WHERE origin_message_id IS NOT NULL;

ALTER TABLE chat_triggers
  DROP COLUMN origin_message_id,
  DROP COLUMN channel_type,
  DROP COLUMN channel_name,
  DROP COLUMN reply_policy;

DROP TABLE chat_integrations;
