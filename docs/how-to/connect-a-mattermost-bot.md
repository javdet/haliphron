# How to connect a Mattermost bot

The bot turns a message into a run. Someone writes

> @devops_duty why are the ingress pods on staging restarting?

in a channel the bot belongs to. Haliphron runs an agent under the role you
chose for the bot, with that message as the prompt, and the answer arrives in
the same thread: either posted by the agent itself, or by the bot when the run
ends.

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

If the agents are going to answer in the thread themselves, you will create a
**second** bot account for them later. Do not hand this one's token to an
agent. See [let agents answer in the thread](#let-agents-answer-in-the-thread).

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
5. **Replies**: see [choose a reply policy](#choose-a-reply-policy). A
   configuration saved from the UI defaults to *if unanswered*.
6. Under **Advanced**, optionally a repository and base branch for every run,
   a run link with `{id}` for the run identifier, and the post and thread
   limits.
7. Switch it on and save.

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
  replyPolicy: always               # or if_unanswered
  repo: https://github.com/example/infra    # point runs at a repository
  baseBranch: main
  runURL: https://haliphron.example.com/runs/{id}   # link replies to the UI
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
3. posts one or two lines in the thread saying which role will handle it and
   why, through the Mattermost tools of its plugins, **before** starting
   anything, so that the note is never mistaken for the answer;
4. calls `run_agent` asynchronously with that role and a self-contained task
   that tells the agent to answer in the same thread;
5. stops.

A request it can answer in a sentence it answers itself, and an unclear one
gets one clarifying question. It runs without a repository, and passes one to
the child only when the request names it.

Out of the box it cannot post anything. Its `settings.json` enables no plugin,
and its `secretEnv` names a secret that does not exist yet. The next section
supplies both. Role descriptions are what it chooses by, so write them for
that reader: what the role does and what it will not do.

## Let agents answer in the thread

An agent answers in Mattermost through an MCP server, as a bot account of its
own. Six things have to be in place.

### 1. Create a separate bot account for agent replies

Create a second bot account, for example `devops_agent`, the same way as the
first. Add it **only** to the channels it should answer in.

This token goes into the agent's environment, and the agent runs
model-generated commands in a repository that may contain a prompt injection.
Treat the token as readable by the agent, because it is. A narrow account
bounds what a leaked token can do. Never reuse the listener's token, nor the
token of a bot another system such as n8n already uses.

### 2. Store its token as a managed secret

Under **Secrets**, or:

```sh
curl -sX PUT https://haliphron.example.com/api/v1/secrets/mattermost-agent-token \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"value": "yyyyyyyyyyyyyyyyyyyyyyyyyy"}'
```

Do this before editing any role that names it: a role whose `secretEnv` names
a secret that does not exist is refused when it is saved.

### 3. Publish a plugin that declares the Mattermost MCP server

A claude-code plugin in a marketplace repository, with a `.mcp.json` at the
plugin's root. The token is not written into the file; it is expanded from the
agent's environment:

```json
{
  "mcpServers": {
    "mattermost": {
      "type": "http",
      "url": "https://mattermost-mcp.example.com/mcp",
      "headers": {
        "Authorization": "Bearer ${MATTERMOST_TOKEN}"
      }
    }
  }
}
```

The exact server entry depends on the Mattermost MCP implementation you
choose: some are a hosted HTTP endpoint like the one above, others a `stdio`
command that reads the token from an environment variable. What matters is that
the token arrives as `${MATTERMOST_TOKEN}`.

The marketplace must be `owner/repo` on GitHub or an `https://` git URL. A
private one is cloned with the run's git token. A run without a repository,
like the classificator's, is given a git token minted for the role's first
marketplace, bound to that host and no other.

### 4. Enable the plugin in the classificator and in every role that answers

Each role that should post in chat needs the plugin and the variable. In the
role's **Config files**, a `settings.json` in claude-code's own format:

```json
{
  "extraKnownMarketplaces": {
    "example-chat": {"source": {"source": "github", "repo": "example/claude-plugins"}}
  },
  "enabledPlugins": {"mattermost@example-chat": true}
}
```

and under **Secret variables**, `MATTERMOST_TOKEN` from the secret
`mattermost-agent-token`. As a spec:

```json
"secretEnv": [{"name": "MATTERMOST_TOKEN", "secret": "mattermost-agent-token"}]
```

The backend reads only `enabledPlugins` and `extraKnownMarketplaces` from that
file and turns them into the role's plugins. The rest of the file is not
applied. A role that sets `plugins` directly does the same job. See the [role
spec](../reference/role-spec.md#settingsjson-as-the-roles-plugins).

The classificator starts children under other roles, so the roles it picks
need this too, or the child does its work and cannot say so.

### 5. Give agents `run_agent`

The classificator starts its child through the haliphron MCP server, and a pod
gets a per-run MCP token only when the installation knows where that server is
reachable from inside a cluster:

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set mcpEndpoint=https://agent-gw.example.com/mcp
```

See [call Haliphron over MCP](call-haliphron-over-mcp.md#let-agents-start-their-own-runs).

### 6. Make both endpoints reachable on public addresses

Agent pods may reach public addresses only. The runtime chart's NetworkPolicy
allows `0.0.0.0/0` minus RFC1918 and link-local ranges
(`agents.networkPolicy.exceptCIDRs`). So `mcpEndpoint` must be a public
hostname, never the in-cluster Service, and the Mattermost server, or the MCP
server in front of it, must be reachable on a public address as well. If yours
are not, narrow the exceptions in the runtime chart instead.

### Tool policy

A role with no `toolPolicy.allow` gets every tool the installation permits. A
role that sets `allow` must list the MCP tools too: `mcp__haliphron__*` for
`list_roles` and `run_agent`, and the plugin's MCP tools under the names
claude-code gives them (`mcp__plugin_<plugin>_<server>__*` in current
releases; the run log's `claude mcp list` output shows the exact server name).
Otherwise the agent sees the servers and is refused every call.

### What every agent is told

Every prompt from chat opens with a block the system writes:

```text
--- chat message ---
platform: mattermost
channel: town-square (Town Square), type O
channel_id: 4xp9…
message_id: 8kq2…
thread_root_id: 8kq2…
sender: @alice (user id 9zt1…)
A reply to this message belongs in channel_id 4xp9… under root_id 8kq2….
--- end of chat message ---
```

Everything after it is the sender's text. Each value is flattened onto its
line, control characters removed and runs of dashes collapsed, so a display
name cannot end the block early and write lines of its own into it.

A child started by a run that came from chat, through `run_agent` or through
REST with that run's token, gets the same block prepended to its prompt by
admission. The parent does not have to copy it, and cannot get it wrong.

## Choose a reply policy

The policy is frozen on each request when it is claimed, so changing it does
not change what a run already in flight owes.

- **`always`**: the bot reacts with :eyes:, replies "Started run …", and posts
  the run's result in the thread when it ends. Use it when the agents do not
  post in chat. It is the default for a configuration from the environment.
- **`if_unanswered`**: the bot only reacts with :eyes:. When the run ends:
  - a run that did not succeed is always reported, whatever it posted before;
  - a run that succeeded is reported only if no bot account posted in the
    thread after the request was claimed. Posts the backend made itself do not
    count.

  It is the default for a configuration saved from the UI, and the one to use
  when the agents answer in the thread.

The reason for the second rule: `Succeeded` means the agent exited with code 0.
It does not mean the person was answered. An agent that exits 0 without saying
anything still leaves the person owed a reply.

A child run started from a chat run owes the same person a reply as well. When
it is inserted, it gets a follow-up record pointing at the same thread, under
the same policy, and grandchildren inherit it in turn. For a child under
`if_unanswered`, only posts after the child started count as its answer, so the
classificator's routing note does not silence a child that never answered.
Under `always`, every child's result is posted in the thread.

## Check that it works

```sh
kubectl -n haliphron logs deploy/haliphron | grep -i mattermost
```

Look for `the Mattermost bot is running`, with its transport, role and reply
policy, and, for the WebSocket, `connected to the Mattermost server`. A line
saying the server `refused the bot's token` means the token is wrong or
revoked. A line saying the bot's role `does not exist` means the role is
missing. `a webhook delivery carried an unknown token` means the token pasted
into the UI is not the webhook's.

Then mention the bot in a channel it belongs to. It reacts with :eyes:. Under
`always` it also replies with the run's identifier. The run appears in the UI
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
- **The replies.** Under `always`, the bot sends three things:
  - an :eyes: reaction and a short "Started run …" reply when the run is
    admitted;
  - a refusal instead, if nothing was started;
  - the result in the same thread when the run ends. That covers its
    summary, the pull request if one was opened, its cost, and the link.

  Under `if_unanswered`, the reaction, the refusal, and the result only when
  the run failed or nothing answered the thread. A result longer than the
  post limit (`max_post_runes`) is cut, with a note saying where the rest is.
- **What "Succeeded" means.** The reply says *the agent exited with code 0*.
  It does not mean the problem was solved, and the bot does not say it was.
- **What is not a request.** The bot ignores its own posts, other bots,
  webhooks, system messages and edits. Over the WebSocket, in a channel, it
  also ignores any message that does not mention it. A webhook delivery is
  addressed to the bot by construction.
- **Mentions in the result.** `@channel`, `@here` and every other `@` in the
  agent's text are defused before posting. The agent read a repository that
  may have been written to manipulate it, and its summary must not be able to
  page a team. This applies to what the bot posts. What an agent posts through
  its own bot account is that account's, and the account's channel membership
  is what bounds it.

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
  attempt. Other failures are retried for up to 20 attempts. Under
  `if_unanswered`, a thread that cannot be read is treated as not yet known
  and read again on the next attempt, never as silence.
- **Attachments are ignored.** Only the text of a message reaches the prompt.
- **A configuration from the environment needs a restart to change.** One
  saved from the UI does not: every replica checks for a change every 15
  seconds and restarts the bot when it finds one.
