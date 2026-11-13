# Shared notes along a run's lineage — implementation plan

Status: implemented (migration 0018, `backend/store/notes.go`, `backend/app/notes.go`,
`backend/mcp/notes.go`). Phase 1 scope unless marked otherwise.

Where the implementation departs from this plan, and why:

- **Partition DDL locking.** The plan had a session `pg_try_advisory_lock` on a
  connection pinned for the whole tick. Instead, each create and each drop is its
  own transaction: it takes `pg_advisory_xact_lock(notePartitionKey)`, rechecks
  `pg_inherits` and sets `lock_timeout`. This is simpler, every replica can call
  it, and the self-healing write path reuses it.
- **`runs_lineage_depth` is `cardinality(lineage) <= depth`.** The plan's
  `= 0 OR = depth` would refuse a child of a run admitted before 0018, whose
  lineage is one entry short of its depth.
- **The expiry is checked inside `PutNote`'s transaction, against
  `clock_timestamp()`.** A partition dropped while the write waited for its lock
  is then reported as expired, not as missing.
- **`PutNote` reads the run `FOR KEY SHARE`.** Without it, a run deleted
  concurrently could leave behind a note its trigger did not see.
- **Metrics are gauges read at scrape time** (`haliphron_note_partitions`,
  `haliphron_note_bytes`, `haliphron_notes`), not in-process counters.
  `backend/metrics` has every replica report the same values from the database,
  and per-process counters would break that.
- **Values are refused if they contain control characters other than line
  breaks and tabs, or bidirectional overrides.** The plan refused only NUL.
- **`get_note`'s data block uses a random boundary per answer**, so a value
  cannot close it early.
- **The maintenance loop uses `HALIPHRON_REAP_INTERVAL`** (1 h by default), not
  an interval of its own.

## 1. Goal

A run that has paid to learn something — how the repository is laid out, which command
builds it, which test is flaky, what a deferred continuation still has to do — can write it
down, and the runs it starts (through `run_agent` or `schedule_agent`) can read it instead
of paying for it again.

Four MCP tools, available to a run's per-run token only:

| Tool | Purpose |
|---|---|
| `put_note` | Write or overwrite one of the caller's own notes |
| `get_note` | Read one note by key from the caller's scope, with where it came from |
| `list_notes` | The keys visible in the caller's scope, with provenance and size, no values |
| `delete_note` | Remove one of the caller's own notes |

Notes are not a result channel (that is `get_run_result` / `output.json`), not a code
channel (git), and not a file channel (artifacts).

## 2. Decisions taken

1. **PostgreSQL, no Valkey.** Every read starts from "who are my ancestors", which is in
   Postgres anyway; the agent reaches notes only through the backend's MCP listener, so the
   store's latency is invisible next to an LLM turn; and ADR 24 already declines a second
   stateful component in the chart at this load. The app layer sees a `NoteStore` port, so
   another adapter stays possible.
2. **Live visibility.** A child sees what its ancestors write after it started, not a
   snapshot taken when it was admitted.
3. **No upward propagation.** A parent never sees its children's notes. A child's result
   reaches the parent through `get_run_result`.
4. **No seeding.** Notes are written only by runs, through MCP. No REST or UI write path,
   no `notes` argument at submission.
5. **Bounded physical size is a requirement, not an optimisation.** Notes are stored in
   daily partitions that are dropped whole (section 5). Expired notes are never `DELETE`d
   one by one.
6. **The lineage is recorded on the run at admission** (`runs.lineage`), so a chain does
   not break when an ancestor row is deleted by retention (section 6).

## 3. Semantics

### 3.1 Scope

```
root A            writes: arch, build_cmd
├── B             writes: build_cmd       ← shadows A's for B and below
│   └── D         sees D + B + A;  build_cmd resolves to B's
└── C             sees C + A;  never B's or D's
```

- A run **writes only its own notes.** It can shadow an ancestor's key with its own, never
  change or delete an ancestor's note.
- A run **reads its own notes and its ancestors'.** On a key present at several levels the
  nearest wins: self, then parent, then grandparent.
- Siblings and their subtrees are invisible to each other. A parent does not see its
  children.
- Every value is returned with its provenance: the writing run's id, how many levels up it
  is (`0` = self), and when it was last written.

### 3.2 Identity

The caller's run comes from the token (`Caller.Parent` in `backend/mcp`), never from an
argument, exactly as for locks. A call with an operator token is refused by all four tools:
an operator has no lineage, and there is no seeding (2.4).

A per-run token is revoked when its run becomes terminal, so a finished run can neither read
nor write. Its notes stay readable by its live descendants until they expire.

