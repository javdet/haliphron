# Changelog

All notable changes to Haliphron are recorded here. The version is the content
of `VERSION`, and it is the tag of the published images.

## Unreleased

### Added

- **A chat thread keeps listening for a while.** Once the bot has been asked
  in a thread, in Mattermost or Slack, the person talking to it can go on
  there without mentioning it, for 240 minutes after the last message the bot
  acted on or answered. Each message extends the window, and when it passes in
  silence the bot needs a mention again. This works only while that person is
  the thread's only human. Once anyone else posts in the thread, every message
  needs a mention, and so does a message that names someone else. Every such
  message is a run of its own. The window is `thread_window_minutes` on the
  integration (UI, REST, `HALIPHRON_*_THREAD_WINDOW_MINUTES`,
  `*.threadWindowMinutes` in the chart), and `0` switches the behaviour off.
  Mattermost needs the WebSocket transport. Slack needs the
  `message.channels`, `message.groups` and `message.mpim` events added to the
  app's manifest; without them it keeps answering mentions only. Migration
  0016 adds the column and an index on `chat_triggers`.

## 0.6.2 — 2026-11-10

### Added

- **Roles can be configured over MCP.** An operator's own agent, connected to
  the MCP listener, can read, create, replace and delete roles with
  `get_role`, `put_role` and `delete_role`, and can read a plugin marketplace
  with `inspect_marketplace` to fill a role's plugins. The scopes are the same
  as over REST: `runs:read` to read, `admin` to write. The validation is the
  same as in the UI. These tools are never offered to a per-run token, so an
  agent that Haliphron started cannot change the roles that decide its own
  children's tools, servers and secrets.

### Changed

- **A role name the database cannot store is refused with a reason.** `PUT
  /api/v1/roles/{name}` with a name such as `Infra Coder` used to fail with an
  internal error. It now returns `400` with a `name` field. The allowed names
  are the same as before.
- **API tokens are shown where they belong.** The tokens minted by hand are
  listed, minted, revoked and removed on the Settings page; a run's own MCP
  token is shown on the run, revocable while the run is live. The separate
  Tokens page is gone, and `/tokens` redirects to `/settings`. A per-run token
  is deleted with its run.
- **`GET /api/v1/tokens` can be filtered.** `kind` (`user`, `service` or
  `run-mcp`, repeatable) and `run_id` narrow the list, which is now newest
  first. An unknown kind is refused with `422 invalid_request`.
- **A run's card names its cluster and links to it.** The cluster is shown by
  name instead of identifier (the identifier is still the fallback while the
  cluster list loads), and both the cluster and the role link to the Clusters
  and Roles pages.

## 0.6.1 — 2026-11-10

### Changed

- **A plugin that cannot be set up no longer fails the run.** If a marketplace
  cannot be fetched or registered, the run continues without its plugins. If a
  plugin will not install, the run continues without that plugin. Before, the
  run failed with exit 20 or 30. Now the run's log and the pod's stderr give
  the cause, the other plugins are still installed, and the agent starts. Its
  instructions list what is missing and ask it to say so in its answer.
- **The platform's own MCP tools no longer need to be in a role's allow
  list.** On claude-code, a run that carries the `haliphron` MCP server always
  gets `mcp__haliphron__*` in `--allowedTools`. Before, a role whose
  `toolPolicy.allow` left the server out got an agent that could see
  `run_agent` and was refused every call. A deny in the role or the ceiling
  still removes these tools.

- **Adding a marketplace to a role takes only its repository.** The role form
  reads the catalogue's `marketplace.json` from GitHub or GitLab, with the
  installation's git token, fills in the marketplace name, and lists its
  plugins as checkboxes. A plugin is now picked or typed by its own name, and
  the form adds the `@marketplace` part. When the catalogue cannot be read,
  nothing is filled in and both the name and the plugins can be typed as
  before. A name you type, or one already saved in a role, is kept, and the
  form warns if it differs from the catalogue's. Renaming a marketplace renames
  its plugins too, and removing one removes them. The role document's format is
  unchanged.

### Added

- `POST /api/v1/plugin-marketplaces/inspect` (`admin`) returns a marketplace's
  declared name and plugins, which the form uses. It needs outbound HTTPS from
  the backend to the forge, through `HTTPS_PROXY` if one is set.

