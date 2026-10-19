# Changelog

All notable changes to Haliphron are recorded here. The version is the content
of `VERSION`, and it is the tag of the published images.

## Unreleased

### Added

- **Model gateways such as OpenRouter.** The model credential takes an optional
  `base_url`: the gateway's root, for example `https://openrouter.ai/api`. Set
  it under **Secrets → Model credential**, or as `base_url` on
  `PUT /api/v1/model-credential`. The lease carries it to the pod as
  `llm-base-url`, next to the key.
  - claude-code gets `ANTHROPIC_BASE_URL`, the key as `ANTHROPIC_AUTH_TOKEN`
    and an empty `ANTHROPIC_API_KEY`. Every model class is pinned to the run's
    model.
  - codex gets a model provider at the root plus `/v1` that uses the
    Responses API.
  - With a gateway configured, the model is passed through as written
    (`anthropic/claude-opus-5`), not stripped to the provider's bare name.

  Before this, an OpenRouter key was sent to Anthropic, and the run failed with
  `claude exited 1` and `401 API key is invalid`.
- **Agent pods write their errors to the container's stderr**, so
  `kubectl logs` and the cluster's log pipeline see them even when storage is
  unreachable or the pod dies before its next log chunk. Only errors go there:
  a failed phase, an expired grace period, a panic with its stack, and a crashed
  agent CLI. A crash includes the error the CLI reported (claude-code's
  `is_error`, codex's `turn.failed`) and the last 8 KiB of the CLI's stderr. A
  successful run writes nothing to stderr. Secrets are redacted.

### Changed

- When the agent CLI exits non-zero, the failure message now gives the reason
  (`claude exited 1 after 3s: Invalid API key`). Before, it gave only the exit
  code.
- A panic in an entrypoint phase becomes an `EntrypointPanic` failure with
  that phase's exit code. Persist and the report still run. Before, the pod
  exited 2 with no report.

- Replacing the model key without restating `base_url` clears the stored
  endpoint. This applies to `PUT /api/v1/model-credential` and to
  `PUT /api/v1/secrets/{name}` alike, so a key meant for Anthropic is never
  sent to a gateway because an old endpoint stayed behind.
- A Claude subscription token with a `base_url` is refused with `422` by the
  backend, and with `CredentialMismatch` by the pod.
- Migration `0008_secret_endpoint` adds the nullable `secrets.endpoint`
  column.

## 0.4.0 — 2026-10-02

### Added

- **Run statistics.** The UI has a Statistics page. It shows run counts per
  cluster and status over 24 hours, 7 days, 30 days or all time. It also shows
  p50 and p95 run duration and queue wait per cluster, and each cluster's
  active runs, free slots and heartbeat. A count links to the runs behind it.
  The data comes from `GET /api/v1/stats/runs` (`runs:read`, optional
  `since`), and `GET /api/v1/clusters` gains `active_runs`.
- **Prometheus metrics.** The backend serves `/metrics` on the health port.
  The series are run counts by cluster and status, duration and queue-wait
  histograms, and cluster capacity, free slots, active runs, heartbeat and
  status. They are read from the database, so aggregate them with
  `max without(instance, pod)`.
  - `metrics.serviceMonitor.enabled` now scrapes a real endpoint. The chart
    gains `metrics.serviceMonitor.metricRelabelings`, and
    `metrics.networkPolicy.from` for letting Prometheus through a narrowed
    `networkPolicy`.
  - The backend gains one dependency, `github.com/prometheus/client_golang`.

- **Mattermost bot.** When `mattermost.enabled` is set, the backend connects to
  a Mattermost server as a bot account. A message that mentions the bot, or
  any direct message to it, starts a run under the role in `mattermost.role`.
  The message, without the mention, is the prompt. Inside a thread, the
  thread's earlier messages are quoted ahead of it as context. The bot reacts
  with :eyes:, replies with the run's identifier, and replies again in the same
  thread when the run ends. A run that succeeded is reported as "the agent
  exited with code 0", not as solved. The connection is outbound, over a
  WebSocket, so the chat server needs no route to the control plane.
  Every API replica connects, and each message still starts one run. Replies
  survive a restart of the backend. Runs from the bot are recorded with
  `created_via` `mattermost` and `created_by` `mattermost:<username>`. Anyone
  who can reach the bot can use its role. See
  [connect a Mattermost bot](docs/how-to/connect-a-mattermost-bot.md). The
  schema gains migration 0007 (`chat_triggers`) and the backend gains one
  dependency, `github.com/coder/websocket`.
- **Git token.** `GET` and `PUT /api/v1/git-credential` report and set the
  token runs clone and push with, under the fallback name (`git-token`) or a
  forge's own (`git-token-github`, `git-token-gitlab`). The `GET` needs only
  `runs:read`. The UI asks for one on every page until one is stored, and a
  browser that does not need one can dismiss the prompt. The Secrets page has
  a Git token card, and the New run dialog warns when the repository it names
  would be handed no token. Before this, a missing token was only found when a
  run failed at `clone` or `push`.
- **Deleting a cluster.** `DELETE /api/v1/clusters/{id}` (`admin`) removes a
  revoked cluster's row, and the Clusters page has a Delete button on revoked
  clusters. A cluster that any run records as where it ran is kept until those
  runs are deleted, and the refusal links to them. The deletion is audited as
  `cluster.deleted`. A controller that is still installed is told to register
  again, so uninstall it first.

### Fixed

- **Chart: the agent image followed `latest`.** `agent.image` defaulted to
  `javdet/haliphron-agent:latest` with `IfNotPresent`, so a node that had
  pulled an older `latest` kept running it after an upgrade — which is how
  the 0.3.0 `clone` fix never reached installations that had not pinned the
  image. The default is now `javdet/haliphron-agent:{{ .Chart.AppVersion }}`,
  rendered with `tpl`, so upgrading the chart upgrades the agent.

## 0.3.0 — 2026-09-30

### Fixed

- **Agent image: every run with a repository failed in `clone`.** `phaseInit`
  creates the run's exchange directory inside `/workspace`, so the plain
  `git clone <url> .` that followed was always refused with `destination path
  '.' already exists and is not an empty directory` (exit code 20, class `git`).
  The repository is now cloned with `--separate-git-dir` into the workspace's
  `.git`, the throwaway work tree goes under the run-private directory, and the
  checkout is done in place with `git reset --hard`. Untracked files, including
  the exchange directory, are left alone. Submodules are initialised after the
  checkout rather than during the clone.
- **Controller: retries no longer come faster than the schedule.** The pause
  before a controller-local retry started at 15 s with half-range jitter
  *below* it, which let a pod that failed fast produce three Jobs in half a
  minute. It now starts at 30 s and doubles (30 s, 60 s, 120 s, …) up to 5
  minutes, and jitter of up to a tenth is only ever added on top.

### Added

- **Run retention.** `runRetention` in the chart (`HALIPHRON_RUN_RETENTION`, a
  Go duration such as `720h`) deletes a finished run after that long, along
  with its attempts, logs, results and artifacts. It is off by default, so runs
  are kept forever unless you set it. Every deleted run stays in the audit log.
- **Deleting a run.** `DELETE /api/v1/runs/{id}` needs the `admin` scope; an
  agent pod's `runs:write` token cannot delete runs. A run that has not ended
  is refused with `409 run_live`: cancel it first. The UI has a delete action
  on the run list and the run page.
- **MCP tasks.** A client that negotiates protocol revision `2025-11-25` can
  call `run_agent` as an MCP task and poll `tasks/get` / `tasks/result` /
  `tasks/cancel` itself. The task ID is the run ID. Clients on `2025-06-18`
  work as before.
- **Model credential.** `GET` and `PUT /api/v1/model-credential` set the
  installation's model credential, which is either an Anthropic API key or a
  Claude subscription OAuth token. The kind is detected from the value and
  passed to the agent the way its CLI expects. The UI has a page for it under
  Secrets.
- **Role descriptions.** A role spec can carry a `description` of up to 1 KiB.
  It changes nothing about how the role runs. The UI shows it, and the MCP
  tool `list_roles` now returns each role's name and description so an agent
  can pick a role for a child run.
- **`maxInfraRetries`** in the chart: the number of extra attempts (0–10) the
  controller starts on its own after an `infra` or `git` failure. The backend
  refuses to start if the value is outside that range.

### Upgrading

- The agent image is published as `javdet/haliphron-agent:0.3.0`. A node that
  already has `:latest` cached keeps running the old image under
  `imagePullPolicy: IfNotPresent`, and that old image still has the `clone`
  failure above. Pin the agent image to `0.3.0`, or pull with `Always`.
