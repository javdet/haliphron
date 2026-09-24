# Diátaxis documentation plan — Haliphron

Durable record for the `diataxis-docs` skill. Not part of the published docs
tree. Future runs read this first and re-ask only what new evidence
contradicts.

## Run history

- **2026-09-24** — first run. All four quadrants written. Output is plain
  Markdown under `docs/`; no generator config exists (no `mkdocs.yml`,
  `conf.py` or `docusaurus.config.*`), and no autodoc pipeline exists, so
  reference is hand-written from code and schemas.

## Approved audience

**Operators installing and running Haliphron**, and **engineers and agents
submitting runs**.

Contributors are *not* an audience for this set — `CLAUDE.md` and the module
READMEs serve them. This decision is what excludes several otherwise-obvious
pages; each is under [Not created](#not-created).

## Approved reference scope

Covers the surface those two audiences touch:

- the public REST API (`/api/v1`)
- the MCP tools
- environment configuration for the backend and the controller
- Helm values for both charts
- the run lifecycle vocabulary: backend statuses, CR phases, runtime phases,
  exit codes, failure classes
- the role spec

Out of scope, each with a reason:

| Excluded | Reason |
|---|---|
| Go package APIs | no autodoc configured; the `api/` wire types are already specified by the four contract documents plus `openapi.yaml` and `output.schema.json`. A hand-written third copy is a third thing to drift |
| The Cluster API (backend ↔ controller) | specified by `docs/contracts/cluster-api.md`; not a surface either audience calls |
| The SQL schema | specified by `docs/contracts/run-store.md` and `db/migrations/` |
| Frontend component APIs | internal to one bundle |
| `make` targets | contributor surface, out by the audience decision |

## Standing decisions

These bind every future run. Phase 6 checks the output against them.

1. **`docs/contracts/*.md` are locked.** Never moved, split or reflowed.
   `test/contract/runtime_test.go` parses the tables in `agent-runtime.md`
   and asserts them against the Go constants; `drift_test.go` compares the
   OpenAPI against the generated CRD. Link to them; never restate them.
2. **`docs/architecture.md` is kept whole** and cited for rationale, not
   inherited from. It describes an OIDC IdP, a workflow engine, a scheduler
   and gin, none of which exist. Generated pages describe what is built and
   say so.
3. **The module READMEs stay in place** — `controller/`, `image/`, `fake/`,
   `frontend/`. They are contributor orientation read next to the code, and
   `CLAUDE.md` cites them by path. `deploy/README.md` is the one exception:
   its operator procedures moved into `docs/`, and it keeps the images,
   pod-placement, Gateway API and chart-development material.
4. **Reference is derived from code and schemas**, never from prose docs.
5. **No placeholder pages and no empty landing sections.**
6. **The root `README.md` stays an overview.** Future runs add links, never
   copied content.

## What exists

### Tutorials — `docs/tutorials/`

| Title | Need | Source |
|---|---|---|
| Your first agent run | learn the system by taking one run from install to an open PR | `deploy/README.md`, `test/backend/restapi_test.go`, `docs/architecture.md` §12.1 |

**Verification status: UNVERIFIED.** No step has been executed. The lesson
needs a Kubernetes cluster, a model provider key and a git forge, none of
which this repository supplies. The page carries a visible notice saying so.
This marker stays until someone runs the tutorial end to end and discharges
it.

### How-to guides — `docs/how-to/`

| Title | Need |
|---|---|
| Install the control plane | get a working control plane |
| Register a target cluster | make a cluster able to run agents |
| Manage installation credentials | read, replace and revoke the KEK and bootstrap token |
| Upgrade an installation | move two charts independently without locking out controllers |
| Diagnose a failed run | the commonest operational task |
| Submit a run and collect its result | the product's primary act, over REST |
| Call Haliphron from another agent over MCP | let an IDE or agent start runs |
| Define a role | reuse a prompt, tool policy and model |
| Give runs a git token and a model key | the credentials a real run needs |

### Reference — `docs/reference/`

| Title | Source |
|---|---|
| REST API | `backend/restapi/` |
| MCP tools | `backend/mcp/tools.go` |
| Configuration | `backend/config/`, `controller/config/` |
| Helm values | `deploy/charts/*/values.yaml` |
| Statuses, phases and exit codes | `api/run/v1/phase.go`, `runtime.go`, `db/migrations/0001_domains.sql` |
| Role spec | `backend/app/role.go`, `backend/run/spec.go`, `api/run/v1/runspec.go` |

### Explanation — `docs/explanation/`

| Title | Why-question it answers |
|---|---|
| About the architecture | what are the pieces, and how do they sit? Carries the C4 System Context and Container diagrams |
| About the pull model | why does a Kubernetes orchestrator hold no kubeconfig? |
| About leases, epochs and attempts | how does the system decide what a cluster's silence means? |
| About the untrusted agent pod | why does the pod have no token, no credentials and no network? |
| About what "Succeeded" means | why does the system refuse to claim the task was solved? |
| About artifact modes | why are there two, and why is the awkward one the default? |
| About credentials and the key encryption key | why is the key not in the database? |
| About why the contracts are normative | why does editing a documented table break a test? |

C4 levels: System Context and Container. Both have evidence — six deployable
units across two charts, so the Container level is not redundant. Component
and Code levels are not created: below the maintainable line, and they drift
from code faster than reruns occur. Remedy: maintain by hand outside this set,
or use IDE tooling.

## Not created

| Item | Reason | Remedy |
|---|---|---|
| Contributor how-to guides (build the images; run one test suite in a container) | out by the approved audience decision | re-run with contributors added to the audience; source material is `CLAUDE.md` "Running a single test" and `deploy/README.md` "Images" |
| `docs/reference/make-targets.md` | same audience decision; `make` is a contributor surface | as above |
| Advanced-operations how-to guides (object storage instead of the relay volume; expose the API through Gateway API) | not selected for this run; both remain documented in `deploy/README.md` | re-run selecting advanced operations, or ask for the two pages by name |
| A verifiable tutorial | no local evaluation path exists: no kind config, no docker-compose, and `DockerLauncher` is named in `docs/architecture.md` §19 but not implemented | implement a kind-based or `DockerLauncher` evaluation path, then re-run; the tutorial becomes executable and its unverified marker can be discharged |
| A second tutorial | one lesson is what phase 1 supports; a second would be a how-to guide wearing a tutorial's title | the phase 3 workflow engine gives a genuine second lesson (a multi-step chain), and is not implemented |
| C4 Component and Code diagrams | below the maintainable line; they drift from code faster than reruns occur | maintain by hand outside the generated set, or use IDE tooling |

## Open findings

Discrepancies found while verifying documentation against code. These are
code/doc bugs, not documentation tasks, and are left for a maintainer.

1. **`postgresql.enabled` is the string `enable`** in
   `deploy/charts/haliphron/values.yaml` — non-empty, so Helm treats it as
   true and installs the subchart by default. `deploy/README.md` and
   `Chart.yaml` both state the subcharts are off by default and that BYO is
   the production answer. The chart also carries a literal default password in
   `postgresql.auth.password`.
2. **`docs/architecture.md` links to `../agent-orkestrator.md`**, which is
   deleted in the working tree. Either the deletion or the link needs
   reverting; `architecture.md` is locked by decision 2, so this was not
   touched.

Resolved during this run: `deploy/README.md` previously gave the
bootstrap-token path as `/v1/clusters/bootstrap-tokens` (the base path is
`/api/v1`) and showed `-d '{}'` for a call that requires `name`. The generated
guides use the correct form. The runtime chart's `NOTES.txt` still says a
cluster should be "Ready"; the `cluster_status` domain has no such value — it
is `Active`.
