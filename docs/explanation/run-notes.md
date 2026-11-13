# About run notes

A run that has spent an hour on a repository knows things the runs it starts
will need: how the code is built, which test is flaky, where the configuration
lives, what is done and what is left. Without notes, every child run pays to
learn all of that again. Notes let a run write those things down for the runs
below it.

This page explains why notes are shaped the way they are. The tools are
described in the [MCP reference](../reference/mcp-tools.md#run-notes).

## A note goes down a lineage, never up or sideways

A run reads its own notes and its ancestors' notes, and nothing else:

```
root A            writes: layout, build/command
├── B             writes: build/command   ← replaces A's for B and below
│   └── D         sees D + B + A; build/command is B's
└── C             sees C + A; never B's or D's
```

**Downward only.** A parent knows what it told its children, and what a child
learned reaches the parent as the child's result, through `get_run_result`. A
channel upward would turn notes into a second result channel with weaker
guarantees.

**Never sideways.** Two siblings are often two attempts at the same problem,
or two independent pieces of work. If one could read the other, the result of
each would depend on timing.

**Nearest first.** A child can correct what an ancestor wrote, for itself and
for the runs below it, by writing the same key. The ancestor's note is not
changed: a child cannot rewrite what its siblings read.

**Live.** A child sees what an ancestor writes after the child started. That is
the point when a parent hands work out in stages. It also means two reads of
the same key can differ if the ancestor is still running.

## Why the lineage is stored on the run

Every read starts from "who are my ancestors". Following `parent_run_id` upward
would break the first time retention deleted a run in the middle of a chain,
because deleting a run sets its children's `parent_run_id` to NULL. So each run
records its ancestors when it is admitted, in `runs.lineage`, and the list never
changes. If a middle run is deleted, only its own notes disappear, and the runs
below it still see the notes of the runs above it.

## Why the storage is partitioned

The notes table must not grow with the age of the installation. In PostgreSQL,
deleting rows does not achieve that:

- a deleted row leaves a dead tuple;
- vacuum makes its space reusable inside the same file but does not return it;
- long values in the TOAST table and the index bloat the same way;
- returning the space needs `VACUUM FULL`, which locks the table, or
  `pg_repack`, which is an extension the installation may not have.

So expired notes are never deleted one by one. The table is split into one
partition per day, by the day the owning run was admitted. All of one run's
notes are in one partition and expire together, and a whole partition becomes
expired at a moment known in advance. Then it is dropped, which returns its
space to the operating system at once. The live size is bounded by the number
of runs admitted within the retention, times the per-run quota.

There is no foreign key from notes to runs, on purpose. A foreign key would
make every partition drop take a lock on `runs`, the busiest table in the
system. A trigger removes a run's notes when the run is deleted instead.

## Why there are quotas

A run can write at most 64 notes and 256 KiB. A single note is at most 16 KiB.
These limits are not about disk alone. Everything a run writes is offered to
every run below it, and an agent that has been talked into dumping a file into
a note would make each descendant read it. A note is meant to be a summary, and
the limits keep it one.

The quota is counted under a lock per run, so an agent that calls `put_note`
many times in parallel cannot slip past it.

## Why notes are data, not instructions

The agent that writes a note may have read a repository that contains a prompt
injection. Notes are the one channel through which such an injection can reach
a whole subtree of runs, because everything below the writer reads them. So:

- the platform never puts a note into a prompt or a role. An agent sees a note
  only when it asks for one;
- every answer says which run wrote the note and how far up it is;
- `get_note` puts the value inside a block that says it is unverified data,
  with a boundary chosen at random for each answer, so the value cannot
  pretend the block has ended;
- keys are refused, not cleaned, and values with hidden control characters are
  refused;
- values are never logged, at any level.

As with [what "Succeeded" means](what-succeeded-means.md), nothing presents a
note as a verified fact.

## What notes are not

- **Not seeded from outside.** Only runs write notes. A person who wants a run
  to know something says it in the prompt.
- **Not shared between unrelated runs.** Memory across lineages, or for the
  whole installation, would let one injected run influence every later run.
  That is a different and far more dangerous feature.
- **Not results, code or files.** Results go to `output.json` and
  `get_run_result`, code goes to git, and files are artifacts.
