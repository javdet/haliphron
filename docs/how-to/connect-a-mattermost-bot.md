# How to connect a Mattermost bot

The bot turns a message into a run. Someone writes

> @devops_duty why are the ingress pods on staging restarting?

in a channel the bot belongs to. Haliphron runs an agent under the role you
chose for the bot, with that message and its thread as the prompt. The bot
reacts with :eyes:, and when the run ends it posts the agent's answer in the
same thread: the answer alone, with no run identifier, status, cost or link.

One bot per installation, one role per bot. The role is the whole of what the
bot may do, and **anyone who can reach the bot can use it**. Mattermost channel
membership is the access boundary, so give the bot a least-privilege role.

The default role is `classificator`. It does no work itself: it reads the
request, picks the role that fits, and starts an agent under that role. See
[the classificator](#the-classificator).

## Choose where the configuration lives

There are two places, and only one is in force at a time.

- **The UI, under Integrations → Mattermost.** This is the usual answer. A
  change reaches every API replica within 15 seconds without a rollout, and
  the tokens are kept as managed secrets, encrypted like every other.
- **The control-plane chart's `mattermost` values**, which become
  `HALIPHRON_MATTERMOST_*` variables. Use this to pin the configuration to the
  release.

When `HALIPHRON_MATTERMOST_URL` is set, the environment wins outright. The UI
shows that configuration read-only, and a save is refused with
`409 managed_by_environment`. The two are never merged field by field: that is
how "I changed it and nothing happened" becomes a support conversation.

Either way, the bot runs beside the public API, so `backend.mode` must be `all`
or `api`.

## Create the bot account

This is the account the backend listens and replies as. In Mattermost:

1. **System Console → Integrations → Bot Accounts**: set *Enable Bot Account
   Creation* to true.
2. **Integrations → Bot Accounts → Add Bot Account**. Choose the username
   people will mention, for example `devops_duty`. The role *Member* is
   enough.
3. Copy the access token Mattermost shows once. If the bot already exists,
   use **Create New Token** on it. If the button is missing, enable
   *Personal Access Tokens* under **System Console → Integrations →
   Integration Management**.
4. Add the bot to its team and to every channel it should answer in. The bot
   sees only channels it is a member of:

   ```text
   /invite @devops_duty
   ```

Direct messages to the bot need no invitation and no mention: every message
in a one-to-one conversation with the bot is a request.

Agents never post in Mattermost and get no Mattermost token. Do not hand this
one's token to a role.

## Configure the bot from the UI

Managed secrets need a key encryption key. Without one, the save is refused with
`409 no_kek`; see [manage installation
credentials](manage-installation-credentials.md).

On **Integrations → Mattermost**:

1. **Server**: the address a browser opens, for example
   `https://chat.example.com`.
2. **Bot token**: the token from the previous section. It is stored as the
   managed secret `mattermost-bot-token` and never shown again. Leave the
   field empty on later saves to keep it.
3. **How messages arrive**: WebSocket, outgoing webhook, or both. See [choose
   how messages arrive](#choose-how-messages-arrive).
4. **Role**: `classificator` unless you want every request to run under one
   fixed role.
5. Under **Advanced**, optionally a repository and base branch for every run,
   and the post and thread limits.
6. Switch it on and save.

The same configuration is `PUT /api/v1/integrations/mattermost`; see the [REST
API reference](../reference/rest-api.md#integrations). A switched-off
configuration may be saved half finished. An enabled one needs a server, a
role and a bot token, and a webhook transport also needs at least one webhook
token.

## Or configure it in Helm values

Put the token in a Secret in the control plane's namespace:

```sh
kubectl -n haliphron create secret generic haliphron-mattermost \
  --from-literal=token='xxxxxxxxxxxxxxxxxxxxxxxxxx'
```

Then enable the bot in the control-plane chart's values:

```yaml
mattermost:
  enabled: true
  url: https://chat.example.com
  role: oncall
  existingSecret: haliphron-mattermost
  # Optional:
  transport: websocket              # or webhook, or both
  repo: https://github.com/example/infra    # point runs at a repository
  baseBranch: main
```

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron -f values.yaml
```

With a webhook transport, the webhook tokens go in the same Secret under
`mattermost.existingSecretWebhookKey` (`webhookToken` by default), one per
line, or in `mattermost.webhookToken`. The chart refuses to render a webhook
transport without them, and refuses `backend.mode` other than `all` or `api`.
[Helm values](../reference/helm-values.md#mattermost-bot) lists every setting.

The environment is read at startup, so a change here needs a rollout. A
literal `mattermost.token` changed in values rolls the pods by itself. A token
rotated inside an existing Secret needs
`kubectl rollout restart deploy/haliphron`.

## Create the role

The bot refuses every request until its role exists. With the default
`classificator` nothing needs creating: it is seeded when the backend starts
(see below). For a fixed role, one that only reads clusters and never pushes
code is a reasonable start. See [define a role](define-a-role.md):

```sh
curl -sX PUT https://haliphron.example.com/api/v1/roles/oncall \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "description": "answers the on-call channel",
        "permissionMode": "default",
        "toolPolicy": {"allow": ["Read", "Grep", "Bash(kubectl get:*)", "Bash(kubectl describe:*)"]}
      }'
```

## Choose how messages arrive

### WebSocket

The backend dials Mattermost as the bot and holds a WebSocket open. Nothing
needs to reach the backend, but the backend needs to reach the server on 443.
This sees every channel the bot is a member of, private channels and direct
messages included, and a channel message is a request when it mentions the
bot.

With `networkPolicy.enabled`, add the server to `networkPolicy.egressTo`:

```yaml
networkPolicy:
  egressTo:
    - to:
        - ipBlock:
            cidr: 10.20.30.40/32
      ports:
        - port: 443
          protocol: TCP
```

If you need an HTTP proxy or a private CA, pass them through
`backend.extraEnv` (`HTTPS_PROXY`, `NO_PROXY`). For the CA, mount a bundle
with `backend.extraVolumes` and `backend.extraVolumeMounts`, and point
`SSL_CERT_FILE` at it.

Every proxy between the two has to allow a long-lived WebSocket. The bot pings
every 30 seconds and reconnects when a ping goes unanswered.

### Outgoing webhook

Mattermost calls the backend. Use it when an installation already feeds a bot
this way, or cannot hold a socket open. It sees less: Mattermost fires an
outgoing webhook **only in public channels**, never in a private channel or a
direct message. The direction is also reversed, so the Mattermost server must
reach the public API.

1. **System Console → Integrations → Integration Management**: set *Enable
   Outgoing Webhooks* to true.
2. In the team, **Integrations → Outgoing Webhooks → Add Outgoing Webhook**.
3. **Content Type**: either. Both `application/x-www-form-urlencoded` and
   `application/json` are read.
4. **Channel** and **Trigger Words**: at least one. A trigger word such as
   `@devops_duty` or `triage:` fires on matching posts in any public channel. A
   channel with no trigger words fires on **every** post in it, and every one
   becomes a request.
5. **Callback URLs**:
   `https://<ui-or-api-host>/api/v1/integrations/mattermost/webhook`. The UI's
   nginx proxies `/api`, so the UI's hostname works; the UI shows the path to
   paste.
6. Save, and copy the token Mattermost shows.
7. Paste it into **Webhook tokens** on the Integrations page. Several webhooks
   may feed one bot: put one token per line. Saving replaces the whole list.

If the callback host resolves to a private address, Mattermost refuses to call
it until the host is listed under **System Console → Environment → Developer →
Allow untrusted internal connections to**. With `networkPolicy.ingressFrom`
set on the control-plane chart, the traffic must arrive through a source it
admits, usually the ingress controller.

The endpoint is not bearer-authenticated, because Mattermost cannot hold one
of this API's tokens. The token in the delivery is compared, in constant time,
against every configured one, and a mismatch gets a bare `401`. The payload is
then treated as a notification and nothing more: the backend answers `{}`,
reads the post back from the server with the bot's token
(`GET /api/v4/posts/{id}`), and takes the author, channel, thread, text and bot
flags from the server's answer. A leaked webhook token therefore lets its
holder make the bot look at a real post it can read, and nothing else. The bot
must be a member of the channel, or the post cannot be read and nothing
starts.

The trigger word is taken off the front of the text the way a mention is, so
`triage: the ingress is down` and `@devops_duty the ingress is down` are the
same request.

With `both`, the same post arriving over the socket and through the webhook
starts one run. The claim on the message is the same claim either way.

## The classificator

`classificator` is a role the backend seeds the first time an API process
starts. It is seeded once: an edit you make is kept, and if you delete it, it
stays deleted across restarts.

It routes rather than works. For each request it:

1. reads the message and the quoted thread;
2. calls `list_roles` on the haliphron MCP server and picks the role whose
   description fits best, never `classificator` itself;
3. calls `run_agent` asynchronously with that role and a self-contained task
   that tells the agent its final answer is posted to the person as written;
4. stops.

When it starts an agent, its final answer is one short sentence saying who
will handle the request and what they will do — *Аудитор проверит состояние
postgres-room с точки зрения задержки диска* — and it is posted in the thread;
that agent's answer follows when it ends. A request it can answer in a sentence it answers itself, and an unclear
one gets one clarifying question; then its own final answer is the reply. It
runs without a repository, and passes one to the child only when the request
names it. It needs no chat tools and no Mattermost token.

Role descriptions are what it chooses by, so write them for that reader: what
the role does and what it will not do.

An installation seeded by an earlier version keeps the classificator it was
seeded with, which tells the agent to post in the thread itself. Open it under
**Roles**, replace its description and system prompt with the ones in
`backend/app/defaults/classificator.json`, and remove its `MATTERMOST_TOKEN`
secret variable and any Mattermost plugin.

## Let the classificator start agents

The classificator starts its child through the haliphron MCP server, and a pod
gets a per-run MCP token only when the installation knows where that server is
reachable from inside a cluster:

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set mcpEndpoint=https://agent-gw.example.com/mcp
```

See [call Haliphron over MCP](call-haliphron-over-mcp.md#let-agents-start-their-own-runs).

Agent pods may reach public addresses only. The runtime chart's NetworkPolicy
allows `0.0.0.0/0` minus RFC1918 and link-local ranges
(`agents.networkPolicy.exceptCIDRs`). So `mcpEndpoint` must be a public
hostname, never the in-cluster Service. If it is not, narrow the exceptions in
the runtime chart instead.

A role with no `toolPolicy.allow` gets every tool the installation permits. A
role that sets `allow` must list `mcp__haliphron__*` for `list_roles` and
`run_agent`, or the agent sees the server and is refused every call.

### What every agent is told

Every prompt from chat opens with a block the system writes:

```text
--- chat message ---
platform: mattermost
channel: town-square (Town Square), type O
sender: @alice
Your final answer is posted in the thread as the reply, exactly as you write it, and nothing else about this run is. Write it for the person who asked. If you start another run with run_agent, that run's answer follows yours: make yours one short sentence saying who will handle the request and what they will do. Do not post in the chat yourself.
--- end of chat message ---
```

Everything after it is the sender's text. Each value is flattened onto its
line, control characters removed and runs of dashes collapsed, so a display
name cannot end the block early and write lines of its own into it.

The block carries no channel, post or user identifiers. The backend keeps them,
and that is what ties a run to its thread: an agent never has to repeat one
back, and has nothing to post with by hand.

A child started by a run that came from chat, through `run_agent` or through
REST with that run's token, gets the same block prepended to its prompt by
admission, and owes the same thread its answer. The parent does not have to
copy either, and cannot get them wrong.

## What gets posted

The bot posts nothing while a run works; the :eyes: reaction is the whole
acknowledgement. When a run ends:

- **It succeeded and wrote an answer**: the answer is posted in the thread,
  exactly as the agent wrote it, with mentions defused.
- **It started another run**: its own answer, if it wrote one, is posted the
  same way — for the classificator, the sentence saying who takes the request.
  That run's answer follows when it ends. If the run that handed the request
  on wrote nothing, or failed, nothing is posted for it: the person is still
  owed the answer, and the run it started gives it.
- **It failed, timed out, or ended without an answer**: one plain sentence,
  *Sorry, I couldn't finish this. Please try again.* What it wrote before it
  stopped is not the answer it was on its way to.
- **It was cancelled**: *This request was cancelled.*

A reply never says the work was done. `Succeeded` means only that the agent
exited with code 0, and whether a run reached it is on the run's page, with
its cost, its pull request and its log.

Every new message in a thread is a new run that reads the whole thread again,
earlier answers included, so the agent needs no memory of its own to carry a
conversation.

## Check that it works

```sh
kubectl -n haliphron logs deploy/haliphron | grep -i mattermost
```

Look for `the Mattermost bot is running`, with its transport and role, and,
for the WebSocket, `connected to the Mattermost server`. A line
saying the server `refused the bot's token` means the token is wrong or
revoked. A line saying the bot's role `does not exist` means the role is
missing. `a webhook delivery carried an unknown token` means the token pasted
into the UI is not the webhook's.

Then mention the bot in a channel it belongs to. It reacts with :eyes:, and
posts nothing until the answer is ready. The run appears in the UI
and in `GET /api/v1/runs` with `created_via` `mattermost` and `created_by`
`mattermost:<username>`.

## What the bot does with a message

- **The prompt.** It opens with the chat header above. Outside a thread, the
  request follows: the message itself, minus a leading `@devops_duty` or
  trigger word. Inside a thread, the earlier messages of the thread are quoted
  ahead of the message as context, and the message is marked as the request.
  The quote is capped at the thread context limit (`max_thread_bytes`). When
  the thread is longer, the first message and the newest ones are kept and the
  middle is left out. A mention with no text, inside a thread, asks about the
  thread.
- **The replies.** An :eyes: reaction when the run is admitted, or a refusal
  instead if nothing was started, and then the answer as described in [what
  gets posted](#what-gets-posted). An answer longer than the post limit
  (`max_post_runes`) is cut and ends with an ellipsis; the whole of it is on
  the run's page.
- **What is not a request.** The bot ignores its own posts, other bots,
  webhooks, system messages and edits. Over the WebSocket, in a channel, it
  also ignores any message that does not mention it. A webhook delivery is
  addressed to the bot by construction.
- **Mentions in the result.** `@channel`, `@here` and every other `@` in the
  agent's text are defused before posting. The agent read a repository that
  may have been written to manipulate it, and its answer must not be able to
  page a team.

## Limits

- **Messages posted while no replica is connected are not seen.** Mattermost
  does not replay them on reconnect, and a webhook delivery the backend did not
  accept is not something to rely on being retried. Mention the bot again. Every reconnect logs how long
  the gap was. Saving the configuration in the UI restarts the bot on every
  replica, which is a short gap of its own.
- **The webhook sees public channels only.** Private channels and direct
  messages need the WebSocket.
- **A run in `Unknown` gets no reply until it resolves.** Its cluster has
  stopped reporting.
- **An operator's retry after the reply was posted produces no second reply.**
- **A reply can be posted twice.** This happens if a replica crashes between
  posting it and recording that it did. Replies are never silently dropped
  while the server accepts posts. A post the server refuses outright, such as
  a 403 because the bot was removed from the channel, is given up on after one
  attempt. Other failures are retried for up to 20 attempts.
- **Attachments are ignored.** Only the text of a message reaches the prompt.
- **A configuration from the environment needs a restart to change.** One
  saved from the UI does not: every replica checks for a change every 15
  seconds and restarts the bot when it finds one.
