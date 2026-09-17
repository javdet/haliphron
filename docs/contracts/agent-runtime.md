# Agent image runtime contract — semantics

Status: ready for implementation
Date: 2026-09-17
Shape: [api/runtime/v1](../../api/runtime/v1) — the completion webhook, the
`output.json` and `state.json` schemas; [api/run/v1/runtime.go](../../api/run/v1/runtime.go) —
variables, paths, phases; [api/run/v1/completion.go](../../api/run/v1/completion.go) —
the report
Basis: [architecture.md](../architecture.md), sections 8, 10, 11, 12.2, 13.5, 14, 15;
[cluster-api.md](cluster-api.md), [agentrun-crd.md](agentrun-crd.md)

The first two contracts describe how work reaches the cluster and turns into a
Job. This one describes the **last twelve inches**: what the controller puts
into the container, what the image is obliged to do with it, what it produces
and how it reports the result.

It is also the only one of the three that can be verified without a cluster at
all. The image has neither a lease nor reconciliation: `docker run` with a set
of variables, a directory of secrets and an address to knock on. That is why the
"image" track proceeds fully autonomously once this document is fixed, rather
than against a fake.

Section [17](#17-deltas) lists the deltas this work introduced into the previous
two contracts and into the architecture document; the edits are **applied**, and
the table is kept as a log. Section [19](#19-deferred) is what we decided not to
do in v1.

---

## 1. Contract boundaries

| What | Where |
|---|---|
| backend ↔ controller | [Cluster API](cluster-api.md), contract 1 |
| controller ↔ kubernetes | [CRD `AgentRun`](agentrun-crd.md), contract 2 |
| controller ↔ agent pod | **this contract** |
| pod ↔ storage | this contract, section 9 (addressing — contract 1, `ArtifactBundle`) |
| client ↔ backend | the external REST `/api/v1` |

The pod is the **least trusted component in the system**. It executes text that
came from outside, with tools that this text selects, in a repository whose
contents also influence the model's behavior. Everything in this document starts
from the assumption that the agent may do anything available to it — not because
it is malicious, but because from inside the pod there is nothing to distinguish
"did what it was asked" from "did what it read in the README".

Hence three cross-cutting rules, to which half the decisions below reduce:

1. **The pod is given exactly what it needs for its work, and not one byte
   more.** No storage credentials — presigned links to its own prefix instead.
   No ServiceAccount token. No backend token. The git token lives for an hour
   and opens one repository.
2. **Everything the pod produces is durable before it is announced** (P5). The
   webhook is an optimization, not a correctness condition.
3. **What the pod says about itself is data, not truth.** Cost, tokens and
   duration are self-declared. The backend checks them against what it observes.

---

## 2. What the pod receives

### 2.1 Environment variables

The controller sets these on the agent container. All of them are
**non-secret**, and that is a property rather than a coincidence: they are
visible in `kubectl describe pod`, they land in `/proc/self/environ`, and they
are inherited by every child process, including the agent itself.

| Variable | Req. | Source | Meaning |
|---|---|---|---|
| `HALIPHRON_CONTRACT` | yes | controller | The major version of the runtime contract the controller expects. A mismatch with the image's own means exit 30 before anything else |
| `HALIPHRON_RUN_ID` | yes | `spec.runID` | The run's ULID; the correlation key for everything — logs, metrics, objects in storage |
| `HALIPHRON_ATTEMPT` | yes | `status.attempt` | The attempt number, from 1. Decides whether to read the checkpoint |
| `HALIPHRON_CLUSTER_ID` | no | controller | For logs and for investigating incidents across several clusters |
| `HALIPHRON_CALLBACK_URL` | yes | controller | Where to send the report. Cluster-local; the backend cannot know it |
| `HALIPHRON_GRACE_SECONDS` | yes | controller | The pod's `terminationGracePeriodSeconds`. Provided so the entrypoint can plan its shutdown rather than guess how long it has between SIGTERM and SIGKILL |
| `HALIPHRON_AGENT` | yes | `spec.agent` | `claude-code` or `codex`; the runtime switch |
| `HALIPHRON_MODEL` | yes | `spec.model` | A provider-qualified identifier |
| `HALIPHRON_ROLE` | no | `spec.role` | The role name; determines which files the chain in section 7 looks for |
| `HALIPHRON_TIMEOUT_SECONDS` | yes | `spec.runtime.timeoutSeconds` | The budget for the **`run` phase**, not for the whole pod |
| `HALIPHRON_MAX_TURNS` | no | `spec.runtime.maxTurns` | |
| `HALIPHRON_PERMISSION_MODE` | no | `spec.runtime.permissionMode` | Already reconciled with the policy ceiling; the entrypoint does not recompute it |
| `HALIPHRON_ALLOWED_TOOLS` | no | `spec.toolPolicy.allow` | Comma-separated |
| `HALIPHRON_DENIED_TOOLS` | no | `spec.toolPolicy.deny` | Comma-separated |
| `HALIPHRON_REPO_URL` | no | `spec.repo.url` | Empty means a run without a repository, which is a legal case |
| `HALIPHRON_GIT_PROVIDER` | no | `spec.repo.provider` | `github`, `gitlab`, `none` |
| `HALIPHRON_BASE_BRANCH` | no | `spec.repo.baseBranch` | |
| `HALIPHRON_TARGET_BRANCH` | no | `spec.repo.targetBranch` | Generated by the backend, deterministically. The agent does not invent the branch name |
| `HALIPHRON_CLONE_DEPTH` | no | `spec.repo.cloneDepth` | 0 means a full clone |
| `HALIPHRON_SUBMODULES` | no | `spec.repo.submodules` | |
| `HALIPHRON_LFS` | no | `spec.repo.lfs` | |
| `HALIPHRON_CREATE_PR` | no | `spec.repo.createPR` | |
| `HALIPHRON_PROMPT_SHA256` | yes | `spec.prompt.sha256` | The digest of `prompt.txt`. A mismatch means exit 30 |
| `HALIPHRON_LOG_CHUNK_SECONDS` | no | `spec.observability.logChunkIntervalSeconds` | The log chunk upload interval |
| `HALIPHRON_OTLP_ENDPOINT` | no | `spec.observability.otlpEndpoint` | Empty means no spans are emitted; `phaseTimings` remain |
| `HALIPHRON_TRACEPARENT` | no | `spec.observability.traceparent` | The parent context: workflow → step → attempt |
| `HALIPHRON_STORAGE_BUCKET` | no | controller | Informational: the pod reaches storage only through presigned links |
| `HALIPHRON_STORAGE_PREFIX` | no | controller | Informational, for log readability |
| `HALIPHRON_IMAGE_VERSION` | no | image | Baked in at build time, returned in the report |

The list is normative: it also lives as `ContractEnv` in
[api/run/v1/runtime.go](../../api/run/v1/runtime.go), and the contract test
fails the build if this table and the constants have drifted apart.
Documentation that can drift from the code is not documentation, it is a
hypothesis.

Runtime-specific variables (`ANTHROPIC_*`, `OPENAI_*`, `CLAUDE_CONFIG_DIR`,
`CODEX_HOME`) are set **by the entrypoint, not the controller** — from the
secrets and from `HALIPHRON_MODEL`. Their names belong to the agent's CLI, and
the controller has no business knowing that for codex the key is called
`OPENAI_API_KEY`: tomorrow a third runtime with a fourth name appears, and what
would have to change is the controller rather than the image.

### 2.2 Secret material as files, not variables

The per-run Secret is **mounted as a volume** at `/haliphron/secrets/`, mode
`0400`, owned by the agent's user.

This is a delta against contract 2, which had `envFrom` (R1), and it has two
independent reasons, either of which would have sufficed.

**Mechanical.** The Secret's keys are fixed by contract 1: `git-token`,
`llm-api-key`, `mcp.json`, `presigned.json`, `callback-token`. Not one of them
is a valid environment variable name. `envFrom` silently skips such keys,
leaving an Event in the namespace — which means the pod starts with no git
token, no MCP configuration and not a single link to storage, and the first
intelligible message about it appears in the `fetch` phase as "could not
download the prompt".

**Substantive.** Environment variables are inherited. The agent is a process we
start ourselves inside the pod, and section 1 says of it that it may do anything
available to it. It needs the write presigned links, the callback token and the
git token for nothing at all: the entrypoint handles those before and after the
run. In files the agent **can** read but does not get for free, they at least do
not land in the log at the model's first `env` in its output.

This is protection against accident, not against malice. There is no isolation
inside a single pod and there cannot be: the agent has a shell. The boundary
runs not here but along the scope of the secrets themselves — an hour-long
repo-scoped token, presigned links to its own prefix, no ServiceAccount token.

**What the entrypoint exports to the agent's child process:** the model key
under the name the CLI expects, and the MCP header values (section 8). Nothing
else. The rest stays in the entrypoint's environment and in files.

### 2.3 Filesystem layout

```
/workspace/                          the repository clone; the agent's working directory
/workspace/.haliphron/               exchange with the agent
/workspace/.haliphron/output.json      ← written by the agent: the payload only
/workspace/.haliphron/artifacts/       ← written by the agent: files that must be kept
/workspace/.haliphron/inputs/          → phase 3: artifacts from previous steps
/haliphron/role/                     the role ConfigMap, read-only
/haliphron/secrets/                  the run Secret, read-only, 0400
/haliphron/run/                      the entrypoint's private state
/home/agent/                         HOME, emptyDir: CLI caches, gh/glab configs
/tmp/                                emptyDir
```

**The clone occupies all of `/workspace`, not a subdirectory.** Agents write
paths relative to the working directory, and every extra level makes "the file
`src/main.go`" ambiguous between the repository and the pod. The price of this
decision is that `.haliphron` ends up inside the git working tree, where the
forced commit of the `commit` phase would pick it up. The entrypoint is
therefore **obliged** to append `.haliphron/` to `.git/info/exclude` right after
the clone, before the agent starts (R13). Not to `.gitignore`: that is a
repository file, and editing it is a change that would ride along into the PR.

**`/haliphron/run/` is outside the working tree and outside everything the agent
is pointed at.** That is where the downloaded prompt, the accumulating log and
the checkpoint live. A prompt injection that talks the agent into "rewriting its
instructions" will rewrite a file nobody else reads.

**There are exactly four writable directories** — `/workspace`,
`/haliphron/run`, `/home/agent`, `/tmp` — and all four are emptyDir. The
container root is **read-only** (R6). That is achievable rather than a pious
wish, under one condition: every CLI cache is redirected into `$HOME` through
`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `XDG_CACHE_HOME`, `npm_config_cache`. This is
verified by running the image under `docker run --read-only` with three tmpfs
mounts, not by argument — see the checklist in section 18.

---

## 3. Phases

`state.json` is keyed by phase names, `phaseTimings` in the report enumerates
the same ones, and the UI groups its timeline by them. The names are contract,
not logging.

| # | Phase | What it does | Failure |
|---|---|---|---|
| 1 | `init` | Runtime identity, directory preparation, `.git/info/exclude` | 30 |
| 2 | `validate` | Checks the variables and the presence of the secret files | 30 |
| 3 | `fetch` | Presigned GET of `prompt.txt`, sha256 check | 21 / 30 |
| 4 | `checkpoint` | Presigned GET of `state.json`; a 404 is a normal answer | 21 |
| 5 | `auth` | Model credentials, git credential helper, `gh`/`glab` | 30 |
| 6 | `clone` | Clone, checkout base, create the target branch | 20 / 30 |
| 7 | `role` | The resolution chain, intersected with the policy ceiling | 30 |
| 8 | `mcp-prepare` | Render the MCP configuration for the runtime, secrets by reference | 30 |
| 9 | `mcp-verify` | Check that the servers came up | 30 |
| 10 | `run` | The agent CLI under a timeout, tee'd to the log and chunked to storage | 10 / 11 |
| 11 | `parse` | Normalize the runtime's output into `result.md` and `usage` | 10 |
| 12 | `output` | Wrap the agent's output in the envelope, validate against the node schema | 12 |
| 13 | `persist` | PUT `result.md`, `output.json`, the log, `state.json` | 21 |
| 14 | `commit` | Commit any leftover changes | 20 |
| 15 | `push` | `push --force-with-lease` to the deterministic branch | 20 |
| 16 | `pr` | create-or-update the PR/MR | 20 |
| 17 | `finalize` | PUT the final log, `completion.json`, the last `state.json` | 21 |
| 18 | `notify` | POST the report to the controller | — |

### Why `persist` comes before git rather than after

In the architecture document the upload stood fifteenth, between `pr` and
`notify`. The "upload before notification" ordering is correct there and is
preserved, but it is not enough, and a single scenario shows why (R2):

> The agent worked for forty minutes and twenty dollars. The `push` phase
> failed: a protected branch, an expired token, a network glitch at the forge.
> Class `git`, retryable. The second attempt reads the checkpoint, sees
> `run: ok`, skips the model — and **finds the result nowhere**: `result.md` and
> `output.json` existed only in the filesystem of a pod that has already been
> deleted.

An idempotent retry (ADR 19) promises not to pay for the model twice. That
promise is not kept if what becomes durable is only what has already survived
the very phases that fail. `persist` therefore runs **immediately after the
result is obtained**, and before anything that is entitled to fail with a
retryable class.

`finalize` remains: it appends the log of the git phases, writes
`completion.json` and the final checkpoint. But everything that has been paid
for is already stored by then.

### Skips are a normal outcome

`skipped` is a full-fledged `outcome`, not a synonym for failure. A run without
a repository skips `clone`, `commit`, `push` and `pr`. A resumed attempt skips
everything up to and including `run`. A run with no declared node schema passes
`output` in a single step. A phase missing from `phaseTimings` and a phase
marked `skipped` are two different messages, and the second is more useful.

---

## 4. Exit codes

The one channel that survives everything: the controller sees it from the pod's
status even if the pod never managed to say anything.

| Code | Class | Meaning | Auto-retry |
|---|---|---|---|
| 0 | `none` | Success: the agent CLI exited zero. Nothing more is implied | — |
| 10 | `agent` | The agent CLI exited non-zero | No |
| 11 | `agent` | The `run` phase timed out | No |
| 12 | `agent` | `output.json` is missing or does not pass the node schema | No |
| 20 | `git` | Clone, push or PR failed for a surmountable reason | Yes |
| 21 | `infra` | A presigned GET or PUT failed | Yes |
| 30 | `config` | Incomplete configuration, an insurmountable access failure, MCP servers did not come up | No |
| >128 | `infra` | A signal: OOMKilled, eviction, drain, cancellation | Yes |

The table is normative and matches `FailureClassForExitCode` in
[api/run/v1/phase.go](../../api/run/v1/phase.go) — a test checks it.

**Code 21 is a delta (R3).** Without it an expired presigned bundle is expressed
as code 30, which is not retried, and a run whose result has already been
obtained dies for good — even though the controller knows how to fix exactly
this breakage: it reissues the bundle before any attempt past the first (D5 of
contract 1). This is the one failure the cluster repairs by itself, and it is
obliged to be retryable.

### The pod determines the class, not the controller

The rule from which the rest follows: **a failure is classified by whoever has
the cause, not the effect.** The controller sees "exit 20". The pod sees "the
forge answered 403 to a push into a protected branch".

The entrypoint must therefore distinguish, inside the git phases (R11):

- network, 5xx, a ref conflict, an exhausted rate limit → **20**, a retry makes
  sense;
- 401, 403, 404 on clone or push, no permission to create a PR → **30**, a retry
  would reproduce the same answer three times and spend three pod runs.

The controller may reclassify only what it observes itself: OOM, eviction,
cancellation. Everything else it takes as given — and that is precisely why
`reason` and `message` in the report (section 10) are not decorative: they are
the only instance of the evidence, and it exists once, in the pod, at the moment
of failure.

---

## 5. The prompt

The prompt travels through storage rather than through the Secret: a workflow
step's context has no natural ceiling, and a Secret is capped at 1 MiB across
all keys together.

The `fetch` phase: a presigned GET of the `prompt.txt` key from the bundle, then
a **sha256 check against `HALIPHRON_PROMPT_SHA256`**. A mismatch means exit 30,
class `config`, `reason: PromptDigestMismatch` (R12). A retry does not help and
is not requested: a run executing something other than what passed admission is
worse than a run that never started. The case is not hypothetical — overwriting
the object under the same key when a task is resubmitted costs exactly one PUT.

The prompt is passed to the CLI **over stdin or via a file, never as a
command-line argument**: arguments are visible in `ps` to any process in the pod,
they land in dumps and in error messages, and they hit `ARG_MAX` on a long
workflow context.

The role's system prompt is a separate entity; it arrives as a file from
`/haliphron/role/` and is supplied through the runtime's own mechanism
(section 8). Once a node declares an output schema, the "return JSON of this
shape" requirement is appended there automatically, from the schema.

---

## 6. The checkpoint and idempotent retries

`runs/{runID}/state.json` is the only object the pod both writes and reads.
Neither the controller nor the backend parses it: this is runtime-internal
state, moved outside only because there is nowhere else to keep it — a pod's
filesystem dies with the pod.

**Exactly one phase is resumed — `run`.** It is the only one whose repetition
costs money, and the only one whose result is already durable by the time the
checkpoint is read (section 3). The rest are cheaper to replay than to trust a
record of: the previous attempt's clone vanished with its pod, and a branch
marked as pushed could have been overwritten by a human since.

**Resume conditions, all mandatory:**

- `state.runID` matches `HALIPHRON_RUN_ID`;
- `state.attempt` is strictly less than `HALIPHRON_ATTEMPT`;
- the major of `state.contractVersion` equals the image's own;
- `phases.run.outcome == "ok"`;
- `artifacts.result` and `artifacts.output` are present and readable.

Any doubt is resolved in favor of a full run. An extra bill for the model is
money; a wrongly resumed attempt is an incorrect result reported as correct, and
it will not be noticed straight away.

`log.nextChunk` is not reset between attempts (R9). Numbering from zero would
overwrite the previous attempt's chunks, and the user would see two runs spliced
together as one, seamlessly.

The schema is [state.schema.json](../../api/runtime/v1/state.schema.json).

---

## 7. Role resolution and the policy ceiling

The chain from section 10 of the architecture, executed in the `role` phase,
after the clone — because the first two steps do not exist before it:

1. `$REPO/.claude/settings.<role>.json`
2. `/haliphron/role/settings.<role>.json` — from the ConfigMap
3. `$REPO/.claude/settings.json`
4. built-in defaults

Then the intersection:

```
effective = resolved ∩ HALIPHRON_ALLOWED_TOOLS \ HALIPHRON_DENIED_TOOLS
```

A repository cannot widen its own permissions, only narrow them. The
intersection is computed by the entrypoint, and until there is egress filtering
by domain **this ceiling remains the only barrier, and it is a software one**.
An accepted risk, not an unclosed hole.

`/haliphron/role/output.schema.json` is a **reserved key** (R8): the structured
output schema declared by the workflow node. It is not a property of the role,
but it travels in the same ConfigMap because the same backend renders it, it is
mounted read-only the same way, and it is consumed once. A role that puts a file
under that name will lose it — that is the price of the reservation and the
reason it is named here rather than agreed verbally. A separate lease field is
introduced in phase 3, once nodes begin declaring schemas in earnest.

---

## 8. Two runtimes, one image

Not two images. Git, PRs, uploads, notification, the checkpoint, log redaction —
all shared. Four things differ: authentication, preparing the MCP configuration,
the launch command and the output parser.

| haliphron concept | claude-code | codex |
|---|---|---|
| model key | `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | `OPENAI_API_KEY` |
| model | `ANTHROPIC_MODEL` | `OPENAI_MODEL` |
| `toolPolicy.allow` | `--allowedTools` | `enabled_tools` on the MCP server + the sandbox mode |
| `toolPolicy.deny` | `--disallowedTools` | `disabled_tools` on the server |
| `permissionMode: plan` | `--permission-mode plan` | `-s read-only` |
| `permissionMode: acceptEdits` | `--permission-mode acceptEdits` | `-s workspace-write` |
| `permissionMode: bypassPermissions` | `--permission-mode bypassPermissions` | `--dangerously-bypass-approvals-and-sandbox` |
| `mcpServers` | `--mcp-config` (JSON) | `[mcp_servers.*]` in `config.toml` |
| system prompt | `--append-system-prompt` | a prompt prefix |
| cost in the output | `total_cost_usd` | `input_tokens` / `output_tokens` |

**The main trap is the asymmetry.** codex has no analogue of `--allowedTools`
for built-in tools: file and shell access is set wholesale by the sandbox mode.
A "read-only" role is expressed on claude-code as `plan` mode plus a deny list,
and on codex as `-s read-only`. This means the same role on the two runtimes can
grant **different powers**, and that is a security hole, not a cosmetic
discrepancy. The table above is normative, and an equivalence test against it is
mandatory: for every role in the fixture, compare the effective set of permitted
operations on both runtimes.

**Secrets in the MCP configuration.** Header values do not land in the file in
plaintext: they are exported into the child process's environment variables and
substituted by reference (`env_http_headers` in codex, `${VAR}` substitution in
claude-code). The configuration file outlives the pod in log chunks and in
dumps; the child process's env does not.

**One token under several names.** The credential arrives as a single `git-token`
file; `gh` expects `GH_TOKEN`, `glab` expects `GL_TOKEN`, and MCP configs
substitute `${GITHUB_TOKEN}`. The entrypoint exports every name from the one
value. Careful: with `HALIPHRON_GIT_PROVIDER=gitlab` a GitHub MCP would receive
a gitlab token — an incompatible combination is rejected explicitly, with exit
30, rather than discovered through a baffled agent.

**The token does not go into `.git/config`.** Cloning through a URL with the
token in it means leaving it in the repository's configuration, which the agent
will read and send wherever it sees fit. Use a credential helper bound to the
host, and reset the remote to a clean URL after the clone.

---

## 9. Structured output

**The agent writes the payload. The entrypoint fills in the envelope** (R5).

```
/workspace/.haliphron/output.json   ← agent: a single JSON object
              ↓ output phase
runs/{runID}/output.json            ← the envelope plus that object in the data field
```

The architecture required the agent to write the whole file, service fields
included. That is a design error in the interface to the model: every formal
requirement placed on the model is another chance to burn a forty-minute paid
run on a forgotten `schemaVersion`. The machine does not forget; the model does.
Everything mechanical — identifiers, timestamps, status, artifact references —
is filled in by the entrypoint, and what is left for the model is exactly what it
was called for: a meaningful object.

**Whether it is mandatory depends on the node.** The node declared a schema
(section 7) → the file is mandatory and is validated, and a failure is exit 12.
No schema → a missing file is normal and `data` becomes `{}`. In v1 there are no
workflows, so there are no consumers of structured output either, and failing a
successfully completed run over a file nobody will read is pure loss. This is a
delta against section 8 of the architecture, where the requirement was
unconditional.

The envelope's `status` is derived from the exit code rather than declared by the
agent: `ok` (0), `partial` (11 — a timeout, but there is work), `failed`
(everything else). The agent's self-assessment in that field would be worth
exactly as much as its assessment of its own cost.

`result.md` is the human-readable summary, kept separately. The entrypoint
writes it **always**, even if the agent produced no text: on empty output, a
generated stub with the exit code and phase timings. A missing object under a
fixed key breaks the read path for the backend and the UI, and "the agent said
nothing" is information too.

The envelope schema is
[output.schema.json](../../api/runtime/v1/output.schema.json).

---

## 10. Logs and redaction

**There is no live `kubectl logs -f`** (the decision on question 5 of the
architecture). The entrypoint writes the agent's combined stream to a file and
uploads the increments in chunks.

- Key: `runs/{runID}/logs/chunks/{seq}.log`, where `seq` is six digits with
  leading zeros. The padding is not cosmetic: S3 listing is lexicographic, and
  without it the tenth chunk would sort between the first and the second.
- The interval is `HALIPHRON_LOG_CHUNK_SECONDS` or an accumulated size,
  whichever comes first.
- `seq` runs continuously across attempts (section 6).
- `logs/agent.log` is the concatenation of the same bytes, uploaded during
  `finalize`. A UI that has read the chunks to the end and switched to the final
  log must see neither a gap nor a repetition.

**Redaction is mandatory** (R14). The entrypoint builds a set from every secret
value at least eight characters long and passes **every byte it uploads**
through the filter: the chunks, `agent.log`, `result.md`, and `summary` in the
report. A match is replaced with `***`.

The reason is simple. A secret that lands in the log becomes durable and travels
to storage with a thirty-day retention policy, and it gets there through normal
operation: a model debugging a failed request prints the headers; `set -x` in a
repository script prints the arguments; an MCP server prints its configuration at
startup. The filter does not protect against an agent that wants to exfiltrate a
secret — it protects against all the other ways, and those are the majority.

---

## 11. Git

- **The backend generates the branch name**, deterministically from the `runID`:
  `haliphron/{run_short}-{slug}`. A name invented by the agent produces a second
  branch and a second PR on the second attempt — which is exactly the
  non-idempotency the whole retry path depends on not having.
- **The entrypoint commits, not only the agent.** The agent's commits are kept as
  they are; leftover changes are committed by force. Otherwise the work of a
  crashed or interrupted agent is lost entirely.
- **`push --force-with-lease`**, not `--force`: the run's branch belongs to the
  run, but overwriting someone else's work without a single check is not the
  behavior one wants by default.
- **create-or-update for the PR.** On a retry the PR already exists: find and
  update it rather than failing. The agent may propose the PR title in
  `output.json` — the title does not affect idempotency.
- **Direct writes to a protected branch never happen.** The only channel for
  changes is a PR. This is also the main control against prompt injection from
  the repository: whatever the agent does, a reviewer will see it.

---

## 12. The completion report

The shape is [openapi.yaml](../../api/runtime/v1/openapi.yaml) and
[completion.go](../../api/run/v1/completion.go). It is **the same schema** as
`CompletionReport` in the Cluster API: the controller forwards the report to
`/ingest/completion` unchanged, merely wrapping it in an envelope carrying the
cluster identity and epoch. The match is held by a test, not by agreement.

The report leaves **in two copies**: as a webhook to the controller, and as the
`runs/{runID}/completion.json` object. Byte-for-byte identical — either may turn
out to be the one that survived. The copy in storage costs one PUT and closes
the one scenario in which the cost and the PR link are lost: the controller
crashed between accepting the webhook and forwarding it, and neither `result.md`
nor `output.json` contains those fields.

**What was added to contract 1's schema** (R4): `runID` and `attempt` inside the
report — a `completion.json` lifted out of the bucket arrives without an
envelope, and a report that cannot name its own run is not a backup copy;
`failedPhase`, `reason`, `message` — section 4; `runtime` with the versions of
the image, the contract and the CLI — without them a behavior change across a
fleet of clusters on different tags is diagnosed by guesswork.

**Delivery.** At-least-once, with the receiver idempotent on
(`runID`, `attempt`). Authentication is the `callback-token`, minted by the
**controller**: `callbackURL` is cluster-local, and without a token any pod in
the agent namespace could send a forged completion for somebody else's run. The
controller checks the token, the token's binding to the run, and the `runID` in
the body.

Pod retries: up to five attempts with exponential backoff, no longer than 60
seconds in total. **An undelivered webhook does not change the exit code** (R10).
The report is not the result of the work but a message about it; the result is
already in storage, the outcome is visible to the controller through the Job's
exit code, and the contents will be lifted by the backend from
`runs/{runID}/`. Failing a successful run because the controller restarted would
be substituting the means for the end.

---

## 13. Timeout, cancellation, eviction

**`HALIPHRON_TIMEOUT_SECONDS` is the budget for the `run` phase, not for the
pod.** Cloning a monorepo and uploading a two-gigabyte log must not eat into the
time allotted to the model. The Job's `activeDeadlineSeconds` is deliberately
larger and serves as a backstop — precisely so that the normal timeout has time
to upload the partial result.

**On a timeout the run is not cut off dry:** SIGTERM to the agent, 30 seconds,
SIGKILL, after which the entrypoint **goes through the remaining phases** — it
parses the partial output, saves it, commits, pushes, opens the PR — and exits
with code 11. Forty minutes of work that did not fit into an hour cost the same
as work that did, and there is no reason to throw them away.

**A SIGTERM from outside** — cancellation, eviction, drain — arrives with a
budget of `HALIPHRON_GRACE_SECONDS`, and within that budget the entrypoint does,
in this order:

1. stop the agent;
2. `persist` — the result into storage;
3. `push`, if the clone has commits and at least half the remaining budget is
   left;
4. `notify` with status `cancelled`.

The order is a priority by value: the durable result, then the code, then the
notification. The push is capped at half of the remainder because a push
interrupted by SIGKILL halfway leaves the branch in a state the next attempt
would spend longer untangling than a full repeat would have cost.

Cancellation is indistinguishable from eviction by exit code — both give 143.
What distinguishes them is `status` in the report, and if the report did not
arrive, the controller, which knows it ordered the kill.

---

## 14. Security inside the pod

Section 14 of the architecture defines the perimeter; here is what the image is
responsible for.

| Control | How |
|---|---|
| non-root | `runAsUser 1000`; the image requires no writes outside the four emptyDirs |
| read-only root | CLI caches redirected into `$HOME` (section 2.3) |
| no ServiceAccount token | `automountServiceAccountToken: false`; the image never reaches the cluster API with anything |
| no storage credentials | presigned links to its own prefix only |
| no token in `.git/config` | a credential helper, remote reset after the clone |
| no secrets in the agent's environment | a volume instead of `envFrom`, a reduced environment for the child process |
| no secrets in the logs | a redaction filter on everything uploaded |
| no secrets in the MCP configuration | substitution by reference to a variable |
| tool ceiling | intersection with the policy in the `role` phase |
| early exit without tools | `mcp-verify` → 30 |

The last one deserves to be said plainly: **an agent that did not get the tools
it was promised does not fail — it cheerfully does something else.** A run that
"succeeded" and produced a plausible but invented result is more expensive than
an explicit failure at startup, because the failure is visible immediately and
the invention only at review, if you are lucky.

---

## 15. Versions

**The contract.** `HALIPHRON_CONTRACT` carries the major the controller expects;
the entrypoint compares it with its own and, on a mismatch, exits 30 before
anything else (R7). The major changes when the meaning of something existing
changes: a phase name, the semantics of an exit code, the directory layout. The
minor is additive by definition — a new optional variable, a new report field —
and an image that has not heard of it behaves exactly as before.

An image a major behind looks, without this check, like a run that "for some
reason cannot find the prompt". With the check, it looks like
`reason: ContractMismatch` in the UI.

**The image.** Pinned by digest in `spec.image`: reproducibility of the second
attempt matters more than the convenience of a moving tag. A role may declare its
own image on top of the base one — a team whose agents work with terraform and
kubectl needs those binaries inside. A base image plus overlays, not a universal
monster; an overlay is obliged to preserve the entrypoint and the contract, and
that is checked by the same test suite.

**Why not "upstream image + entrypoint from a ConfigMap"** (the original
specification's option): upstream images have no `git`, no `gh`/`glab`, no `jq`;
versions drift uncontrolled; and fifteen hundred lines of shell through a
ConfigMap means no versioning, no tests and a 1 MiB limit. Our own image, our own
tag, our own tests.

**The base is Debian slim, not Alpine.** Claude Code needs Node, and a full
glibc `curl` and GNU coreutils are required.

---

## 16. One container, not two

The specification assumed an init container for the clone. The decision is **one
container**: the entrypoint does the clone. The clone requires the same
credential helper, the same git identity and the same user as the subsequent
commit and push; splitting it across containers forces the setup to be
duplicated and synchronized through an emptyDir. There is no gain, and three
points of divergence.

An init container is justified in one case — **downloading the input
artifacts** of previous workflow steps into `/workspace/.haliphron/inputs/`: a
separate responsibility with separate credentials. It is introduced in phase 3,
together with workflows.

---

## 17. Deltas

**The edits are applied.** The table is a log: it explains why the listed files
say what they say, and what to look at during review.

| # | What | Was | Became | Where applied |
|---|---|---|---|---|
| R1 | The per-run Secret in the pod | `envFrom` | a volume at `/haliphron/secrets/`, 0400 | `agentrun-crd.md` §11 |
| R2 | When the result is uploaded | after `pr`, before `notify` | the `persist` phase right after `output`, before the git phases | `architecture.md` §11, `runtime.go` |
| R3 | A storage failure | code 30, class `config`, not retried | code 21, class `infra`, retried | `phase.go`, `architecture.md` §12.2 |
| R4 | `CompletionReport` | no identification and no failure cause | `runID`, `attempt`, `failedPhase`, `reason`, `message`, `runtime` | `api/cluster/v1/openapi.yaml`, `completion.go` |
| R5 | `output.json` | the agent writes the whole file, always mandatory | the agent writes the payload; the envelope is the entrypoint's; mandatory when the node declares a schema | `architecture.md` §8, `output.schema.json` |
| R6 | `readOnlyRootFilesystem` | "not always achievable" | `true`, by moving the CLI caches into `$HOME` | `architecture.md` §14, `agentrun-crd.md` §11 |
| R7 | The contract version | none | `HALIPHRON_CONTRACT`, major checked at startup | `runtime.go` |
| R8 | The node's output schema | nothing said about how it reaches the pod | the reserved `output.schema.json` key in the role ConfigMap | `runtime.go` |
| R9 | Log chunk numbering | unspecified | continuous across attempts, six digits with leading zeros | `state.schema.json` |
| R10 | An undelivered webhook | unspecified | does not change the exit code | `api/runtime/v1/openapi.yaml` |
| R11 | Classification of git failures | all git → 20, retried | 401/403/404 → 30; surmountable → 20 | this document, §4 |
| R12 | Checking the prompt digest | "makes it possible to verify" | mandatory, a mismatch → 30 | `runtime.go` |
| R13 | `.haliphron` in the working tree | not considered | `.git/info/exclude` before the agent starts | this document, §2.3 |
| R14 | Redacting secrets from what is uploaded | not considered | a filter on every byte going to storage | this document, §10 |
| R15 | The agent child process's environment | inherits everything | reduced to the model key and the MCP headers | this document, §2.2 |
| R16 | The name of the runtime switch | `AGENT_TYPE` (from nib) | `HALIPHRON_AGENT` | `runtime.go` |

---

## 18. Contract test checklist

The image is verified **without a cluster and without a backend**: `docker run`,
a directory of secret files instead of a volume, MinIO and an HTTP stub instead
of the controller. That harness — `FakeControlPlane` — ships as part of the
contract, like `FakeBackend` and `FakeController` in the first two: without it
the "image" track cannot proceed in parallel.

**Configuration and startup:**

- [ ] `HALIPHRON_CONTRACT` a major above the image's own → 30, `reason: ContractMismatch`, the agent never started
- [ ] a required variable is missing → 30 before any network call
- [ ] a secret file is not in place → 30, with the file name in the message rather than "permission denied"
- [ ] `docker run --read-only` with tmpfs on the four directories → the run completes end to end
- [ ] the container starts as non-root and requires no write outside an emptyDir

**Prompt and storage:**

- [ ] the prompt digest did not match → 30, `PromptDigestMismatch`, the model was never called
- [ ] a presigned GET of `state.json` returned 404 → that is `skipped`, not a failure
- [ ] a presigned PUT returned 403 (an expired signature) → 21, not 30
- [ ] promptURL is unreachable over the network → 21, retryable

**Resumption:**

- [ ] `state.json` with `run: ok` and an attempt lower than the current one → the `run` phase is skipped, the model was never called
- [ ] `state.json` from a different `runID` → a full run, somebody else's state is not used
- [ ] `state.json` with a higher contract major → a full run
- [ ] `state.json` with `run: ok` but an unreachable `artifacts.result` → a full run
- [ ] a resumed attempt continues the chunk numbering instead of overwriting earlier chunks

**Ordering and durability:**

- [ ] `push` fails after a successful run → `result.md` and `output.json` are already in storage
- [ ] a failure at `pr` → everything but the PR link is in storage; the second attempt does not pay for the model
- [ ] `completion.json` in storage is byte-for-byte equal to the webhook body
- [ ] the controller is unreachable for all 60 seconds of retries → the exit code is the same as it would have been on successful delivery

**Git:**

- [ ] the token does not appear in `.git/config` after the clone
- [ ] `.haliphron/` did not make it into a single commit
- [ ] the leftover changes of a crashed agent are committed and pushed
- [ ] a repeat run against an existing PR updates it, `prAction: updated`
- [ ] a 403 on a push to a protected branch → 30, not 20
- [ ] a 500 from the forge on push → 20
- [ ] a run without `HALIPHRON_REPO_URL` → the four git phases are `skipped`, the run succeeds

**Agent and output:**

- [ ] an MCP server did not come up → 30 before the model is started
- [ ] `HALIPHRON_GIT_PROVIDER=gitlab` together with a GitHub MCP → 30, with an explicit message
- [ ] the node schema is declared and the file is missing → 12
- [ ] the node schema is declared and the file does not match it → 12, with the path to the mismatch in `message`
- [ ] no schema and no file → success, `data: {}`
- [ ] the agent produced no text → `result.md` exists and contains the stub
- [ ] `usage` is normalized: claude-code gives cost, codex gives tokens, and the fields are present for both

**Timeout and signals:**

- [ ] a `run` timeout → 11, but `persist`, `commit`, `push` and `pr` have been passed
- [ ] SIGTERM in the middle of a run → the result is uploaded, the report carries `status: cancelled`, and it fits in the grace period
- [ ] SIGTERM with `HALIPHRON_GRACE_SECONDS=10` → `push` is skipped, `persist` and `notify` are performed

**Redaction:**

- [ ] the agent prints the contents of `/haliphron/secrets/` → not one log chunk contains the values
- [ ] a secret in `result.md` → absent from the report's `summary` as well

**Runtime equivalence:**

- [ ] for every role in the fixture, the effective set of operations on claude-code and on codex is the same
- [ ] a "read-only" role cannot write a file into `/workspace` on either runtime

---

## 19. Deferred

| # | Question | Decision | Condition for revisiting |
|---|---|---|---|
| 1 | `opencode` as a third runtime | Not in v1. `HALIPHRON_AGENT` is a ready-made extension point | When there is demand. It would need a row in the role translation table and an equivalence test |
| 2 | A repository cache | Not doing it. Cloning a monorepo on every run costs minutes and traffic | When clone time becomes a noticeable share of the duration. The solution: a PVC with a bare mirror and `--reference` |
| 3 | Live log viewing through `kubectl logs -f` | Not doing it. Chunks work identically for a local cluster and for a cluster behind NAT, and they outlive the pod | If the upload-interval latency becomes a complaint — shorten the interval, it is a parameter |
| 4 | Verifying cost inside the pod | Not doing it: the pod is an interested party and its arithmetic is untrustworthy by construction | Once there is a proxy in front of the LLM; accounting moves there and becomes unforgeable |
| 5 | Input artifacts of workflow steps | An init container, separate credentials, `/workspace/.haliphron/inputs/` | Phase 3, together with the workflow engine |
| 6 | Signing the report with a per-pod key | Not doing it: `callback-token` gives the same origin guarantee inside the cluster, and per-pod key management is a separate operational burden | If a non-repudiation requirement for reports appears |
