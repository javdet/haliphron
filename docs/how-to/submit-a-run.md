# How to submit a run and collect its result

The primary act. A run takes a prompt, optionally a repository, and produces a
result — and, when it touched a repository, a pull request.

You need a token carrying `runs:write`.

## Submit a run against a repository

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "The retry in internal/queue/worker.go drops the last item when the buffer is full. Fix it and add a regression test.",
    "repo": "https://github.com/acme/widgets.git",
    "base_branch": "main"
  }'
```

You get `202` and a run object immediately. `run_id` is what everything else
hangs off.

Everything except `prompt` is optional. Unstated fields come from the role, or
from the installation defaults. See the [REST API
reference](../reference/rest-api.md#post-apiv1runs) for the full list.

## Submit without a repository

Omit `repo`. The run skips the four git phases, carries no git token, and
produces a result rather than a branch.

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"prompt":"Draft release notes from this changelog: ..."}'
```

## Bound what it may spend

```sh
-d '{
  "prompt": "...",
  "repo": "...",
  "max_cost_usd": "5.00",
  "timeout_seconds": 1800,
  "max_turns": 40
}'
```

`timeout_seconds` is between 60 and 86400. `max_cost_usd` is a decimal string,
not a number.

## Make a retry safe

If your caller retries on a network error, send an `Idempotency-Key`. The same
key with the same body returns the first run's response verbatim, with `200`
instead of `202`.

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: deploy-4417-fix-retry" \
  -d '{"prompt":"...","repo":"..."}'
```

The same key with a *different* body is refused with `422`
`idempotency_conflict` rather than silently handing back the earlier run. Keys
are remembered for 24 hours by default.

## Wait for it inline

For a short run, when a connection held open is cheaper than polling:

```sh
-d '{"prompt":"...","async":false,"wait_seconds":600}'
```

You get `200` if it finished within the wait, and `202` with the run still in
flight if it did not. A synchronous call holds a connection open for minutes;
most callers should poll instead.

## Poll for the outcome

```sh
curl -s https://haliphron.example.com/api/v1/runs/$RUN_ID \
  -H "Authorization: Bearer $TOKEN"
```

Watch `status`. It is terminal at `Succeeded`, `Failed`, `TimedOut` or
`Cancelled` — and `CompletedWithoutResult`, which means the run ended but its
report never arrived.

`Succeeded` means the container exited 0. It does not mean the task was
solved; read the result before you act on it. See [what "Succeeded"
means](../explanation/what-succeeded-means.md).

When the run touched a repository, `pr_url`, `pr_number` and `commit_sha` are
populated.

## Fetch the result

```sh
curl -sL https://haliphron.example.com/api/v1/runs/$RUN_ID/result \
  -H "Authorization: Bearer $TOKEN"
```

Use `-L`. In object-store mode this answers `302` with a presigned link valid
for 15 minutes; in relay mode it streams the bytes directly. Your client
should not care which.

## Read the logs

The logs are chunks, not a stream. List them, then fetch what you want:

```sh
curl -s "https://haliphron.example.com/api/v1/runs/$RUN_ID/logs?limit=100" \
  -H "Authorization: Bearer $TOKEN"
```

Each chunk carries a `url`. Page with `after=` set to the previous response's
`next_after`. There is no SSE.

## Find runs again

```sh
# everything that failed under one role
curl -s "https://haliphron.example.com/api/v1/runs?role=coder&status=Failed" \
  -H "Authorization: Bearer $TOKEN"

# full-text
curl -s "https://haliphron.example.com/api/v1/runs?q=queue+worker" \
  -H "Authorization: Bearer $TOKEN"
```

`status` repeats for several. Page with `before=` set to the previous
response's `next_before`; its absence means you have reached the end.

## Stop one

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs/$RUN_ID/cancel \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"reason":"superseded"}'
```

You get `202`. Delivery is asynchronous: the instruction reaches the cluster on
its next heartbeat, within 10 seconds by default. A run that finishes first
finishes — cancellation does not override a successful exit.

## Start it again

```sh
curl -sX POST https://haliphron.example.com/api/v1/runs/$RUN_ID/retry \
  -H "Authorization: Bearer $TOKEN"
```

Use this after an `agent` or `config` failure, which the cluster does not
retry on its own. `infra` and `git` failures are already retried in the
cluster without being asked.
