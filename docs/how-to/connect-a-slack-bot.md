# How to connect a Slack bot

The bot turns a Slack message into a run. Someone writes

> @devops_duty the nightly backup failed again, can you look?

in a channel the app was invited to, often as a reply in a thread where the
problem is already being discussed. Haliphron reads the **whole thread**, and
starts a run under the bot's role with the thread quoted as context and the
message as the request.

The default role is the `classificator`. It does no work itself. It reads the
request, picks the role that fits with `list_roles`, and starts an agent under
that role with `run_agent`. The bot then reports in the same thread:

1. it reacts with 👀 and posts the classificator's run identifier;
2. it posts the classificator's answer when it ends: which role took the
   request and why, or a short answer or clarifying question when no run was
   needed;
3. it posts the result of every run the classificator started when that run
   ends.

The agents hold no Slack credentials. The backend speaks for them, which is
why a Slack configuration defaults to the `always` reply policy.

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
3. Leave **Replies** at *The bot acknowledges and posts every result*.
4. Optionally set the **Run link**, so each reply links to the run in the UI.
5. Tick **Enabled** and save. Within 15 seconds the backend logs
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
  runURL: https://haliphron.example.com/runs/{id}
  existingSecret: haliphron-slack
```

The tokens are read once at startup, so a token rotated inside the Secret
needs a `kubectl rollout restart`.

## An installation that already has a classificator

The default roles are seeded once and never overwritten. A `classificator`
created before Slack support was added still says to post through Mattermost
tools. On Slack it has none, so the post fails and the routing still happens,
but its final answer is less useful than it could be. Open the role under
**Roles** and replace its system prompt with the one in
`backend/app/defaults/classificator.json`. That prompt tells the agent to post
only when it has tools for the platform the header names, and otherwise to
leave the reply to the bot.

## What the agent is told

Every prompt opens with a block the system writes:

```text
--- chat message ---
platform: slack
channel: ops, type O
channel_id: C0OPS00001
message_id: C0OPS00001:1700000000.000300
thread_root_id: C0OPS00001:1700000000.000100
sender: @alice (user id U0ALICE)
A reply to this message belongs in channel_id C0OPS00001 under root_id C0OPS00001:1700000000.000100.
--- end of chat message ---
```

A Slack message is identified by its channel and its timestamp together, so
both identifiers are written as `<channel>:<ts>`. After the block come the
thread's earlier messages, bounded by **Thread context**, and then the request.
A child run that the classificator starts gets the same block, and its result
is posted in the same thread.

## Check it

- Mention the app in a thread. Expect 👀, then a post naming the run.
- In the UI, open the run and read its prompt. The thread should be quoted
  above the request.
- When the classificator ends, expect its answer in the thread. When the run it
  started ends, expect that run's result there too.

"Succeeded" in a reply means the agent exited with code 0, and nothing more.
Read the summary under it.

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
