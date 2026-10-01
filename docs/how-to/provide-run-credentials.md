# How to give runs a git token and a model key

An agent run needs two credentials it does not carry itself: a token for the
forge, and a key for the model provider. Both are held by the control plane
and handed to a pod for the life of one run.

The pod never receives a credential it can keep. Nothing here puts a secret
into a role, a prompt, or a Kubernetes Secret you manage.

## Set the model credential

Every run needs one, so it has a place of its own: **Secrets → Model
credential** in the UI, or its own endpoint. Choose what kind it is:

| Type | What | Works for |
|---|---|---|
| `api_key` | an Anthropic key (`sk-ant-api…`), an OpenAI key, or a gateway's token | claude-code and codex |
| `oauth_token` | a Claude Pro/Max subscription token from `claude setup-token` (`sk-ant-oat…`) | claude-code only |

```sh
curl -sX PUT https://haliphron.example.com/api/v1/model-credential \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"type":"api_key","value":"sk-ant-api03-..."}'
```

A value whose shape contradicts the declared type is refused with `422`: a
subscription token pasted as an API key would otherwise be sent to the model as
one and fail in the pod with a `401`.

It is stored as the secret `llm-api-key` (see [Use different
names](#use-different-names)). Until it is set, every page of the UI says so.

### Use a Claude subscription instead of an API key

On a machine signed in to the plan:

```sh
claude setup-token        # prints sk-ant-oat01-...
```

Then store it with `"type":"oauth_token"`, or choose **Claude subscription
(OAuth)** in the UI. The pod hands it to claude-code as
`CLAUDE_CODE_OAUTH_TOKEN` and sets no `ANTHROPIC_*` key, which the CLI would
otherwise prefer.

Runs then count against the plan's usage limits rather than an API bill, and
the `cost_usd` a run reports is what the CLI computed, not what was charged.
A codex run with a subscription token fails in the `auth` phase with
`CredentialMismatch`. Check that the plan's terms allow automated use before
relying on it for more than trying Haliphron out.

## Store the git token

Most runs need one: a private repository cannot be cloned without it, and no
pull request can be pushed. Until one is stored, every page of the UI asks for
it, and the New run dialog warns when the repository it names would get none.
Set it under **Secrets → Git token**, or through its own endpoint:

```sh
curl -sX PUT https://haliphron.example.com/api/v1/git-credential \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"value":"github_pat_..."}'
```

It is stored as the secret `git-token` (see [Use different
names](#use-different-names)), and a plain `PUT /api/v1/secrets/git-token`
does the same thing. The dedicated endpoint also refuses a pasted value with
whitespace in it, and a reference, which a lease would fail on.

If an installation only ever runs against public repositories without pull
requests, it needs no token: choose **Not needed** on the prompt, and this
browser stops showing it.

A stored value is encrypted under a data key of its own, wrapped by the
installation's key encryption key. The API never returns it again — listing
secrets shows names, kinds and timestamps only.

Storing a managed secret needs a key encryption key. If the installation has
none, this fails; see [how to manage installation
credentials](manage-installation-credentials.md#supply-your-own-key-encryption-key).

## Confirm they are there

```sh
curl -s https://haliphron.example.com/api/v1/model-credential \
  -H "Authorization: Bearer $TOKEN"

curl -s https://haliphron.example.com/api/v1/git-credential \
  -H "Authorization: Bearer $TOKEN"
```

The first answers `configured`, `type`, and a `problem` when a stored
credential cannot reach a pod — a missing key encryption key, or a reference
the backend does not resolve. The second lists `git-token`,
`git-token-github` and `git-token-gitlab`, each with `configured` and, when it
cannot reach a pod, a `problem`. A git token that is a reference is worse than
none: a lease fails on it, and the run stays queued.

## Rotate one

The same `PUT` with a new value. Runs already in flight keep the credential
they were given; the next lease carries the new one.

```sh
curl -sX PUT https://haliphron.example.com/api/v1/secrets/git-token \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"value":"ghp_new..."}'
```

## Reference a secret you already manage

If your secrets live in Vault or External Secrets, store a pointer instead of
a value. The backend never resolves it itself.

```sh
curl -sX PUT https://haliphron.example.com/api/v1/secrets/llm-api-key \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"ref":"vault://secret/data/haliphron#llm"}'
```

A secret is either managed or referenced. Sending both `value` and `ref` is
refused with `422`.

Referenced secrets need no key encryption key.

## Use a different token per forge

An installation with both GitHub and GitLab repositories does not have to
share one credential between them. A per-provider name is tried first, and the
plain name is the fallback:

```sh
curl -sX PUT https://haliphron.example.com/api/v1/git-credential \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"provider":"github","value":"github_pat_..."}'

curl -sX PUT https://haliphron.example.com/api/v1/git-credential \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"provider":"gitlab","value":"glpat-..."}'
```

These are stored as `git-token-github` and `git-token-gitlab`. In the UI,
choose **GitHub only** or **GitLab only** in the Git token dialog. A token
whose prefix names the other forge is refused.

The provider is derived from the run's `repo` URL. With only `git-token`
stored, both forges use it.

## Use different names

If `llm-api-key` and `git-token` clash with something you already have, change
what the backend looks for:

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set secretNames.llmApiKey=prod-openrouter \
  --set secretNames.gitToken=prod-forge-pat
```

Store the secrets under the new names before you roll the Deployment.

## Scope the git token

The token is handed to the pod with about an hour of life, through the
per-run Secret. It is never written into `.git/config`, and it is never
logged.

It still has whatever access you gave it. Give it the least that opens a pull
request on the repositories you intend runs to touch, and no more. See [the
untrusted agent pod](../explanation/the-untrusted-agent-pod.md).

## Run without a repository

A run that names no `repo` gets no git token at all, and skips the four git
phases. Use that for work that produces a result rather than a branch:

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"prompt":"Summarise the incident timeline below: ..."}'
```

## Check that a run got them

A missing model key fails the run in the `validate` phase, before anything
costs money, with exit code 30 and failure class `config`.

```sh
curl -s https://haliphron.example.com/api/v1/runs/$RUN_ID \
  -H "Authorization: Bearer $TOKEN"
```

`status_message` names what was missing.

A missing **git** credential is not refused at admission. A run against a
public repository with `create_pr` false needs none, so the absence of the
secret is allowed and the run fails later — at `clone` or `push`, with exit
code 20 — if it turns out one was needed. See [how to diagnose a failed
run](diagnose-a-failed-run.md).
