# How to give runs a git token and a model key

An agent run needs two credentials it does not carry itself: a token for the
forge, and a key for the model provider. Both are held by the control plane
and handed to a pod for the life of one run.

The pod never receives a credential it can keep. Nothing here puts a secret
into a role, a prompt, or a Kubernetes Secret you manage.

## Store the two secrets

The backend looks for them under fixed names — `llm-api-key` and `git-token`
by default.

```sh
curl -sX PUT https://haliphron.example.com/api/v1/secrets/llm-api-key \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"value":"sk-..."}'

curl -sX PUT https://haliphron.example.com/api/v1/secrets/git-token \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"value":"ghp_..."}'
```

A stored value is encrypted under a data key of its own, wrapped by the
installation's key encryption key. The API never returns it again — listing
secrets shows names, kinds and timestamps only.

Storing a managed secret needs a key encryption key. If the installation has
none, this fails; see [how to manage installation
credentials](manage-installation-credentials.md#supply-your-own-key-encryption-key).

## Confirm they are there

```sh
curl -s https://haliphron.example.com/api/v1/secrets \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

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
curl -sX PUT https://haliphron.example.com/api/v1/secrets/git-token-github \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"value":"ghp_..."}'

curl -sX PUT https://haliphron.example.com/api/v1/secrets/git-token-gitlab \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"value":"glpat-..."}'
```

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
