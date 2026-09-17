# The controller

The only haliphron component that runs inside a customer's cluster. It pulls
work over the [Cluster API](../docs/contracts/cluster-api.md), materialises it
into the [`AgentRun` CRD](../docs/contracts/agentrun-crd.md) and the Job that
executes it, receives the pod's completion report, and tells the control plane
what it saw.

Nothing connects inward. The backend has no route into this process, no
credential for this cluster and no way to create an object in it. That is not a
deployment detail — it is the assumption the rest of the design is built on, and
every awkward thing here (the long poll, the epoch, the two deadlines, the
reports that queue during an outage) is what it costs.

## Layout

| Path | What |
|---|---|
| `clusterapi/` | contract 1 as a client: transport, token, and the action a failure carries |
| `identity/` | the Ed25519 pair, its Secret, and the one-time registration |
| `lease/` | the long poll, and making a lease durable before acknowledging it |
| `materialize/` | lease → Secret, ConfigMap, AgentRun, and the pruning read-back |
| `agentrun/` | the reconciler: the phase table, retries, cancellation, the finalizer, the TTL |
| `launcher/` | section 11 of the CRD contract, executable: the Job |
| `report/` | the outbound queue, the ingest path and the heartbeat |
| `callback/` | the endpoint the agent pod posts its report to |
| `config/` | what the chart sets, and what the control plane overrides at runtime |

The contract tests live in [test/controller](../test/controller); this module
does not depend on `fake`, so what ships to a customer cannot carry a test
double.

## The four rules

**Nothing starts before the acknowledgement.** The backend reassigns an
unacknowledged lease on the grounds that the work cannot have started. The Job
therefore waits for the ack, recorded as an annotation on the CR, and a run that
never gets one is discarded rather than launched. Without that, the one case
that matters — the ack was lost, the backend reassigned the run — ends with two
clusters running the same agent against the same branch.

**Reconciliation remembers nothing.** Everything that must survive a restart is
in the status; everything that arrives from outside, including the backend's
commands, arrives as an annotation. A cancellation held in a map would be
forgotten by a rescheduled pod, and commands have no acknowledgement by design,
so nobody would ever find out.

**A failure is acted on by its action, never by its status code.** Two
independently written sides always diverge on what a 409 means. `Problem.action`
is what removes the question, and [`clusterapi`](clusterapi/client.go) is the
only place that reads it.

**The lease body is never logged.** It is the one message in the system carrying
secret material in the clear: an hour-long git token, a model key and bearer
capabilities on a bucket prefix. There is no debug mode that would print it, and
[a test](clusterapi/client_test.go) says so.

## Configuration

Set by the chart, all prefixed `HALIPHRON_`:

| Variable | Meaning |
|---|---|
| `BACKEND_URL` | the control plane's Cluster API root |
| `BOOTSTRAP_TOKEN` / `BOOTSTRAP_TOKEN_FILE` | the one-time registration token; the file form keeps it out of `kubectl describe` |
| `CLUSTER_NAME`, `CLUSTER_LABELS` | the identity and the placement facts |
| `AGENT_NAMESPACE`, `NAMESPACE` | where runs go, and where the controller's own Secret lives |
| `CALLBACK_URL`, `CALLBACK_ADDR` | where pods report, and where that is served |
| `CAPACITY_SLOTS`, `RUNTIMES` | what this cluster will accept |
| `GRACE_SECONDS`, `DEADLINE_SLACK_SECONDS`, `STARTUP_DEADLINE_SECONDS` | the pod's shutdown budget, the Job's backstop, and how long a pod may fail to start |
| `PREFLIGHT_JOB` | dry-run the Job before acknowledging, to refuse quota and policy failures before anything is spent |

The intervals — heartbeat, lease TTL, ack timeout, long poll ceiling — are
**not** here. They come from `/register` and may be replaced by any heartbeat,
because a value that drifts between installations means no two clusters agree on
what "stale" means.

## Running it

```
make controller-test    # unit tests under -race, then the contract tests
make controller-build   # the binary
make generate           # the CRD and this controller's RBAC, from the markers
```

The generated `config/rbac/role.yaml` is a ClusterRole because that is what
controller-gen emits. The chart is expected to bind it as a **Role** in the
agents namespace and a second one in the controller's own: the permissions on
Secrets and ConfigMaps are namespace-scoped by design, and `pods/log` is absent
on purpose — the pod uploads its own logs, and the controller must not become a
log delivery channel by accident.
