# Docker host runtime — semantics

Status: implemented; the network assumptions of §6 are not yet verified on a real host (see §12)
Date: 2026-11-13
Shape: [dockerhost/state](../../dockerhost/state) — the row;
[dockerhost/launcher](../../dockerhost/launcher) — the container;
[dockerhost/reconcile](../../dockerhost/reconcile) — the phase table;
[deploy/dockerhost](../../deploy/dockerhost) — the network, the firewall, the install
Basis: [cluster-api.md](cluster-api.md), [agentrun-crd.md](agentrun-crd.md),
[agent-runtime.md](agent-runtime.md), [architecture.md](../architecture.md) sections 7 and 14

This is the second runtime. The first runs agents as Jobs in a Kubernetes
cluster; this one runs them as containers on **one Linux host with Docker**,
driven by the controller `haliphron-dockerhost`. To the control plane the host
is one more cluster: it registers with a bootstrap token, leases work for
itself, and reports over the unchanged Cluster API. To the agent image it is
one more way of being started: the image does not change and cannot tell.

So this document is what the CRD contract is for Kubernetes: the controller's
side of the world it materialises work into. Where a rule is the CRD
contract's unchanged, it says so and does not restate the reasoning.

---

## 1. Contract boundaries

| What | Where |
|---|---|
| backend ↔ controller | [Cluster API](cluster-api.md), contract 1 — unchanged; `runtimeKind: docker` at registration |
| controller ↔ its durable state | **this contract**, §4 (on Kubernetes: the `AgentRun` CRD, contract 2) |
| controller ↔ Docker | **this contract**, §5–§7 |
| controller ↔ agent container | [agent-runtime.md](agent-runtime.md), contract 3 — unchanged |
| the host's network | **this contract**, §6 |

## 2. What this runtime is

- **One host, one controller, runc.** Exactly one `haliphron-dockerhost` runs
  per host; a second one exits at once (a `flock` on the state directory). Two
  would each lease and each start work. gVisor, Kata, rootless Docker and
  userns-remap are not supported.
- **The Kubernetes controller is not involved.** The new module imports only
  the Kubernetes-free packages of `controller/` — the Cluster API client, the
  spool, the identity core, the observation queue — and `controller/launcher`
  for one purpose, the container environment (§5.1).
- **The pull model holds.** Nothing connects inward to this host. The callback
  listens on the agents bridge only; health and metrics on the loopback.

## 3. Registration

| Field | Value |
|---|---|
| `runtimeKind` | `docker` |
| `agentNamespace` | the agents network's name, default `haliphron-agents` — "where agents run" |
| `dockerVersion` | the daemon's server version, at registration and in heartbeat facts |
| `crdVersions` | `["v1alpha1"]`: the spec shape this controller materialises |
| `labels` | the operator's, plus `haliphron.io/insecure-network=true` when the network check is disabled (§6.4) |

`k8sVersion` is not sent. Placement is unchanged: an explicit `cluster`, a
role's `clusterSelector`, or the default cluster.

## 4. The state store

A SQLite file, `/var/lib/haliphron/state.db`, mode 0600, in a 0700 directory,
written by a pure-Go driver. One row per run — the equivalent of an `AgentRun`
with its Secret and its ConfigMap.

| Column | Kubernetes equivalent |
|---|---|
| `run_id` (primary key) | the CR's name |
| `epoch`, `lease_attempt` | `spec.leaseEpoch`, the lease's attempt |
| `spec` (JSON `RenderedRunSpec`) | `spec`, immutable |
| `spec_raw_sha256` | diagnostic: the digest of the spec as it arrived on the wire |
| `status` (JSON) | `status`: phase, reason, attempt, container, startup anchor, stop record, retry budget, completed phases, result refs, artifacts-relayed, reported |
| `leased_at`, `acked_at` | the `leased-at` and `acknowledged-at` annotations |
| `cancel_requested_at`, `cancel_grace_seconds`, `cancel_reason` | the cancel annotations |
| `abandoned_at` | the abandon annotation |
| `callback_token` | the Secret's `callback-token` key, minted here |
| `materials` (JSON: prompt, secrets, role files, artifact bundle) | the per-run Secret and ConfigMap |
| `version` | `resourceVersion`: every write is a compare-and-swap on it |
| `created_at`, `terminal_at` | creation timestamp; the TTL anchor |