### Fixed

- **A run in relay mode no longer fails when the agent keeps a file.** If the
  agent wrote anything to its artifacts directory, the `output` phase crashed
  after the model had finished, and the run ended with exit 12
  (`EntrypointPanic`). In chat this showed as "Sorry, I couldn't finish this".
  Relay is the default artifact mode. Object-store mode was not affected.

## 0.6.0 — 2026-11-08

### Added

- **Ended per-run tokens are deleted after two weeks.** Every run gets its own
  MCP token, which stops working when the run ends but stayed in the tokens
  list for good. A per-run token that is revoked or expired is now deleted 14
  days after it was created. Tokens minted by hand are not affected. Change the
  age, or keep per-run tokens forever, on the Settings page or with `GET` and
  `PUT /api/v1/token-retention` (`admin` to change).
- Migration `0014_token_retention` adds the `token_retention` table and the
  `api_tokens_run_created` index.
- **Agents can tell each other what shared infrastructure they are changing.**
  Four new MCP tools for a run's own token. `set_lock` records that the run is
  working on something, such as `prod/db/orders`, and returns the locks of
  other runs that were set before it. `unset_lock` releases the run's own
  locks. `list_locks` shows every live lock with its holder's status and last
  activity. `schedule_agent` hands the rest of a task to a new run that starts
  once the named locks are released or at a set time. The backend never decides
  whether two locks conflict; the agents do, following the protocol in the tool
  descriptions. A lock is released when its run ends and expires at the latest
  when the run's timeout would. Operator tokens with `runs:read` can call
  `list_locks`.
- **A Locks page in the UI** lists live locks and deferred runs. An admin can
  force-release a lock and cancel a waiting run. A run's page shows the locks
  it holds and the work it deferred. A run that deferred work usually ends
  `Succeeded`, which still means only that it exited 0.
- New REST endpoints: `GET /api/v1/locks`, `DELETE /api/v1/locks/{id}`
  (`admin`), `GET /api/v1/deferred-runs`, `GET /api/v1/deferred-runs/{id}` and
  `POST /api/v1/deferred-runs/{id}/cancel`.
- Migration `0015_resource_locks` adds the `resource_locks` and
  `deferred_runs` tables, and the `runs_release_locks` trigger.

## 0.5.6 — 2026-11-06

### Added

- **The web UI shows its version.** The sidebar shows the release next to the
  name, such as `v0.5.6`. It is the version the UI image was built for. A bundle
  built outside the image build shows `dev`.

### Fixed

- **The classificator's line no longer shows a run identifier.** Its prompt
  asks for none, but a small model still appended the run it started, such as
  *… Run 01M6R7GDAAGEEV8Q526M7BSVW6.* Run identifiers are now removed from that
  line before it is posted, with the word "run" or "запуск" before them and any
  brackets around them. A line that held nothing else is not posted.

## 0.5.5 — 2026-10-29

### Changed

- **The classificator's line is posted again.** A run that started another run
  posts its own answer too. For the classificator, that is one sentence saying
  who will handle the request and what they will do, such as *Аудитор проверит
  состояние postgres-room с точки зрения задержки диска*. The answer of the run
  it started follows. A run that hands the request on without a word, or fails
  after starting one, still posts nothing, because the run it started answers.
  The default `classificator` prompt and the chat header say so. An installation
  that copied the 0.5.4 prompt into its `classificator` should copy it again.
- Replies that are due together are posted in the order their requests were
  claimed, so the classificator's line never lands after the answer.

## 0.5.4 — 2026-10-29

### Changed

- **A chat reply is the agent's answer and nothing else.** On Mattermost and
  Slack the bot reacts with 👀 when a run starts and posts nothing else until
  the answer is ready. The "Started run …" post is gone. When a run ends, the
  bot posts the agent's final answer exactly as written, with no run
  identifier, status, cost, pull request or link. A run that fails, times out
  or ends without an answer gets one plain sentence, and a cancelled run says
  so. A run that started another run posts nothing, because that run's answer
  is the reply. A request routed by the `classificator` therefore gets one
  post, from the agent that did the work. Everything else about a run is on its
  page in the UI.
