# The state store — semantics

Status: ready for implementation
Date: 2026-09-17
Schema: [db/migrations](../../db/migrations), the contract queries —
[db/queries](../../db/queries)
Basis: [architecture.md](../architecture.md), sections 6, 7, 9.1, 12;
[Cluster API](cluster-api.md), sections 3–6

The DDL describes **what is stored**. This document describes **what it means**:
who writes each column, which invariants the schema holds structurally and which
remain code, and why the boundary is drawn where it is.

This is the fourth contract of phase 1 and the first with no second party across
a network. A divergence here looks different from the first three: not "two teams
understood a field differently" but "the ingest path and the heartbeat path,
written weeks apart, apply the same observation differently, and the run ends up
terminal twice with different outcomes". The schema therefore takes on exactly
those rules that can be checked on write.

Section [14](#14-deltas-to-architecturemd) lists the changes this work
introduced into the architecture document. Section [16](#16-deferred) is what we
decided not to do in phase 1.

---

## 1. Contract boundaries

| What | Where |
|---|---|
| the schema, the invariants, applying migrations | **this contract** |
| backend ↔ controller | [Cluster API](cluster-api.md) |
| controller ↔ kubernetes | [CRD `AgentRun`](agentrun-crd.md) |
| controller ↔ pod | [the agent image runtime contract](agent-runtime.md) |
| unbounded content (logs, results, artifacts) | the artifact store, key layout in section 9.2 of the architecture |

There is no *unbounded* content anywhere in the schema. There are three
deliberate bounded cases:

- `runs.prompt` (512 KiB), which is here because the alternative — an object in
  a bucket — made an object store a prerequisite for *starting* a run rather
  than for finishing one;
- `runs.result_summary` (64 KiB), a duplicate so that the run list does not hit
  storage row by row;
- `run_attempts.completion` (the pod's report in full, see section 8).

Everything else is references, and references are derived from `runs.id`: the
`runs/{id}/` layout is fixed by the contract, so there are no columns for keys.
`runs.result_ref` is the one exception, and it holds a URI with its scheme —
`file://…` or `s3://…` — so a row says which store wrote it.

The schema is the **system of record**, and it became more so. The CR is
ephemeral and goes away by TTL, the pod's report lives in the controller's
memory between the webhook and the forwarding, and the artifact store holds
content without relationships. Two facts that used to live in that store are now
columns here — the prompt and the retry checkpoint — because both are facts
about a run, and a run's facts belong where the rest of them are.

---

## 2. The source of truth and applying migrations

The migrations are ordinary SQL under goose, baked into the binary through
`go:embed` ([db/migrations.go](../../db/migrations.go)). The schema the build
applies is the one the build was tested against; a directory mounted next to the
binary is something the operator can mix up.

Numbering is sequential (`0001`, `0002`, …) rather than time-based. The review
order matches the application order, and that matters more than avoiding merge
conflicts in a team this size.

**Locking.** `Migrate` takes a `pg_advisory_lock` on a fixed identifier before
running goose. Without it, rolling out N replicas at once produces a race on
`CREATE TABLE`, N−1 replicas fail on "duplicate object", and it gets diagnosed as
a broken migration. The lock is session-scoped rather than transactional: a
migration with `NO TRANSACTION` (a concurrent index build, once the schema grows
into one) runs outside a transaction and would lose a transactional lock halfway.

**No extensions.** Not a single `CREATE EXTENSION`: an on-prem installation is not
guaranteed a role permitted to do that. `gen_random_uuid()` has been in the core
since version 13, and ULIDs are minted by the backend.

**Domains instead of `CREATE TYPE ... AS ENUM`.** `ALTER TYPE ... ADD VALUE`
cannot be used in the same transaction where the value was added — a data
migration becomes a two-release affair; an enum value cannot be removed at all;
and the ordering an enum brings with it is a trap here — the only significant
ordering in this schema (the phase rank) is not alphabetical and is set
explicitly.

---

## 3. Identifiers, money, time

**A ULID is stored as text, in the form it travels on the wire.** The `ulid`
domain checks the same regex as `components.schemas.ULID` in the OpenAPI. The
`uuid`/`bytea` option saves 10 bytes per row and costs a conversion at four
boundaries — the identifier in a log line, the `AgentRun` object name, the S3
prefix, and `sub` in the JWT. That is exactly where the identifier in the log
stops matching the identifier in the bucket. Crockford base32 sorts
lexicographically by minting time, so index locality is not lost, unlike with
uuidv4.

**`numeric(18,6)` was derived from the contract, not chosen.** The wire pattern
`^-?[0-9]{1,12}(\.[0-9]{1,6})?$` is exactly twelve digits before the point and
six after, that is, precision 18, scale 6. A test checks the derivation, so
widening the pattern without widening the column fails here rather than on the
first bill that does not fit. The schema does not accept a minus sign: a negative
self-declared spend is a broken or hostile pod, and it must fail on write rather
than quietly subtract from somebody else's run inside a `SUM`.

**`timestamptz` everywhere, and every deadline is computed from the server's
clock.** Cluster clocks are not synchronized; no ordering in this schema is built
on a time a cluster sent. The only column holding somebody else's time is called
`run_attempts.cluster_observed_at` — the name was chosen so that a query that
takes it into its head to order by it would look wrong.

---

## 4. Three groups of columns in `runs`, and who writes them

| Group | Columns | Written by | When |
|---|---|---|---|
| admission | `spec`, `prompt`, `prompt_sha256`, `agent`, `model`, `timeout_seconds`, `repo_*` | backend | once, at `INSERT` |
| ownership | `cluster_id`, `lease_epoch`, `attempt`, `ack_deadline`, `lease_deadline` | backend | lease, ack, heartbeat, expiry |
| observation | `status`, `observed_phase`, the result, the cost | backend, from the controller's reports | ingest |

**Admission is immutable, and a trigger checks it.** `spec` is rendered once and
handed out byte-for-byte on every lease — including a lease after reassignment.
Re-rendering between attempts would silently pick up an edited role, and attempt
2 would stop being a repeat of attempt 1 — exactly the property for which the CRD
makes `spec` immutable with a CEL rule and for which the image is pinned by
digest.

`spec` sits whole in `jsonb` rather than being shredded into columns: it is
already a versioned, additively evolving contract with its own schema and its own
drift test, and shredding it into forty columns would mean a migration for every
new optional field. Only the fields the UI filters and sorts by, and placement
selects by, are duplicated outward; the trigger freezes them together with `spec`,
so they cannot diverge.

---

## 5. The epoch rises on the revocation of ownership, not on issuance

Section 12.1 of the architecture showed `status=Leased, epoch++` at the moment of
the lease. That is wrong, and the schema and its queries correct it.

Section 3 of the Cluster API enumerates the events that raise the epoch:
`ackDeadline` expiry, a negative ack, an operator retry, reassignment. All of
them are **revocations of ownership**. Issuing work to a cluster that did not own
it yet revokes nothing.

The difference is not cosmetic. If the epoch rises on issuance, a run returned to
`Queued` sits in the queue with the very epoch the abandoned controller still
holds. A report from it would compare as **equal** and would be applied to a run
it no longer owns — precisely the zombie fencing exists to prevent, in the window
between the return to the queue and the next issuance. The window is longer the
longer no free cluster is available.

Raising it at the moment of revocation leaves no window. As a side effect it
makes the table in section 3 literally true: the first lease issues epoch 1,
because `lease_epoch` starts at one on `INSERT` and `lease.sql` reads it without
touching it.

The reverse asymmetry is in `expire_lease.sql`: there the epoch is **not** raised.
`leaseDeadline` is authoritative for the backend and informative for the
controller — one that has lost connectivity plays the work out and sends its
reports with its own epoch, and the run recovers by itself. Raising the epoch
here would turn a ten-second disconnect into a discarded hour of an agent's work.

---

## 6. `Unknown` is not a phase, and therefore has no rank

`status` and `observed_phase` are different columns, and the split was made for a
single transition: `Unknown → Running`.

`Unknown` means "the backend lost sight of the run". That is a statement about
visibility, not about the progress of the work, and it must be reversible once
the cluster comes back with a matching epoch. If `Unknown` occupied its own rank
in the monotonic sequence, a returning `Running` would either roll the state back
(a lower rank → `PhaseRegression`, and the run is stuck in `Unknown` forever) or
require an exception to the monotonicity rule — and an exception to the
monotonicity rule is precisely the bug that rule prevents.

The solution: `expire_lease` changes `status` and **does not touch**
`observed_phase`. The rank stays 30, a `Running` report from the returning
controller compares as equal and is applied as an idempotent repeat, clearing
`Unknown` along the way.

`observed_rank` is a generated column, a `CASE` over the phase. Generated so that
it cannot drift from `observed_phase`, the way every application-maintained
column drifts eventually. `ELSE 0` mirrors `Phase.Rank()` in `api/run/v1`: a
phase this build does not know about loses every comparison rather than moving
the run backwards. A test executes both copies and compares them.

---

## 7. The attempt key carries the epoch

```
PRIMARY KEY (run_id, lease_epoch, attempt)
```

Not `(run_id, attempt)`, as in the sketch in section 9.1 of the architecture.
`attempt` resets to 1 on every epoch increase, so under the shorter key the
second owner's first attempt collides with the first owner's first — and the row
it collides with is the accounting record of an attempt that has already spent
money.

The idempotency of `/ingest/completion` in section 6 of the Cluster API is
declared on the `(runID, attempt)` pair. After an epoch increase that pair is
ambiguous; the envelope carries the epoch too, and the epoch check happens
earlier in any case, so in practice the sides agree — but the key in the store is
obliged to carry all three.

`runs.cost_usd` is a maintained sum over `run_attempts`, not truth in its own
right. The unit of accounting is the attempt; "charged once" holds because the
row exists at most once per `(run, epoch, attempt)`.

### One live attempt per run, as a constraint

```sql
CREATE UNIQUE INDEX run_attempts_one_live ON run_attempts (run_id)
  WHERE finished_at IS NULL;
```

Preventing two live attempts is a constraint, not a convention. Kubernetes
already contributes `restartPolicy: Never` and `backoffLimit: 0`, so every
attempt is a Job some controller created deliberately — but `backoffLimit`
constrains one Job, not two controllers, and a rule that lives only in a
controller's code is a rule the *second* controller does not know about. A
constraint in the system of record is the only place it cannot be forgotten.

The controller is the monitor in the sense that it is the side that finds out.
The row is opened by the lease and by the first observation of a new attempt, so
a conflict comes back as a refusal it can act on. A conflict means somebody
else's attempt is still open, which is either a zombie controller — answered by
the epoch — or its own duplicate reconcile, which is answered by doing nothing.

This bounds duplicate *attempts*. It does not and cannot bound duplicate
*execution* across a fencing boundary; that is what the epoch is for, and the
honest formulation of section 7 of the architecture still stands: at-least-once
execution with converging side effects.

### Raising the epoch closes the open attempt

Without this the index above is a trap. A controller that vanished mid-run
leaves `finished_at IS NULL` behind, and the cluster the work is reassigned to
can never open its own row — the run becomes permanently unrunnable by the very
mechanism meant to keep it from running twice.

So every event that raises `lease_epoch` — ack expiry, negative ack,
reassignment, operator retry — finishes the outstanding attempt. It is a
trigger, `runs_close_attempt`, rather than a line in each of those statements,
for the same reason the rule above is a constraint: there are four call sites
today and the fifth is the one that forgets.

The accounting record survives, which is the point of keeping the epoch in the
attempt key: what is released is the claim, not the history of what was spent.
`failure_class` becomes `infra` because the attempt was ended by the platform's
fence rather than by anything the agent or the user did, and the reason names
the fence so that "why does this attempt say Failed when the run succeeded on
the next cluster" has an answer in the row itself.

### `completed_phases`: the checkpoint, as a column

`run_attempts.completed_phases` is `runtime_phase[]` and holds the entrypoint
phases an attempt got through. It is what used to be `runs/{id}/state.json`.

It moved here because it is a fact about the run's progress, and the run's
progress already lives in this schema — `observed_phase`, `observed_rank`, the
`phase` column beside it. Keeping a second copy in an object store meant two
writers for one piece of state, the thing D2 rejects for workflows, and it put
that store on the critical path of *starting* an attempt rather than finishing
one.

Two properties fall out that the object never had. The checkpoint survives the
pod's prefix being unreadable, because it never lived there. And it is visible:
`SELECT completed_phases FROM run_attempts` answers "how far did this get before
it died", which previously required fetching an object out of a bucket.

It is **unioned on write, never replaced**. Reports arrive reordered, and a
heartbeat carrying an earlier snapshot must not shorten a list the ingest path
already grew — which would hand the next attempt a checkpoint saying the model
had not run when it had. The read that assembles a lease's `completedPhases`
unions across every attempt of the current epoch, because that is what makes a
retry cheap: attempt 2 skips the model precisely because attempt 1 ran it.

---

## 8. What the schema guarantees structurally, and what stays in Go

The `runs_guard` trigger holds **assertions, not logic**. It fixes nothing and
fills in nothing except `updated_at`. It rejects writes that no correct path
produces:

| Rule | Line of the contract |
|---|---|
| `spec`, the prompt and the admission fields are immutable | CRD, section 4 |
| `lease_epoch` does not decrease | Cluster API, section 3 |
| `attempt` does not decrease within an epoch | Cluster API, section 3 |
| a terminal status changes only when the epoch or the attempt grows | Cluster API, section 5 |

The last rule is a precise retelling of two rows of the report application table:
"attempt greater than current → accept, reset phaseRank" and "attempt equal,
rank 40, a different terminal phase → the first one won". Without it a defect in
the ordering produces a run whose cost belongs to somebody else and whose PR link
points at somebody else's branch — and it gets noticed a week later.

**The decision table in section 5 stays in Go.** It is domain logic: it decides
what a run means, and it is what the ingest path and the heartbeat path are
obliged to share. A stored procedure would put the domain into the schema and
make every refinement of the rule a migration. The database gives something else:
a row lock (`db/queries/lock_run.sql`) that makes read-decide-write atomic, and a
trigger that rejects what the rule should not produce. There is no contention on
that lock — a run is owned by one cluster at a time; it orders the low-latency
ingest against a heartbeat carrying the same observation.

`audit_log` is immutable, through a trigger on `UPDATE`. `DELETE` is left open:
retention is a real operation with a boundary set by the chart, and an audit
record that can be edited in place is not an audit record.

---

## 9. The contract queries

The four queries are not ordinary data access but the shape that sections 4 and 5
of the Cluster API describe. They live in [db/queries](../../db/queries) next to
the schema they depend on, and the backend and the contract tests execute **the
same text**: a test that retypes the lease query is checking its own copy.

| Query | What in it is contract |
|---|---|
| `lease.sql` | `FOR UPDATE SKIP LOCKED`; the ordering matches the index; the epoch is read, not raised; cancelled runs are not handed out |
| `expire_ack.sql` | `→ Queued`, epoch +1 **in the same statement**, the assignment is preserved |
| `expire_lease.sql` | `→ Unknown`, the epoch is untouched, `observed_phase` is untouched |
| `lock_run.sql` | the single entry point of all three report application paths |

`SKIP LOCKED` is not an optimization. Two simultaneous polls are not a rare race:
a controller reconnects before the previous long poll has unwound, and a
controller rollout gives two replicas asking at the same moment. Without
`SKIP LOCKED` one blocks until the other commits and then receives the same run.

---

## 10. Indexes: all partial, and for one and the same reason

| Index | Path |
|---|---|
| `runs_lease_queue (cluster_id, priority DESC, created_at) WHERE status='Queued'` | leasing |
| `runs_needs_placement (created_at) WHERE status='Queued' AND cluster_id IS NULL` | cluster assignment |
| `runs_ack_expiry (ack_deadline) WHERE status='Leased'` | scanner 1 |
| `runs_lease_expiry (lease_deadline) WHERE status IN ('Dispatched','Starting','Running')` | scanner 2 |
| `runs_active_by_cluster (cluster_id) WHERE status IN (…non-terminal…)` | heartbeat reconciliation |
| `runs_cancel_pending (cluster_id) WHERE cancel_requested_at IS NOT NULL AND …` | cancellation delivery |

What is selective in each of these paths is the status, which almost every row
left long ago. A year of operation means millions of terminal rows; the queue and
the two scanners look at dozens. The composite `runs(status, ack_deadline)` from
the architecture sketch indexes those millions too.

There are two deadlines, so there are two indexes: their consequences are
opposite — one expiry returns the run to the queue, the other deliberately does
not — so they will never become a single query.

**There are no indexes on `clusters` at all**, apart from the keys. The table
holds dozens of rows, and `free_slots` and `last_heartbeat_at` are rewritten by
every heartbeat, once every ten seconds per cluster. An index on a column written
that often costs more than the scan it saves, and it would also break the HOT
update.

---

## 11. Secrets, tokens, idempotency

**Secrets come in two modes behind the `SecretResolver` port.** `managed`: the
value is encrypted by the application under the row's DEK, the DEK is stored
wrapped under a KEK that lives in an env var, a file or a KMS and **never appears
in the database**. The property being bought is narrow and worth naming plainly:
a database dump is not a credential leak. `referenced`: the value lives in the
customer's Vault or External Secrets, the database holds only a reference, and
the backend never sees the value. The mutual exclusivity and the completeness of
each mode are `CHECK`s, because a half-filled managed secret is a failure in the
pod at the `auth` phase, five minutes and one image pull after the mistake.

**There is not a single credential granting access INTO a cluster.** `clusters`
holds the public half of a pair the controller generated for itself, and the
`key_id` used to find the row when verifying a JWT. There is no private key and
there never will be — ADR 1 rests on that. Revocation is a status change and
takes effect within the token's lifetime (≤ 5 minutes); no revocation list is
needed.

The uniqueness of `(bootstrap_token_id, public_key)` is the idempotency of
`/register` exactly as section 6 of the Cluster API defines it. Without it a
controller that received a `clusterID` and crashed before writing it into its
Secret is incurable: the token is spent and there is no identity.

**Tokens are stored as digests only.** The `run-mcp` class is the one that makes
child runs accountable: the token carries a `run_id` and dies with the run, so a
`run_agent` arriving from inside an agent is bound to its parent, checked against
the depth limit and charged to the right budget. A global token makes all three
impossible at once.

`api_tokens.last_used_at` is written on authentication, which means every read of
this table is potentially a write. Update it no more than once a minute
(`WHERE last_used_at IS NULL OR last_used_at < now() - interval '1 minute'`): the
column exists so an operator can spot an unused token, and minute resolution
answers that question exactly as well as microsecond resolution. Nothing indexed
changes in the process, so the update stays HOT.

**The first row in `api_tokens` cannot come from the API.** Every endpoint needs
a bearer token and `POST /tokens` is admin-scoped, so a fresh installation has
no way in. The control-plane chart generates a token into a Secret and the
backend writes it at startup: `kind = 'user'`, `name = 'bootstrap'`,
`scopes = {admin}`, `created_by = 'bootstrap'`.

The write is keyed on `token_sha256`, never on the name, and that is the whole
of its semantics. A row that already exists is left exactly as it is — including
when it is revoked or expired, which is the case worth stating: an operator who
revoked the bootstrap credential did so because a real one now exists, and a
startup path that reinstated it would be a back door that reopens on every node
drain. Installing a replacement is therefore a *different token*, which is a
different digest and a new row, and the spent one stays spent. Nothing in this
schema makes `name = 'bootstrap'` unique, and nothing should: the spent rows are
the record of which credentials an installation has been through.

**`idempotency_keys` carries a digest of the body.** The same key with a
different body is a client defect, and answering with the first `run_id` would
return the result of work nobody ordered. That is a 422, and it is told apart by
this column.

---

## 12. There is no table of downward commands

Both Cluster API commands are functions of the current state:

- `cancel` — while `runs.cancel_requested_at` is set and the phase is not
  terminal;
- `abandon` — when a report arrived under a stale epoch, or a cluster mentioned a
  run it no longer owns.

That is also why the contract gives the commands no acknowledgements: an
acknowledgement would have to be stored and to expire. A command queue table would
add exactly the state that was decided against, plus the need to keep it in
agreement with `runs`. Cancellation is four columns in `runs` and a partial index.

---

## 13. What did not make it into phase 1

The schema covers `run_agent` end to end and nothing beyond it.

| Not created | When | Why not now |
|---|---|---|
| `workflows`, `workflow_versions`, `workflow_instances`, `workflow_steps` | phase 3 | the engine is a separate piece of work; the shape of a step depends on decisions about branch joins, and there is nothing to guess it from four months ahead |
| `schedules` | phase 4 | |
| `mcp_servers` | phase 2 | in phase 1 MCP servers are declared inside `roles.spec`; the registry appears together with the role resolution chain |
| `budgets` | phase 4 | `runs.max_cost_usd` covers the ceiling of a single run, and that is all phase 1 admission checks |
| users and RBAC | phase 4 | `api_tokens.scopes` is enough for the three v1 roles |
| `artifacts` | phase 3 | the key layout is derived from `runs.id`; enumerating artifacts and chunks is a `LIST` by prefix, not a table that must be kept in agreement with the bucket |
| partitioning `runs`, `audit_log` | by measurement | hundreds to thousands of runs a day; monthly partitioning is an additive migration, to be introduced on evidence rather than on a hunch |

`tenant_id` exists everywhere and takes part in no key and no index. A column
constant across all rows is dead weight in an index; adding it to the indexes
later is a migration, whereas adding it to every `INSERT` later is an edit to
every write path. So the column is there and there are no indexes on it.

---

## 14. Deltas to architecture.md

**The edits are applied** — the table is kept as a log: it explains why the
architecture document says what it says, and what to look at during review.

| # | What | Was (9.1, 12.1) | Became | Why |
|---|---|---|---|---|
| D1 | Raising the epoch | `status=Leased, epoch++` on issuance | on the revocation of ownership; issuance reads it | otherwise, between the return to the queue and the next issuance, an abandoned controller's report compares as equal (section 5) |
| D2 | The attempt key | `run_attempts(id, run_id, attempt)` | `PRIMARY KEY (run_id, lease_epoch, attempt)` | `attempt` resets when the epoch rises and collides with the previous owner's accounting |
| D3 | Status and phase | a single `status` column | `status` + `observed_phase` + a generated `observed_rank` | `Unknown` is about visibility, not about progress; with a rank of its own the `Unknown → Running` transition is unachievable |
| D4 | Indexes | `runs(cluster_id, status, priority, created_at)`, `runs(status, ack_deadline)` | the same paths, but as partial indexes on the status | terminal rows number in the millions over time, and dozens are actually looked at |
| D5 | Commands | a command queue was implied | no table; both commands are derived from the state | which is also why the contract gives them no acknowledgements (section 12) |
| D6 | The attempt report | columns for exit_code/cost | `completion jsonb` in full, plus fields promoted outward | `CompletionReport` is a contract with a schema of its own; shredding it means a migration for every new field |
| D7 | The negative ack | did not exist | `run_cluster_exclusions` with a code and a message | a consequence of Cluster API D16: exclude the cluster and show the reason, rather than accumulating the run in the queue forever |
| D8 | The cluster credential | `credential_hash` | `public_key`, `key_id`, `bootstrap_token_id`, and uniqueness of the pair | a consequence of Cluster API D1, materialized here |
| D9 | Money | `cost_usd` with no type | `numeric(18,6)`, derived from the wire pattern | precision and scale are not taste but a recomputation of `^-?[0-9]{1,12}(\.[0-9]{1,6})?$` |
| D10 | Identifiers | unspecified | a `ulid` domain over `text` with the same regex | the conversion at four boundaries is where the id in the log stops matching the id in the bucket |
| D11 | Idempotency | `Idempotency-Key` with no storage | `idempotency_keys` with a digest of the body | the same key with a different body must be a 422, not somebody else's `run_id` |
| D12 | Artifacts | `artifacts(id, run_id, kind, s3_key, …)` | no table in phase 1 | the keys are derived from `runs.id`; a table that must be kept in agreement with the bucket is a source of divergence, not an index |
| D13 | Invariants | entirely in code | four of them, through the `runs_guard` trigger | the decision table stays in Go, but its conclusions are checked on write (section 8) |
| D14 | The prompt | an object in S3, referenced by `prompt_sha256` | `runs.prompt`, 512 KiB, frozen with the admission group | an object store was a prerequisite for *starting* a run rather than for finishing one. The ceiling is real and is paid deliberately: the value has to fit in the per-run Secret beside three credentials, and a Secret is capped at 1 MiB |
| D15 | The retry checkpoint | `runs/{id}/state.json`, written and read by the pod | `run_attempts.completed_phases`, unioned on write | progress is already this schema's to hold; a second copy meant two writers for one fact. It is also queryable now — "how far did this get" is a `SELECT` |
| D16 | Duplicate attempts | a convention plus `backoffLimit: 0` | `run_attempts_one_live`, a partial unique index, plus `runs_close_attempt` | `backoffLimit` constrains one Job, not two controllers. The trigger is what keeps the index from becoming a trap that makes a run permanently unrunnable |
| D17 | Where the result is | implied to be S3 | `runs.result_ref`, a URI carrying its scheme | an installation that switches artifact modes keeps its old runs readable instead of orphaning them |

---

## 15. Contract test checklist

Implemented in [test/store](../../test/store), run by `make db-test` against a
real PostgreSQL — generated columns, domains, partial indexes,
`FOR UPDATE SKIP LOCKED` and the trigger live in the database and nowhere else.

- [x] every domain's values match the enum in `api/cluster/v1/openapi.yaml`, in both directions
- [x] every controller phase has a corresponding `run_status`
- [x] `observed_rank` from the schema matches `Phase.Rank()` from `api/run/v1` for every phase, and 0 for an unobserved one
- [x] the `ulid` domain accepts and rejects exactly what the wire pattern does, including I, L, O and U
- [x] the precision and scale of `money_usd` are derived from the `MoneyUSD` pattern; the maximum permitted value survives a round trip without rounding; a negative one is rejected
- [x] eight simultaneous polls of one cluster drain the queue without a single duplicate
- [x] the first lease issues epoch 1
- [x] `ackDeadline` expiry → `Queued`, the epoch raised **in the same statement**, the deadlines cleared, the assignment preserved
- [x] `leaseDeadline` expiry → `Unknown`, the epoch untouched, the rank still 30
- [x] `runs_guard`: an epoch regression, an attempt regression, a second terminal phase, an edit to `spec`, an edit to the prompt or its digest — all rejected; an attempt reset under a new epoch, an attempt increase out of a terminal state, an operator retry — all accepted
- [x] `run_attempts_one_live` refuses a second open attempt for one run, and permits one after the first has finished
- [x] raising `lease_epoch` by any path — ack expiry, negative ack, reassignment, operator retry — closes the outstanding attempt with `failure_class = 'infra'`, keeps its accounting, and lets the next owner open a row
- [x] `completed_phases` is unioned rather than replaced: an observation carrying a shorter list does not shorten what is stored
- [x] the checkpoint a lease carries unions across every attempt of the current epoch, and is empty for a new epoch
- [x] `prompt` over 512 KiB is refused by the check constraint, and admission refuses it first with a 413 naming the limit
- [x] `result_ref` round-trips both schemes
- [x] `(run_id, lease_epoch, attempt)` separates the attempts of two owners, and the total cost adds up
- [x] `audit_log` cannot be edited but can be deleted by retention
- [x] `idempotency_keys` is separated by scope, and a different body gives a different digest
- [x] the migrations apply from scratch, roll back to an empty database and apply again
- [x] `Migrate` is idempotent (every replica calls it at startup)
- [x] every contract query `PREPARE`s against the schema it ships with

Not covered, and cannot be covered here: the decision table in section 5 of the
Cluster API. It lives in the backend
([backend/run/report.go](../../backend/run/report.go)) and is tested against
`FakeController` in [test/backend](../../test/backend) — the schema only checks
that its conclusions do not produce impossible states.

One thing the backend does that this schema cannot express, and which is worth
finding here rather than in the code: `CompletedWithoutResult` is not a value of
the `run_status` domain and never will be. A terminal run whose report has not
arrived is stored as the terminal status it observed, with
`completion_received_at` still NULL, and the composite name is derived on the
way out. It is the absence of a collected result, not a state of the run, and
giving it a value of its own would put a run into a status no phase maps back
from.

---

## 16. Deferred

| # | Question | Decision | Condition for revisiting |
|---|---|---|---|
| 1 | Partitioning `runs` and `audit_log` | Not doing it. The order of the load is hundreds to thousands of runs a day | A measured `VACUUM` time or index size; monthly partitions are an additive migration |
| 2 | A separate queue (NATS/Redis) | Not doing it. `SKIP LOCKED` gives a correct queue on the same Postgres and adds no component to the chart | A measured throughput ceiling, not a hunch |
| 3 | KEK rotation | The `kek_id` column is in place, the re-encryption procedure is not | A customer's key rotation policy |
| 4 | A read replica for the UI | Not in v1 | When the run list starts to get in the way of the lease path — and it runs on different indexes, so not soon |
| 5 | Cost accounting on the LLM proxy | Outside the bounds of v1. `declared_duration_ms` and `observed_duration_ms` sit side by side, and a divergence is written to the audit log | A billing requirement for which self-declaration is not enough |