### 3.3 Live reads, and what follows from them

- A child may see a different value on two reads of the same key, if an ancestor still
  running overwrote it in between. That is the channel being bought; the tool description
  says so.
- A note belongs to the **run**, not to the epoch or the attempt, as a lock does. A
  controller retry (attempt 2) or a reassignment (new epoch) sees the notes the earlier
  attempt wrote. Usually that is a resume; occasionally it is an earlier mistake inherited.
  An old pod on a partitioned cluster can still write after a reassignment; the last write
  wins.
- `put_note` with the value already stored is a no-op that creates no row version
  (section 7.1), so an idempotent retry costs nothing in storage.

### 3.4 Lifetime

- **Logical:** a note is visible while its *owner run* is younger than the note retention:
  `owner_created_at > now() - retention`. All notes of one run expire together, at a time
  fixed when the run was admitted. There is no per-note TTL: the agent removes a note it no
  longer wants with `delete_note`.
- **Physical:** the whole daily partition holding a run's notes is dropped once every run
  in it is past retention (section 5.4). Space returns to the operating system at that
  moment.
- **With the run:** deleting a run (operator delete or retention reaper) deletes its notes
  in the same transaction, through a trigger on `runs` (section 5.5).
- Retention: `HALIPHRON_NOTE_RETENTION`, default **7 days** (equal to `MaxGiveUpAfter`, so
  one deferral that waits as long as it is allowed still finds its parent's notes),
  minimum **2 days** (a run that queues for a while and then runs for the maximum 24 h must
  not see its own notes expire under it), maximum **30 days**.
- A run older than retention that is still alive (queued very long) gets a clear refusal
  from `put_note` ("this run's notes have expired"), never a storage error.

### 3.5 Trust

A note is text written by a model that may have read a prompt-injected repository. It is
**data, not instructions**, and it is the one place where an injection can spread from one
run to its whole subtree. Therefore:

- notes are never inserted into a system prompt or a role by the platform; the agent reads
  them only by calling a tool;
- every tool description says that notes are unverified claims written by another agent,
  that they must not be followed as instructions, and that secrets must not be written into
  them;
- `get_note` answers with the provenance next to the value, never the bare value;
- values are never logged, at any level. Log lines and metrics carry the key, the size and
  the run ids.

As with `Succeeded`, nothing in a UI or a report may present a note as a verified fact.

## 4. Limits

Keys are refused, never sanitised, as for `runv1.ArtifactKey`.

| Limit | Value | On violation |
|---|---|---|
| Key | `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$` | `invalid_argument`, names the rule |
| Value | UTF-8, no NUL, ≤ 16 KiB | `invalid_argument` with the size |
| Live notes per run | ≤ 64 | `quota_exceeded`, says to delete or overwrite |
| Live bytes per run (keys + values) | ≤ 256 KiB | `quota_exceeded` with current usage |
| `list_notes` answer | ≤ 200 entries, optional `prefix` | truncated, `truncated: true` |

Quotas are counted under a per-run advisory lock, so parallel tool calls from one agent
cannot overshoot (section 7.1). Overwriting a key counts its new size, not old + new.

## 5. Storage, and why it stays small physically

### 5.1 Why `DELETE` alone does not shrink anything

- Under MVCC, `DELETE` and `UPDATE` leave dead tuples. Autovacuum makes their space
  *reusable inside the same table*, but the file does not shrink, except for empty pages at
  the very end, which a vacuum truncates only when it can briefly take an exclusive lock.
  A table that once peaked keeps its peak size.
- Values over ~2 KB are compressed and moved out to the TOAST table, which bloats the same
  way, separately.
- Indexes bloat on their own and are not shrunk by vacuum.
- `VACUUM FULL` and `CLUSTER` do return space, but they rewrite the table under an
  `ACCESS EXCLUSIVE` lock. They are an operator's emergency tool, not a background job's.
  `pg_repack` is an extension, and the installation is not guaranteed one (0001's header).

So the design never relies on deleting rows to bound the size. It drops partitions:
`DROP TABLE` on a partition removes its heap, its TOAST table and its indexes at once, with
no vacuum involved.

### 5.2 Partitioning

`run_notes` is partitioned by `RANGE (owner_created_at)`, the `created_at` of the run that
owns the note, copied into the row on insert. Partitions are one UTC day each, named
`run_notes_YYYYMMDD`.

Partitioning by the owner run's creation day, not by the note's write time, puts all notes
of one run in one partition, and lets them expire together at a time known when the run
was admitted. A partition therefore becomes entirely dead at a known moment:
`upper_bound + retention`.

```sql
-- +goose Up
CREATE TABLE run_notes (
  run_id            ulid        NOT NULL,   -- the owner; no FK, see 5.5
  owner_created_at  timestamptz NOT NULL,   -- runs.created_at of the owner; partition key
  key               text        NOT NULL,
  value             text        NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, owner_created_at, key)
) PARTITION BY RANGE (owner_created_at);
```

The primary key must include the partition key. `owner_created_at` is a function of
`run_id`, so `(run_id, owner_created_at, key)` is as unique as `(run_id, key)`, and
`ON CONFLICT` works against it. No other index: every access is by owner run.

The migration creates no partitions. Maintenance (5.4) creates them at startup and on every
tick, so a migration stays deterministic and a test database gets them the same way.

### 5.3 Row versions on overwrite

- Partitions are created `WITH (fillfactor = 90)`. With free space on the page, and no
  indexed column changing (`value` and `updated_at` are not indexed), an overwrite is a
  HOT update: no new index entry, and the old version is pruned on the page without a
  vacuum.
- `put_note` with an unchanged value changes nothing: `ON CONFLICT … DO UPDATE … WHERE
  run_notes.value IS DISTINCT FROM EXCLUDED.value`.
- Autovacuum defaults are adequate: each partition is small, and the vacuum's work is
  bounded by the partition's life.

### 5.4 Partition maintenance

`(*app.Service).RunNoteMaintenance(ctx, interval)`, started next to `RunSweeper` and
`RunReaper` in `backend/cmd/haliphron-backend/main.go`, every 1 h. It also runs once,
synchronously, before the MCP listener starts serving.

Every replica runs the loop, and there is no leader. A tick does its DDL only after it wins
`pg_try_advisory_lock(noteMaintenanceKey)`. It is a session-level lock, so the whole tick
runs on one connection taken from the pool, and the lock is released at the end of the
tick. A replica that loses skips the tick.

1. **Ensure.** Create every missing partition for the days from `now() - retention` to
   `now() + 3 days`. Existing ones are found through `pg_inherits`, so a normal tick issues
   no DDL at all. A missing day is created in two steps, in one transaction with
   `SET LOCAL lock_timeout = '2s'`:
   - first a standalone `CREATE TABLE run_notes_YYYYMMDD (…, PRIMARY KEY (run_id,
     owner_created_at, key)) WITH (fillfactor = 90)`;
   - then `ALTER TABLE run_notes ATTACH PARTITION … FOR VALUES FROM (…) TO (…)`.

   `CREATE TABLE … PARTITION OF` would take `ACCESS EXCLUSIVE` on the parent. `ATTACH`
   takes only `SHARE UPDATE EXCLUSIVE`, which blocks neither reads nor writes. Its
   validation scan is free on an empty table, and its primary key becomes a child of the
   parent's.
2. **Drop.** Drop every partition of `run_notes` whose name parses as `run_notes_YYYYMMDD`
   and whose upper bound is `≤ now() - retention`. Bounds come from the name, which ensure
   produced, and each table is checked to be a partition of `run_notes` through
   `pg_inherits`. Nothing else is ever dropped.
   - Dropping a partition takes `ACCESS EXCLUSIVE` on the parent for milliseconds. Each
     drop runs in its own transaction with `SET LOCAL lock_timeout = '2s'`. Without it, a
     drop queued behind one slow read blocks every read and write on notes that arrives
     after it. A drop that times out is retried on the next tick and counted in a metric.
   - `DETACH PARTITION … CONCURRENTLY` avoids even that lock, but it cannot run in a
     transaction and leaves a "detach pending" state to finalise after an interruption. It
     is a phase 2 option if the 2 s lock ever shows up in latency.
3. **Self-healing on insert.** If an insert still fails with "no partition of relation
   found for row" (SQLSTATE `23514`), `put_note` runs ensure once and retries once. A
   second failure is an internal error, logged, with the metric raised.

Retention changes act on the next tick. Lowering it drops more partitions at once, and
retention is a ceiling. Raising it keeps partitions longer, and no row's visibility depends
on what was stored when it was written.

### 5.5 Notes go with their run, through a trigger and not a foreign key

```sql
CREATE FUNCTION runs_delete_notes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  DELETE FROM run_notes WHERE run_id = OLD.id AND owner_created_at = OLD.created_at;
  RETURN OLD;
END $$;
CREATE TRIGGER runs_delete_notes AFTER DELETE ON runs
  FOR EACH ROW EXECUTE FUNCTION runs_delete_notes();
```

- Run deletion has two paths, operator delete and retention batch (`backend/store/deletion.go`).
  A trigger covers both, and any future one, the way `runs_release_locks` covers run ends.
- `owner_created_at = OLD.created_at` prunes the delete to one partition.
- **No foreign key on purpose.** A foreign key from a partition to `runs` ties the
  partition's DDL to the referenced table, so dropping a partition could take a lock on
  `runs`, the hottest table in the schema. Without the foreign key, a drop touches only
  `run_notes`. Integrity is still kept: a note's `run_id` always comes from an
  authenticated per-run token, so it names an existing run, and the trigger removes notes
  when the run goes.

### 5.6 The bound

The live footprint is at most the number of runs created within `retention + 1 day`,
times 256 KiB. At 1 000 runs a day with the default retention, the worst case is about
2 GiB. The realistic case, a few KiB per run, is tens of MiB. Dead-tuple slack is limited
to one partition's lifetime and goes with the partition. Nothing grows with installation
age.

The remaining unbounded quantity is churn: one run overwriting the same keys many times
within its life. HOT pruning and vacuum reclaim it in place, so it costs I/O, not size. If
the metrics ever show otherwise, a per-run write counter (for example, ≤ 1 000 writes) is
the phase 2 lever.

## 6. `runs.lineage`

```sql
ALTER TABLE runs ADD COLUMN lineage ulid[] NOT NULL DEFAULT '{}';
ALTER TABLE runs ADD CONSTRAINT runs_lineage_depth
  CHECK (cardinality(lineage) = 0 OR cardinality(lineage) = depth);
```

- Adding a column with a constant default does not rewrite the table (PostgreSQL 11+). No
  backfill either: updating every existing row of `runs` would create exactly the bloat this
  plan is about. Runs admitted before the migration have `'{}'`. A child admitted after it
  gets `parent.lineage || parent.id`, so in the worst case it sees its parent's notes but
  not those of ancestors further up. That is acceptable for a feature that did not exist
  for those ancestors.
- Written once, inside `InsertRun`, from the parent row in the same statement:
  `(SELECT lineage || id FROM runs WHERE id = $parent)`. Nothing else writes it.
- Immutable: a separate `BEFORE UPDATE OF lineage` trigger raises
  `integrity_constraint_violation`, as `runs_guard` does for the admission fields. It is
  not an edit to `runs_guard`, so 0003's function is not copied into a new migration.
- Why not a recursive CTE over `parent_run_id`: a deleted ancestor sets its child's
  `parent_run_id` to NULL, which would cut off everything above it. With `lineage`, only
  the deleted run's own notes disappear.

## 7. Queries

### 7.1 `put_note`

One transaction:

```sql
SELECT pg_advisory_xact_lock($noteClass, hashtext($run_id));

SELECT count(*), coalesce(sum(octet_length(key) + octet_length(value)), 0)
FROM run_notes
WHERE run_id = $run_id AND owner_created_at = $created AND key <> $key;
-- app checks: count + 1 ≤ 64, bytes + len(key) + len(value) ≤ 256 KiB

INSERT INTO run_notes (run_id, owner_created_at, key, value)
VALUES ($run_id, $created, $key, $value)
ON CONFLICT (run_id, owner_created_at, key) DO UPDATE
  SET value = EXCLUDED.value, updated_at = now()
  WHERE run_notes.value IS DISTINCT FROM EXCLUDED.value;
```

`$created` comes from the caller's run row (already loaded by `ResolveParent`). The
expiry check (3.4) happens before the transaction. The advisory class follows the
two-key pattern of `leaseLockClass` in `backend/store/lease.go`.

### 7.2 `get_note` / `list_notes`

```sql
WITH scope AS (
  SELECT u.run_id, u.ord
  FROM runs r, unnest(r.lineage || r.id) WITH ORDINALITY AS u(run_id, ord)
  WHERE r.id = $caller
)
SELECT DISTINCT ON (n.key)
       n.key, n.value, octet_length(n.value), n.run_id, n.updated_at,
       (SELECT max(ord) FROM scope) - s.ord AS levels_up
FROM run_notes n
JOIN scope s ON s.run_id = n.run_id
WHERE n.owner_created_at > now() - $retention   -- logical expiry; prunes old partitions
  AND ($key = '' OR n.key = $key)
  AND ($prefix = '' OR starts_with(n.key, $prefix))
ORDER BY n.key, s.ord DESC;                    -- nearest level wins
```

At depth ≤ 8, the scope has at most 9 runs, and each probes the primary-key index of the
non-expired partitions. `list_notes` uses the same query but leaves out `value`.

### 7.3 `delete_note`

`DELETE … WHERE run_id = $caller AND owner_created_at = $created AND key = $key`.
Deleting a key the caller does not own (including an ancestor's) reports "no such note of
yours". It never says whether an ancestor has that key.

## 8. Where the code goes

Rules go in `app`, SQL in `store`, and transport in `mcp`, as the hexagonal split requires.

| Place | What |
|---|---|
| `db/migrations/0018_run_notes.sql` | `run_notes` parent table, `runs.lineage` + check + immutability trigger, `runs_delete_notes` trigger; the header comment states the rules of this plan |
| `backend/store/notes.go` | `PutNote`, `GetNotes`, `DeleteNote`, `EnsureNotePartitions`, `DropExpiredNotePartitions`; advisory keys next to `lockWriteKey` |
| `backend/store/runs.go` | `InsertRun` fills `lineage` |
| `backend/app/notes.go` | key/value validation, quotas, expiry refusal, the self-healing retry, `RunNoteMaintenance` |
| `backend/mcp/notes.go` | the four tools, their descriptions (3.5), refusal of operator tokens |
| `backend/config` | `HALIPHRON_NOTE_RETENTION`, validated against 2 d … 30 d |
| `backend/cmd/haliphron-backend/main.go` | synchronous ensure before serving, then the loop |
| `backend/metrics` | `haliphron_note_ops_total{op,outcome}`, `haliphron_note_refusals_total{reason}`, `haliphron_note_partitions`, `haliphron_note_bytes` (sum of `pg_total_relation_size` over `pg_partition_tree('run_notes')`, sampled per tick), `haliphron_note_maintenance_failures_total{step}` |

No audit row per write: that would only move the growth problem into `audit`. Admission
already audits the run, and the metrics count the writes.

`fake/` is unaffected. Notes are not part of the Cluster API, the CRD or the agent runtime
contract, and the controller and image never see them.

## 9. Documents

- `docs/contracts/run-store.md`: a "Run notes" subsection after "Resource locks and deferred
  runs" (partitioning, no foreign key and why, the trigger, `lineage` and its single
  writer), plus checklist items in section 15.
- `docs/reference/mcp-tools.md`: the four tools.
- `docs/explanation/`: a short page next to `cooperative-locks.md` on scope, live reads, and
  why notes are untrusted.
- `CLAUDE.md` "Invariants that are easy to break": `lineage` is written only by
  `InsertRun`; a note is removed only by its owner's `delete_note`, the run-deletion
  trigger, or a partition drop; values are never logged.
- `docs/architecture.md` 13.3: the tool list.

## 10. Tests

`test/store` (real PostgreSQL):
- scope: self + ancestors visible, siblings and children invisible, nearest level wins,
  `levels_up` correct;
- live read: an ancestor's overwrite after the child's admission is visible;
- quotas under 20 concurrent `PutNote` calls from one run: never above 64 / 256 KiB;
- unchanged rewrite creates no new row version (`xmin` unchanged);
- expiry: owner older than retention is invisible, and `put_note` is refused;
- deleting a run removes its notes and leaves its descendants' view of the other ancestors
  intact (the `lineage` case);
- `lineage` is immutable, and `runs_lineage_depth` holds for new runs;
- maintenance: ensure is idempotent and issues no DDL when nothing is missing; drop removes
  exactly the expired partitions and never a table outside `pg_inherits`; a drop blocked by
  a held lock gives up after `lock_timeout` and succeeds on the next tick; two replicas
  ticking at once do the DDL once;
- missing partition: insert self-heals once.

`backend/mcp` and `test/backend`:
- identity from the token, and a `run_id` argument is ignored or rejected;
- an operator token is refused by all four tools;
- keys are refused, not sanitised (`"../x"`, `""`, 129 characters, a NUL in the value);
- `delete_note` on an ancestor's key does not remove it and does not reveal it;
- no log line at any level contains a value (as `controller/clusterapi/client_test.go`
  does for the lease body).

## 11. Out of scope

- Upward propagation, seeding, a per-note TTL (decisions 2.3, 2.4, 3.4).
- A UI or REST read of a run's notes. It is the first phase 2 candidate, for operators
  tracing how an injection spread.
- Notes shared across lineages or installation-wide memory: a different and far more
  dangerous feature.
- Workflow scope: when the workflow engine arrives, a workflow instance can become a scope
  root alongside a root run. The query in 7.2 changes only in how `scope` is built.
- A Valkey adapter for `NoteStore`: only on a measured need.
