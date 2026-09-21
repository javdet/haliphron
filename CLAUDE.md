# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Haliphron runs headless coding agents (claude-code, codex) as one-shot Kubernetes Jobs, on
request from REST, MCP, Slack or a UI, and orchestrates them into workflows. On-prem, single
tenant, delivered as two Helm charts.

The repository is currently phase 1 of five (see section 19 of `docs/architecture.md`): the
vertical `run_agent` slice. There is no frontend directory yet, and no workflow engine or
scheduler — the domain model for both is designed but unimplemented.

## Commands

Everything builds and tests **inside a container**. Neither a laptop nor CI is expected to
have a Go toolchain, and the generated contract artifacts must come out byte-identical in
both. Do not run `go build` / `go test` on the host as a shortcut — the module set is a
`go.work` of eleven modules and several suites need envtest binaries or PostgreSQL that only
the Makefile wires up.

```sh
make generate         # deepcopy + AgentRun CRD + controller RBAC, from the Go types
make chart-gen        # copy the generated CRD/RBAC into the runtime chart
make verify           # fails if any committed generated artifact is stale

make test             # contract tests (test/contract) — needs envtest
make backend-test     # backend/... then test/backend — needs a real PostgreSQL
make controller-test  # controller/... then test/controller — needs envtest
make image-test       # image/... then test/image — against fake/controlplane, in-process
make fake-test        # fake/...
make db-test          # test/store — needs a real PostgreSQL

make backend-build backend-image
make controller-build controller-image
make image-build                      # the agent image
make chart-lint chart-template chart-package
```

`make generate` also runs `gofmt -l`, `go build` and `go vet` over `api/` and `controller/`;
a non-empty `gofmt -l` is a failure. There is no golangci-lint.

### Running a single test

Reuse the Makefile's container invocation rather than inventing one — the named volumes are
what keep the module and build caches warm, and `GOMAXPROCS=2` is deliberate.

```sh
# a plain suite (no cluster, no database)
docker run --rm -e GOMAXPROCS=2 \
  -v haliphron-gomod:/go/pkg/mod -v haliphron-gocache:/root/.cache/go-build \
  -v "$PWD":/w -w /w golang:1.26 \
  sh -c 'cd /w/image && go test -count=1 -race -run TestAFailedPushLeavesTheResultAlreadyInStorage ./entrypoint'
```

For a suite that needs envtest (`test/contract`, `controller/...`, `test/controller`), add
`-v haliphron-envtest:/envtest -v haliphron-gobin:/go/bin` and export `KUBEBUILDER_ASSETS`
the way `hack/test.sh` does. For a suite that needs PostgreSQL (`test/store`, `test/backend`),
it is less trouble to run the whole `make db-test` / `make backend-test` target: they start
and tear down a `postgres:18.1-bookworm` container on a throwaway network and set
`HALIPHRON_TEST_DSN`.

Test names are full sentences (`TestAControllersLocalRetryIsANewAttemptAndNotANewEpoch`), and
`-run` works against them.

## Architecture

### The one thing that shapes everything: the pull model

The control plane never connects to a cluster. The controller leases work outward, drives it
to completion on its own, and reports back. Consequences that show up everywhere: clusters
behind NAT need no VPN, the backend stores no kubeconfig, the Cluster API long-polls for up
to 30 s (every proxy in front of it must tolerate that), and the controller must finish a run
it has taken **even while the backend is down**.

A second constraint: the agent pod executes model-generated commands in a repository that may
contain prompt injection. It is untrusted code. Nothing is accepted from it directly by the
control plane; it has no ServiceAccount token, no storage credentials and a default-deny
NetworkPolicy.

### Modules (`go.work`)

| Module | What |
|---|---|
| `api/` | the four wire contracts as Go types + OpenAPI/JSON Schema. Depends on nothing internal |
| `backend/` | control plane: `restapi` `mcp` `clusterapi` listeners → `app` use cases → `store` (pgx) |
| `controller/` | the in-cluster controller: `lease` → `materialize` → `agentrun` reconciler → `launcher` (Job) |
| `image/` | the agent image entrypoint: eighteen phases, `entrypoint/runner.go` is the spine |
| `db/` | migrations and the contract queries (`lease.sql`, `expire_*.sql`, `lock_run.sql`) |
| `fake/` | `backend`, `controller`, `controlplane` — one faithful implementation of each contract side |
| `test/*` | cross-module contract suites, one per track, kept out of the shipping modules |

Backend layering is hexagonal and enforced by convention: listeners are transport only, `app`
holds every rule, `store` is SQL. The reason is stated in the package comment of
`backend/app/service.go` — the REST path and the MCP path must not admit runs by different
rules, and the ingest path and the heartbeat path must not apply reports by different ones.
Put a new rule in `app`, not in a handler.

The HTTP stack is stdlib `net/http`, not gin, despite what `docs/architecture.md` says.

### Four contracts, and `docs/` is normative

`docs/contracts/*.md` are not commentary. The schema says what is transmitted; the contract
document says what it means and how to behave — fencing, deadlines, monotonicity, idempotency,
failure behaviour. When code and a contract document disagree, that is a bug in one of them,
not a stale doc.

