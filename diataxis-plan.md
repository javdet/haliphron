# Diátaxis documentation plan — Haliphron

Working file for the `diataxis-docs` skill. Not part of the published docs
tree; `docs/` is rendered as plain Markdown, this file is not.

## Run context

- **Run date:** 2026-09-22 — first run, no prior plan.
- **Docs tooling:** none detected. No `mkdocs.yml`, `conf.py` or
  `docusaurus.config.*` at the root or in `docs/`; a bare `docs/` directory
  with four hand-written Markdown files. **Output format: Markdown under
  `docs/`.** Mermaid renders on GitHub, which is where this tree is read, so
  diagrams go in fenced ```mermaid blocks with no fallback needed.
- **Autodoc:** none. No pkgsite/godoc target, no `typedoc.json`, no Sphinx.
  Go package comments are unusually rich but nothing publishes them.
  **Reference is hand-written**, derived from code and schemas.
- **Repo state:** clean tree, 10 commits, no GitHub issues or PRs (checked
  with `gh`; the remote is `javdet/haliphron`).

## Constraints discovered in the survey

These bind the plan and every later run.

1. **The four `docs/contracts/*.md` files are machine-verified.**
   `test/contract/runtime_test.go` parses the tables in
   `agent-runtime.md` and asserts them against the Go constants;
   `drift_test.go` compares `api/cluster/v1/openapi.yaml` against the
   generated CRD. Moving, splitting or reflowing those documents breaks
   tests. They are treated as a fixed artifact class and linked to, never
   restated.
2. **`docs/architecture.md` is partly aspirational.** It describes an OIDC
   IdP, a workflow engine, a scheduler and gin — none of which exist in
   phase 1 (`CLAUDE.md` flags the gin divergence explicitly). New
   explanation pages must describe the system as built, and cite
   architecture.md for rationale rather than inherit its claims.
3. **There is no root `README.md`.** Phase 6 must create one rather than
   update one.
4. **There is no local evaluation path.** No kind config, no
   docker-compose, and `DockerLauncher` is named in architecture.md but not
   implemented. An end-to-end tutorial needs a real cluster, a model key and
   a git forge — it cannot be verified from this repo. See the tutorial
   entries below.

## Existing documentation — compass pass

Classified whole-document and section by section.

| Document | Wide view | Close view | Annotation |
|---|---|---|---|
| `docs/architecture.md` | explanation | §13 Contracts, §15 Metrics, §16.1 Charts are reference-register tables; §16 "Local development" is how-to | **keep** — see note |
| `docs/contracts/cluster-api.md` | reference | §2.1, §4.1, §7.2 are "why" prose = explanation | **keep** (locked, constraint 1) |
| `docs/contracts/agentrun-crd.md` | reference | §2.1 "Why `api` is a separate Go module" = explanation | **keep** (locked, constraint 1) |
| `docs/contracts/agent-runtime.md` | reference | §3.1 "Why `persist` comes before git" = explanation | **keep** (locked, constraint 1) |
| `docs/contracts/run-store.md` | reference | §5, §6, §7.1 are explanation | **keep** (locked, constraint 1) |
| `deploy/README.md` | how-to | install/register/upgrade/uninstall steps = how-to; "What the agents' namespace is for", the KEK rationale, the bootstrap-token properties = explanation; the chart table and value names = reference | **split** |
| `controller/README.md` | blurred | layout table = reference; "The four rules" = explanation; config table = reference | **keep** — see note |
| `image/README.md` | blurred | layout table = reference; "The three rules" = explanation; "Running the tests" = how-to | **keep** — see note |
| `fake/README.md` | explanation | whole document is rationale | **keep** — see note |
| `frontend/README.md` | blurred | "Why the proxy" + "Authentication" = explanation; page table = reference; "Working on it" = how-to | **keep** — see note |
| `agent-orkestrator.md` | neither | original requirements brief, a source artifact | **keep**, untouched |
| `CLAUDE.md` | neither | agent instructions | **keep**, untouched |

### Note on the **keep** annotations

`docs/architecture.md` is kept whole and in place. It is the repository's
design-decision record, cited by `CLAUDE.md` and by all four contract
documents as the rationale source, and its value is as a single dated
argument rather than as four quadrant-shaped fragments. The generated
explanation pages describe the system as built and link into it for the
"why"; the generated reference pages supersede its §13/§15/§16 tables and
are derived from code, not from it.

The four module READMEs (`controller/`, `image/`, `fake/`, `frontend/`) are
kept in place as contributor orientation — they are read by `cat`ting them
next to the code they describe, and `CLAUDE.md` points at them by path. The
docs set links to them.

*This grouping is a judgement call and is put up for approval as its own
item — see the questions below.*

### `deploy/README.md` — split destinations

The split follows the approved audience: the operator-facing procedures move
into `docs/how-to/` and `docs/explanation/`, and the contributor-facing and
advanced-installation material **stays in `deploy/README.md`**, which gains a
pointer to the new guides at the top. Nothing is dropped, and nothing lands in
a document this run is not writing.

| Source section | Destination quadrant | Destination document |
|---|---|---|
| "Two charts" table | explanation | `docs/explanation/architecture.md` (Container level) |
| "Before you start" | how-to | `docs/how-to/install-the-control-plane.md` (prerequisites) |
| "1. The control plane", `existingSecret` handling, "Split deployment" | how-to | `docs/how-to/install-the-control-plane.md` |
| "The key encryption key" — the commands | how-to | `docs/how-to/manage-installation-credentials.md` |
| "The key encryption key" — the rationale | explanation | `docs/explanation/credentials-and-the-key-encryption-key.md` |
| "The first admin token" — the commands | how-to | `docs/how-to/manage-installation-credentials.md` |
| "The first admin token" — the rationale | explanation | `docs/explanation/credentials-and-the-key-encryption-key.md` |
| "2. Each target cluster", "Uninstalling a cluster" | how-to | `docs/how-to/register-a-target-cluster.md` |
| "What the agents' namespace is for" | explanation | `docs/explanation/the-untrusted-agent-pod.md` |
| "Observability" | how-to | `docs/how-to/register-a-target-cluster.md` (metrics section) |
| "Upgrades" | how-to | `docs/how-to/upgrade-an-installation.md` |
| "The Cluster API and long polls" | explanation | `docs/explanation/the-pull-model.md` |
| Chart values named throughout | reference | `docs/reference/helm-values.md` |
| **"Images"** | — | **stays in `deploy/README.md`** (contributor audience, not in scope) |
| **"Gateway API instead of Ingress"** | — | **stays in `deploy/README.md`** (advanced operations, not in scope) |
| **"Development"** | — | **stays in `deploy/README.md`** (contributor audience, not in scope) |

## Approved audience

**Operators installing and running Haliphron**, and **engineers and agents
submitting runs**. Contributors are *not* an audience for this set — the
repository already serves them through `CLAUDE.md` and the module READMEs.
This decision removes several otherwise-obvious pages; each is recorded
under "Not created" with a remedy.

## Proposed reference scope

**Approvable item.** The reference quadrant covers **the surface those two
audiences touch**, and nothing else:

- the public REST API (`/api/v1`, 22 routes, `backend/restapi/`)
- the six MCP tools (`backend/mcp/tools.go`)
- environment configuration: 60 backend + 25 controller variables
- Helm values for both charts
- the run lifecycle vocabulary: backend statuses, CR phases, the eighteen
  runtime phases, exit codes, failure classes
- the role spec

**Explicitly out of scope**, each for a stated reason:

- **Go package APIs.** No autodoc is configured, and the wire types in
  `api/` are already specified by the four contract documents plus
  `openapi.yaml` and `output.schema.json`. Restating them by hand would
  create a third copy that no test guards.
- **The Cluster API** (backend ↔ controller). Specified by
  `docs/contracts/cluster-api.md` and `api/cluster/v1/openapi.yaml`, and
  not a surface either audience calls. Linked, not restated.
- **The SQL schema.** Specified by `docs/contracts/run-store.md` and
  `db/migrations/`.
- **Frontend component APIs.** Internal to one bundle.
- **`make` targets.** Contributor surface; out by the audience decision.

## Proposed documents

Every entry: title — the user need it serves — source material.

### Reference — `docs/reference/` (6 pages)

| Title | Need | Source |
|---|---|---|
| REST API | "What does `POST /api/v1/runs` accept and return?" | `backend/restapi/server.go`, `runs.go`, `admin.go`; scopes in `backend/store` |
| MCP tools | "What arguments does `run_agent` take?" | `backend/mcp/tools.go` |
| Configuration | "What does `HALIPHRON_LEASE_TTL_SECONDS` default to?" | `backend/config/`, `controller/config/` |
| Helm values | "What can I set on either chart?" | `deploy/charts/*/values.yaml` |
| Run statuses, phases and exit codes | "What does exit 21 mean?" | `api/run/v1/phase.go`, `runtime.go`, `db/migrations/0001_domains.sql` |
| Role spec | "What fields does a role take?" | `backend/run/spec.go`, `backend/restapi/admin.go` |

### How-to guides — `docs/how-to/` (9 pages)

Goal-shaped, not tool-shaped. Operations core (5) plus usage core (4), as
approved.

| Title | Need | Source |
|---|---|---|
| Install the control plane | get a working control plane | `deploy/README.md` §1, `deploy/charts/haliphron/` |
| Register a target cluster | make a cluster able to run agents | `deploy/README.md` §2, `controller/identity/`, `test/backend/register_test.go` |
| Manage installation credentials | read, replace and revoke the KEK and the bootstrap token | `deploy/README.md`, `test/backend/bootstrap_test.go` |
| Upgrade an installation | move two charts independently without locking out controllers | `deploy/README.md`, `backend/config` version range |
| Diagnose a failed run | the commonest operational task | `api/run/v1/phase.go`, `/runs/{id}/attempts`, `docs/contracts/agent-runtime.md` §4 |
| Submit a run and collect its result | the product's primary act, over REST | `backend/restapi/runs.go`, `test/backend/restapi_test.go` |
| Call Haliphron from another agent over MCP | let an IDE or an agent start runs | `backend/mcp/`, `test/backend/mcp_test.go` |
| Define a role | reuse a prompt, tool policy and model | `backend/run/spec.go`, `backend/restapi/admin.go` |
| Give runs a git token and a model key | the credentials a real run needs | `backend/restapi/admin.go`, `backend/config` `GIT_SECRET`/`LLM_SECRET` |

### Explanation — `docs/explanation/` (8 pages)

| Title | Need | Source |
|---|---|---|
| About the architecture | what the pieces are and how they sit | diagram evidence below |
| The pull model | why nothing connects into a cluster, and what it costs | `docs/architecture.md` §7, `controller/README.md`, `deploy/README.md` |
| Leases, epochs and attempts | the one fencing mechanism, and its two owners | `docs/contracts/cluster-api.md` §3–5, `run-store.md` §5–7 |
| The untrusted agent pod | why the pod has no token, no credentials and no network | `docs/architecture.md` §14, `deploy/README.md`, `image/README.md` |
| What "Succeeded" means | exit 0 is not "the task was solved" | `api/run/v1/phase.go`, `docs/architecture.md` §1 |
| Artifacts: relay and object-store modes | why there are two, and which to run | `docs/architecture.md` §9.2, `backend/artifacts/` |
| Credentials and the key encryption key | why the key is not in the database, and what losing it costs | `deploy/README.md`, `backend/store` |
| Why the contracts are normative | why editing a documented table breaks a test | `test/contract/`, `CLAUDE.md` |

### Tutorials — `docs/tutorials/` (1 page)

| Title | Need | Source |
|---|---|---|
| Your first agent run | learn the system by taking one run from install to an open PR | `deploy/README.md`, `test/backend/restapi_test.go`, `docs/architecture.md` §12.1 |

**Verification status: UNVERIFIED.** Approved on that basis. The lesson
needs a Kubernetes cluster, a model API key and a git forge, none of which
this repository provides, so no step in it has been executed. The page
carries a visible note saying so, and this marker stays in the plan file
until someone runs the tutorial through end to end and discharges it.

### Root `README.md` (new, requested)

The repository has none. Requested explicitly by the user in this run, and
required by Phase 6 in any case — generated docs the README never mentions
are invisible.

| Title | Need | Source |
|---|---|---|
| `README.md` | "What is this, and where do I start?" | `docs/architecture.md` §1, `agent-orkestrator.md`, `deploy/README.md`, `CLAUDE.md` |

Contents, and nothing more: what Haliphron is in a short paragraph, the one
constraint that shapes it (the pull model), the status (phase 1 of five —
`run_agent` only; no workflow engine, no scheduler), a documentation section
linking the `docs/` landing page with one sentence per quadrant, and pointers
to the module READMEs and the two charts. It is the front door, not a docs
mirror: no installation steps, no API tables, no rationale — those live in
the quadrants and are linked.

## Not created

| Item | Reason | Remedy |
|---|---|---|
| Contributor how-to guides (build the images; run one test suite in a container) | Out by the approved audience decision — this set serves operators and run submitters | Re-run with contributors added to the audience; the source material is `CLAUDE.md` "Running a single test" and `deploy/README.md` "Images" |
| `docs/reference/make-targets.md` | Same audience decision; `make` is a contributor surface | As above |
| Advanced-operations how-to guides (object storage instead of the relay volume; expose the API through Gateway API) | Not selected for this run; both stay documented in `deploy/README.md` | Re-run selecting advanced operations, or ask for the two pages by name |
| A verifiable tutorial | No local evaluation path exists: no kind config, no docker-compose, and `DockerLauncher` is named in `docs/architecture.md` §19 but not implemented | Implement a kind-based or `DockerLauncher` evaluation path, then re-run; the tutorial becomes executable and its unverified marker can be discharged |
| A second tutorial | One lesson is what the evidence supports at phase 1; a second would be a how-to guide wearing a tutorial's title | Phase 3 workflows give a genuine second lesson (a multi-step chain), which is not implemented yet |

## Diagram evidence (C4)

For `docs/explanation/architecture.md`. Both levels proposed; both have
evidence. Removed in Phase 6 once the diagrams are committed.

**System Context (L1) — has evidence.**

| Element | Kind | Evidence |
|---|---|---|
| Engineer / operator | actor | `frontend/README.md` page table; `backend/store` scopes `runs:read`, `runs:write`, `admin` |
| External agent or IDE | actor | `backend/mcp/tools.go`; `SubmitRequest.ParentRunID` for agent-started children |
| CI or script | actor | `backend/restapi` bearer tokens, `Idempotency-Key` |
| Haliphron | system | this repo |
| Git forge (GitHub/GitLab) | external | `image/entrypoint/git.go`; `TestAGitLabRunWithAGitHubMCPIsRefusedExplicitly` |
| LLM provider | external | `image/entrypoint/agent.go`, `HALIPHRON_LLM_SECRET` |
| External MCP servers | external | `runv1.MCPServer`, `image/entrypoint` mcp-prepare/mcp-verify phases |
| Kubernetes clusters | external | `deploy/charts/haliphron-runtime` |

Not shown: the OIDC IdP in `architecture.md` §3. It does not exist — the UI
holds a bearer token in `localStorage` (`frontend/README.md`).

**Container (L2) — has evidence.** Five deployable units, not one, so the
level is not redundant.

| Container | Evidence |
|---|---|
| Backend (REST :8080, MCP :8081, Cluster API :8082, metrics :9090) | `deploy/charts/haliphron/templates/deployment.yaml`, `backend/config` |
| Frontend (nginx + React, proxies `/api`) | `deploy/charts/haliphron/templates/frontend-deployment.yaml`, `frontend/nginx.conf.template` |
| PostgreSQL | `Chart.yaml` dependency, `db/migrations/` |
| Object storage (MinIO or S3), optional | `Chart.yaml` dependency, `backend/artifacts/` |
| Controller (in each target cluster) | `deploy/charts/haliphron-runtime/templates/deployment.yaml` |
| Agent pod (one-shot Job) | `controller/launcher/`, `image/` |

## Parking lot

*(Content surfacing in the wrong phase is parked here. Must be empty before
the run closes.)*

## Standing decisions

Recorded so future runs and Phase 6 can check the output against them.

1. `docs/contracts/*.md` are **locked**. Never moved, split or reflowed;
   `test/contract` parses their tables.
2. `docs/architecture.md` is **kept whole**, and is cited for rationale, not
   inherited from. Where it describes something that does not exist (OIDC,
   the workflow engine, the scheduler, gin), the generated pages describe
   what is built and say so.
3. The four module READMEs stay where they are. Docs link to them by
   relative path.
4. Reference is derived from **code and schemas**, never from prose docs.
5. No placeholder pages and no empty landing sections anywhere.
6. The root `README.md` stays an overview. When a future run adds pages, the
   README gains links, never copied content.
