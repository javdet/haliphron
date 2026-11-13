# dockerhost

The Docker host controller, `haliphron-dockerhost`: Haliphron's second runtime.
It leases work over the unchanged Cluster API and runs the unchanged agent
image as hardened containers on one Linux host, with a SQLite row in place of
the `AgentRun` and an installed, self-checked firewall in place of the agents
NetworkPolicy.

- What it does and why: [docs/contracts/docker-host.md](../docs/contracts/docker-host.md)
- How to install and operate it: [deploy/dockerhost/README.md](../deploy/dockerhost/README.md)

| Package | What |
|---|---|
| `cmd/haliphron-dockerhost` | wiring; `serve`, `runs list/get`, `netcheck`, `healthcheck`, `version` |
| `config` | the `HALIPHRON_*` environment |
| `state` | the SQLite store: one row per run, compare-and-swap writes |
| `rawspec` | captures each lease's raw spec for the pruning guard |
| `materialize` | lease → preflight → row |
| `lease` | the long poll, the ack, commands as column writes |
| `launcher` | row + attempt → container create request and secret files |
| `docker` | the narrow Engine API interface and its client over the socket |
| `reconcile` | the phase table, the stop sequence, retries, abandon, the reaper, the work queue |
| `callback` | `/completion`, `/phase`, `/artifacts` for agent containers |
| `report` | ingest, heartbeat, artifact relay |
| `netcheck` | the network self-test |
| `capacity` | free slots and disk floors |
| `gc` | orphaned containers, volumes and old agent images |
| `server` | host facts, health and metrics listeners |

Rules specific to this module:

- **It reuses, it does not fork.** The Cluster API client, the spool, the
  identity protocol, the observation queue and the container environment
  (`controller/launcher`) are imported from `controller/`. A change to how a
  run is reported or what the image is given is made once, there.
- **It never imports `fake/`.** The fake daemon and the contract suite live in
  `test/dockerhost`.
- **The row is the only memory.** A field in a struct that must survive a
  restart is a bug; put it in `state.Status`.
- **Every signal is recorded before it is sent** (`status.stop`). That record is
  the only way to tell a container this controller stopped from one a daemon
  restart killed.
- **Nothing logs the materials column, a lease body or a secret file.**

Tests: `make dockerhost-test` (unit + `test/dockerhost`, no daemon needed);
`make dockerhost-e2e` against a real Linux daemon, opt-in.
