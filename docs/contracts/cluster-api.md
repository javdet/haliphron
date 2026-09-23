# Cluster API — semantics

Status: ready for implementation
Date: 2026-09-16
Message shape: [api/cluster/v1/openapi.yaml](../../api/cluster/v1/openapi.yaml)
Basis: [architecture.md](../architecture.md), sections 7, 12, 13.2

The OpenAPI describes **what is transmitted**. This document describes **what it
means and how to behave** — the rules that do not fit into a schema: fencing,
deadlines, monotonicity, idempotency, behavior under failure. The two sides are
written in parallel and independently, and they always diverge exactly here
rather than in the shape of the fields.

Section [12](#12-deltas-to-architecturemd) lists the changes this work
introduced into the architecture document; deltas D15–D17 came later, out of the
work on the [CRD contract](agentrun-crd.md). Section [14](#14-deferred) is what
we decided not to do in v1.

---

## 1. Contract boundaries

| What | Where |
|---|---|
| backend ↔ controller | **this contract** |
| controller ↔ kubernetes | [CRD `AgentRun`](agentrun-crd.md), contract 2 |
| controller ↔ agent pod | [the agent image runtime contract](agent-runtime.md), contract 3 (environment variables, exit codes, `/completion`) |
| client ↔ backend | the external REST `/api/v1`, a separate contract |

The Cluster API listens on a **separate port, `:8082`**, and is not exposed
through the ingress when the clusters are inside the perimeter. It is not the
same listener as the public API: they have different authentication models,
different consumers and different reasons to be reachable from outside.

Naming convention: machine contracts (the Cluster API, the CRD, the pod webhook)
use **camelCase**, the public REST uses **snake_case**. The rule is arbitrary but
uniform; at the moment `architecture.md` mixes them inside a single contract, and
that is a guaranteed source of translation bugs.

---

## 2. Authentication

### Why the controller issues its own tokens

The backend stores **only the public key**. The private Ed25519 key is generated
by the controller inside the cluster on first start, written to a Secret, and
never leaves.

The alternative — a shared secret or a refresh token from which the backend
issues access tokens — would give the control plane the ability to issue a valid
token on behalf of any cluster. For a system whose entire threat model is built
on "the control plane has no access inside the cluster" (ADR 1), that is a
contradiction: a compromised backend gets the clusters back.

With self-signed JWTs, compromising the backend gives access to what the backend
already knows, and gives no ability to impersonate a cluster.

### Checks on the backend side

| Check | Failure |
|---|---|
| the signature, by the `kid` from the header | 401 `Unauthenticated` |
| `aud` = `haliphron-cluster-api` | 401 `Unauthenticated` |
| `exp` ≤ `iat` + 300 s, with a 60 s clock tolerance | 401 `Unauthenticated` |
| the cluster exists and is not revoked | 401 `ClusterRevoked`, action=`fatal` |
| `sub` == the `clusterID` in the path / in the body | 403 `ClusterMismatch` |

Revoking a cluster is a status change in the database and takes effect within the
token's lifetime (≤ 5 minutes). A separate revocation list is unnecessary: the
cluster's row is read on every request anyway.

`jti` in the token is mandatory, but the replay check is not performed in
phase 1: a five-minute window under TLS does not justify a cache on the backend
side. The field is in place so the check can be turned on without changing the
contract.

### Clock skew

`/register` returns `serverTime`. The controller is obliged to compare it with
its own clock at startup and **log an error** for a divergence greater than 30
seconds. Otherwise clock skew shows up as random 401s under load — half a day of
diagnostics.

---

## 3. Epoch: the one fencing mechanism

```
epoch is about ownership of the work.  Raised only by the backend, on every issuance.
attempt is about attempts within an ownership. Raised only by the controller.
```

They get confused, hence: **the epoch does not grow on a retry**, and `attempt`
does not grow on reassignment to another cluster.

| Event | epoch | attempt |
|---|---|---|
| the first lease issuance | 1 | 1 |
| `ackDeadline` expired, the work returned to `Queued` | +1 | reset to 1 |
| the controller repeats an `infra` failure locally | unchanged | +1 |
| an operator pressed retry in the UI (`POST /runs/{id}/retry`) | +1 | reset to 1 |
| the cluster disappeared, the run is `Unknown`, an operator reassigned it | +1 | reset to 1 |

**The invariant.** The backend rejects any message whose epoch is lower than the
current one for that run, with 409 `EpochMismatch` and `action: abandon`. A
message with an epoch **higher** than the current one is a defect: the backend
never issued such an epoch, and the answer is 400 `InvalidRequest`.

This is the protection against "zombies": a controller that comes back after a
disconnect, and whose work has meanwhile been reassigned, receives `abandon` and
cleans up after itself instead of appending statuses to somebody else's run.

---

## 4. The lease: two deadlines, not one

`architecture.md` speaks of a single `lease_deadline`. It is not enough: before
the `ack` and after it the risks are directly opposite.

| Deadline | Default | Meaning | Expiry |
|---|---|---|---|
| `ackDeadline` | 60 s | the controller did not manage to create the Secret/ConfigMap/CR | **the work is guaranteed not to have started** → epoch +1, `Queued`, safe to reassign immediately; the fifth such expiry of one run → epoch +1, `Failed` (`AckTimeoutExhausted`) |
| `leaseDeadline` | 120 s, extended by every heartbeat | the controller stopped reporting | the Job may be executing right now → `Unknown`, **do not reassign automatically** |

A single shared deadline would force a choice: either a short one (and then a
slow `ImagePullBackOff` looks like losing the cluster) or a long one (and then a
controller that crashed before creating the Job keeps the work idle for minutes).

### Ack timeouts are bounded

A negative ack ends by itself: every refusal excludes the cluster, and once every
eligible cluster has refused, the run fails with `NoEligibleCluster`. An ack
timeout has no such brake. It keeps the assignment and excludes nothing, so a
controller that materialises the lease and whose ack never reaches the backend
(a proxy dropping the Cluster API, a load balancer cutting the long poll) would
be handed the same run every `ackDeadline`, indefinitely, with a per-run token
minted for every lease.

The backend therefore counts unacknowledged expiries per run. The one that
reaches the ceiling (`HALIPHRON_MAX_ACK_EXPIRIES`, default **5**) fails the run
instead of requeueing it: `Failed`, class `infra`, reason `AckTimeoutExhausted`.
The epoch still rises, so the controller holding the last lease gets `abandon`
on its next message. An operator's retry resets the count. A restart between
lease and ack costs one expiry, a rolling update two or three; five is beyond
anything transient, and at the default timings it gives up after about five
minutes. Every expiry revokes the per-run token its lease carried: nothing
started before the ack, so no pod ever held it.

### The expiry of `leaseDeadline` does not oblige the controller to stop

A deliberate asymmetry. `leaseDeadline` is authoritative **for the backend** (a
basis for ceasing to regard the work as belonging to the cluster) and merely
informative **for the controller**.

A controller that has lost connectivity plays the work it took through to the end
and accumulates reports — the availability decoupling principle (P4) and ADR 6.
When connectivity returns:

- the epoch matches → the reports are accepted and the run recovers by itself;
- the epoch is stale → `abandon` and local cleanup. Tokens have been spent, and
  the PR may have been opened twice; convergence is provided by the deterministic
  branch name and `create-or-update` (ADR 6, 23).

The opposite behavior — "the lease expired, kill the Job" — is worse: it turns a
ten-second network glitch into the loss of an hour of an agent's work.

---

## 5. Report monotonicity

At-least-once delivery with retries and backoff means reports arrive reordered. A
`Running` that arrives after `Succeeded` must not resurrect a run.

**Ordering is by `(attempt, phaseRank)`, not by time.** Cluster clocks are not
synchronized, and ordering cannot be built on them. `observedAt` in the
observations is for diagnostics only.

```
phaseRank:  Pending 10  →  Starting 20  →  Running 30  →  terminal 40
            Succeeded | Failed | TimedOut | Cancelled — all 40
```

The algorithm for applying a report `(runID, epoch, attempt, phase)`:

| Condition | Action | Answer |
|---|---|---|
| epoch < current | ignore | 409 `EpochMismatch`, action=`abandon` |
| epoch > current | ignore | 400 `InvalidRequest`, action=`fatal` |
| attempt < current | ignore | `accepted: false`, code=`AttemptRegression`, action=`abandon` |
| attempt > current | accept, **reset phaseRank**, write `run_attempts` | `accepted: true` |
| attempt = current, rank > current | accept | `accepted: true` |
| attempt = current, rank = current, the same phase | no-op (an idempotent repeat) | `accepted: true` |
| attempt = current, rank = 40, a **different** terminal phase | **the first one won**, write to the audit log | `accepted: false`, code=`RunTerminal`, action=`abandon` |
| attempt = current, rank < current | ignore | `accepted: false`, code=`PhaseRegression`, action=`retry` |

`PhaseRegression` is `retry`, not `abandon`: it is almost always a reordering
within a batch rather than a loss of ownership. The controller simply sends the
current state with the next heartbeat.

### The same path for the heartbeat and for `/ingest/status`

`runs[]` in the heartbeat and `reports[]` in `/ingest/status` are processed by
**the same code**. `/ingest/status` is the low-latency path and the heartbeat is
the periodic reconciliation; they must not diverge in their rules, or the
heartbeat will start rolling back state delivered by ingest.

### Backend statuses versus CR phases

The controller reports only what it sees in the cluster. `Queued`, `Leased`,
`Dispatched` and `Unknown` are backend states, and the controller never names
them.

| Phase from the controller | Run status |
|---|---|
| `Pending` | `Dispatched` |
| `Starting` | `Starting` |
| `Running` | `Running` |
| `Succeeded` / `Failed` / `TimedOut` / `Cancelled` | the same name |
| — | `Unknown` — set only by the backend, on lease expiry or from `unknownRuns` |

---

## 6. Idempotency

| Call | Idempotency key | A repeat returns |
|---|---|---|
| `/register` | (bootstrapToken, publicKey) | the same `clusterID` and the same response |
| `/leases` | none, and there cannot be one | **different** work — this is not a repeat but a new request |
| `/leases/{id}/ack` | (runID, epoch) | 200, the state does not change; a repeat with a different `accepted` does not replay the first |
| `/leases/{id}/artifacts` | none | a new bundle with a new expiry — the call is deliberately not idempotent |
| `/heartbeat` | (runID, epoch, attempt, phase) per row | the current leases and commands |
| `/ingest/status` | the same, per row | per-row results |
| `/ingest/completion` | (runID, attempt) | 200, `duplicate: true` |
| `/ingest/artifacts` | (runID, key) | 200, `duplicate: true` — the same bytes under the same key changed nothing |

The idempotency of `/register` keyed on the pair is not decoration. A controller
that received a 200 and crashed before writing the `clusterID` into its Secret
would otherwise be incurable: the token is spent and there is no identity, and a
human with a UI is required. With the binding to the key it simply repeats the
call after a restart.

---

## 7. Errors → controller behavior

The `action` field in `Problem` exists so that the controller **does not infer
behavior from the response code**. That is the most common divergence between two
independently written sides: one treats a 409 as grounds to retry, the other as
grounds to give up.

| action | What the controller does |
|---|---|
| `retry` | exponential backoff with jitter, base 1 s, ceiling 60 s, indefinitely |
| `backoff` | the same, but not before `Retry-After` |
| `abandon` | cancel the Job, delete the CR (the Secret, ConfigMap and Pod go with it by ownerReference), **report nothing further** about this run |
| `resync` | the next heartbeat with `reportComplete: true` |
| `reregister` | the credential is unusable: stop polling, raise an event for the operator |
| `fatal` | stop the operation, record an event, the `reconcile_errors_total` metric. A repeat will not help |

Additionally, regardless of `action`:

- **A 204 on `/leases`** is not an error. Poll again immediately, **without**
  backoff. Backoff here would turn a long poll into polling.
- **A dropped long-poll connection** is expected (proxies, backend restarts).
  Repeat at once, but with storm protection: no more than once per second.
- **A network error on `/ingest/status` or `/ingest/completion`** — an
  in-memory queue plus duplication into the heartbeat. Losing a report is not
  critical: the heartbeat will deliver the same state an interval later, and the
  result is already durable.
- **A network error on `/ingest/artifacts`** is the one exception, and it is
  not queued in memory. The object stays in the controller's spool and is
  retried until the backend acknowledges it, because there is no second channel
  that carries it and the controller is holding the only copy. That ordering —
  acknowledge, then delete — is what makes the relay at-least-once.

### Double delivery of the result

`/ingest/completion` and the terminal status are independent channels. The
backend is obliged to work under any combination:

| What arrived | State |
|---|---|
| completion + status | normal |
| status only | `CompletedWithoutResult`: the backend reads `runs/{runID}/` back out of the artifact store itself |
| completion only | the status is awaited; on lease expiry — `Unknown` with the contents already known |
| nothing | lease expiry → `Unknown` |

This is a direct consequence of ADR 15: the result is durable in the data plane
**before** the callback, so the callback is an optimization, not a correctness
condition. "Durable" is the controller's spool in relay mode and the object store
in the other; the rule never required a bucket, and reading it as though it did
is what made one look mandatory.

**Artifacts are forwarded before the completion that names them.** The
controller's flush order is artifacts, then completions, then observations. A
report forwarded first would leave a window in which the control plane holds a
result pointing at objects it does not have, and the `CompletedWithoutResult`
recovery would read that window as a lost result.

---

## 8. State reconciliation

A mechanism that is absent from `architecture.md`, and without which the loss of
a CR is cured only by a ninety-second `staleAfter`.

In the heartbeat the controller sends **all** the non-terminal runs it owns and
sets `reportComplete: true`. The backend can then interpret the absence of an
entry. Without the flag two cases are indistinguishable:

- "I do not have this run" — which requires a reaction;
- "I have not told you about it yet" (the controller has just come up and its
  informer cache is not warm) — which requires silence.

The backend answers with `unknownRuns` — the runs it considers active on this
cluster and the controller did not mention. The controller checks and either
sends the phase or acknowledges the loss, and then `Unknown` is set at once.

The scenario this was built for: the controller was moved with the loss of its PV,
or the agent namespace was recreated. All the CRs are gone. Without the
reconciliation the backend waits out `staleAfter` for every run and gets a batch
of `Unknown`s for manual triage. With it, it finds out in a single heartbeat.

The first heartbeat after startup must be sent with `reportComplete: false` until
the initial CR list has completed.

---

## 9. Secrets in the channel

The lease is the only place in the system where secret material travels over the
network in the clear. The backend cannot create a Secret in the cluster
(pull model), so the controller creates it from what it received.

Requirements, mandatory to check in code review on both sides:

1. `Cache-Control: no-store` on the `/leases` and `/leases/{id}/artifacts`
   responses.
2. The bodies of those responses are **not logged** at any logging level. Only
   `runID`, `epoch` and the number of leases are logged.
3. Tracing does not write the body into span attributes.
4. **No value from `secrets` or `artifacts` ends up in the CR's `spec`.** This is
   checked by an admission policy: the `get agentruns` permission must not grant
   the ability to read tokens.
5. The per-run Secret gets an `ownerReference` to the CR. No long-lived secrets
   remain in the agent namespace.
6. Presigned links are secrets too: they are a bearer capability to write into
   somebody's prefix. Their place is in the Secret, not in the CR and not in
   environment variables that show up in `kubectl describe`. They exist only in
   object-store mode; in relay mode there is no signature in the lease at all.
7. **The prompt is not a credential and is handled like one anyway**, for points
   1–3. It is the customer's text: no-store, never logged, never a span
   attribute. Where it differs from the rest is point 4 — it becomes exactly one
   environment variable, through `valueFrom.secretKeyRef` on a single named key
   of the per-run Secret. That is deliberate and is ADR 38: the prompt is the
   one value the agent is *meant* to read, and nothing is protected by
   withholding it from the process whose purpose is to act on it. What
   `secretKeyRef` buys over a literal is that the text stays out of the Job's
   spec and out of `kubectl describe pod`; what it buys over `envFrom` is that
   it names one key rather than spraying a Secret whose other keys are
   credentials.

The git token is hour-long and scoped to one repository (section 14 of the
architecture). It is the main compensating control in the absence of egress
filtering, and it is also what makes points 1–3 acceptable: the cost of a logging
mistake is bounded by an hour.

### What in the report cannot be trusted

`usage` in `CompletionReport` is assembled by the **pod** — the least trusted
component in the system. Cost and tokens are self-declared. A compromised agent
can understate its spend and bypass the budget.

The phase 1 mitigation: the backend compares `usage.durationMs` with the Job
duration observed by the controller and writes a divergence beyond a threshold
into the audit log. The full solution — accounting on the LLM proxy side — is
outside the bounds of v1.

---

## 10. Limits

| Quantity | Value | Why |
|---|---|---|
| request body | 1 MiB | `summary` is up to 64 KiB, the rest are references |
| `/ingest/artifacts` body | 256 MiB | the one endpoint whose body is an object rather than a message |
| artifact bytes per run | 1 GiB, configurable | enforced by the controller, so an over-large upload is refused one hop from the pod rather than after crossing the network |
| `prompt` | 512 KiB | it has to fit in the per-run Secret beside three credentials, and a Secret is capped at 1 MiB across all its keys |
| `reports[]` | 100 | the ingest batch |
| `runs[]` in the heartbeat | 500 | the cluster capacity ceiling with room to spare |
| `capacitySlots` | 256 | declared at registration and in the heartbeat; a larger value is a 400 `fatal`. Below the heartbeat's `runs[]` limit, so a full cluster fits in one heartbeat |
| leases per poll | `min(freeSlots, 10, capacitySlots − held)` | the response size and the volume of secrets in one body; `held` is the backend's own count of the cluster's `Leased`, `Dispatched`, `Starting` and `Running` runs. `freeSlots` is the controller's word and the controller re-polls at once after any poll that returned work, so without the third term the cap bounds one answer and not how much of the queue one cluster takes. An undeclared `capacitySlots` (0) counts as 256 |
| `waitSeconds` | ≤ 30 | the ingress `proxy_read_timeout` must be **greater** |
| JWT TTL | ≤ 300 s | |
| `summary` | 64 KiB | duplicated in `runs.result_summary` |

`waitSeconds` ≤ 30 is operational caveat 4 of section 16: if the ingress cuts the
connection sooner, the controller gets disconnects instead of empty responses and
it looks like network instability.

---

## 11. Version compatibility

The control plane and the `haliphron-runtime` chart are installed **separately
and upgraded independently**. A version divergence is a normal state, not a
fault.

The rules:

1. `/cluster-api/v1` changes only additively. An incompatible change means `v2`
   and both versions running in parallel for the duration of the migration.
2. **Both sides ignore unknown fields.** Not "may" — must.
3. **Both sides ignore unknown enum values.** An unknown `Command.type` is a no-op
   plus a log entry, not a panic. Otherwise rolling out a control plane with a new
   command takes down every controller of the older version.
4. The backend declares `supportedControllerVersions` in `/register`; outside the
   range it answers 422 `UnsupportedControllerVersion` with `action: fatal`. At
   least N-1 minor versions of the controller are supported.
5. `X-Haliphron-Controller-Version` is mandatory on every request: without it, in
   a multi-cluster installation there is no telling which version sent what.

Points 2 and 3 cost one line of code each and remove a whole class of upgrade
failures. They are skipped just as regularly.

---

## 12. Deltas to architecture.md

Working through the contract changed some decisions. **The edits are applied** —
the table is kept as a log: it explains why the architecture document says what
it says, and what to look at during review.

| # | What | Was | Became | Why |
|---|---|---|---|---|
| D1 | The cluster credential | a long-lived credential from the backend, `clusters.credential_hash` | an Ed25519 pair generated by the controller; the backend stores `public_key`, `key_id` | the backend must not be able to impersonate a cluster — otherwise ADR 1 does not hold |
| D2 | The lease deadline | a single `lease_deadline` | `ackDeadline` (60 s) + `leaseDeadline` (120 s) | before the ack the work has not started and can be reassigned safely; after it, not |
| D3 | The prompt | an object in S3 read by presigned GET | `Lease.prompt` in the clear, into the per-run Secret, out as one env var | it put an object store on the path to *starting* a run. The Secret's 1 MiB cap is real and is answered with a stated 512 KiB ceiling and a 413 at admission, not by moving the value back out |
| D4 | The presigned bundle | PUT and GET | PUT and POST only, and only in object-store mode | the two objects the pod read — `prompt.txt`, `state.json` — are both gone from the store. That removes exit 21's most common cause and one whole class of "the run failed and the reason was a URL expiry" |
| D5 | Reissuing presigned links | none | `POST /leases/{runID}/artifacts` | a TTL of timeout × 2 does not survive 3 infra retries; the failure looks like a lost result on successful work |
| D6 | `/ingest/status` | one record | a batch of up to 100 | a reconcile burst produces dozens of events per second |
| D7 | State reconciliation | none | `reportComplete` + `unknownRuns` | the loss of a CR is otherwise cured only through `staleAfter` |
| D8 | Downward commands | `cancel` | `cancel` + `abandon` | "cancel yourself" and "this is no longer your work" require different behavior: in the second case there is nothing to report |
| D9 | Operational parameters | the cluster's values.yaml | issued in `/register` | otherwise `staleAfter` drifts across installations and does not match the server's |
| D10 | `callbackURL` in the CR | a CR field with no stated source | filled in by the controller | the value is cluster-local; the backend cannot know it |
| D11 | `attempt` | implicitly the backend's | the controller's counter, which the backend accepts as monotonically increasing | a local infra retry does not go to the backend for a number |
| D12 | Field names | `run_id` in 13.5, `runID` in 13.2 | camelCase in every machine contract | |
| D13 | Indexes | `runs(status, lease_deadline)` | + `runs(status, ack_deadline)` | two deadlines mean two expiry scanners |
| D14 | Capacity | `free_slots` | + `quotaExhausted` in the heartbeat | an exhausted ResourceQuota is otherwise visible only as a series of `Failed`s with `exceeded quota` |
| D15 | `roleConfig` | a `RenderedRunSpec` field | a `Lease` field, next to `secrets` and `artifacts` | it is material: the controller turns it into a ConfigMap and only the name rides in the CR. It has no place in `spec`, by the same rule as secrets |
| D16 | The ack | positive only | `accepted: false` + `rejection` | otherwise "I cannot materialize this" can only be expressed by a burned run: create the Job, let it fail, report `Failed` |
| D17 | The PUT bundle's keys | `output.json`, `result.md`, `state.json`, `logs/agent.log` | `output.json`, `result.md`, `completion.json`, `logs/agent.log` | the report lives in the controller's memory between the webhook and ingest; without a durable copy, losing it takes the cost and the PR link with it, and neither `result.md` nor `output.json` carries them. `state.json` is gone: the checkpoint is a column now |
| D18 | Artifact storage | one path, through S3 | a port with two modes; `/ingest/artifacts` is the relay's half | requiring MinIO to run one agent is a tax on every evaluation and every on-prem install. The mode rides in `ArtifactBundle.mode`, and an absent value reads as relay so a newer controller defaults to the path that needs no configuration |
| D19 | The retry checkpoint | `state.json`, read by the pod | `RunObservation.completedPhases` up, `Lease.completedPhases` down | the checkpoint is a fact about the run's progress, and the progress already lives in the control plane. Unioned on write, because reports arrive reordered and a stale heartbeat must not shorten what the fast path grew |
| D20 | `ObjectRef` | carried a `bucket` | key, digest and `uploaded` | which store holds an object is the installation's business; a pod that never learns a bucket name cannot leak one, and `uploaded` is what the backend checks before believing a ref |

---

## 13. Contract test checklist

Written before either side is implemented, and run against a fake of the other
side.

**Backend, against a fake controller** — implemented in
[test/backend](../../test/backend), run by `make backend-test` against
`FakeController` and a real PostgreSQL:

- [x] a repeat `/register` with the same key → the same `clusterID`; with a different key → 401
- [x] `/leases` on an empty queue → 204 after exactly `waitSeconds`
- [x] two concurrent leases for one cluster never hand out the same `runID` twice
- [x] `ackDeadline` expiry → epoch +1, `Queued`, the work is offered again **on the same cluster**
- [x] the fifth `ackDeadline` expiry of one run → epoch +1, `Failed`, class `infra`, reason `AckTimeoutExhausted`, no per-run token left live; an operator's retry resets the count
- [x] `leaseDeadline` expiry in `Running` → `Unknown`, **not** `Queued`
- [x] a report with a stale epoch → 409 `abandon`, the run's state unchanged
- [x] `Running` after `Succeeded` → `PhaseRegression`, the run stays terminal
- [x] two different terminal phases → the first wins, the second goes to the audit log
- [x] `attempt` +1 resets phaseRank and creates a `run_attempts` row
- [x] a repeat `/ingest/completion` → `duplicate: true`, the cost is charged once
- [x] a terminal status without a completion → `CompletedWithoutResult`, the contents lifted from the artifact store
- [x] a lease carries the prompt and a digest that matches it, and the digest in `spec` agrees with both
- [x] a prompt over 512 KiB is refused at admission with a 413 naming the limit
- [x] `/ingest/artifacts` stamps the run prefix from the authenticated envelope and refuses a key that tries to escape it
- [x] `/ingest/artifacts` under a stale epoch is refused with `abandon`, so a cluster that lost a run cannot overwrite the result of the one that holds it
- [x] a relayed body that does not match its `X-Haliphron-SHA256` is refused with `retry` and nothing is stored
- [x] `completedPhases` is unioned across observations rather than replaced, and a shorter later report does not shorten it
- [x] a lease in relay mode carries no bucket, no endpoint and no signature
- [x] `reportComplete: true` without a known run → it appears in `unknownRuns`
- [x] `reportComplete: false` without a known run → `unknownRuns` is empty
- [x] `cancel` is redelivered until the phase is terminal
- [x] the `/leases` response body is absent from the logs at debug level
- [x] `ack {accepted: false}` → epoch +1, `Queued`, this cluster excluded from selection for this run
- [x] `ack {accepted: false}` with only one cluster → `Failed`, class `config`, the reason visible in the UI
- [x] a terminal status without a completion but with `completion.json` in storage → the cost and `prURL` are lifted from it

Row 4 is the one that changed while it was being implemented. It read "the work
goes to another cluster"; `expire_ack.sql` keeps the assignment instead, and
the store contract states why: the ordinary cause of a missed ack is a
controller that was restarting, and sending every one of those through
re-placement moves work away from a healthy cluster on the strength of a
rolling update. Reassignment is still what a negative ack and an operator's
retry do — both consult the exclusion table — but it is not what a timeout
means. The test asserts the assignment is kept.

**Controller, against a fake backend** — implemented in
[test/controller](../../test/controller), run by `make controller-test`:

- [x] 409 `abandon` → the Job cancelled, the CR deleted, no further reports
- [x] an unavailable backend → the work taken is played out, reports accumulate and are sent later
- [x] 204 → immediate re-poll without backoff
- [x] a dropped long poll → a repeat no more than once per second
- [x] an unknown `Command.type` → a no-op, not a panic
- [x] an unknown field in `Lease` → ignored
- [x] a restart between the lease and the ack → a repeat ack with the same epoch
- [x] a restart after the ack → state restored from the CR, heartbeat with `reportComplete: false` until the informer is warm
- [x] the bundle's `expiresAt` earlier than the expected finish → reissued before the Job is created
- [x] values from `secrets` are absent from the created CR's `spec`
- [x] `roleConfig` from the lease ended up in the ConfigMap, and only its name in the CR
- [x] a spec some of whose fields this cluster's CRD prunes gives `ack {accepted: false, code: SpecFieldsPruned}` rather than a run with a silently lost setting

The two fakes — `FakeBackend` and `FakeController` — are part of the contract's
deliverable, not a test utility belonging to one of the teams. They are the only
thing that makes parallel development possible.

`DockerLauncher` (section 5 of the architecture) keeps the protocol under
constant load in local development: the controller leases work **by the same
path** and executes it through Docker. A divergence is found on one's own machine
rather than in a customer's cluster.

---

## 14. Deferred

The decisions are made and require no work in phase 1. The first three are
reflected in section 18 of the architecture document.

| # | Question | Decision | Condition for revisiting |
|---|---|---|---|
| 1 | Rotating the cluster key | Not doing it. A compromise is cured by revocation and reinstalling the chart | A `POST /clusters/{id}/credentials/rotate` endpoint is an additive contract change. Introduce it on the request of a customer with a key rotation policy |
| 2 | Deregistration on `helm uninstall` | Not doing it. `Unreachable` will happen by itself, and a `pre-delete` hook on release deletion is unreliable | If cleaning up the records by hand starts to annoy the operator |
| 3 | mTLS on top of JWT | Not in v1. A JWT on a key that never leaves the cluster gives the same origin guarantee, and distributing client certificates is a separate operational burden on the customer | A customer requirement with a hard perimeter |
| 4 | `abandon` permissions for deleting the CR | The controller deletes the CR itself. It is ephemeral and goes away by TTL in any case (ADR 4), and Postgres remains the system of record | — |
