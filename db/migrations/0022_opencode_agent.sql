-- opencode, the third runtime.
--
-- agent_type gains 'opencode' by the drop-and-re-add 0020 uses for
-- runtime_phase, with the whole list written out. NOT VALID: every existing
-- row satisfies the wider constraint, and checking them all would hold a lock
-- on runs and clusters for nothing.

-- +goose Up
ALTER DOMAIN agent_type DROP CONSTRAINT agent_type_check;

ALTER DOMAIN agent_type ADD CONSTRAINT agent_type_check
  CHECK (VALUE IN ('claude-code', 'codex', 'opencode')) NOT VALID;

-- +goose Down

-- Narrowing. A cluster's declared runtimes are rewritten by its next
-- heartbeat, and a heartbeat still naming opencode would then fail against the
-- restored constraint, so opencode is removed from them here. A run's agent is
-- left alone: runs_guard refuses to change it, and NOT VALID does not check
-- the rows already there — an opencode run stays in the history it made.
UPDATE clusters
   SET runtimes = array_remove(runtimes::text[], 'opencode')::agent_type[]
 WHERE 'opencode' = ANY (runtimes::text[]);

ALTER DOMAIN agent_type DROP CONSTRAINT agent_type_check;

ALTER DOMAIN agent_type ADD CONSTRAINT agent_type_check
  CHECK (VALUE IN ('claude-code', 'codex')) NOT VALID;
