# How to connect a Slack bot

The bot turns a Slack message into a run. Someone writes

> @devops_duty the nightly backup failed again, can you look?

in a channel the app was invited to, often as a reply in a thread where the
problem is already being discussed. Haliphron reads the **whole thread**, and
starts a run under the bot's role with the thread quoted as context and the
message as the request.

The default role is the `classificator`. It does no work itself. It reads the
request, picks the role that fits with `list_roles`, and starts an agent under
that role with `run_agent`. In the thread, the person sees:

1. a 👀 reaction on their message, when the run starts;
2. the answer, when the run that has it ends: the agent's final answer, as it
   wrote it, with no run identifier, status, cost or link.

When the classificator starts another agent, that agent's answer is the reply
and the classificator says nothing. When it needs no other agent, its own short
answer or clarifying question is the reply. A run that fails, or ends without
an answer, gets one plain sentence. The details of every run are in the UI.

The agents hold no Slack credentials and never post in Slack. The backend
posts for them, and ties every run to the thread itself.

One app per installation, one role per app, and **anyone who can reach the app
can use the role**. Make it a least-privilege one; the classificator is, since
it only routes.

## Choose where the configuration lives

As with Mattermost, there are two places and only one is in force at a time:

- **The UI, under Integrations → Slack.** This is the usual answer. A change
  reaches every API replica within 15 seconds, and the tokens are kept as the
  managed secrets `slack-bot-token` and `slack-app-token`.
- **The control-plane chart's `slack` values**, which become
  `HALIPHRON_SLACK_*` variables. While `slack.enabled` is on, the UI shows
  that configuration read-only.

The bot runs beside the public API, so `backend.mode` must be `all` or `api`.

## Create the Slack app

The backend reaches Slack over **Socket Mode**: a WebSocket it opens to Slack.
Nothing has to reach the backend, so a control plane with no public address
works. It needs egress to `slack.com` and `wss-*.slack.com`.

1. At <https://api.slack.com/apps>, choose **Create New App → From an app
   manifest**, pick the workspace, and paste:

   ```yaml
   display_information:
     name: devops_duty
   features:
     bot_user:
       display_name: devops_duty
       always_online: true
     app_home:
       messages_tab_enabled: true
       messages_tab_read_only_enabled: false
   oauth_config:
     scopes:
       bot:
         - app_mentions:read
         - chat:write
         - reactions:write
         - channels:history
         - groups:history
         - im:history
         - mpim:history
         - channels:read
         - groups:read
         - im:read
         - mpim:read
         - users:read
   settings:
     event_subscriptions:
       bot_events:
         - app_mention
         - message.im
     socket_mode_enabled: true
     token_rotation_enabled: false
   ```

   The `*:history` scopes let the bot read a thread before it acts. The
   `*:read` and `users:read` scopes let it name the channel and the sender in
   the prompt.

2. **Basic Information → App-Level Tokens → Generate Token and Scopes**. Add
   the scope `connections:write` and copy the `xapp-…` token. This token only
   opens the Socket Mode connection.
3. **Install App → Install to Workspace**, then copy the **Bot User OAuth
   Token**, `xoxb-…`. Every read and post uses it.
4. Invite the app to every channel it should answer in. It sees mentions only
   in channels it is a member of:

   ```text
   /invite @devops_duty
   ```

Direct messages to the app need no invitation and no mention. Every message
in the app's Messages tab is a request.

## Configure it in the UI

Under **Integrations → Slack**:

1. Paste the bot token and the app-level token.
2. Keep the role `classificator`, which the backend seeds on first start. The
   other roles it chooses from are the ones you defined; give each a
   `description` that says what it is for, because that is what the
   classificator reads.
3. Tick **Enabled** and save. Within 15 seconds the backend logs
   `connected to Slack`.

## Or configure it in the chart

Put both tokens in a Secret and point the chart at it:

```sh
kubectl -n haliphron create secret generic haliphron-slack \
  --from-literal=botToken=xoxb-… --from-literal=appToken=xapp-…
```

```yaml
slack:
  enabled: true
  role: classificator
  existingSecret: haliphron-slack
```

The tokens are read once at startup, so a token rotated inside the Secret
needs a `kubectl rollout restart`.

## An installation that already has a classificator

The default roles are seeded once and never overwritten. A `classificator`
seeded by an earlier version still tells the agent to post in the thread
itself, and to say which role took the request. Open the role under **Roles**,
replace its description and system prompt with the ones in
`backend/app/defaults/classificator.json`, and remove its `MATTERMOST_TOKEN`
secret variable. The current prompt says that the agent never posts, and that
when it starts another agent, that agent's answer is the reply.

## What the agent is told

Every prompt opens with a block the system writes:

```text
--- chat message ---
platform: slack
channel: ops, type O
sender: @alice
Your final answer is posted in the thread as the reply, exactly as you write it, and nothing else about this run is. Write it for the person who asked. If you start another run with run_agent, your final answer is not posted: that run's is. Do not post in the chat yourself.
--- end of chat message ---
```

The block carries no channel, message or user identifiers. The backend keeps
them, and that is what ties a run, and every run it starts, to the thread.
After the block come the thread's earlier messages, earlier answers included,
bounded by **Thread context**, and then the request. Every new message in the
thread is a new run that reads the whole thread again, so the agent needs no
memory of its own. A child run that the classificator starts gets the same
block, and its answer is posted in the same thread.

## Check it

- Mention the app in a thread. Expect 👀 and nothing else while the work runs.
- In the UI, open the run and read its prompt. The thread should be quoted
  above the request.
- When the run that has the answer ends, expect that answer in the thread, and
  only that.

A reply says nothing about whether the run succeeded: it is the agent's own
words. Whether a run reached `Succeeded`, which means only that the agent
exited with code 0, is on the run's page.

## When it does not answer

- **`Slack refused the bot's token`** in the backend's log: a token is wrong,
  revoked, or the wrong kind. The bot token starts with `xoxb-`, the app-level
  one with `xapp-`.
- **`Socket Mode is disabled in the Slack app's settings`**: turn it on under
  **Socket Mode** in the app's settings.
- **No log line at all for a mention**: the app is not in the channel, or the
  `app_mention` event is not subscribed.
- **A refusal saying the role does not exist**: create the role, or seed it by
  restarting the backend with the integration enabled.