| Contract | Semantics | Shape |
|---|---|---|
| Cluster API (backend ↔ controller) | `docs/contracts/cluster-api.md` | `api/cluster/v1/openapi.yaml` |
| `AgentRun` CRD (controller ↔ k8s) | `docs/contracts/agentrun-crd.md` | `api/agentrun/v1alpha1` |
| Agent runtime (controller ↔ pod) | `docs/contracts/agent-runtime.md` | `api/runtime/v1`, `api/run/v1` |
| State store | `docs/contracts/run-store.md` | `db/migrations`, `db/queries` |

`test/contract/` enforces the joins between them: `drift_test.go` compares the hand-written
OpenAPI against the generated CRD key by key, and `runtime_test.go` parses the tables in
`agent-runtime.md` and asserts them against the Go constants. Editing a documented table
without editing the Go, or the reverse, fails a test.

Naming convention across the wire: machine contracts (Cluster API, CRD, pod webhook) are
**camelCase**; the public REST API is **snake_case**.

### Generated artifacts, never hand-edited

`api/**/zz_generated.deepcopy.go`, `config/crd/bases/`, `config/rbac/role.yaml`, and the
runtime chart's `templates/crd.yaml` + `templates/rbac-controller.yaml`. Change the Go types
or the `+kubebuilder:` markers, then `make generate chart-gen`, and commit the results —
`make verify` is what catches a miss.

The CRD ships in the chart's `templates/`, not `crds/`, on purpose: Helm installs `crds/` once
and never updates it, so an additive schema change would silently be pruned out of `spec`.

### The fakes are a deliverable, not test utilities

`fake/` implements each contract side faithfully enough that the other side can be built
against it, including the rules that only fire on failure (epoch fencing, report monotonicity,
the two deadlines). A permissive fake is worse than none. `controller/` deliberately does not
depend on `fake/`, so nothing that ships to a customer can carry a test double.

Known divergence worth checking before asserting against it: `fake/controller/materialize.go`
sets `runAsUser: 65532` where the real `controller/launcher` uses 1000 (the agent image is
`USER 1000:1000`), and its `containerEnv` omits `HALIPHRON_CREATE_PR`, `SUBMODULES`, `LFS`,
`CLONE_DEPTH` and `LOG_CHUNK_SECONDS` — the image reads an absent `CREATE_PR` as false.

## Invariants that are easy to break

**Epoch and attempt have different owners.** `lease_epoch` is ownership of the work and is
raised only by the backend. `attempt` is a retry inside one ownership and is raised only by the
controller, without asking. An attempt is the triple `(run_id, lease_epoch, attempt)`, and
`attempt` resets to 1 on every epoch increase.

**Two places must close an open `run_attempts` row**, or the `run_attempts_one_live` partial
unique index makes the run permanently unrunnable. An epoch rise is handled by the
`runs_close_attempt` trigger on `runs`; an attempt rise inside an epoch is handled by
`closeSupersededAttempts` in `backend/store/report.go`, which closes rows with a *strictly
lower* attempt — reports arrive reordered, and a late attempt-1 observation must not close
attempt 2. Never write the row directly from a new path.

**Artifact keys are refused, never sanitised.** `filepath.Clean("/" + "../escaped.md")` is
`/escaped.md`: nothing escapes the volume, and the object has silently left the run's prefix.
The single allow-list is `runv1.ArtifactKey` in `api/run/v1/progress.go`, called by both the
controller's callback handler and the backend's ingest. A key one side accepts and the other
refuses is an object spooled, forwarded, rejected and dropped with the pod gone.

**Nothing starts before the acknowledgement.** The backend reassigns an unacknowledged lease
on the grounds that the work cannot have started, so the Job waits for the ack (recorded as a
CR annotation) and a run that never gets one is discarded rather than launched.

**The lease body is never logged.** It is the one message carrying secret material in the
clear — an hour-long git token, a model key, bucket capabilities. There is no debug mode that
prints it, and `controller/clusterapi/client_test.go` asserts so.

**A failure is acted on by its `Problem.action`, never by its status code.** Two independently
written sides always diverge on what a 409 means. `controller/clusterapi/client.go` is the only
place that reads the action.

**Reconciliation remembers nothing.** Anything that must survive a restart is in the CR status;
anything arriving from outside, including backend commands, arrives as an annotation.

**In the image, `persist` runs before the git phases** — what has been paid for is made durable
before anything allowed to fail, so a retry never pays for the model twice.
`TestAFailedPushLeavesTheResultAlreadyInStorage` is what keeps that true.

**`Succeeded` means exit code 0 and nothing more.** Not "the task was solved". Wording in any
UI or report must reflect that, or the system systematically claims success it did not have.

## Further reading

`docs/architecture.md` is the rationale for all of the above (section 7 for the pull model,
9.1–9.3 for the stores and the duplicate guard, 11 for the image, 14 for security, 19 for
phasing). `agent-orkestrator.md` is the original requirements source. Each of
`controller/`, `image/`, `fake/` and `deploy/` has a README with the rules specific to it.
