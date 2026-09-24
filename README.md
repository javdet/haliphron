# Haliphron

Run headless coding agents — claude-code, codex — as one-shot Kubernetes Jobs,
on request from a REST API, MCP, Slack or a web UI.

An agent starts, does the work in a repository, opens a pull request, and
dies. Haliphron admits the request, places it on a cluster, watches it, and
reports what happened: the result, the logs, the cost, the pull request.

On-prem, single-tenant, delivered as two Helm charts.

## The one thing that shapes the design

**The control plane never connects to a cluster.** The controller inside each
cluster fetches work over outbound HTTPS, drives it to completion on its own,
and reports back.

So a cluster behind NAT needs no VPN, and the control plane stores no
kubeconfig. It also means a run already accepted finishes even while the
control plane is down — which is what most of the protocol's machinery exists
to guarantee. See [about the pull model](docs/explanation/the-pull-model.md).

A second assumption follows it: the agent pod executes model-generated
commands in a repository that may contain prompt injection, so it is treated
as untrusted code — no ServiceAccount token, no storage credentials, and a
default-deny NetworkPolicy.

## Status

Phase 1 of five: the vertical `run_agent` slice, complete through every layer.

There is no workflow engine and no scheduler. Both are designed — see section
19 of [docs/architecture.md](docs/architecture.md) — and neither is built.

## Documentation

Full documentation is in [`docs/`](docs/).

- **[Tutorial](docs/tutorials/)** — [Your first agent
  run](docs/tutorials/your-first-agent-run.md) takes an empty cluster to an
  open pull request in about thirty minutes.
- **[How-to guides](docs/how-to/)** — installing the control plane,
  registering clusters, submitting runs, defining roles, managing credentials,
  diagnosing failures.
- **[Reference](docs/reference/)** — the REST API, the MCP tools, every
  environment variable and Helm value, the role spec, and the statuses and
  exit codes a run is described in.
- **[Explanation](docs/explanation/)** — why the system is shaped this way:
  the pull model, the untrusted pod, leases and epochs, and what `Succeeded`
  deliberately does not mean.

Two document sets alongside those follow different rules.
[`docs/contracts/`](docs/contracts/) holds the normative specifications of the
four seams between components — tests parse them, so editing a documented
table without editing the Go fails a build.
[`docs/architecture.md`](docs/architecture.md) is the dated design record; it
describes more than exists.

## Repository layout

Eleven Go modules in a `go.work`.

| Path | What |
|---|---|
| [`api/`](api/) | the four wire contracts as Go types, plus OpenAPI and JSON Schema |
| [`backend/`](backend/) | the control plane: REST, MCP and Cluster API listeners over use cases over pgx |
| [`controller/`](controller/) | the in-cluster controller: lease, materialize, reconcile, launch |
| [`image/`](image/) | the agent image entrypoint: eighteen phases |
| [`db/`](db/) | migrations and the contract queries |
| [`fake/`](fake/) | one faithful implementation of each contract side — a deliverable, not test utilities |
| [`frontend/`](frontend/) | the web UI: React behind an nginx that proxies `/api` |
| [`deploy/`](deploy/) | the two Helm charts |
| [`test/`](test/) | cross-module contract suites, kept out of the shipping modules |

Each of [`controller/`](controller/README.md), [`image/`](image/README.md),
[`fake/`](fake/README.md), [`frontend/`](frontend/README.md) and
[`deploy/`](deploy/README.md) has a README with the rules specific to it.

## Building and testing

Everything builds and tests **inside a container**. Neither a laptop nor CI is
expected to have a Go toolchain, and the generated contract artifacts must come
out byte-identical in both.

```sh
make generate         # deepcopy, the AgentRun CRD and controller RBAC, from the Go types
make chart-gen        # copy the generated CRD and RBAC into the runtime chart
make verify           # fails if any committed generated artifact is stale

make test             # contract tests
make backend-test     # needs a real PostgreSQL
make controller-test  # needs envtest
make image-test       # against fake/controlplane, in-process
make db-test          # needs a real PostgreSQL

make images           # backend, controller, frontend
make image-build      # the agent image
make chart-lint chart-template chart-package
```

Test names are full sentences —
`TestAControllersLocalRetryIsANewAttemptAndNotANewEpoch` — and `-run` works
against them.

See [CLAUDE.md](CLAUDE.md) for the invariants worth knowing before changing
anything, and for how to run a single suite without the Makefile.

## Installing

```sh
helm dependency build deploy/charts/haliphron

helm install haliphron deploy/charts/haliphron \
  --namespace haliphron --create-namespace \
  --set database.dsn='postgres://...' \
  --set agent.image=ghcr.io/automagicops/haliphron-agent@sha256:...
```

Then install `deploy/charts/haliphron-runtime` into every cluster that should
run agents. The full procedure, including what to do about credentials, is in
[how to install the control
plane](docs/how-to/install-the-control-plane.md) and [how to register a target
cluster](docs/how-to/register-a-target-cluster.md).
