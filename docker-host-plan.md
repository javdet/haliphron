# Docker host runtime — implementation plan

Status: plan, not started. Branch `docker-host-controller`.

## 1. Goal

Run agents on **one Linux host with Docker installed**, with the same behaviour the
Kubernetes runtime gives: the same lease/ack/heartbeat/report semantics, the same agent
image, the same failure classes and retries, the same isolation of the agent from
everything that is not its own work.

To the backend a Docker host is **one more cluster**. It registers with a bootstrap
token, leases work for itself, and reports back over the unchanged Cluster API. Placing
work on that host uses the existing mechanisms: an explicit `cluster`, a role's
`clusterSelector`, or the default cluster.

## 2. Decisions taken

1. **A new module, `dockerhost/`, with the binary `haliphron-dockerhost`.** The
   Kubernetes controller is not modified. The new module imports only the packages of
   `controller/` that do not depend on Kubernetes (section 4.1). Extracting a shared
   core waits until both implementations have settled (section 15).
2. **runc only.** No gVisor/Kata support. The isolation comes from the container
   hardening in section 6.3 and the network rules in section 7.
3. **Delivered as a container run by docker compose**, with the Docker socket mounted.
   An install script prepares the host: directories, the agents network and the
   firewall rules.
4. **The network policy is installed by the script and checked by the controller.** The
   controller has no firewall privileges. It refuses to take work while the check
   fails (section 7.4).
5. **The image does not change.** Contract 3 (`agent-runtime.md`) contains nothing
   specific to Kubernetes that the image depends on. The container gets exactly the
   environment, files and mounts listed in that contract.
6. **The backend change is additive and optional for correctness.** A `runtimeKind`
   field at registration, for the UI and operators (section 9). Without it a Docker host
   would still work, because `agentNamespace` is reused (section 9).
7. **Secret material is not encrypted at rest** in the local store (section 5). It is
   protected by file permissions, matching a default Kubernetes installation.
8. **Two placements of the control plane are supported.** It may run on a separate
   management host, or on the same host as the agents. The second case gets its own
   compose file and its own network assertions (sections 7.5 and 13).
9. **Name:** module `dockerhost/`, binary and image `haliphron-dockerhost`.

## 3. What the Kubernetes controller relies on, and its replacement here

| Kubernetes provides | Used for | Docker host replacement |
|---|---|---|
| `AgentRun` CR in etcd | the controller's durable memory: spec, status, retry budget, report bookkeeping | a row in a local SQLite store (section 5) |
| annotations (ack, cancel, abandon, leased-at) | commands from outside, surviving restarts | columns on the same row |
| per-run `Secret`, mounted 0400 | secret material as files | files on a host tmpfs, bind-mounted read-only (section 6.2) |
| role `ConfigMap` | the role's fallback config | a read-only bind-mounted directory |
| four `emptyDir` volumes | the only writable paths | four named Docker volumes per attempt (section 6.2) |
| `Job` + pod status | execution, exit code, OOM, eviction | a container plus `docker inspect` and the events stream |
| `activeDeadlineSeconds` | backstop for a hung entrypoint | a deadline the controller enforces, measured from a start time it has persisted |
| `ImagePullBackOff` + startup deadline | an image that never arrives | the controller pulls the image itself, bounded by the startup deadline |
| ownerReference + GC | cleanup | the reaper removes the container, its volumes, the run directory and the row |
| leader election | one active controller | `flock` on the state directory |
| `NetworkPolicy` | agent isolation | a bridge network with ICC disabled, plus iptables rules (section 7) |
| ResourceQuota / `QuotaExhausted` | capacity ceiling | host resource accounting (section 8) |
| `PREFLIGHT_JOB` dry run | refuse before ack | a local preflight: spec, capacity, disk, network check |
| `kubectl get agentruns` | operator visibility | `haliphron-dockerhost runs list/get` |

## 4. Module layout

