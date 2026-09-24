-- The plugins phase.
--
-- runtime_phase is re-declared rather than extended in place because a domain
-- CHECK cannot be added to: the constraint is dropped and re-added, and writing
-- the whole list here keeps the accepted set readable in one file instead of
-- assembled from a chain of ALTERs. It must stay identical to RuntimePhases in
-- api/run/v1/runtime.go, which TestRuntimePhaseDomainMatchesGo enforces against
-- the last migration to touch the domain — this one, until the next phase.
--
-- Widening only. Every name the previous constraint accepted is still accepted,
-- so no existing run_attempts.completed_phases row can be invalidated and the
-- migration needs no backfill.
--
-- NOT VALID is required rather than chosen. PostgreSQL refuses to re-scan a
-- domain that an array column uses — run_attempts.completed_phases is
-- runtime_phase[] — and answers "cannot alter type because column uses it".
-- NOT VALID skips only that scan of rows already stored; every subsequent
-- insert and update is still checked. For a widening that is exactly right:
-- the rows it declines to re-read are rows that satisfied a stricter list.

-- +goose Up
ALTER DOMAIN runtime_phase DROP CONSTRAINT runtime_phase_check;

ALTER DOMAIN runtime_phase ADD CONSTRAINT runtime_phase_check
  CHECK (VALUE IN ('init', 'validate', 'fetch', 'checkpoint', 'auth', 'clone',
                   'role', 'plugins', 'mcp-prepare', 'mcp-verify', 'run', 'parse',
                   'output', 'persist', 'commit', 'push', 'pr',
                   'finalize', 'notify')) NOT VALID;

-- +goose Down
-- Narrowing. A checkpoint recorded against the plugins phase would not satisfy
-- the restored constraint, so the rows that carry one are rewritten first:
-- dropping the name is the whole of what going back means, and a retry reruns
-- the phase, which is idempotent.
UPDATE run_attempts
   SET completed_phases = array_remove(completed_phases::text[], 'plugins')::runtime_phase[]
 WHERE 'plugins' = ANY (completed_phases::text[]);

ALTER DOMAIN runtime_phase DROP CONSTRAINT runtime_phase_check;

ALTER DOMAIN runtime_phase ADD CONSTRAINT runtime_phase_check
  CHECK (VALUE IN ('init', 'validate', 'fetch', 'checkpoint', 'auth', 'clone',
                   'role', 'mcp-prepare', 'mcp-verify', 'run', 'parse',
                   'output', 'persist', 'commit', 'push', 'pr',
                   'finalize', 'notify')) NOT VALID;
