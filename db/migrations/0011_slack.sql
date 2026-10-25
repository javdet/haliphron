-- Slack as a second chat platform.
--
-- 0007 keyed chat_triggers by platform so that a Slack adapter could share it,
-- and 0009 did the same for chat_integrations. Both still allowed Mattermost
-- alone; this widens them. Nothing else about either table changes: a Slack
-- message is claimed, answered and retried by exactly the rules a Mattermost
-- one is, and created_via has accepted 'slack' since 0007.
--
-- Slack identifies a message by its channel and its timestamp together, not by
-- either alone. The adapter writes both into message_id and thread_id as
-- '<channel>:<ts>', which fits the existing 64-character bound with room to
-- spare, so the columns keep their meaning: the message, and the root a reply
-- is posted under.
--
-- The Slack bot's credentials are managed secrets, as Mattermost's are:
-- slack-bot-token (xoxb-) for everything the bot reads and posts, and
-- slack-app-token (xapp-), which opens the Socket Mode connection and does
-- nothing else.

-- +goose Up
ALTER TABLE chat_triggers DROP CONSTRAINT chat_triggers_platform_check;
ALTER TABLE chat_triggers ADD CONSTRAINT chat_triggers_platform_check
  CHECK (platform IN ('mattermost', 'slack'));

ALTER TABLE chat_integrations DROP CONSTRAINT chat_integrations_platform_check;
ALTER TABLE chat_integrations ADD CONSTRAINT chat_integrations_platform_check
  CHECK (platform IN ('mattermost', 'slack'));

-- +goose Down
-- Slack's rows go first, or the narrower constraints could not be added back.
DELETE FROM chat_triggers WHERE platform = 'slack';
DELETE FROM chat_integrations WHERE platform = 'slack';

ALTER TABLE chat_integrations DROP CONSTRAINT chat_integrations_platform_check;
ALTER TABLE chat_integrations ADD CONSTRAINT chat_integrations_platform_check
  CHECK (platform IN ('mattermost'));

ALTER TABLE chat_triggers DROP CONSTRAINT chat_triggers_platform_check;
ALTER TABLE chat_triggers ADD CONSTRAINT chat_triggers_platform_check
  CHECK (platform IN ('mattermost'));
