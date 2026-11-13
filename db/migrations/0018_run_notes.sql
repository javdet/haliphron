-- Run notes: what a run has learned, written down for the runs it starts.
--
-- A run writes notes under keys of its own; it reads its own notes and those
-- of its ancestors, the nearest level winning on a key present at several.
-- Siblings never see each other's notes, and a parent never sees its
-- children's. Reads are live: a child sees what a still-running ancestor
-- writes after the child was admitted. Notes are written only by runs,
-- through MCP, under the identity of the per-run token; nothing seeds them.
--
-- The size of this table must be bounded physically, not only logically, and
-- that decides its shape. Under MVCC a DELETE leaves dead tuples; vacuum makes
-- their space reusable inside the same file but does not give it back, and
-- TOAST and the indexes bloat on their own. VACUUM FULL gives it back under an
-- ACCESS EXCLUSIVE lock, and pg_repack is an extension this schema may not
-- assume (0001). So expired notes are never deleted row by row. The table is
-- partitioned by RANGE on the owner run's created_at, one UTC day per
-- partition, which puts all notes of one run in one partition and makes the
-- partition entirely dead at a moment known in advance: its upper bound plus
-- the note retention. Dropping it removes the heap, its TOAST table and its
-- index at once, and the space goes back to the operating system.
--
-- This migration creates no partitions. The backend's note maintenance
-- (backend/store/notes.go) creates the days it needs and drops the expired
-- ones, every hour and once before the MCP listener serves: a migration that
-- created "today" would be correct only on the day it ran. A partition is
-- created standalone and then ATTACHed, because CREATE TABLE ... PARTITION OF
-- takes ACCESS EXCLUSIVE on this table and ATTACH takes only SHARE UPDATE
-- EXCLUSIVE, which blocks neither reads nor writes.
--
-- The primary key carries the partition key, as PostgreSQL requires.
-- owner_created_at is a function of run_id, so the key is as unique as
-- (run_id, key). There is no other index: every access is by owner run.
--
-- There is no foreign key to runs, on purpose. A foreign key from a partition
-- ties the partition's DDL to the referenced table, so dropping a partition
-- would take a lock on runs, the busiest table in this schema. Notes go with
-- their run through the trigger below instead, which covers every path that
-- deletes runs, present and future, the way runs_release_locks covers every
-- path that ends one.
--
-- runs.lineage is the run's ancestors, root first, recorded once at admission
-- by InsertRun and never changed. A read walks it rather than parent_run_id,
-- because deleting a run sets its children's parent_run_id to NULL, which
-- would cut a chain off at the first ancestor retention removed. With lineage
-- only the deleted run's own notes disappear. It is added with a constant
-- default, which rewrites nothing, and is not backfilled: updating every row
-- of runs would create precisely the bloat this migration is about. A run
-- admitted before it has '{}', so its descendants see the notes of the
-- ancestors admitted after it and not of those before, which is why the check
-- is <= depth rather than = depth.

-- +goose Up
CREATE TABLE run_notes (
  run_id            ulid        NOT NULL,
  owner_created_at  timestamptz NOT NULL,
  key               text        NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 128),
  value             text        NOT NULL CHECK (octet_length(value) BETWEEN 1 AND 16384),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, owner_created_at, key)
) PARTITION BY RANGE (owner_created_at);

-- +goose StatementBegin
CREATE FUNCTION delete_notes_with_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- owner_created_at prunes the delete to the one partition the run's notes
  -- are in.
  DELETE FROM run_notes WHERE run_id = OLD.id AND owner_created_at = OLD.created_at;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runs_delete_notes AFTER DELETE ON runs
  FOR EACH ROW EXECUTE FUNCTION delete_notes_with_run();

ALTER TABLE runs
  ADD COLUMN lineage ulid[] NOT NULL DEFAULT '{}',
  ADD CONSTRAINT runs_lineage_depth CHECK (cardinality(lineage) <= depth);

-- A trigger of its own rather than a clause in runs_guard, so that 0003's
-- function is not copied into a second migration to change one line.
-- +goose StatementBegin
CREATE FUNCTION runs_lineage_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'run %: lineage is immutable', OLD.id
    USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runs_lineage_guard BEFORE UPDATE OF lineage ON runs
  FOR EACH ROW
  WHEN (OLD.lineage IS DISTINCT FROM NEW.lineage)
  EXECUTE FUNCTION runs_lineage_guard();

-- +goose Down
DROP TRIGGER runs_lineage_guard ON runs;
DROP FUNCTION runs_lineage_guard();
ALTER TABLE runs DROP CONSTRAINT runs_lineage_depth, DROP COLUMN lineage;
DROP TRIGGER runs_delete_notes ON runs;
DROP FUNCTION delete_notes_with_run();
DROP TABLE run_notes;