- **Agents never post in chat.** The prompt header no longer carries channel,
  message, thread or user identifiers. It tells the agent that its final answer
  is the reply. The backend ties a run, and every run it starts, to the thread.
  The default `classificator` no longer posts, and no longer names a
  `MATTERMOST_TOKEN` secret variable.
- **The reply policy and the run link are removed.** `reply_policy` and
  `run_url` are gone from the integrations API and the UI, as are
  `HALIPHRON_{MATTERMOST,SLACK}_REPLY_POLICY` and `_RUN_URL` and the chart's
  `replyPolicy` and `runURL` values. Old settings are ignored, not refused.
  Migration `0013_chat_answers` drops the columns.

### Upgrading

- **Update an existing `classificator`.** The default role is seeded once and
  never overwritten. On an installation that already has one, replace its
  description and system prompt with the ones in
  `backend/app/defaults/classificator.json`, and remove its `MATTERMOST_TOKEN`
  secret variable and any Mattermost plugin. Until then it may still post a
  routing note in the thread through its own tools.

## 0.5.3 — 2026-10-28

### Added

- **Failed runs are deleted after three days.** A run whose status is
  `Failed` is now deleted three days after it finished, with its attempts,
  logs, result and artifacts; the audit log keeps a `run.deleted` record with
  reason `failed_retention`. `TimedOut` and `Cancelled` runs are not affected.
  Change it, or keep failed runs forever, on the new Settings page or with
  `GET` and `PUT /api/v1/run-retention` (`admin` to change). The deployment's
  `runRetention` still applies to every run, and a failed run goes at whichever
  comes first. MCP tasks now carry the shorter of the two as their `ttl`.
- Migration `0012_run_retention` adds the `run_retention` table.
- **Slack.** A Slack app in Socket Mode turns mentions, and direct messages,
  into runs, the way the Mattermost bot does. Before it acts, the bot reads the
  whole thread the message was posted in and quotes it into the prompt. The
  run goes to the `classificator`, which starts the agent that fits, and the
  bot posts the classificator's answer and the result of every run it started
  in the same thread. The backend opens the connection, so nothing has to
  reach it. It is configured under Integrations → Slack, with
  `GET` and `PUT /api/v1/integrations/slack` (`admin`), or with
  `HALIPHRON_SLACK_*` and the chart's `slack` values. The tokens are write-only
  and stored as the managed secrets `slack-bot-token` and `slack-app-token`.
  The default reply policy on Slack is `always`, because the agents hold no
  Slack credentials. See `docs/how-to/connect-a-slack-bot.md`.
- **The `classificator` works on any chat platform.** It posts in the thread
  itself only when it has tools for the platform the header names. Otherwise
  it leaves the reply to the bot and makes its final answer the routing note.
  Default roles are seeded once, so an installation that already has a
  `classificator` keeps the old prompt until an operator replaces it from
  `backend/app/defaults/classificator.json`.
- Migration `0011_slack` lets `chat_triggers` and `chat_integrations` hold
  Slack's rows.
- **Mattermost from the UI.** The Integrations page configures the bot:
  server, bot token, transport, role, reply policy, and an optional repository,
  run link and limits. Every API replica picks up a saved change within 15
  seconds and restarts the bot; no rollout. The same configuration is
  `GET` and `PUT /api/v1/integrations/mattermost` (`admin`). The tokens are
  write-only and stored as the managed secrets `mattermost-bot-token` and
  `mattermost-webhook-token`, so the page needs a key encryption key
  (`409 no_kek` without one). When `HALIPHRON_MATTERMOST_URL` is set, the
  environment's configuration is in force, the page shows it read-only, and a
  save is refused with `409 managed_by_environment`.
- **Outgoing-webhook transport.** Besides the WebSocket, the bot can be fed by
  a Mattermost outgoing webhook at
  `POST /api/v1/integrations/mattermost/webhook`. The endpoint takes no bearer
  token: the webhook's token is compared in constant time against each
  configured one, and a mismatch is a bare `401`. The backend answers `{}` and
  then reads the post back from the server with the bot's token, so the
  author, channel, thread and text are the server's, not the payload's. A
  trigger word is removed like a mention. Outgoing webhooks fire only in
  public channels, and the Mattermost server must reach the public API. With
  both transports on, one post still starts one run.
