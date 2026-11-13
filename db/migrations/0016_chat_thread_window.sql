-- A thread the bot was asked in keeps listening for a while.
--
-- Until now a message in a channel was a request only when it named the bot.
-- In a thread where one person is talking to the bot, naming it every time is
-- friction for nothing: there is nobody else the message could be for. So a
-- thread listens without a mention while two things hold.
--
-- The thread is recent: some row of chat_triggers in it was claimed, or
-- replied to, within the integration's thread_window_minutes. The window is
-- measured from the later of the two, so a long run does not use up the window
-- before its answer is posted, and every message the bot acts on extends it.
-- When the window passes in silence the thread needs a mention again. 0 turns
-- the behaviour off.
--
-- The thread is one person's: no human but the sender has posted in it, root
-- included, up to the message. That is decided from the thread as the chat
-- server reports it, not from this table, which holds only the messages that
-- were requests; the table is the cheap first filter, read before anything is
-- claimed. Once a second person posts, every message needs a mention again.
--
-- There is no subscription to keep or expire: whether a thread listens is
-- computed from rows that exist anyway. chat_triggers_thread is what makes
-- that read cheap.

-- +goose Up
ALTER TABLE chat_integrations
  ADD COLUMN thread_window_minutes integer NOT NULL DEFAULT 240
    CHECK (thread_window_minutes BETWEEN 0 AND 10080);

CREATE INDEX chat_triggers_thread ON chat_triggers (platform, thread_id);

-- +goose Down
DROP INDEX chat_triggers_thread;

ALTER TABLE chat_integrations DROP COLUMN thread_window_minutes;
