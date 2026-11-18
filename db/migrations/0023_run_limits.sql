-- Several repositories on one run.
--
-- run_limits is a run's installation-wide limits, kept as data so an operator
-- changes them from the UI and never from Helm or the environment: every
-- replica reads the row on every admission, so a saved value applies to the
-- next request on all of them. One row per tenant, and no row means the
-- defaults the backend states (run.DefaultMaxRepos, ten), the way
-- token_retention reads before it is first saved. Named for the run rather
-- than for repositories: the next limit on a run is another column here.
--
-- max_repos_per_run counts the working repository too. The ceiling of 32 is
-- the schema's (RenderedRunSpec.extraRepos has maxItems 31), and a limit the
-- CRD would refuse is refused here first.
--
-- runs.extra_repo_results is what happened to each of the spec's extraRepos,
-- a JSON array of ExtraRepoResult keyed by path. The working repository keeps
-- its columns (pr_url, commit_sha, …). Written by promoteAttempt, which merges
-- an incoming report into it field by field with the COALESCE rule those
-- columns follow: a later attempt that opened no pull request does not erase
-- the one an earlier attempt opened.
--
-- runtime_phase gains 'clone-extra', between clone and inputs, by the
-- drop-and-re-add 0020 uses, with the whole list written out.

-- +goose Up
CREATE TABLE run_limits (
  tenant_id         uuid        PRIMARY KEY DEFAULT '00000000-0000-0000-0000-000000000000',
  max_repos_per_run smallint    NOT NULL CHECK (max_repos_per_run BETWEEN 1 AND 32),
  updated_by        text        NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE runs ADD COLUMN extra_repo_results jsonb
  CHECK (extra_repo_results IS NULL OR jsonb_typeof(extra_repo_results) = 'array');

ALTER DOMAIN runtime_phase DROP CONSTRAINT runtime_phase_check;

ALTER DOMAIN runtime_phase ADD CONSTRAINT runtime_phase_check
  CHECK (VALUE IN ('init', 'validate', 'fetch', 'checkpoint', 'auth', 'clone',
                   'clone-extra', 'inputs', 'role', 'plugins', 'mcp-prepare',
                   'mcp-verify', 'run', 'parse', 'output', 'persist', 'commit',
                   'push', 'pr', 'finalize', 'notify')) NOT VALID;

-- +goose Down
UPDATE run_attempts
   SET completed_phases = array_remove(completed_phases::text[], 'clone-extra')::runtime_phase[]
 WHERE 'clone-extra' = ANY (completed_phases::text[]);

ALTER DOMAIN runtime_phase DROP CONSTRAINT runtime_phase_check;

ALTER DOMAIN runtime_phase ADD CONSTRAINT runtime_phase_check
  CHECK (VALUE IN ('init', 'validate', 'fetch', 'checkpoint', 'auth', 'clone',
                   'inputs', 'role', 'plugins', 'mcp-prepare', 'mcp-verify', 'run',
                   'parse', 'output', 'persist', 'commit', 'push', 'pr',
                   'finalize', 'notify')) NOT VALID;

ALTER TABLE runs DROP COLUMN extra_repo_results;
DROP TABLE run_limits;