The rules are the CRD contract's §14, unchanged: **reconciliation remembers
nothing** — every decision is a function of the row and of what the daemon
reports at that moment; **commands are column writes**, recorded the first time
and no-ops after; **nothing starts before the acknowledgement**; a row that
never gets one within five minutes is deleted unstarted.

**At most one row per run.** A lease at a higher epoch marks the held row
abandoned and is left unacknowledged until the reconciler has removed the old
epoch's containers and the row; the ack deadline then reissues it, exactly as a
Terminating `AgentRun` defers a new epoch on Kubernetes. A lease at a lower
epoch than the one held is refused with `MaterializationFailed`.

**Secrets at rest are not encrypted** (decision 7 of the plan), the same as
Secrets in etcd without encryption at rest. The file's mode is the protection.
Nothing else on the host keeps a copy except the per-attempt files of §5.2,
which are on tmpfs and removed with the attempt. `runs get` never prints the
materials or the callback token.

**Materialisation preflight**, before anything durable, with the contract's
codes: `InvalidSpec` (empty or malformed image, unknown pull policy, an agent
this host did not declare, no prompt, a prompt digest mismatch, an unparsable
quantity, a secret or role key that cannot be a file name);
`SpecFieldsPruned` (§4.1); `QuotaExhausted` (§8); `MaterializationFailed` (the
network check failing, no callback URL, a store that cannot be written).

### 4.1 The pruning guard without a CRD

