# CRD `AgentRun` — semantics

Status: ready for implementation
Date: 2026-09-16
Shape: [api/run/v1](../../api/run/v1), [api/agentrun/v1alpha1](../../api/agentrun/v1alpha1),
manifest [config/crd/bases/haliphron.io_agentruns.yaml](../../config/crd/bases/haliphron.io_agentruns.yaml)
Basis: [architecture.md](../architecture.md), sections 7, 12.2, 13.4, 16;
[cluster-api.md](cluster-api.md)

The Go types describe **what is stored**. This document describes **what it
means and how the controller behaves**: how the phase is derived from the Job
and the pod, who is entitled to write what, what happens on cancellation, on a
restart and on a loss of connectivity, and why the boundary between `spec` and
materials is drawn where it is.

Section [16](#16-deltas) lists the deltas this work introduced into
`architecture.md` and into the Cluster API. Section [18](#18-deferred) is what
we decided not to do in v1.

---

## 1. Contract boundaries

| What | Where |
|---|---|
| backend ↔ controller | [Cluster API](cluster-api.md), contract 1 |
| controller ↔ kubernetes | **this contract** |
| controller ↔ agent pod | [the agent image runtime contract](agent-runtime.md), contract 3 |
| client ↔ backend | the external REST `/api/v1` |

The CR is not the system of record. The system of record is PostgreSQL.
`AgentRun` exists for four things, and not one of them is "hold the truth about
a run":

1. the controller survives its own restart without keeping local storage;
2. pod failures become an ordinary reconciliation task rather than separate code;
3. work already taken is played out while the backend is unavailable (P4, ADR 6);
4. `kubectl get agentruns` is real value for a platform team buying an on-prem
   product.

From which follows the main constraint: **everything the controller puts into
the CR must be either a specification received from above or an observation of
the cluster.** No domain state, no result contents, no logs.

---

## 2. The source of truth and code generation

```
api/run/v1              shared types: RenderedRunSpec, Phase, FailureClass, Usage
api/agentrun/v1alpha1   CRD: AgentRun = RenderedRunSpec + 4 controller fields
   ↓ controller-gen
config/crd/bases/haliphron.io_agentruns.yaml
```

The source is the Go types. They carry validation markers that do not and
cannot exist in OpenAPI, and both the deepcopy functions and the CRD manifest
are generated from them. `make generate` reproduces both, and `make verify`
fails CI if what is committed has drifted from the types.

`api/cluster/v1/openapi.yaml` stays hand-written: its prose is what the backend
team reads, and no generator will write it. Synchrony is held by the
`TestOpenAPIAndCRDAgreeOnRenderedRunSpec` test: it compares the schemas across
every key that affects whether a value is **accepted** (`type`, `enum`,
`required`, `default`, bounds, patterns) and ignores those that only affect
reading.

A divergence in the bounds is not cosmetic. A `maxLength` that exists in the CRD
and not in the Cluster API means a lease the backend issued and the controller
cannot materialize: the run dies at dispatch with no visible cause. That is
exactly the category the test found on its first run: 63 divergences, 60 of them
missing length and count bounds, plus `tolerations`, described in the Cluster
API in free form.

### Why `api` is a separate Go module

The backend imports `api/run/v1` and **does not acquire a dependency on
apimachinery**: there is not a single k8s type in that package, and resource
quantities are strings with a pattern rather than `resource.Quantity`. The
controller imports both packages. Backend tests that need to assert about the CR
import `agentrun/v1alpha1` — with apimachinery, but only in the tests.

There is no reverse dependency and there must not be: `api` knows nothing about
the backend or the controller. A module boundary makes that rule checkable by
the compiler rather than by agreement.

### What else lives in the shared package besides the shapes

Not just structures, but the rules both sides are obliged to agree on:

| What | Why not one copy each |
|---|---|
| `Phase.Rank()`, `IsTerminal()` | report ordering; once they diverge, a heartbeat starts rolling back state delivered by ingest |
| `FailureClassForExitCode()` | the controller decides whether to retry; the backend explains that in the UI |
| Secret key names (`git-token`, `llm-api-key`, …) | the backend puts them there and the controller reads them; a typo looks like "the agent silently had no token" |
| the storage layout (`runs/{id}/result.md`, …) | three components address it independently |
| `ObjectName`, `JobName`, `SecretName` | backend tests assert about the objects the controller builds |

An unknown phase gets rank 0 rather than a panic or the maximum: a newer
participant that sent a phase this build has not heard of must not be able to
move the run either forward or backward.

---

## 3. `spec` is the lease minus the materials

```
Lease = spec + materials
        │      └── secrets, artifacts, roleConfig
        └── RenderedRunSpec: what survives materialization
```

The controller adds exactly four fields to `RenderedRunSpec`, and all four are
things the backend cannot know:

| Field | Why not from the backend |
|---|---|
| `runID` | in the lease it sits next to the spec rather than inside it; in the CR it must be inside, because the CR is the unit of storage |
| `leaseEpoch` | the fencing token of the issuance |
| `materials` | the names of the Secret and ConfigMap the controller has just created |
| `callbackURL` | a cluster-local value: a Service in the controller's namespace |

**The boundary rule is mechanical: whatever the controller materializes does not
go into `spec`.** Secrets, presigned links and role files become cluster
objects, and only the names of those objects ride in the CR. The invariant
"there is not a single secret value in `spec`" is then held not by an admission
policy but by the **structural schema**: there is simply no place in the type to
put a secret, and unknown fields are pruned by the API server.

There is one residual risk, named honestly: `runtime.env[].value` is a legal
free-form field, and the backend is obliged not to put secrets there. This is
checked by the contract test "no value from `lease.secrets` appears in the
created CR", not by a CEL rule: a string's contents cannot be checked by policy.

Because of this rule `roleConfig` moved from `RenderedRunSpec` into `Lease`
(delta D15): the contents of the role files are material, and the CR gets
`materials.configMapName`.

---

## 4. `spec` immutability, and why a new epoch means a new CR

`spec` is immutable in its entirety, through the transitive CEL rule
`self == oldSelf`. Verified against API server 1.34: the rule fits the cost
budget — **but only because the schema contains not a single unbounded string
and not a single unbounded map**. The cost estimator, on meeting a string
without `maxLength`, treats its length as the request size limit, and the rule
is rejected when the CRD is installed. Hence `LabelValue` as a named type:
otherwise there is no way to bound `nodeSelector`.

This is exactly the kind of failure that surfaces during a customer's
`helm install` and never during manifest generation. The
`TestCRDInstallsAndAcceptsAFullSpec` test stands at that spot.

**A new epoch does not edit the CR, it replaces it.** Raising the epoch means a
change of ownership over the work: the old attempt is no longer authoritative,
and its status must not be inherited. The controller deletes the previous
`AgentRun` (the finalizer waits for the Job to stop) and creates a new one under
the same name.

The invariant: **at most one `AgentRun` per `runID` in a cluster**. The name is a
pure function of the `runID`, so "does this work already exist" is a `get`, not a
filtered `list`.

If the deletion does not complete within the `ackDeadline` (60 s), the controller
simply does not acknowledge the lease. The backend will reissue it at epoch+1 —
a self-healing mechanism that needs no special code.

---

## 5. Names and labels

```
AgentRun   ar-01j8x4k2zq7yb3m9f0r5w6t8cd        29 characters
Job        ar-01j8x4k2zq7yb3m9f0r5w6t8cd-j2     per attempt
Secret     ar-01j8x4k2zq7yb3m9f0r5w6t8cd-s
ConfigMap  ar-01j8x4k2zq7yb3m9f0r5w6t8cd-c
```

**The whole ULID, not the first eight characters.** The `ar-01j8x4k2`
abbreviation from section 13.4 of the architecture looks tidy and is wrong: the
first eight characters of a ULID encode time to about a second, so two runs
submitted at the same moment get the same name — and the second quietly picks up
the first one's Secret. The full ULID costs 26 of the 253 allowed characters, and
lowercase loses nothing for Crockford base32.

The Job is named with the attempt number because **each attempt is its own
Job**. `backoffLimit: 0`: a restart performed by Kubernetes itself would not
increment `attempt`, would not appear in the report, and would bypass the rule
that only the infrastructure class is retried automatically. Every restart must
be a decision of the controller — counted and reported.

Labels (selectable facts) and annotations (everything else, because every label
value is an entry in an etcd index):

| Label | Meaning |
|---|---|
| `haliphron.io/run-id` | the lowercase ULID; links the CR, the Job, the pod and the Secret |
| `haliphron.io/epoch`, `/attempt` | "show me the current generation of this work" becomes a selector |
| `haliphron.io/cluster-id`, `/agent`, `/component` | |

| Annotation | Meaning |
|---|---|
| `haliphron.io/spec-hash` | the digest of the spec as it arrived in the lease — see section 6 |
| `haliphron.io/leased-at` | for investigating delays |
| `haliphron.io/run-url` | a link into the backend's UI |

The names are lowercase ULIDs, so an alphabetical `kubectl get ar` is
chronological into the bargain.

---

## 6. Pruning of unknown fields — the main trap of this contract

The Cluster API's compatibility rule says both sides are **obliged** to ignore
unknown fields. In a CR that rule does not work the way it appears to.

A structural schema does not ignore an unfamiliar field — it **silently deletes**
it. The backend is newer, the cluster's chart is older, the backend rendered a
new field — the API server accepts the object, prunes the field and answers 201.
The run executes with a lost setting, and nobody finds out.

The behavior is pinned by the `TestUnknownSpecFieldIsPruned` test — as a fact of
the contract, not as an assumption.

The mitigation is two steps, both cheap:

1. **Hash the spec and read it back.** The controller computes the `sha256` of
   `RenderedRunSpec` as it arrived, puts it in an annotation (annotations are not
   pruned), reads the object back after creation and compares. A divergence names
   the specific paths that were lost.
2. **A negative ack** (delta D16). On detecting pruning **before** creating the
   Job, the controller answers `ack {accepted: false, code: SpecFieldsPruned,
   fields: [...]}`. The backend raises the epoch, returns the run to `Queued` and
   excludes this cluster from selection; if there are no others — `Failed` with
   class `config` and an intelligible reason in the UI.

The negative ack is needed for more than this. Until now the only way to say
"I cannot materialize this" was to burn a run: create the Job, let it fail,
report `Failed`. There are several causes, and all of them are detectable before
the Job: an unparsable resource quantity, an exhausted ResourceQuota, an image
outside the allowlist, tolerations the cluster does not accept.

`ClusterFacts.crdVersions` from the heartbeat helps with version-aware planning:
the backend knows which cluster should not be handed a new spec, before it issues
the lease at all.

---

## 7. Status: who writes it, and what exactly

`status` is a subresource, verified: writing to it cannot smuggle in a change to
`spec` (`TestStatusIsASubresource`). That is what makes it possible to give the
controller the right to write the status without the right to rewrite the work it
was issued.

The status is written by **the controller alone**. Its contents are observations
and pointers:

| Present | Absent, and will stay absent |
|---|---|
| phase, reason, message (≤ 1 KiB) | the result text, not even a summary |
| conditions, `observedGeneration` | logs and fragments of them |
| Job, pod and node names; timestamps; the exit code | workflow context, branch joins |
| the failure class, the attempt counter and the retry budget | cost as truth — only as a self-declaration |
| an `ObjectRef` to result/output/log/state/completion | |

A 64 KiB result summary in etcd would be read on every reconciliation of every
run. Its place is `runs.result_summary` in Postgres.

`status.usage` is what **the pod declared**, the least trusted component. It is
convenient in `kubectl`; it is not a basis for billing. The backend checks
`usage.durationMs` against the observed Job duration.

`status.reported` is delivery bookkeeping: what the backend has already accepted.
Without it, after a restart the controller either resends everything forever or
loses the condition "do not delete the CR until the backend has heard the
outcome".

### Deriving the phase from the Job and the pod

The one place where controller bugs live, so the table is exhaustive:

| Observation | Phase | Note |
|---|---|---|
| the CR exists, the Job does not | `Pending` | |
| the Job exists, the pod has not reached Running | `Starting` | |
| the pod is Pending with `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError` | `Starting` until `startupDeadlineSeconds` (600 s), then `Failed`/`infra` | otherwise a nonexistent image hangs forever |
| the pod is Pending, `Unschedulable` | the same | an exhausted quota or no nodes matching `nodeSelector` |
| the pod is Running | `Running` | |
| the container exited with code 0 | `Succeeded` | and nothing more: success = exit code 0 |
| code 11 | `TimedOut`/`agent` | the entrypoint stopped the agent itself on `timeoutSeconds` |
| Job `DeadlineExceeded` | `TimedOut`/`infra` | the backstop fired: the entrypoint itself hung |
| code ≠ 0 | `Failed` + the class from the exit code table | |
| the pod was deleted or evicted while Running | `Failed`/`infra` | `backoffLimit: 0`; the next attempt is created by the controller |
| the controller executed `cancel` | `Cancelled` | **does not override an already observed terminal phase** |

The last row matters: if the pod managed to exit with zero while the
cancellation was travelling to the cluster, the run is `Succeeded`. The first
terminal phase wins — the same rule as on the backend side.

The phase is monotonic within an attempt: a late observation does not move it
back. A growing `attempt` resets the rank.

---

## 8. Failure class and attempts

```go
FailureClassForExitCode(code)   // api/run/v1 — one source for both sides
```

| Code | Class | Auto-retry |
|---|---|---|
| 0 | `none` | — |
| 10, 11, 12 | `agent` | no |
| 20 | `git` | yes |
| 30 | `config` | no |
| > 128 (137 OOMKilled, 143 SIGTERM) | `infra` | yes |
| any other ≤ 128 | `agent` | no |

The last row is a decision: an unknown deliberate exit code is no reason to
believe a repeat would go better.

The retry is local: the controller increments `status.attempt` and creates a new
Job without asking the backend (`attempt` is its counter, and the epoch does not
change). The budget is `spec.retry.maxInfraRetries`, 3 by default, stored in
`status.retry.infraRetries`: after a controller restart, a failing run must not
receive a fresh set of attempts.

Between attempts there is an exponential pause with jitter,
`status.retry.nextAttemptAt`. Without it an `ImagePullBackOff` on a broken node
turns into a tight create/fail loop against the API server.

The repeat is idempotent thanks to `runs/{id}/state.json`: a `run` phase already
passed does not call the LLM a second time but finishes push/PR/upload/notify
(ADR 19).

---

## 9. Conditions

The phase answers "where the run is"; the conditions answer "what the controller
has managed to do about it".

| Type | True means |
|---|---|
| `Validated` | the spec passed the checks that are not in the schema: quantities parse, the image reference resolved, the tolerations were accepted by the cluster, and **the read-back matched the hash** |
| `JobCreated` | the Job for the current attempt exists |
| `Completed` | the phase is terminal; `reason` carries which one — a client tells `Succeeded` from `Cancelled` without parsing `phase` |
| `ResultReported` | the completion webhook from the pod has been received |

The absence of `ResultReported` on a finished run is exactly what the backend
sees as `CompletedWithoutResult`, and its signal to read storage itself.

---

## 10. Cancellation, abandon, the finalizer, deletion

Commands travel top-down through the ack and the heartbeat, and they have no
acknowledgements by design (a repeat is harmless).

| Command | Controller | Reporting |
|---|---|---|
| `cancel` | delete the Job with `gracePeriodSeconds`, drive to `Cancelled` | reports; including the result, if the pod managed to upload it |
| `abandon` | delete the Job and the CR | **reports nothing** |

The `haliphron.io/terminate-job` finalizer holds the CR until the Job and the pod
have stopped. It is removed **unconditionally** after a bounded wait
(`terminationGracePeriodSeconds` plus slack): a stuck finalizer is worse than the
leak it prevents — it makes the namespace undeletable, and the cure is editing an
object by hand in production.

Reporting is not tied to the finalizer, however. The completion webhook and its
forwarding live in the controller's memory; if it crashes between the two, the
report is lost. That is tolerable precisely because the pod also puts the same
report into `runs/{id}/completion.json` (delta D17): without that copy, the cost
and the PR link would be lost, and neither `result.md` nor `output.json` carries
them.

### Cleanup order

```
terminal phase
  → the report is delivered (status.reported.completionDelivered)
    → TTL (spec.ttlSecondsAfterFinished, 24 h by default)
      → the CR is deleted
        → the Job, pod, Secret and ConfigMap go with it by ownerReference
```

**The Job does not get `ttlSecondsAfterFinished`.** Otherwise the Kubernetes TTL
controller deletes the Job and the pod before the controller has observed and
reported them, and the run finishes as `Unknown` on an actual success. Ownership
and cleanup run solely through the `ownerReference` on the CR.

If the report was not delivered, the CR is held past the TTL up to a hard ceiling
(7 days) — after which it is deleted with an audit record: accumulating CRs
forever over an unavailable backend is not acceptable.

---

## 11. What the controller sets on the Job

Not part of the CR's schema, but part of the contract: how the exit codes are
interpreted depends on these values.

| Parameter | Value | Why |
|---|---|---|
| `restartPolicy` | `Never` | a restart inside the pod is invisible from outside |
| `backoffLimit` | `0` | every attempt is a controller decision, counted and reported |
| `activeDeadlineSeconds` | `timeoutSeconds` + slack for the clone and the upload | a backstop; the normal timeout must have time to upload the partial result |
| requests | **equal to limits** | QoS class Guaranteed: an agent evicted halfway costs an hour of work and a second bill for the model |
| `automountServiceAccountToken` | `false` | the pod does not need the cluster API |
| `securityContext` | non-root, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault` | section 14 of the architecture. Writes are needed in four directories, all emptyDir; the root stays read-only because the image moves the CLI caches into `$HOME` (contract 3, delta R6) |
| `terminationGracePeriodSeconds` | from the cancel command's `gracePeriodSeconds` | give the pod time to upload the partial result before SIGKILL |
| `volumeMounts` | the per-run Secret at `/haliphron/secrets/` (0400), the ConfigMap at `/haliphron/role/` | secrets end up neither in `spec`, nor in `kubectl describe`, nor in the environment the agent inherits |
| `env` | only the non-secret runtime contract variables | the list is `ContractEnv` in `api/run/v1/runtime.go` |

The `callback-token` in the Secret is generated by the **controller**, not the
backend: `callbackURL` itself is cluster-local, and without a token any pod in
the namespace could send a forged completion for somebody else's run.

**`envFrom` will not do here**, and this is delta R1 of contract 3. The Secret's
keys are fixed by contract 1 — `git-token`, `llm-api-key`, `mcp.json`,
`presigned.json`, `callback-token` — and not one of them is a valid environment
variable name. `envFrom` silently skips such keys, leaving an Event in the
namespace: the pod starts with no token, no MCP configuration and not a single
link to storage, and the first intelligible message about it appears much later
and reads as "could not download the prompt".

---

## 12. Controller RBAC

```
agentruns, agentruns/status, agentruns/finalizers   full
jobs                                                full
pods                                                get, list, watch
secrets, configmaps                                 full, in its own namespace only
events                                              create, patch
```

`pods/log` is **not needed**: the pod uploads the logs to storage in chunks. The
absence of that permission is not thrift but a property of the design: the
controller is not a log delivery channel and will not accidentally become one.

---

## 13. Versioning

`v1alpha1` is the only version and also the storage version. The rule for v1:

1. Additive changes — new optional fields, within `v1alpha1`. But remember the
   pruning (section 6): "the older side will ignore it" here means "the API
   server will delete it silently".
2. An incompatible change means `v1` and a conversion webhook.
3. **The webhook plumbing is installed from the very beginning**
   (`config/crd/patches/`): the Service, the certificate, CA injection, the chart
   upgrade path. That is what is expensive, not the handler itself; adding it to a
   working installation is noticeably more painful.
4. **Helm does not update CRDs from `crds/`.** The decision is made here: the CRD
   lives in `templates/` with `helm.sh/resource-policy: keep`. Otherwise the very
   first additive schema change silently fails to reach the cluster, and it
   surfaces as pruned fields.

With a single version the conversion handler is never invoked, so it is not
tested by the mere fact of installation. Its test is separate: a throwaway CRD
with two versions in envtest, exercising a real conversion.

---

## 14. Limits and cost

| Quantity | Value | Why |
|---|---|---|
| object in etcd | 1.5 MiB | the schema is bounded such that the spec fits with room to spare |
| `status.message` | 1 KiB | |
| CEL rule budget | 10⁷ units | the immutability rule fits only with bounded strings and maps |
| status write frequency | no more than once per 2 s per run | every write is an etcd transaction; the pod's event stream would otherwise produce dozens of writes per second |
| `startupDeadlineSeconds` | 600 s | from Job creation to Running |

Reconciliation is obliged to be a pure function of (`spec`, the observed cluster
state). No memory between calls: everything that must survive a restart lives in
`status`. That is also what makes the controller testable without a cluster.

---

## 15. Contract test checklist

Implemented in [test/contract](../../test/contract), run by `make test` against a
real API server — CEL, pruning, defaults and subresources live there and nowhere
else:

- [x] the CRD installs: the immutability rule fits the cost budget
- [x] a full spec is accepted and survives a round trip
- [x] a change to `spec` is rejected by the API server
- [x] an unknown `spec` field is silently pruned
- [x] defaults are applied (`timeoutSeconds`, `baseBranch`, `createPR`, TTL)
- [x] a status write does not change `spec`
- [x] phase ranks are monotonic, an unknown phase gives 0
- [x] the exit code → failure class table; only `infra` and `git` are retried
- [x] names do not collide for runs in the same millisecond; the Job name is ≤ 63
- [x] the Cluster API and the CRD agree on every significant schema key

Beyond that — on the controller, against a fake backend:

- [ ] field pruning → `ack {accepted: false, SpecFieldsPruned}`, no Job created
- [ ] `cancel` after the pod exited with zero → `Succeeded`, not `Cancelled`
- [ ] `abandon` → the Job and the CR are deleted, no further reports
- [ ] a restart between CR creation and the ack → a repeat ack with the same epoch
- [ ] a restart after an attempt failed → the retry budget was not reset
- [ ] epoch+1 to the same cluster → the old CR deleted, a new one created, no two Jobs
- [ ] `ImagePullBackOff` longer than `startupDeadlineSeconds` → `Failed`/`infra`
- [ ] the Job deleted externally while Running → `Failed`/`infra`, not a hang
- [ ] the finalizer is removed after a bounded wait even with a live pod
- [ ] values from `lease.secrets` do not appear in the created CR
- [ ] `roleConfig` ended up in the ConfigMap, and only its name in the CR

---

## 16. Deltas

**The edits are applied.** The table is a log: it explains why the documents say
what they say.

| # | What | Was | Became | Why |
|---|---|---|---|---|
| C1 | The CR name | `ar-01j8x4k2` (13.4) | the full lowercase ULID | eight characters of a ULID are time to the second; two runs in the same second share a Secret |
| C2 | References in the CR | `credentialsRef`, `mcpConfigRef`, `roleConfigRef`, `presignedRef` | a single `materials {secretName, configMapName}` field | the Secret's keys are fixed by the contract; four references to two objects are four places for a typo |
| C3 | `roleConfig` | a `RenderedRunSpec` field | a `Lease` field (Cluster API delta D15) | material, not spec: the controller makes a ConfigMap out of it |
| C4 | `spec` immutability | unspecified | CEL `self == oldSelf` + a new epoch means a new CR | the status is obliged to describe the run that actually started |
| C5 | Field bounds | unspecified | `maxLength`/`maxItems`/`maxProperties` on everything | without them the CEL rule fails the budget and the CRD does not install |
| C6 | The Job's `backoffLimit` | "`backoffLimit` at the Job level" (12.2) | `0`, an attempt equals a Job | a restart by Kubernetes does not increment `attempt`, never reaches the report and bypasses the "retry only for infra" rule |
| C7 | `ttlSecondsAfterFinished` on the Job | implied | **not set** | the TTL controller would otherwise delete the Job before the controller reports the outcome, turning a success into `Unknown` |
| C8 | Field pruning | not considered | the spec hash + a read-back + `SpecFieldsPruned` | the "ignore unknown fields" rule means "delete silently" in a CR |
| C9 | A negative ack | none (Cluster API delta D16) | `ack {accepted: false}` | otherwise "I cannot materialize this" can only be expressed by a burned run |
| C10 | `completion.json` | none (Cluster API delta D17) | the pod also puts the report into storage | the report lives in the controller's memory between the webhook and ingest |
| C11 | `callback-token` | none | the controller puts it into the per-run Secret | otherwise any pod in the namespace sends a forged completion for somebody else's run |
| C12 | The CRD in the chart | "decide before the first release" (16) | `templates/` + `resource-policy: keep` | `crds/` is not updated by `helm upgrade`, and it surfaces as pruned fields |
| C13 | `tolerations` | free form | an explicit schema | free form means `x-kubernetes-preserve-unknown-fields`, that is, giving up the structural schema for the sake of five fields |
| C14 | The pod's QoS | unspecified | requests = limits | Guaranteed: an evicted agent costs an hour of work and a second bill |
| C15 | `pods/log` in RBAC | implied | not needed | the pod uploads the logs; the controller must not accidentally become a log delivery channel |

---

## 17. What this unblocks

The controller and the backend can now be written in parallel:

- the controller — against `FakeBackend`, implementing reconciliation per the
  table in section 7;
- the backend — against `FakeController`, asserting in tests about the objects the
  controller builds, using the same `ObjectName`/`JobName` functions from the
  shared package;
- the agent image — fully autonomously: `docker run` with environment variables,
  no cluster and no backend. Its contract (variables, exit codes, `/completion`,
  `output.json`) is worked out separately: [agent-runtime.md](agent-runtime.md).

---

## 18. Deferred

| # | Question | Decision | Condition for revisiting |
|---|---|---|---|
| 1 | "Adopting" a CR applied by hand | Not doing it. A `ValidatingAdmissionPolicy` permits creating an `AgentRun` only to the controller's ServiceAccount | When there is demand for submitting runs through GitOps. Then: the controller registers such a CR with the backend through a report, and the backend creates the `runs` row |
| 2 | An image registry allowlist | A chart parameter, a CEL rule in the VAP, off by default | A customer requirement with a hard perimeter |
| 3 | `AgentRun` as cluster-scoped | No: Secret, ConfigMap and Job are namespace-scoped, and there is no reason to separate owner from owned | — |
| 4 | Metrics from the status via kube-state-metrics | No: the controller exposes its own metrics directly | If a cheap dashboard without our exporter is ever needed |
| 5 | `v1alpha1` → `v1` conversion | The plumbing exists, the handler does not | At the first incompatible schema change |
