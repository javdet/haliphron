-- Run inputs: the files a person sent with a request, and the inputs phase.
--
-- A file attached to a chat message is downloaded by the backend, sniffed,
-- capped and stored under the run's prefix, runs/{id}/inputs/{n}-{name}, before
-- the run is inserted; run_inputs is the manifest of what was stored. The lease
-- carries it to the controller, which fetches every object before it
-- acknowledges, and the pod reads them from the controller in the inputs phase.
--
-- run_inputs is written only inside InsertRun's transaction, like a run's
-- chat-trigger link, so a run never commits without its manifest and a
-- manifest never exists without its run. It goes with the run: ON DELETE
-- CASCADE, and the primary key leads with run_id, which is the index run
-- retention's batch deletes need.
--
-- The bytes are stored before the row exists, because the row's transaction
-- is also the run's and the upload must not hold it open. An admission that
-- stores its inputs and then fails to insert — refused, or a process that died
-- between the two — leaves objects under a prefix no run owns. run_input_uploads
-- is the ledger that finds them without listing a bucket: a row is written
-- before the first object, deleted by InsertRun in the run's own transaction,
-- and a row older than an hour whose run does not exist names a prefix the
-- sweeper deletes. It has no foreign key, because its whole point is to outlive
-- a run that never came to be.
--
-- runtime_phase gains 'inputs', between clone and role, by the drop-and-re-add
-- 0006 uses, with the whole list written out.

-- +goose Up
ALTER DOMAIN runtime_phase DROP CONSTRAINT runtime_phase_check;

ALTER DOMAIN runtime_phase ADD CONSTRAINT runtime_phase_check
  CHECK (VALUE IN ('init', 'validate', 'fetch', 'checkpoint', 'auth', 'clone',
                   'inputs', 'role', 'plugins', 'mcp-prepare', 'mcp-verify', 'run',
                   'parse', 'output', 'persist', 'commit', 'push', 'pr',
                   'finalize', 'notify')) NOT VALID;

CREATE TABLE run_inputs (
  run_id             ulid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  ordinal            smallint    NOT NULL CHECK (ordinal BETWEEN 0 AND 999),
  -- The key relative to the run's prefix. The shape is runv1.InputKey's; the
  -- CHECK is the database's copy of the part of it a regex can say.
  key                text        NOT NULL
                     CHECK (key ~ '^inputs/[0-9]{1,3}-[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
                            AND key NOT LIKE '%..%'),
  -- What the person called the file. Display only.
  name               text        NOT NULL CHECK (length(name) <= 1024),
  -- What the bytes sniffed as, never what the platform claimed.
  media_type         text        NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
  size_bytes         bigint      NOT NULL CHECK (size_bytes >= 0),
  sha256             bytea       NOT NULL CHECK (length(sha256) = 32),
  -- The chat message the file was attached to, for "which post was this".
  source_message_id  text        CHECK (length(source_message_id) <= 256),
  PRIMARY KEY (run_id, ordinal),
  UNIQUE (run_id, key)
);

CREATE TABLE run_input_uploads (
  run_id      ulid         PRIMARY KEY,
  started_at  timestamptz  NOT NULL DEFAULT now()
);

CREATE INDEX run_input_uploads_started ON run_input_uploads (started_at);

-- +goose Down
DROP TABLE run_input_uploads;
DROP TABLE run_inputs;

-- Narrowing. A checkpoint recorded against the inputs phase would not satisfy
-- the restored constraint, so the rows that carry one are rewritten first, as
-- 0006's Down does for 'plugins'.
UPDATE run_attempts
   SET completed_phases = array_remove(completed_phases::text[], 'inputs')::runtime_phase[]
 WHERE 'inputs' = ANY (completed_phases::text[]);

ALTER DOMAIN runtime_phase DROP CONSTRAINT runtime_phase_check;

ALTER DOMAIN runtime_phase ADD CONSTRAINT runtime_phase_check
  CHECK (VALUE IN ('init', 'validate', 'fetch', 'checkpoint', 'auth', 'clone',
                   'role', 'plugins', 'mcp-prepare', 'mcp-verify', 'run', 'parse',
                   'output', 'persist', 'commit', 'push', 'pr',
                   'finalize', 'notify')) NOT VALID;
