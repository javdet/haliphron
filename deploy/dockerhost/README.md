# Running agents on one Docker host

`haliphron-dockerhost` runs Haliphron's agents as containers on a single Linux
machine with Docker, instead of as Jobs in a Kubernetes cluster. To the control
plane the machine is one more cluster: it registers, leases work and reports
back over the same Cluster API, and runs land on it the usual ways — an
explicit `cluster`, a role's `clusterSelector`, or the default cluster.

The normative description is [docs/contracts/docker-host.md](../../docs/contracts/docker-host.md).
This file is how to install and run it.

## Requirements

- Linux, root.
- Docker Engine 25 or later, with the compose plugin.
- iptables (legacy or nft backend). Docker's experimental native nftables mode
  is not supported.
- `/run` on tmpfs (systemd does this). Per-run secret files live there.
- A free `/24` for the agents network; the default is `172.31.250.0/24`.
- The resolvers agents should use. They are configured explicitly, because
  Docker's embedded DNS falls back to public servers when the host's resolver is
  a local stub.
- An agent image that the host can pull, normally pinned by digest. It is also
  what the network check probes from.

Docker Desktop on macOS or Windows is **not** a supported host: it has neither
the host's iptables nor a usable host network. For development there,
`--insecure-skip-netcheck` starts the controller without verifying isolation,
and registers the cluster with the label `haliphron.io/insecure-network=true`.

## Install, with the control plane elsewhere

On the control plane, mint a bootstrap token for this host (UI → Clusters, or
`POST /api/v1/clusters/bootstrap-tokens`), and save it to a file on the host.
Then, from a checkout of this directory:

```sh
sudo ./install.sh \
  --backend-url https://haliphron.example.com:8082 \
  --bootstrap-token-file /root/hlb.token \
  --name build-box-1 \
  --dns 10.0.0.53 --dns 10.0.0.54 \
  --agent-image javdet/haliphron-agent:0.6.2@sha256:… \
  --controller-image javdet/haliphron-dockerhost:0.6.2 \
  --capacity 4
```

The Cluster API is a long poll of up to 30 seconds: any proxy between the host
and the control plane needs a read timeout above that.

Agents can reach the internet on TCP 80 and 443, and nothing private. If they
need an internal service — a GitLab, a package mirror, the control plane's MCP
endpoint on a private address — allow exactly that:

```sh
  --allow 10.20.0.5/32:443 --allow 10.30.0.0/24:443,22
```

Each `--allow` is a rule of its own, so one internal address does not open its
whole range. `--deny` adds destinations to drop even on 80/443.

## Install, with the control plane on the same host

```sh
sudo ./install.sh --with-control-plane \
  --name all-in-one --dns 10.0.0.53 \
  --agent-image javdet/haliphron-agent:0.6.2@sha256:… \
  --public-addr 203.0.113.10
```

This starts PostgreSQL, the backend and the UI from
`compose.control-plane.yaml`, mints the cluster bootstrap token from the backend
itself, and then starts the controller against `127.0.0.1:8082`. The Helm chart
remains the reference deployment of the control plane; this is for small
installations.

| Port | Bound on | Reached by |
|---|---|---|
| 8080 | `--public-addr` | the API, for people and integrations |
| 8443 | `--public-addr` | the UI |
| 8082 | 127.0.0.1 | the controller only |
| 8081 | the agents gateway | agents calling MCP with their run token |
| 9090, 5432 | not published | |

The agents can reach 8081 and nothing else of the control plane; the network
check asserts 8080, 8082, 8443, 9090 and 5432 are closed to them. The first
admin token is in `/etc/haliphron/admin-token`: sign in with it, mint a token of
your own, revoke it. Then give runs a git token and a model key as on any
installation.

## What is where

| | |
|---|---|
| `/opt/haliphron/compose.yaml` | the controller |
| `/etc/haliphron/dockerhost.env` | its settings (every variable: `dockerhost/config/config.go`) |
| `/etc/haliphron/firewall.env` | the firewall's settings |
| `/usr/local/lib/haliphron/firewall.sh`, `haliphron-agent-firewall.service` | the firewall, re-applied after every Docker restart |
| `/var/lib/haliphron/state.db` | the runs this host holds, **including their secrets** (mode 0600) |
| `/var/lib/haliphron/identity.json` | the host's key; losing it means registering again |
| `/var/lib/haliphron/spool/` | relayed artifacts not yet taken by the backend |
| `/run/haliphron/runs/` | each attempt's secret files, on tmpfs |

Backups: `identity.json` is worth keeping. `state.db` is not — it holds git
tokens, and losing it costs at most runs the backend then reports as
`Unknown`. Exclude it.

## Operating

```sh
docker logs -f haliphron-dockerhost
docker exec haliphron-dockerhost /haliphron-dockerhost runs list
docker exec haliphron-dockerhost /haliphron-dockerhost runs get 01J…   # never prints secrets
docker exec haliphron-dockerhost /haliphron-dockerhost netcheck        # every check and why
docker ps --filter label=haliphron.io/managed-by=haliphron-dockerhost
```

`runs list` columns: the run, its epoch and attempt, the phase and reason this
host observed, the container, whether the backend acknowledged the lease, and
the last phase the backend accepted.

**When the network check fails** the host stops taking work (it reports no free
slots and `quotaExhausted`), and runs already executing continue. `netcheck`
says which assertion failed. "connection refused" on a check that should be
blocked means no DROP rule is in the path: `systemctl status
haliphron-agent-firewall` and `/usr/local/lib/haliphron/firewall.sh show`.

**Upgrade:** change `DOCKERHOST_IMAGE` in `/etc/haliphron/compose.env` (or rerun
`install.sh --controller-image …`) and `docker compose … up -d`. Running agents
are not touched by a controller restart.

**Uninstall:**

```sh
docker compose -p haliphron-dockerhost -f /opt/haliphron/compose.yaml down
docker ps -aq --filter label=haliphron.io/managed-by=haliphron-dockerhost | xargs -r docker rm -f
docker volume ls -q --filter label=haliphron.io/managed-by=haliphron-dockerhost | xargs -r docker volume rm
systemctl disable --now haliphron-agent-firewall
docker network rm haliphron-agents
rm -rf /var/lib/haliphron /run/haliphron /etc/haliphron /opt/haliphron
```

Then revoke the cluster in the UI.

## What is isolated, and what is not

Each agent runs as uid 1000 with a read-only root, no capabilities,
`no-new-privileges`, the daemon's seccomp and AppArmor profiles, its own PID and
IPC namespaces, CPU, memory (no swap) and PID limits, and no published ports. It
cannot reach other agents, the host's ports other than the callback, the Docker
API, private networks, or the cloud metadata service.

Not isolated, or not as strongly as on a hardened Kubernetes node:

- **One kernel.** runc only. A kernel exploit in one agent is the host.
- **Disk.** There is no per-run disk quota: one agent can fill the filesystem
  under `/var/lib/docker`. The controller stops taking work under a free-space
  floor (20 GiB by default) but does not kill running agents. A dedicated
  filesystem for `/var/lib/docker` is recommended.
- **Logs.** An agent's stdout is kept by Docker's `local` driver, size-capped and
  **not redacted**, until the attempt ends — as the kubelet keeps pod logs.
  `HALIPHRON_AGENT_LOG_DRIVER=none` keeps nothing.
- **The prompt** is visible to `docker inspect` on the host, which needs root.
- **The controller** holds the Docker socket: it is root on the host, as the
  Kubernetes controller is effectively cluster-admin over its namespace.