```
dockerhost/
  cmd/haliphron-dockerhost/   main; subcommands: serve (default), runs list|get, netcheck
  config/                     HALIPHRON_* environment, validated at start
  state/                      SQLite store: runs, materials, schema migrations
  identity/                   file-backed identity.Store
  materialize/                lease → preflight → durable row → ack
  launcher/                   pure function: (run, attempt, phases) → container create config
  docker/                     narrow interface over the Engine API + the real client
  reconcile/                  the phase table, deadlines, cancel/abandon, retries, TTL reaper
  callback/                   /completion, /phase, /artifacts for the agent container
  report/                     heartbeat, ingest loop, artifact relay, commands
  netcheck/                   the network self-test (section 7.4)
  capacity/                   free slots from host resources (section 8)
  gc/                         images and leftover volumes
  Dockerfile
  README.md
test/dockerhost/              contract suite against fake/backend and a fake Docker
deploy/dockerhost/            compose.yaml, install.sh, firewall.sh, systemd unit, README
docs/contracts/docker-host.md the normative materialization contract (section 11)
```

`go.work` gains `./dockerhost` and `./test/dockerhost`.

### 4.1 What is reused from `controller/` as is

| Package | Why it can be imported |
|---|---|
| `controller/clusterapi` | no Kubernetes imports. Transport, JWT, `Problem.action`: it stays the only place that reads the action |
| `controller/spool` | no Kubernetes imports. The relay spool, its budget and its sweep |
| `controller/identity` (`Identity`, `Bootstrap`, `Store`) | the core is Kubernetes-free. Only `SecretStore` is not, and it is not used |
| `controller/report.Queue` | the bounded observation queue, generic |
| `api/...` | all wire types, `runv1.ContractEnv`, `ArtifactKey`, `AgentEnvName`, the exit-code → phase/class functions |

Cost: `identity` and `report` are packages that also contain Kubernetes code
(`secretstore.go`, `reporter.go`), so the binary links client-go transitively. That is
binary size only. No behaviour changes and no Kubernetes connection is made. It
disappears when the shared core is extracted (section 15).

Everything else is written in the new module: `materialize`, `agentrun`, `launcher`,
`callback` and `report.Reporter` all take a controller-runtime client and CR types as
their state.

## 5. Local state — `dockerhost/state`

