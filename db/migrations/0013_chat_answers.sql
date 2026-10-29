-- A chat reply is the agent's answer and nothing else.
--
-- 0009 made the reply a policy: 'always' acknowledged the message and posted
-- the run's result with its status, cost and a link, and 'if_unanswered' left
-- the thread to an agent that posted through chat tools of its own. Both are
-- gone. Agents never post in chat: the backend reacts to the message, and when
-- a run ends it posts what the agent answered, as written, or one plain
-- sentence when the run failed or the agent said nothing. A run that started a
-- run of its own posts nothing, because the answer is its child's, and the
-- child has a follow-up row of its own. With one behaviour there is nothing to
-- freeze on a row and nothing to configure, so reply_policy is dropped from
-- both tables.
--
-- chat_integrations.run_url linked the result post to the run in the UI. A
-- reply carries no link now, and the column is dropped with the policy.

-- +goose Up
ALTER TABLE chat_triggers DROP COLUMN reply_policy;

ALTER TABLE chat_integrations
  DROP COLUMN reply_policy,
  DROP COLUMN run_url;

-- +goose Down
-- What 0009 added, with its defaults. What a row was claimed under is not
-- recoverable, and 'always' is what a row written before 0009 read as.
ALTER TABLE chat_integrations
  ADD COLUMN run_url text NOT NULL DEFAULT '' CHECK (length(run_url) <= 2048),
  ADD COLUMN reply_policy text NOT NULL DEFAULT 'if_unanswered'
    CHECK (reply_policy IN ('always', 'if_unanswered'));

ALTER TABLE chat_triggers
  ADD COLUMN reply_policy text NOT NULL DEFAULT 'always'
    CHECK (reply_policy IN ('always', 'if_unanswered'));