- **Reply policy.** `always` is the existing behaviour: an acknowledgement,
  then the result. `if_unanswered` is for agents that answer in the thread
  themselves: the bot only reacts with :eyes:. A run that did not succeed is
  always reported; a run that succeeded gets no post from the bot, whether or
  not the agent answered. The policy is frozen on each request when it is
  claimed. A configuration saved from the UI defaults to `if_unanswered`; one
  from the environment keeps `always`.
- **Children of a chat run answer the same person.** A run started by a run
  that came from chat owes a reply in the same thread, and so do its own
  children. Its prompt opens with the same chat header as its parent's, added
  by admission.
- **Chat header in every chat prompt.** A block between `--- chat message ---`
  and `--- end of chat message ---` states the platform, the channel, the
  message and thread identifiers, the sender, and where a reply belongs.
  Values are flattened, so a display name cannot write lines of its own into
  it.
- **Default role `classificator`.** Seeded once at startup, and the default
  role of a configuration saved from the UI. It picks the role that fits a
  request with `list_roles`, posts a short routing note in the thread through
  its Mattermost plugin, and starts the agent with `run_agent`. An edit to it
  is kept, and a deleted one stays deleted.
- **`secretEnv` on roles.** A role can add variables to the agent's own
  environment from managed secrets, such as `MATTERMOST_TOKEN` for a plugin's
  MCP server that expands `${MATTERMOST_TOKEN}`. The values travel in a new
  per-run Secret key, `agent-env.json`, together with the role's literal `env`.
  Names the entrypoint sets itself (`ANTHROPIC_*`, `CLAUDE_*`, `CODEX_*`,
  `OPENAI_*`, `HALIPHRON_*`, `GIT_*`, `PATH`, `HOME`, `GH_TOKEN`,
  `LD_PRELOAD`, …) are refused when the role is saved, and with exit 30
  `MalformedSecret` by the pod. Every value is redacted. This is a deliberate
  exception to ADR 33, recorded as ADR 39: give the agent a narrow
  credential, a separate bot account for agent replies, never the listener's.
  The role form has a Secret variables section.
- **A role's `settings.json` declares its plugins.** A role without `plugins`
  may carry `settings.json` or `settings.<role>.json` in claude-code's format.
  Its `enabledPlugins` and `extraKnownMarketplaces` (`github` and `git`
  sources only) are checked when the role is saved and become the role's
  `plugins.json`. Nothing else in the file is applied.
- **Private plugin catalogues without a repository.** A run with no repository
  whose role names a marketplace is given a git token minted for the first
  marketplace, and the pod binds its credential helper to that host only.
- **Chart.** `mattermost.transport`, `mattermost.webhookToken`,
  `mattermost.existingSecretWebhookKey` (default `webhookToken`) and
  `mattermost.replyPolicy`, as `HALIPHRON_MATTERMOST_TRANSPORT`,
  `HALIPHRON_MATTERMOST_WEBHOOK_TOKEN[_FILE]` and
  `HALIPHRON_MATTERMOST_REPLY_POLICY`. `deploy/values/ai-infra-control-plane.yaml`
  exposes MCP on the public hostname and sets `mcpEndpoint`, which agents need
  for `run_agent`.
- Migration `0009_chat_integrations` adds the `chat_integrations` table and
  the `reply_policy`, `channel_name`, `channel_type` and `origin_message_id`
  columns of `chat_triggers`.
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

- **Upgrading starts deleting old failed runs.** The first retention pass, one
  `reapInterval` (an hour by default) after the new backend starts, deletes
  every failed run that finished more than three days ago. To keep them, choose
  *Keep forever* on the Settings page before then.
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
- The role's literal `env` now also reaches the agent's own environment, not
  only the container's, except names the entrypoint reserves.
- Changing the Mattermost bot's configuration no longer needs a restart when it
  is saved from the UI. A configuration from the environment still does.
- On the Secrets page, the row button that replaces a secret's value or
  reference is now **Edit** instead of **Rotate**. On the model credential's row
  it opens the model credential dialog, so the gateway endpoint and the type
  check are kept.

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