SQLite through a pure-Go driver (no CGO; the build stays `CGO_ENABLED=0` like the
controller's). A file at `/var/lib/haliphron/state.db`, mode 0600, owned by root, in a
0700 directory.

One table `runs`, the equivalent of an `AgentRun` plus its Secret and ConfigMap:

| Column | Equivalent |
|---|---|
| `run_id`, `epoch` (PK) | CR name. A new epoch is a new row, the same as "a new epoch is a new CR" |
| `spec` (JSON, `RenderedRunSpec`) | `spec`, immutable |
| `spec_raw_sha256` | the pruning guard (section 6.1) |
| `status` (JSON, the `AgentRunStatus` shape) | `status`: phase, attempt, retry, completedPhases, result refs, `reported` |
| `leased_at`, `acked_at` | `leased-at` annotation, ack annotation |
| `cancel_requested_at`, `cancel_grace_seconds` | cancel annotation |
| `abandoned` | abandon annotation |
| `callback_token` | the `callback-token` Secret key |
| `materials` (JSON: secrets + role files + presigned bundle) | the Secret and the ConfigMap |
| `version` | `resourceVersion`: every write is compare-and-swap on it |
| `created_at`, `terminal_at` | creation timestamp, TTL anchor |

Rules carried over unchanged:

- **Reconciliation remembers nothing.** Every decision is a function of the row and of
  what Docker reports at that moment. Maps in memory are caches only.
- **Commands arrive as column writes**, never as fields in memory. A heartbeat's
  `cancel` sets `cancel_requested_at`; the reconciler acts on it.
- **Nothing starts before the ack.** The container is created only for a row with
  `acked_at` set. A row that never gets one within the ack wait budget is deleted, the
  same as `awaitAck`.
- **Secrets at rest (decision 7).** `materials` sits in the database file in clear
  text, protected by file mode and directory permissions. This matches a default
  Kubernetes installation, where Secrets sit in etcd without encryption at rest. The row,
  and the secrets with it, are removed by the reaper on the same TTL rules as the CR.
  Nothing else on the host gets a copy: the backups the README recommends exclude
  `state.db`, because a lost row costs at most an `Unknown` run the backend already
  knows how to surface.

## 6. Materialization and the container

### 6.1 Lease → row → ack — `dockerhost/materialize`

1. **Preflight**, refusing before anything durable, with the same `AckRejection` codes
   as the Kubernetes controller:
   - `InvalidSpec`: empty image, no prompt, prompt digest mismatch, unparsable
     quantities, an image reference that does not parse, an agent runtime this host does
     not declare;
   - `SpecFieldsPruned`, the Docker counterpart of the CRD pruning trap. The lease's
     `spec` is decoded into `RenderedRunSpec`, which silently drops fields it does not
     know. So the raw `spec` JSON is compared, canonicalised, with the re-marshalled
     struct. A difference means a newer control plane sent a setting this binary would
     ignore, and the lease is refused;
   - `QuotaExhausted`: the run does not fit the host (section 8), or free disk is below
     the floor;
   - `MaterializationFailed`: the network check is failing (section 7.4) or the store
     cannot be written.
2. **One transaction** writes the row: spec, materials, a freshly minted callback token,
   `leased_at`.
3. **Ack.** The rejection paths and `handleAckError` behave exactly as
   `controller/lease` does. `Resume` on start re-acks rows that were written but not
   acknowledged.

### 6.2 What the container gets — `dockerhost/launcher`

A pure function, testable without Docker, from `(row, attempt, completedPhases)` to a
container create request.

**Environment.** Exactly the variables of `agent-runtime.md` §2.1, derived from the spec
by the same rules as `controller/launcher`'s `env()`. `HALIPHRON_PROMPT` is a literal
value in the container config. Contrary to Kubernetes, that makes it visible in
`docker inspect`. Accepted, because anyone who can run `docker inspect` already has root
on the host. Two drift guards, as tests:

- the set of names is a subset of `runv1.ContractEnv`, with every required name present;
- for the same input, the variables equal what `controller/launcher.Builder.Job`
  produces. The test imports the Kubernetes builder and compares name by name. A
  variable added to one side and not the other fails the build.

**Mounts.**

| Path | Source | Notes |
|---|---|---|
| `/workspace` | named volume `haliphron-<run>-<epoch>-a<n>-workspace` | a new volume is initialised from the image's directory, so it is owned by 1000 like the image's |
| `/haliphron/run` | named volume `…-run` | same |
| `/home/agent` | named volume `…-home` | same |
| `/tmp` | named volume `…-tmp` | same, mode 1777 from the image |
| `/haliphron/secrets` | bind mount, read-only: `/run/haliphron/runs/<run>/<epoch>/secrets` | files 0400, owned 1000:1000. The host path is on tmpfs. It is regenerated from `materials` before every attempt and removed with the container |
| `/haliphron/role` | bind mount, read-only: `…/role` | role files 0444 |

The four volumes are disk-backed like the Kubernetes `emptyDir`s (which have no
`medium` and no `sizeLimit` today). They live as long as the attempt and are removed
with its container.

**Same-path rule.** The controller runs in a container but asks the daemon to
bind-mount host paths. So `/run/haliphron` and `/var/lib/haliphron` are mounted into the
controller **at the same paths as on the host**. Otherwise the paths the controller
writes and the paths the daemon mounts differ. `config` checks this at start by writing
a marker file and finding it through `docker inspect` of its own container.

### 6.3 Container settings

```
User              1000:1000
ReadonlyRootfs    true
CapDrop           ALL
SecurityOpt       no-new-privileges:true      (seccomp and AppArmor: the daemon defaults)
Privileged        false; no devices; private IPC and PID namespaces
RestartPolicy     no                          (every attempt is the controller's decision)
AutoRemove        false                       (the exit code must be read first)
StopSignal        SIGTERM; StopTimeout = HALIPHRON_GRACE_SECONDS
Init              not set: the image's ENTRYPOINT is tini
NanoCPUs          runtime.resources.cpu
Memory            runtime.resources.memory; MemorySwap = Memory (no swap)
PidsLimit         from config, default 4096
Ulimits           nofile from config
NetworkMode       haliphron-agents; DNS = the configured resolvers (section 7.2)
LogConfig         local driver, max-size 10m, max-file 2 (configurable; "none" allowed)
Labels            haliphron.io/run-id, epoch, attempt, cluster-id, agent, managed-by
Name              haliphron-<runID>-<epoch>-a<attempt>
```

The name is deterministic, so a create that answers 409 is the controller's own earlier
create. It is adopted if its labels match and treated as a conflict otherwise.

**Spec fields that mean something different here:**

| Field | Behaviour |
|---|---|
| `imagePullSecrets` | ignored with a warning in the controller log: registry credentials come from the host's Docker config, mounted read-only into the controller |
| `imagePullPolicy` | `Always`, `IfNotPresent` and `Never` map to pull decisions |
| `runtime.resources.ephemeralStorage` | **not enforced** (section 8.2). It is used for disk admission accounting only |

### 6.4 Container logs

The `local` log driver keeps the container's stdout on the host, size-capped. It is not
redacted, exactly like the kubelet's pod logs; redaction applies to what the entrypoint
uploads. The README states this. `none` is available to operators who prefer nothing on
disk.

## 7. Network

### 7.1 The target behaviour

The same as the chart's agents NetworkPolicy by default:

- no ingress at all; no published ports;
- egress to the controller's callback port only on the host;
- egress to the control plane's **MCP endpoint** when one is configured: agents call
  back into it (`run_agent`, locks, `schedule_agent`) with their per-run token. That is
  the only control-plane port an agent may reach (section 7.5);
- egress to DNS resolvers listed in the config;
- egress on TCP 80/443 to `0.0.0.0/0` except `10.0.0.0/8`, `172.16.0.0/12`,
  `192.168.0.0/16`, `169.254.0.0/16`, with one configurable allow list and one
  configurable deny list;
- no agent-to-agent traffic;
- nothing else on the host: not the Docker API, not PostgreSQL or the backend if they
  share the machine, not the controller's metrics or health ports.

Unlike the chart, allow entries are **separate rules**, not CIDRs with the same `except`
attached. So an internal GitLab at `10.20.0.5/32` can be allowed without opening all of
`10.0.0.0/8`.

### 7.2 How it is built

- **Network** `haliphron-agents`: a user-defined bridge with a fixed subnet (default
  `172.31.250.0/24`, configurable), `com.docker.network.bridge.enable_icc=false`, IPv6
  disabled, and a fixed bridge interface name, `br-haliphron`, so rules can match it.
- **Controller** runs with `network_mode: host`. The callback listens on the bridge
  gateway address (`172.31.250.1:8083`, the same port the runtime chart uses) and
  `HALIPHRON_CALLBACK_URL` points there. Health and metrics bind to `127.0.0.1:8084` and
  `127.0.0.1:9091`. Those are not the runtime chart's 8081 and 9090, because on a shared
  host the control plane already listens on 8080–8082 and 9090 (section 7.5). Every
  port is configurable.
- **Agent → host** traffic goes through `INPUT`. A chain `HALIPHRON-AGENTS-IN`, jumped to
  for `-i br-haliphron`, allows established traffic and TCP to the callback port, and
  drops everything else.
- **Agent → elsewhere** traffic goes through `FORWARD`, in the `DOCKER-USER` chain
  (Docker's supported hook, which it never flushes). For `-i br-haliphron`: allow
  established; allow configured allow-list entries; allow DNS to the configured
  resolvers; drop the private ranges, `169.254.0.0/16` and the deny list; allow TCP
  80/443; drop the rest.
- **DNS.** Docker's embedded resolver forwards from inside the container's network
  namespace, so upstream queries cross `FORWARD`. When the host's resolver is local
  (systemd-resolved at 127.0.0.53), Docker silently falls back to public resolvers. The
  resolvers are therefore **explicit configuration**: set on each container as `DNS` and
  allowed by IP in the rules.

`deploy/dockerhost/firewall.sh` renders these rules from one config file and applies
them idempotently with `iptables-restore --noflush`. It runs as the systemd unit
`haliphron-agent-firewall.service`, `After=docker.service`, `PartOf=docker.service`, so
it re-applies after a daemon restart or a reboot. Supported baseline: iptables (legacy
or nft backend) on Docker Engine 25+. Docker's experimental native-nftables mode is out
of scope.

### 7.3 Why not a network per run

ICC disabled on one shared bridge already separates agents from each other. Per-run
networks would add a firewall rule set per run, and with it a privilege the controller
was deliberately not given (decision 4).

### 7.4 The check — `dockerhost/netcheck`

The controller cannot read iptables (it has no `NET_ADMIN`). It checks the result rather
than the rules, in two parts:

1. **Configuration**, through the Docker API: the network exists with the expected
   subnet, gateway, bridge name, ICC disabled and IPv6 disabled.
2. **Behaviour**: a short-lived probe container on `haliphron-agents`, run as the same
   hardened user, using the configured probe image (default: the configured base agent
   image, which ships `curl`). It must:
   - reach the callback port (the answer may be 401: it is there);
   - **fail** to reach the controller's metrics port on the gateway, a configured private
     address, `169.254.169.254`, and another container on the network (a second probe);
   - resolve and reach one configured public HTTPS endpoint (default: the LLM endpoint);
   - when the control plane shares the host (section 7.5): reach the MCP port and
     **fail** to reach every other control-plane port in the configured list.

It runs at start, then every 10 minutes and after every Docker daemon restart seen in
the events stream. While it fails, the controller reports `freeSlots: 0` and
`QuotaExhausted: true` in the heartbeat facts, refuses new leases with
`MaterializationFailed`, and logs the failing assertion. Runs already executing
continue: killing them would not undo whatever they could reach.

**For development only:** `HALIPHRON_INSECURE_SKIP_NETCHECK=true` disables the check
(Docker Desktop on macOS has neither the host's iptables nor a usable `network_mode:
host`). The controller logs it at every start and adds the label
`haliphron.io/insecure-network=true` to its registration, so the cluster is marked in
the UI.

### 7.5 Where the control plane runs

**On a separate management host.** The controller reaches the Cluster API over the
network (TLS, through a proxy whose read timeout exceeds the 30 s long poll). Agents
reach the MCP endpoint only if its address is in the allow list. A public address on
443 already passes the default rules. An address in a private range needs its own allow
entry, which 7.1 makes possible without opening the whole range.

**On the same host as the agents.** The control plane runs from
`deploy/dockerhost/compose.control-plane.yaml` (section 13), and the controller talks to
the Cluster API at `http://127.0.0.1:8082`. Two Docker facts decide the firewall rules:

- A port the control plane **publishes** is reached through DNAT. An agent connecting to
  `<host IP>:5432` is rewritten to the PostgreSQL container's bridge address, so the
  packet crosses `FORWARD` (`DOCKER-USER`), not `INPUT`. The rule that drops private
  destinations for `-i br-haliphron` already covers this, because container addresses
  are private. The netcheck asserts it, not assumes it.
- A port bound on the host's own namespace (`network_mode: host`, or a published port on
  `127.0.0.1`) is reached through `INPUT`. There `HALIPHRON-AGENTS-IN` drops everything
  except the callback port.

The control plane's networks are separate user-defined bridges. Docker's own isolation
chains drop traffic between bridges, so agents never see the control-plane containers
directly.

The one exception is MCP. `firewall.sh` takes a `controlPlaneMCP` setting (address and
port) and allows exactly that destination from `br-haliphron`, ahead of the private-range
drop. The backend's `HALIPHRON_MCP_ENDPOINT` must then name an address an agent can
reach. On a shared host that is the bridge gateway address plus the published MCP port.
`install.sh` writes both sides from the same value so they cannot disagree.

The netcheck control-plane port list defaults to 8080, 8081 (unless it is the MCP
port), 8082, 9090 and 5432. It is configurable.

## 8. Capacity and disk

### 8.1 Free slots — `dockerhost/capacity`

`freeSlots = min(CAPACITY_SLOTS − active runs, the number of default-sized runs that
still fit)`, where "fit" means the sum of `cpu` and `memory` limits of live containers
plus the candidate is within host totals (from `docker info`) minus a configured
reserve for the OS, the daemon and the controller. Preflight applies the same check to
the specific spec, so a large run is refused with `QuotaExhausted` rather than
overcommitting the host.

### 8.2 Disk

Docker has no per-volume size limit on the `local` driver (`--storage-opt size` covers
only the container root filesystem, and only on overlay2 over xfs with pquota). v1
therefore guards the host, not the run:

- a free-disk floor on the volumes' filesystem: under it, no new leases and
  `QuotaExhausted` in the facts;
- a critical floor: under it, a warning on every heartbeat and in metrics. Running runs
  are not killed;
- `ephemeralStorage` from specs is summed for admission against free disk;
- the README recommends a dedicated filesystem for `/var/lib/docker`.

Per-run hard quotas (xfs project quotas on volume directories) are deferred (section 15).

### 8.3 Images

Images are pinned by digest, so every new agent image accumulates. `gc` removes agent
images that no live container uses and that are not among the last N used (default 3),
and removes orphaned `haliphron-*` volumes and containers that have no row.

## 9. Backend and API changes

All additive. Existing clusters and controllers see no difference.

1. **`api/cluster/v1/register.go` and `openapi.yaml`:** an optional
   `runtimeKind: kubernetes | docker`, where absent means `kubernetes`. Optional
   `dockerVersion` in `RegisterRequest` and `ClusterFacts`.
2. **`agentNamespace` keeps its meaning of "where agents run".** A Docker host sends its
   agents network name (`haliphron-agents`). The registration check in
   `backend/app/lease.go` and the `NOT NULL` column stay as they are. The field comment
   and `cluster-api.md` say so.
3. **Migration `0017_cluster_runtime_kind.sql`:** `clusters.runtime_kind text NOT NULL
   DEFAULT 'kubernetes' CHECK (runtime_kind IN ('kubernetes','docker'))`, plus
   `docker_version`. `backend/store/clusters.go` writes and reads them.
4. **REST admin clusters + `frontend/src/pages/ClustersPage.tsx`:** show the kind, and
   show the Docker version instead of the Kubernetes one.
5. **Untouched:** placement, leasing, epochs, reports, ingest, all `db/queries`.

The Docker controller sends `CRDVersions: ["v1alpha1"]`, meaning "the spec shape I
materialise". The backend stores this value and does not gate on it today.

## 10. The reconciler — `dockerhost/reconcile`

### 10.1 Driving it

- A keyed work queue: one worker at a time per `run_id`, several runs in parallel.
- Triggers: the Docker events stream (container `start`, `die`, `oom`, `destroy`,
  filtered by `haliphron.io/managed-by`), row writes (lease, ack, callback, command), a
  per-run timer (startup deadline, run deadline, retry backoff, TTL), and a full resync
  every 30 s and after every reconnect of the events stream. Events are a hint. The
  resync is what guarantees convergence.
- `flock` on `/var/lib/haliphron/lock`: a second instance exits at once. Two
  controllers on one host means double execution.

### 10.2 The phase table

A port of `agentrun.Derive` with Docker inputs. It is a pure function and tested as
one. Reasons that mean the same thing keep the same strings.

| Observed | Phase | Reason | Class |
|---|---|---|---|
| no container, not acked | Pending | `AwaitingAck` | — |
| no container, acked, none recorded for this attempt | Pending | `AwaitingContainer` | — |
| a container was recorded and is gone, run not terminal | Failed | `ContainerDisappeared` | infra |
| image pull failing past the startup deadline | Failed | `ImagePullFailed` | infra |
| created, not running, past the startup deadline | Failed | `StartupDeadlineExceeded` | infra |
| running | Running | `Running` | — |
| running past `timeoutSeconds + DEADLINE_SLACK` from the persisted start | → stop (grace) → TimedOut | `DeadlineExceeded` | infra |
| exited, `State.OOMKilled` | Failed | `OOMKilled` | from the exit code, as in Kubernetes |
| exited, code 0 | Succeeded | `Completed` | none |
| exited, other code | `runv1.PhaseForExitCode` | the code's phase | `runv1.FailureClassForExitCode` |
| exited, code 137/143, but the controller did not stop it (daemon restart, host reboot) | Failed | `HostRestarted` | infra |
| cancel requested | stop with the cancel grace → Cancelled | `Cancelled` | — |

"The controller did not stop it" is a column: every stop or kill the controller issues
is recorded before it is issued. `Monotonic` and the `(attempt, phaseRank)` rule apply
unchanged.

### 10.3 Lifecycle

- **Retries:** only infra failures, the same budget (`maxInfraRetries`) and backoff with
  jitter as `controller/agentrun/attempt.go`. Each attempt gets a new container and new
  volumes, and `HALIPHRON_COMPLETED_PHASES` from `status.completedPhases`. That is what
  keeps a retry from paying for the model twice.
- **Presigned bundle refresh** before an attempt, in object-store mode: the same rule as
  `ensureFreshBundle`.
- **Cancel:** `docker stop -t <cancel grace>`. The entrypoint's own SIGTERM path
  persists, pushes if there is budget, and reports `cancelled`.
- **Abandon:** kill, remove the container, volumes, run directory and row; `Forget` in
  the reporter; report nothing.
- **Reaper:** after terminal state plus TTL, and only once `reported.completionDelivered`
  and `ArtifactsRelayed` hold (or the hard TTL passed), the same guard as `reap`.
- **Controller restart:** containers keep running. The resync re-attaches by labels;
  deadlines resume from persisted timestamps.
- **Daemon restart or host reboot:** containers are dead. The table above classifies
  them as `HostRestarted`, infra, retried within the epoch. The secret files on tmpfs are
  regenerated from `materials` before the next attempt. The git token is an hour long,
  so a retry after a long outage may fail at clone with a clear `exit 20`. That is the
  same as in Kubernetes.

## 11. Contract documentation

- **New `docs/contracts/docker-host.md`**, normative, in the style of
  `agentrun-crd.md`:
  - the state store and its fields;
  - section 6.2–6.3 as "what the controller sets on the container" (the counterpart of
    CRD §11);
  - the phase table of 10.2;
  - the network requirements of 7.1 and the check of 7.4;
  - the spec fields with different meaning (6.3);
  - a contract test checklist.
- **`cluster-api.md`:** `runtimeKind`, `dockerVersion`, and the meaning of
  `agentNamespace` for a Docker host. Minor version bump, additive.
- **`agent-runtime.md`:** one paragraph in §1 saying the contract applies to a
  container on a Docker host as to a pod. **No table edits**: `test/contract`
  parses them.
- **`architecture.md`:** a delta entry and a line in section 19.
- **`CLAUDE.md`:** the module table and the module count.

## 12. Tests

| Suite | What | Needs |
|---|---|---|
| `dockerhost/...` unit, `-race` | `launcher` (env parity with `controller/launcher`, mounts, settings), `reconcile.Derive` table, `state` CAS and migrations, `materialize` preflight including the pruning guard, `capacity`, `netcheck` assertion logic | nothing |
| `test/dockerhost` | the whole controller against `fake/backend` and an in-memory fake of the Docker interface: lease→ack→run→report; ack lost; backend down for the whole run; epoch fencing; cancel vs abandon; infra retry with `completedPhases`; restart mid-run; container disappearing; OOM; reordered reports | nothing; same container setup as `make controller-test` |
| `make dockerhost-e2e` (opt-in, Linux only) | a real daemon: real agent image against `fake-controlplane`; `--read-only` with the four volumes; the network check passes with `firewall.sh` applied **and fails without it**; daemon restart mid-run | the host's Docker socket mounted into the test container; root for `firewall.sh` |

The fake Docker lives in `test/dockerhost`, so nothing that ships carries a test double.
This is the same rule `controller/` follows with `fake/`.

New Makefile targets: `dockerhost-test`, `dockerhost-build`, `dockerhost-image`,
`dockerhost-e2e`. All in the same `golang:1.26` container with the shared cache volumes.

## 13. Delivery — `deploy/dockerhost/`

- `compose.yaml`:
  - the `haliphron-dockerhost` image, `network_mode: host`, `restart: unless-stopped`;
  - `read_only: true`, `cap_drop: [ALL]`, `cap_add: [CHOWN, FOWNER]` (to hand secret
    files to uid 1000);
  - mounts: `/var/run/docker.sock`, `/var/lib/haliphron` and `/run/haliphron` at the same
    paths, the host Docker config read-only;
  - a healthcheck on `127.0.0.1`.
- `install.sh` (root, idempotent):
  - checks the Docker version, the iptables backend and that `/run` is tmpfs;
  - creates the directories with modes, the network (7.2), the firewall config, unit and
    rules, and `.env` (backend URL, bootstrap token file, name, labels, capacity,
    resolvers, allow and deny lists);
  - then `docker compose up -d`.
- `README.md`: requirements, install, upgrade, uninstall, what is and is not isolated,
  log and disk caveats, and how to read `runs list`.
- `dockerhost/Dockerfile`: static binary on distroless. It runs as root inside its
  container, because it holds the Docker socket (root-equivalent on the host anyway) and
  must chown secret files.
- `compose.control-plane.yaml`, for decision 8 when the control plane shares the host:
  - PostgreSQL (`postgres:18.1-bookworm`, data on a named volume);
  - the backend, with the same `HALIPHRON_*` settings the `haliphron` chart renders in
    `_env.tpl`, the artifacts volume (relay mode), and the KEK and bootstrap token as
    files;
  - the frontend nginx;
  - the API and UI ports published on the host's public address; the Cluster API
    published on `127.0.0.1:8082` only; MCP published where 7.5 says.

  `install.sh --with-control-plane` brings it up first, mints the bootstrap token, then
  starts the controller with it. It is a convenience for small installations; the Helm
  chart remains the reference deployment of the control plane.

## 14. Order of work

| # | Step | Done when |
|---|---|---|
| 0 | **Spike** on a real Linux host: volume init ownership from the image; tmpfs bind-mount 0400 readable by uid 1000; ICC off + INPUT/DOCKER-USER rules + explicit DNS; the probe container; events stream after daemon restart; same-path mounts from a compose container | each assumption in sections 6–7 confirmed or the plan amended |
| 1 | `docs/contracts/docker-host.md` draft + section 9 backend/API/UI changes | `make verify`, `make backend-test`, `make test` green; old controller registers unchanged |
| 2 | Module skeleton: `config`, `state`, file `identity`, registration, heartbeat with no runs, lease → row → ack, `Resume` | registers against `fake/backend`, acks and rejects correctly |
| 3 | `docker` interface + real client, `launcher`, `reconcile` (table, deadlines, cancel, abandon, retry, reaper) | unit table green; `test/dockerhost` lifecycle cases green |
| 4 | `callback` + `report` (completion, phases, artifact relay, object-store mode) | the whole run reports end to end against `fake/backend` |
| 5 | `netcheck`, `capacity`, `gc`, disk floors | netcheck passes with the firewall and fails without it, in e2e |
| 6 | Packaging: Dockerfile, compose, `install.sh`, `firewall.sh`, unit, Makefile targets, READMEs | install on a clean Linux VM, real backend, a real `run_agent` completes with a PR |
| 6a | `compose.control-plane.yaml` and `install.sh --with-control-plane` | same VM, everything on one host: a run completes, an agent calls MCP, and the netcheck shows 8080/8082/9090/5432 closed to agents |
| 7 | Hardening: security review of sections 6–7; failure injection (kill the controller mid-run, restart dockerd, backend down 10 min, reboot) | each scenario converges as section 10.3 says |

## 15. Deferred

- Extracting a shared core (state port, launcher port, the phase rules, the reporter)
  from `controller/` and `dockerhost/`, once both are stable. That removes the
  transitive client-go dependency and the second `env()`.
- Per-run hard disk quotas (xfs project quotas).
- gVisor/Kata, rootless Docker, userns-remap.
- Several Docker hosts behind one controller; Podman.
- Domain-name (FQDN) egress filtering.
- Encrypting `materials` at rest (decision 7).

## 16. Questions answered

1. **Encrypt `materials` at rest?** No (decision 7).
2. **Will the control plane share the host?** It may, or it may be on a separate
   management host. Both are supported (decision 8, sections 7.5 and 13).
3. **The name?** `haliphron-dockerhost` (decision 9).
