# How to diagnose a failed run

A run ended and you need to know why, and whether to retry it. The two
questions are answered by different fields, and the order below is the fastest
route to both.

## Start with the exit code and the failure class

```sh
curl -s https://haliphron.example.com/api/v1/runs/$RUN_ID \
  -H "Authorization: Bearer $TOKEN"
```

Four fields carry the answer:

| Field | What it tells you |
|---|---|
| `exit_code` | what the pod did |
| `failure_class` | whether the cluster retried it, and whether you should |
| `observed_phase` | how far it got |
| `status_reason`, `status_message` | the specific cause |

The failure class decides your next move:

| Class | Retried in the cluster | Your move |
|---|---|---|
| `config` | no | fix the configuration, then retry |
| `agent` | no | read the result and the logs; retrying replays the same prompt |
| `budget` | no | raise the ceiling, or accept the outcome |
| `infra` | yes, already | check whether attempts were exhausted |
| `git` | yes, already | check the forge; the result is already saved |

Look up the exit code in the [exit code
reference](../reference/statuses-and-exit-codes.md#exit-codes).

## Read the attempt ledger

One run can have several attempts, and the interesting one is rarely the last.

```sh
curl -s https://haliphron.example.com/api/v1/runs/$RUN_ID/attempts \
  -H "Authorization: Bearer $TOKEN"
```

Each row carries `attempt`, `epoch`, `cluster_id`, `job_name`, `pod_name`,
`node_name`, the exit code and the cost.

Read it this way:

- **`attempt` climbing, `epoch` flat** — the cluster retried locally. An
  `infra` or `git` failure it was allowed to handle.
- **`epoch` climbing** — the control plane took ownership back and reassigned
  the work. The cluster stopped reporting, or never acknowledged the lease.
- **`cluster_id` changing** — it moved between clusters. Compare the failures;
  one cluster failing where another succeeds is an infrastructure fault, not
  an agent one.
- **`declared_duration_ms` far below `observed_duration_ms`** — the pod
  finished long before the lease did. The report was delayed, not the work.

Each attempt has its own cost. A run retried three times was paid for three
times.

## Then read the logs

```sh
curl -s "https://haliphron.example.com/api/v1/runs/$RUN_ID/logs" \
  -H "Authorization: Bearer $TOKEN"
```

Fetch the chunks by their `url`. Chunk numbering continues across attempts, so
one listing covers the whole run.

Everything uploaded has been through the redactor. A secret missing from the
logs is not evidence that it was missing from the run.

## Work through the common causes

### Exit 30, class `config`

The run was misconfigured and refused before anything cost money.
`status_message` names what was missing.

Usually a missing model key or git token — see [how to give runs a git token
and a model key](provide-run-credentials.md) — or MCP servers that did not
come up, caught by the `mcp-verify` phase.

### Exit 20, class `git`

Clone, push or pull request failed. **The result is already in storage**: the
`persist` phase runs before the git phases, so the model was paid for once and
the work is not lost.

```sh
curl -sL https://haliphron.example.com/api/v1/runs/$RUN_ID/result \
  -H "Authorization: Bearer $TOKEN"
```

A 403 on push usually means a protected branch or a token without permission
on that repository. That is a configuration problem wearing a git exit code; a
retry will discover the same answer.

### Exit 11, status `TimedOut`

The entrypoint killed the agent at its own timeout. The work up to that point
is kept. Raise `timeout_seconds` on resubmission if the task genuinely needs
longer.

### Exit 12, class `agent`

`output.json` is missing or fails the declared schema. The agent ran; it did
not produce what was asked for.

### Exit code above 128

The platform stopped the process. 137 is SIGKILL — OOMKilled or eviction; 143
is SIGTERM — a drain or preemption. Both classify `infra` and are retried.

A repeated 137 means the pod needs more memory. Raise `resources.memory` on
the role.

### `status` is `CompletedWithoutResult`

The run reached a terminal phase and its completion report never arrived. Its
recorded cost is zero, and that zero is not the truth.

Check whether the cluster is still reporting:

```sh
curl -s https://haliphron.example.com/api/v1/clusters \
  -H "Authorization: Bearer $TOKEN"
```

### `status` is `Unknown`

The cluster holding the run stopped reporting. This is a statement about the
channel, not about the run — the work may well be continuing. A returning
controller clears it in one heartbeat.

### The run sits in `Queued`

Nothing has leased it. Either no cluster is `Active`, every cluster is at
capacity, or no cluster matches the role's `clusterSelector`.

```sh
curl -s https://haliphron.example.com/api/v1/clusters \
  -H "Authorization: Bearer $TOKEN"
```

Compare `free_slots` and `quota_exhausted` against the run's role.

## Go into the cluster if you have to

Everything above works without cluster access, and should be exhausted first.
When you do need to look:

```sh
kubectl -n haliphron-agents get agentrun $RUN_ID_LOWERCASED -o yaml
kubectl -n haliphron-agents describe job $JOB_NAME
kubectl -n haliphron-agents logs $POD_NAME
```

The run identifier is lowercased to become a Kubernetes object name. `job_name`
and `pod_name` come from the attempt ledger.

Everything that had to survive a restart is in the CR's `status`; anything that
arrived from the control plane, including cancellation, is an annotation.

A finished run's objects are kept for `agent.ttlSeconds` — 24 hours by default
— and then removed.

## Retry it

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs/$RUN_ID/retry \
  -H "Authorization: Bearer $TOKEN"
```

This raises the epoch and issues a fresh lease. Do not use it for `infra` or
`git` failures without first checking the ledger — those were already retried,
and if they exhausted their attempts the cause is not transient.
