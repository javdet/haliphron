-- Alert replies in a team-chat thread.
--
-- Alertmanager notifies people in a chat channel and, separately, a Haliphron
-- alert receiver, which starts a run. A receiver may name one channel on one
-- platform; when it does, and that platform's integration is enabled when a
-- notification is admitted, the run owes an answer there, in the thread of the
-- channel post that announced the same alerts. The agent never touches the
-- chat: the backend posts, through the same chat_triggers reply chat-started
-- runs use.
--
-- The two notifications are joined by Alertmanager's fingerprints, which the
-- chat notification must print. The row is written inside InsertRun's
-- transaction (store.linkChatAlertReply, next to linkAlertFire), so a run never
-- commits without the reply it owes. It does not know its thread yet:
-- thread_id is NULL, and the reply loop's search pass looks for the post
-- between thread_search_since and thread_search_until, at next_search_at, and
-- sets thread_id once with
--
--   UPDATE ... SET thread_id = $post WHERE ... AND thread_id IS NULL
--
-- so that one replica wins and only the winner reacts; thread_found_at records
-- that the search, rather than a fallback root post, gave the row its thread. The deliver pass holds
-- such a row back until its thread is known or its window has closed; a closed
-- window posts a new root post in the channel, and that post becomes the
-- thread. Follow-up rows of an alert row (message_id 'run:<child>') copy the
-- NULL and the window, and read their thread through their origin's row.
--
-- alert_fingerprints are the alerts the run carries, which the search matches
-- on; alert_names their alertname labels, for the header of a root post made
-- when no post was found.
--
-- An alert row is message_id 'alert:<run id>' and user_id
-- 'alert-receiver:<receiver id>', never a chat account, so a thread it is in
-- never listens without a mention.
--
-- alert_receivers.chat_channel_name is what the platform called the channel
-- when the receiver was saved, for display only.

-- +goose Up
ALTER TABLE alert_receivers
  ADD COLUMN chat_platform     text CHECK (chat_platform IN ('mattermost', 'slack')),
  ADD COLUMN chat_channel_id   text CHECK (length(chat_channel_id) BETWEEN 1 AND 64),
  ADD COLUMN chat_channel_name text NOT NULL DEFAULT '' CHECK (length(chat_channel_name) <= 256),
  ADD CONSTRAINT alert_receivers_chat_pair
    CHECK ((chat_platform IS NULL) = (chat_channel_id IS NULL));

ALTER TABLE chat_triggers
  ALTER COLUMN thread_id DROP NOT NULL,
  ADD COLUMN alert_fingerprints  text[],
  ADD COLUMN alert_names         text[],
  ADD COLUMN thread_search_since timestamptz,
  ADD COLUMN thread_search_until timestamptz,
  ADD COLUMN next_search_at      timestamptz,
  ADD COLUMN thread_found_at     timestamptz,
  ADD CONSTRAINT chat_triggers_thread_known
    CHECK (thread_id IS NOT NULL OR thread_search_until IS NOT NULL);

CREATE INDEX chat_triggers_thread_search ON chat_triggers (next_search_at)
  WHERE thread_id IS NULL;

-- +goose Down
DROP INDEX chat_triggers_thread_search;

DELETE FROM chat_triggers WHERE thread_id IS NULL;

ALTER TABLE chat_triggers
  DROP CONSTRAINT chat_triggers_thread_known,
  DROP COLUMN thread_found_at,
  DROP COLUMN next_search_at,
  DROP COLUMN thread_search_until,
  DROP COLUMN thread_search_since,
  DROP COLUMN alert_names,
  DROP COLUMN alert_fingerprints,
  ALTER COLUMN thread_id SET NOT NULL;

ALTER TABLE alert_receivers
  DROP CONSTRAINT alert_receivers_chat_pair,
  DROP COLUMN chat_channel_name,
  DROP COLUMN chat_channel_id,
  DROP COLUMN chat_platform;
