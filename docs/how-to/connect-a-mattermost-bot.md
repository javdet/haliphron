# How to connect a Mattermost bot

The bot turns a mention into a run. Someone writes

> @devops_duty why are the ingress pods on staging restarting?

in a channel the bot belongs to. Haliphron runs an agent under the role you
chose for the bot, with that message as the prompt. When the run ends, the bot
replies in the same thread.

One bot per installation, one role per bot. The role is the whole of what the
bot may do, and **anyone who can reach the bot can use it**. Mattermost channel
membership is the access boundary, so give the bot a least-privilege role.

## Create the role

The bot refuses every mention until its role exists. A role that only reads
clusters and never pushes code is a reasonable start. See [define a
role](define-a-role.md):

```sh
curl -sX PUT https://haliphron.example.com/api/v1/roles/oncall \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "description": "answers the on-call channel",
        "permissionMode": "default",
        "toolPolicy": {"allow": ["Read", "Grep", "Bash(kubectl get:*)", "Bash(kubectl describe:*)"]}
      }'
```

## Create the bot account

In Mattermost:

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

## Install the token and enable the bot

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
  repo: https://github.com/example/infra    # point runs at a repository
  baseBranch: main
  runURL: https://haliphron.example.com/runs/{id}   # link replies to the UI
```

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron -f values.yaml
```

The bot runs beside the public API, so `backend.mode` must be `all` or `api`.
The chart refuses to render otherwise. [Helm
values](../reference/helm-values.md#mattermost-bot) lists every setting.

### Let the backend reach the server

The backend dials Mattermost and holds a WebSocket open. Nothing needs to
reach the backend, but the backend needs to reach the server on 443. With
`networkPolicy.enabled`, add the server to `networkPolicy.egressTo`:

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

## Check that it works

```sh
kubectl -n haliphron logs deploy/haliphron | grep -i mattermost
```

Look for `connected to the Mattermost server`. A line saying the server
`refused the bot's token` means the token is wrong or revoked. A line saying
the bot's role `does not exist` means the role is missing.

Then mention the bot in a channel it belongs to. It reacts with :eyes: and
replies with the run's identifier. The run appears in the UI and in
`GET /api/v1/runs` with `created_via` `mattermost` and `created_by`
`mattermost:<username>`.

## What the bot does with a message

- **The prompt.** Outside a thread, the prompt is the message itself, minus a
  leading `@devops_duty`. Inside a thread, the earlier messages of the thread
  are quoted ahead of the message as context, and the message is marked as the
  request. The quote is capped at `mattermost.maxThreadBytes`. When the
  thread is longer, the first message and the newest ones are kept and the
  middle is left out. A mention with no text, inside a thread, asks about the
  thread.
- **The replies.** The bot sends three things:
  - an :eyes: reaction and a short "Started run …" reply when the run is
    admitted;
  - a refusal instead, if nothing was started;
  - the result in the same thread when the run ends. That covers its
    summary, the pull request if one was opened, its cost, and the link.

  A result longer than `mattermost.maxPostRunes` is cut, with a note saying
  where the rest is.
- **What "Succeeded" means.** The reply says *the agent exited with code 0*.
  It does not mean the problem was solved, and the bot does not say it was.
- **What is not a request.** The bot ignores its own posts, other bots,
  webhooks, system messages and edits. In a channel, it also ignores any
  message that does not mention it.
- **Mentions in the result.** `@channel`, `@here` and every other `@` in the
  agent's text are defused before posting. The agent read a repository that
  may have been written to manipulate it, and its summary must not be able to
  page a team.

## Limits

- **Messages posted while no replica is connected are not seen.** Mattermost
  does not replay them on reconnect. Mention the bot again. Every reconnect
  logs how long the gap was.
- **A run in `Unknown` gets no reply until it resolves.** Its cluster has
  stopped reporting.
- **An operator's retry after the reply was posted produces no second reply.**
- **A reply can be posted twice.** This happens if a replica crashes between
  posting it and recording that it did. Replies are never silently dropped
  while the server accepts posts. A post the server refuses outright, such as
  a 403 because the bot was removed from the channel, is given up on after one
  attempt. Other failures are retried for up to 20 attempts.
- **Attachments are ignored.** Only the text of a message reaches the prompt.
- **Changing the role, repository or token needs a restart.** These are read
  at startup. A literal `mattermost.token` changed in values rolls the pods by
  itself. A token rotated inside an existing Secret needs
  `kubectl rollout restart deploy/haliphron`.
