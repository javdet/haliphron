# Statuses, phases and exit codes

The vocabulary a run is described in. Four separate sets, owned by four
different parts of the system, and they are not interchangeable.

| Set | Owner | Where it appears |
|---|---|---|
| [Backend statuses](#backend-statuses) | the control plane | `status` in the REST API |
| [CR phases](#cr-phases) | the controller | `observed_phase` in the REST API; `status.phase` on an `AgentRun` |
| [Runtime phases](#runtime-phases) | the agent pod | the run timeline, phase timings, the checkpoint |
| [Exit codes](#exit-codes) | the agent pod | `exit_code` in the REST API |

Defined in [`api/run/v1/phase.go`](../../api/run/v1/phase.go),
[`api/run/v1/runtime.go`](../../api/run/v1/runtime.go) and
[`db/migrations/0001_domains.sql`](../../db/migrations/0001_domains.sql).

---

## Backend statuses

The control plane's state machine. Four of these are backend-owned and cannot
be observed from a cluster; the rest are named identically to the CR phases
they are set from.

| Status | Owner | Meaning |
|---|---|---|
| `Queued` | backend | admitted, not yet leased to a cluster |
| `Leased` | backend | handed to a cluster, not yet acknowledged |
| `Dispatched` | backend | acknowledged, no phase reported yet |
| `Starting` | from the CR | the Job exists; the pod has not reached Running |
| `Running` | from the CR | the agent container is running |
| `Unknown` | backend | the holding cluster has stopped reporting |
| `Succeeded` | from the CR | the container exited 0 |
| `Failed` | from the CR | the container exited non-zero, or never ran |
| `TimedOut` | from the CR | the run exceeded its budget |
| `Cancelled` | from the CR | a cancel command was carried out |

`Unknown` is not a phase and has no rank. It is a statement about the
reporting channel, not about the run, and it is undone when the cluster
returns.

### `CompletedWithoutResult`

The REST API's `status` field can also read `CompletedWithoutResult`. It is
not a stored status: it is what the API reports when a run has reached a
terminal phase but its completion report has never arrived.

Such a run has a recorded cost of zero.

### Cluster statuses

A registered cluster is one of:

| Status | Meaning |
|---|---|
| `Registering` | the record exists; registration is not complete |
| `Active` | heartbeating |
| `Unreachable` | silent for longer than the stale threshold |
| `Revoked` | ended by an operator |

---

## CR phases

What the controller observes in the cluster, and the only phases it ever
names.

| Phase | Rank | Meaning |
|---|---|---|
| `Pending` | 10 | the `AgentRun` exists; no Job yet |
| `Starting` | 20 | the Job exists; the pod has not reached Running |
| `Running` | 30 | the agent container is running |
| `Succeeded` | 40 | the container exited 0 |
| `Failed` | 40 | the container exited non-zero, or the pod never got far enough |
| `TimedOut` | 40 | exit 11, or the Job's `activeDeadlineSeconds` backstop |
| `Cancelled` | 40 | a cancel command was carried out |

Reports are ordered by `(attempt, rank)` and never by time.

A phase this build has never heard of ranks 0, and so loses every comparison.

Rank 40 is terminal: no successor within the same attempt.

**`Succeeded` means the container exited 0 and nothing more.** It does not
mean the task was solved. See [what "Succeeded"
means](../explanation/what-succeeded-means.md).

---

## Runtime phases

The eighteen phases of the agent image entrypoint, in execution order. The
order is contract: the checkpoint's resume rule is "every phase before the
first unfinished one is done", which is only meaningful against a fixed
sequence.

| # | Phase | What it does |
|---|---|---|
| 1 | `init` | establishes identity and the writable layout |
| 2 | `validate` | checks configuration before anything costs money |
| 3 | `fetch` | reads the prompt and verifies it against its digest |
| 4 | `checkpoint` | reads which phases a previous attempt completed |
| 5 | `auth` | wires the model credential and the git credential helper |
| 6 | `clone` | clones the repository |
| 7 | `role` | resolves the role chain and intersects it with the policy ceiling |
| 8 | `mcp-prepare` | renders MCP configuration, referencing secrets rather than inlining them |
| 9 | `mcp-verify` | proves the MCP servers came up |
| 10 | `run` | the agent CLI, under the timeout |
| 11 | `parse` | normalises the runtime's output into `result.md` and usage |
| 12 | `output` | wraps the payload in the envelope and validates any declared schema |
| 13 | `persist` | uploads the result, output and log so far |
| 14 | `commit` | commits what the agent left uncommitted |
| 15 | `push` | pushes with `--force-with-lease` |
| 16 | `pr` | creates or updates the pull request |
| 17 | `finalize` | uploads the final log and the completion report |
| 18 | `notify` | posts the report to the controller |

`persist` runs **before** the git phases, so what the model produced is durable
before anything allowed to fail. See [what "Succeeded"
means](../explanation/what-succeeded-means.md#persist-before-git).

`notify` is last and cannot change the exit code.

### Phase outcomes

| Outcome | Meaning |
|---|---|
| `ok` | the phase ran and succeeded |
| `skipped` | the phase did not apply |
| `failed` | the phase ran and failed |

`skipped` is a first-class answer, not a synonym for failure. A run without a
repository skips four phases; a resumed attempt skips everything up to and
including `run`.

---

## Exit codes

Produced deliberately by the image entrypoint.

| Code | Name | Meaning | Failure class |
|---|---|---|---|
| 0 | success | | `none` |
| 10 | agent error | the agent CLI exited non-zero | `agent` |
| 11 | agent timeout | the entrypoint killed the agent at its timeout | `agent` |
| 12 | output invalid | `output.json` is missing or fails the declared schema | `agent` |
| 20 | git | clone, push or PR failed | `git` |
| 21 | storage | an artifact upload failed | `infra` |
| 30 | config | incomplete configuration, or MCP servers did not come up | `config` |

Codes above 128 are signals and are not produced by the entrypoint. 137 is
SIGKILL (OOMKilled, eviction); 143 is SIGTERM (drain, preemption). All of them
mean the platform stopped the process rather than the agent finishing badly,
so all of them classify as `infra`.

An unrecognised code below 128 is the agent's own and classifies as `agent`.

Exit 21 is `infra` in both artifact modes. See [artifact
modes](../explanation/artifact-modes.md).

### Exit code to phase

| Code | Terminal phase |
|---|---|
| 0 | `Succeeded` |
| 11 | `TimedOut` |
| anything else | `Failed` |

Cancellation is not derivable from the exit code — a cancelled pod is killed
and reports 143 like any other eviction. The controller overrides the mapping
with `Cancelled` when it was the one that asked for the kill.

---

## Failure classes

Which failures the controller may retry on its own, without asking the control
plane.

| Class | Retried locally | Meaning |
|---|---|---|
| `none` | — | no failure |
| `infra` | **yes** | the platform failed: eviction, storage, a node going away |
| `git` | **yes** | clone, push or PR failed |
| `agent` | no | the agent ran and did not succeed |
| `config` | no | the run was misconfigured |
| `budget` | no | the run exhausted a ceiling |

A retry inside one lease raises the **attempt**, not the epoch. See [leases,
epochs and attempts](../explanation/leases-epochs-and-attempts.md).
