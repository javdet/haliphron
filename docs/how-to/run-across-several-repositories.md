# Run an agent across several repositories

A change that spans an API and its client, or a service and its deployment
manifests, can be done by one run. You give the run a list of repositories.
The agent works in the first one and finds the others beside it. Every
repository it changes gets its own pull request.

## Before you start

- The cluster that will run it must be running the runtime chart from the
  release that added `repos`. A run with several repositories is leased only to
  a controller that declares the `extraRepos` feature. On an older cluster the
  run waits in `Queued`. It does not fail.
- Each repository's host needs a git token: `git-token`, or `git-token-github`
  / `git-token-gitlab` per forge, under **Secrets** in the UI (see [Give runs a
  git token and a model key](provide-run-credentials.md)). A token is minted
  per host, so repositories on github.com and on a GitLab instance can be mixed
  in one run.
- A role that pins `image` must pin an agent image from that release or later.
  An older image ignores the extra repositories.

## Submit the run

Use `repos` instead of `repo` and `base_branch`:

```sh
curl -sS -X POST "$HALIPHRON/api/v1/runs" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "prompt": "Add a `region` field to the orders API and use it in the client.",
    "repos": [
      {"url": "https://github.com/acme/orders-api", "base_branch": "main"},
      {"url": "https://github.com/acme/orders-client"},
      {"url": "https://gitlab.example.com/ops/deploy", "base_branch": "release", "name": "deploy", "depth": 0}
    ],
    "create_pr": true
  }'
```

- The **first** entry is the working repository. It is cloned to `/workspace`,
  and that is where the agent starts. It takes no `name` and no `depth`.
- The **others** are cloned to `/repos/<name>`. The name defaults to the URL's
  last segment. The clone is shallow (`depth` 1) unless you ask for `0`, a full
  clone. When `base_branch` is left out, the remote's default branch is used.
- All of them get the run's branch (`target_branch`, generated when left out).

The agent is told where each repository is, and it may write to all of them.
Plugins, settings and MCP servers come only from the working repository.

From another agent over MCP, pass the same list as `repos` to `run_agent`. A
schedule's or an alert receiver's run template takes it too; in the UI, use
**Add repository** under the repository field.

## Read the result

`GET /api/v1/runs/{id}` returns `repos`, with every repository's branch, commit,
whether it was pushed and its pull request. The run page shows the same list as
a table. `pr_url` is still the working repository's.

- A repository the agent did not change is not pushed and gets no pull request.
- The working repository's pull request lists the others, and each of theirs
  points back to it.
- If one repository refuses the push (a protected branch, a missing
  permission), the others are still pushed and proposed. The run then fails.
  It is retried only if at least one of the refusals could go differently next
  time.

`Succeeded` means the agent's container exited 0, nothing more. Read the pull
requests.

## Change the limit

A run may name at most 10 repositories, the working one included, until an
admin changes it. Change it under **Settings → Run limits** in the UI, or with
`PUT /api/v1/run-limits` (`{"max_repos_per_run": 5}`, 1 to 32). There is no
environment variable or Helm value for it. A request over the limit is refused
with a `422` that names the limit. Lowering the limit does not affect runs
already admitted.