There is no structural schema to drop fields here, but there is the same trap
one step earlier: Go's JSON decoder drops a field the struct does not have. So
the raw `spec` object of each lease is captured from the transport under the
Cluster API client (which stays the only place that parses a lease and reads a
Problem's action), and compared with the decoded spec re-encoded. A path present
in the raw spec, with a non-zero value, and absent after decoding refuses the
lease with `SpecFieldsPruned`, naming the paths. A field sent with its zero
value is not a loss: it is what an absent field means to the image.

## 5. What the controller sets on the container

The counterpart of CRD contract §11.

### 5.1 Environment

Taken from `controller/launcher.Builder.Job` for the same run and attempt — the
Kubernetes builder itself, not a copy — so the two runtimes cannot hand the
image different variables. The one change: `HALIPHRON_PROMPT`, a
`secretKeyRef` on Kubernetes, is a literal. It is therefore visible to
`docker inspect`, which needs root on the host. `HALIPHRON_CALLBACK_URL` is the
agents bridge's gateway address and the callback port, default
`http://172.31.250.1:8083`.

### 5.2 Mounts

| Path | Source | Mode |
|---|---|---|
| `/workspace` | named volume `haliphron-<run>-<epoch>-a<n>-workspace` | read-write; a fresh volume is a copy of the image's directory, owned by 1000 |
| `/haliphron/run` | named volume `…-run` | read-write, same |
| `/home/agent` | named volume `…-home` | read-write, same |
| `/tmp` | named volume `…-tmp` | read-write, same |
| `/haliphron/secrets` | bind, `/run/haliphron/runs/<run>/<epoch>/a<n>/secrets` | read-only; files 0400 and the directory 0500, owned 1000:1000 |
| `/haliphron/role` | bind, `…/a<n>/role`, only when the role has files | read-only; files 0444 |

The secret files are the per-run Secret's keys: every lease secret, `prompt`,
`callback-token`, and `presigned.json` in object-store mode. They are written
before every attempt from the row and removed with the attempt. `/run` must be
a tmpfs (install.sh refuses otherwise), so a git token never reaches a disk.

The controller runs in a container but asks the daemon to bind-mount host
paths, so `/var/lib/haliphron` and `/run/haliphron` are mounted into it **at the
same paths as on the host**. The network check (§6.3) proves it, with a marker
file the probe must see through a bind mount.

The volumes are disk-backed with no size limit, like the `emptyDir`s on
Kubernetes; they live as long as the attempt.

### 5.3 Settings

| Setting | Value | Why |
|---|---|---|
| `User` | `1000:1000` | the image's `USER`; the caches moved into `$HOME` belong to it |
| `ReadonlyRootfs` | true | the four mounts are the only writable paths |
| `CapDrop` | `ALL` | |
| `SecurityOpt` | `no-new-privileges:true`; seccomp and AppArmor at the daemon's defaults | RuntimeDefault |
| `Privileged`, devices, `PortBindings` | none | no ingress at all |
| `IpcMode` | `private`; PID namespace private | |
| `RestartPolicy` | `no` | every attempt is the controller's decision |
| `AutoRemove` | false | the exit code must be read first |
| `StopSignal`, `StopTimeout` | `SIGTERM`, `HALIPHRON_GRACE_SECONDS` | the controller stops by signal itself (§7.3) |
| `Init` | unset | the image's entrypoint is tini |
| `NanoCPUs`, `Memory` | `runtime.resources.cpu`, `.memory`, parsed with Kubernetes' quantity parser | |
| `MemorySwap` | equal to `Memory` | no swap |
| `PidsLimit`, `nofile` | config, default 4096 and 65536 | |
| `NetworkMode`, `Dns` | the agents network; the configured resolvers | §6.2 |
| `LogConfig` | `local`, 10m × 2, configurable, `none` allowed | |
| Labels | `haliphron.io/managed-by=haliphron-dockerhost`, `role=agent`, run-id, epoch, attempt, cluster-id, agent | selection, events, GC |
| Name | `haliphron-<run>-<epoch>-a<attempt>` | deterministic: a 409 on create is our own earlier create, adopted if its labels match |

### 5.4 Spec fields that mean something different here

| Field | Behaviour |
|---|---|
| `imagePullSecrets` | ignored: credentials come from the host's Docker config, mounted read-only into the controller (inline `auths` only) |
| `imagePullPolicy` | `Always` pulls (and falls back to a local copy if the registry is down), `IfNotPresent` (the default) pulls if absent, `Never` waits for the image to appear until the startup deadline |
| `runtime.resources.ephemeralStorage` | **not enforced**; counted for admission only (§8) |
| `runtime.nodeSelector`, `runtime.tolerations` | ignored: there is one node |

### 5.5 Logs

The `local` driver keeps a container's stdout on the host, size-capped, and it
is **not redacted** — exactly like the kubelet's pod logs. Redaction applies to
what the entrypoint uploads. The container, and its log, are removed when the
attempt's terminal state has been recorded; `logs/agent.log` in the run's
artifacts is the durable copy.

## 6. Network

### 6.1 Required behaviour

The same as the runtime chart's agents NetworkPolicy by default:

- no ingress at all; no published ports; agents cannot reach each other;
- egress to the controller's callback port on the bridge gateway;
- egress to the configured DNS resolvers;
- egress to the control plane's MCP endpoint, when it is configured (§6.5);
- egress to the configured ALLOW entries, each a rule of its own;
- egress on TCP 80 and 443 to anything that is not private (`10/8`,
  `172.16/12`, `192.168/16`, `169.254/16`, `100.64/10`, `127/8`), not multicast
  and not a DENY entry;
- nothing else on the host: not the Docker API, not PostgreSQL or the backend if
  they share the machine, not the controller's health and metrics ports.

All filtering **DROPs** and never REJECTs (§6.3 depends on it). IPv4 only: the
network is created with IPv6 off.

### 6.2 How it is built

- The network: a user-defined bridge, fixed subnet (default `172.31.250.0/24`,
  gateway `.1`), `com.docker.network.bridge.name=br-haliphron`,
  `com.docker.network.bridge.enable_icc=false`.
- The controller: `network_mode: host`. Callback on `<gateway>:8083`, health on
  `127.0.0.1:8084`, metrics on `127.0.0.1:9091`. (Not the runtime chart's 8081
  and 9090: on a shared host the control plane listens there.)
- Agent → host goes through `INPUT`: chain `HALIPHRON-AGENTS-IN`, jumped to for
  `-i br-haliphron`.
- Agent → elsewhere goes through `FORWARD`, in `DOCKER-USER`, which Docker never
  flushes: chain `HALIPHRON-AGENTS-OUT` for `-i br-haliphron`.
- Anything → agents: chain `HALIPHRON-AGENTS-TO` for `-o br-haliphron`,
  replies only.
- DNS: Docker's embedded resolver forwards from inside the container's
  namespace, so upstream queries cross `FORWARD`, and with a local stub resolver
  on the host it silently falls back to public servers. Resolvers are therefore
  explicit configuration (`HALIPHRON_AGENT_DNS`, required), set on every
  container and allowed by address.

`deploy/dockerhost/firewall.sh` renders the chains from
`/etc/haliphron/firewall.env`, builds them beside the live ones and swaps them
in. The systemd unit `haliphron-agent-firewall.service` runs it after Docker
and again whenever Docker restarts. Baseline: iptables (legacy or nft backend),
Docker Engine 25+; Docker's experimental native nftables mode is out of scope.

### 6.3 The check

The controller has no `NET_ADMIN` and cannot read the rules. It checks their
effect:

1. **Configuration**, through the Docker API: the network exists, bridge driver,
   not internal, IPv6 off, ICC off, the expected bridge name, subnet and gateway.
2. **Behaviour**, from a probe container on the agents network, hardened exactly
   as an agent (§5.3), running the configured image (normally the agent image,
   which has `curl`), plus a second idle probe to aim at:

| Check | Expected |
|---|---|
| callback port on the gateway | reachable |
| port 9, the health port and the metrics port on the gateway | blocked |
| `169.254.169.254` | blocked |
| the other probe's address | blocked |
| each `HALIPHRON_NETCHECK_BLOCKED` host:port | blocked |
| each control-plane port on `HALIPHRON_CONTROL_PLANE_HOST` | blocked |
| `HALIPHRON_CONTROL_PLANE_MCP` | reachable |
| `HALIPHRON_NETCHECK_PUBLIC_URL` (name resolution included) | reachable |
| a marker file through a bind mount of the run directory | present |

"Blocked" is a connect **timeout**. A refused connection means the packet
reached its destination, so no DROP rule is in the path — which is exactly how a
missing firewall shows up, and why the rules never REJECT.

It runs at start, every ten minutes (every minute while failing), and after
every reconnect of the daemon's events stream. While it fails — and before its
first pass — the heartbeat reports `freeSlots: 0` and `quotaExhausted: true`,
leases are refused with `MaterializationFailed`, and `/readyz` answers 503. Runs
already executing continue: killing them would not undo what they could reach.
`haliphron-dockerhost netcheck` runs it once and prints every result.

### 6.4 Development only

`HALIPHRON_INSECURE_SKIP_NETCHECK=true` disables the check (Docker Desktop has
neither the host's iptables nor a usable host network). The controller logs it
at every start and registers with `haliphron.io/insecure-network=true`.

### 6.5 The control plane on the same host

A port the control plane **publishes** is reached through DNAT: from the agents
bridge the packet crosses `FORWARD` with a private destination (the container's
address) and is dropped by the private-range rule. A port bound in the host's
own namespace is reached through `INPUT` and dropped there. The control plane's
networks are separate bridges.

The one exception is MCP. The firewall's `CONTROL_PLANE_MCP` (host:port) is
allowed ahead of the private-range drop — in `FORWARD` by the connection's
*original* destination (`--ctorigdst`), because DNAT has already rewritten the
destination — and in `INPUT` for a server bound on the host. The backend's
`HALIPHRON_MCP_ENDPOINT` must name that same address; `install.sh
--with-control-plane` writes both from one value. The published ports are
bound deliberately: the Cluster API on `127.0.0.1` only, MCP on the agents
gateway only, the API and UI on the public address.

## 7. Reconciliation

### 7.1 Driving it

A keyed work queue, one worker at a time per run. Triggers: container events
(`create`, `start`, `die`, `oom`, `destroy`, filtered by the managed-by label),
row writes (lease, ack, command, callback), each reconcile's own requeue time
(startup deadline, run deadline, kill deadline, retry backoff, TTL), and a full
resync every 30 seconds and after every reconnect of the events stream. Events
are a hint; the resync is what guarantees convergence.

### 7.2 The phase table

`reconcile.Derive`, a pure function of the row and the current attempt's
container. Reasons that mean what a Kubernetes reason means keep its string.

| Observed | Phase | Reason | Class |
|---|---|---|---|
| no container, not acknowledged | Pending | `AwaitingAck` | — |
| no container, acknowledged, nothing begun | Pending | `AwaitingContainer` | — |
| pulling, within the startup deadline | Starting | `PullingImage`, or `ImagePullBackOff` after a failed pull | — |
| a pull failing past the startup deadline | Failed | `ImagePullFailed` | infra |
| nothing started past the startup deadline | Failed | `StartupDeadlineExceeded` | infra |
| a container was recorded and is gone, run not terminal | Failed | `ContainerDisappeared` | infra |
| created, the runtime refused to start it | Failed | `ContainerStartFailed` | infra |
| created, not started, past the startup deadline | Failed | `StartupDeadlineExceeded` | infra |
| running | Running | `Running` | — |
| exited after the controller's deadline stop | TimedOut | `DeadlineExceeded` | infra |
| exited 0 | Succeeded | `Completed` | none |
| exited, `OOMKilled` | from the exit code | `OOMKilled` | from the exit code (137: infra) |
| exited above 128 with no stop recorded | Failed | `HostRestarted` | infra |
| exited, any other code | `PhaseForExitCode` | the phase | `FailureClassForExitCode` |
| cancel requested, the process gone, no deliberate exit first | Cancelled | `Cancelled` | none |

The startup deadline (`HALIPHRON_STARTUP_DEADLINE_SECONDS`, default 600) runs
from `status.startupBeganAt`, persisted when the attempt first tries to pull or
create. The run deadline is `timeoutSeconds + HALIPHRON_DEADLINE_SLACK_SECONDS`
from the start time the daemon reports.

`Monotonic` and the `(attempt, phase rank)` rule apply unchanged. A
cancellation that races a deliberate exit (code ≤ 128) loses to it.

### 7.3 Lifecycle

- **Stop sequence.** The controller stops a container itself: it records
  `status.stop` (reason, time, grace) **before** sending SIGTERM, and records
  `killedAt` before sending SIGKILL once the grace and five seconds have passed.
  No call blocks a worker for a grace period. The record is what tells a
  container this controller stopped from one a daemon restart stopped.
- **Retries:** only infra and git, within `retry.maxInfraRetries` (default 3),
  backoff 30 s doubling to 5 min plus up to 10 % jitter — CRD contract §8. The
  terminal phase of the failed attempt stays in the row until the next attempt
  begins. Each attempt gets a new container, new volumes, freshly written secret
  files and `HALIPHRON_COMPLETED_PHASES` from `status.completedPhases`.
- **After a terminal phase is recorded** the attempt's container, its four
  volumes and its secret files are removed.
- **Presigned bundle refresh** before an attempt, object-store mode only: CRD
  contract §8, unchanged.
- **Cancel:** the stop sequence with the command's grace. The entrypoint's
  SIGTERM path persists, pushes if it can and reports `cancelled`.
- **Abandon:** every container and volume with the run's label is removed, then
  its directory, then the row; queued observations, completions and the spool
  are dropped; nothing more is reported.
- **Reaper:** after the terminal phase plus `ttlSecondsAfterFinished` (default
  24 h), and only once the phase is reported, the completion delivered and the
  spool empty; or unconditionally after seven days.
- **Controller restart:** containers keep running; the resync re-attaches by
  name; deadlines resume from persisted timestamps.
- **Daemon restart or reboot:** containers are dead, classified
  `HostRestarted`, infra, retried within the epoch. The secret files are
  rewritten before the next attempt. The git token lives an hour, so a retry
  after a long outage may fail at clone with exit 20 — as on Kubernetes.

## 8. Capacity and disk

`freeSlots = min(HALIPHRON_CAPACITY_SLOTS − active runs, the number of
default-sized runs that still fit)`, where fitting means the CPU and memory
limits of the active runs plus the candidate stay within the host's totals
(`docker info`) minus a reserve (`HALIPHRON_RESERVE_CPU`, `_MEMORY`). A run
with no limits is sized by `HALIPHRON_AGENT_DEFAULT_*`. Preflight applies the
same test to the lease's own resources and refuses with `QuotaExhausted`.

Docker's `local` volume driver has no per-volume size limit, so v1 guards the
host rather than the run: under `HALIPHRON_DISK_FLOOR` (default 20 GiB free on
`HALIPHRON_DISK_PATH`, default the state directory) no new work is taken and
`quotaExhausted` is reported; under `HALIPHRON_DISK_CRITICAL` (5 GiB) a warning
is logged on every heartbeat. Declared `ephemeralStorage` is summed for
admission against what is free above the floor. Running runs are never killed
for disk.

The collector removes, hourly: agent containers and volumes no row accounts for
(after a ten-minute grace), leftover probes, and agent images — only images an
agent container ran, recorded in the store — that no container uses, no held
run names, and that are not among the three most recently used.

## 9. Installation

| Path | Mode | What |
|---|---|---|
| `/var/lib/haliphron/state.db` | 0600 | the rows |
| `/var/lib/haliphron/identity.json` | 0600 | the cluster's Ed25519 key and ID |
| `/var/lib/haliphron/spool/` | | relayed artifacts awaiting the backend |
| `/var/lib/haliphron/lock` | | the single-instance lock |
| `/run/haliphron/runs/` | tmpfs | per-attempt secret and role files |
| `/etc/haliphron/` | 0700 | `dockerhost.env`, `firewall.env`, `bootstrap-token`, and with the control plane its secrets |

| Port | Bound on | Who |
|---|---|---|
| 8083 | the agents gateway | agents' callback |
| 8084 | 127.0.0.1 | health (`/healthz`, `/readyz`) |
| 9091 | 127.0.0.1 | metrics |

`deploy/dockerhost/README.md` is the operator's guide.

## 10. Contract test checklist

`make dockerhost-test`: unit tests, then `test/dockerhost` against FakeBackend
and an in-memory daemon.

- [x] the environment equals the Kubernetes builder's, variable for variable,
      with the prompt as a literal; every name is in `ContractEnv`
- [x] the container settings and mounts of §5.2–§5.3
- [x] the phase table of §7.2, row by row
- [x] the store: round trip, compare-and-swap conflict, private file mode
- [x] the pruning guard: an unknown field is named, a zero value is not
- [x] preflight refuses with the contract's codes and leaves no row
- [x] registration as `docker` with the network as the agent namespace
- [x] lease → ack → container → callback → artifact → completion → report, end to end
- [x] nothing starts before the acknowledgement
- [x] infra failure retried as a new attempt with the checkpoint, same epoch
- [x] an agent failure is not retried
- [x] OOM kill and daemon restart reported and retried
- [x] a removed container is `ContainerDisappeared` and retried
- [x] cancel: the stop is recorded before it is sent; Cancelled; no retry
- [x] reassignment: the old epoch removed, a later epoch started at attempt 1
- [x] a controller restart re-attaches without a second attempt
- [x] a callback with another run's token, or an escaping key, is refused
- [x] the network check's verdict: DROP passes, a reset fails; the network's configuration
- [ ] `make dockerhost-e2e` on a Linux host: the check fails without the firewall and passes with it
- [ ] the same host with the control plane: an agent reaches MCP, and 8080/8082/8443/9090/5432 are closed

## 11. Deltas from the plan

- The environment is not a second implementation with a parity test: it is the
  Kubernetes builder's output (§5.1). The second `env()` the plan deferred to
  "a shared core" does not exist.
- One row per run, not per (run, epoch) (§4): the plan's own description of a
  new epoch already implied it.
- The controller's container keeps `DAC_OVERRIDE` besides `CHOWN` and `FOWNER`:
  removing an attempt's files means writing into a directory uid 1000 owns.
- The same-path check is part of the network check (a marker seen through a
  bind mount) rather than an inspection of the controller's own container,
  which under `network_mode: host` cannot find itself by hostname.
- Firewall chains are swapped rather than flushed and refilled, so the bridge is
  never without rules during a re-apply.

## 12. Deferred, and not yet verified

Not verified on a real host (step 0 of the plan): the ownership of a fresh named
volume over a read-only root; DNS through Docker's embedded resolver under the
`FORWARD` rules; reaching the MCP port published on the gateway from the agents
bridge via DNAT and `--ctorigdst`; the events stream across a daemon restart.
`make dockerhost-e2e` covers the first three.

Deferred: a shared core of `controller/` and `dockerhost/`; per-run disk quotas
(xfs project quotas); gVisor, Kata, rootless and userns-remap; several hosts per
controller; Podman; FQDN egress filtering; encrypting the materials at rest.
