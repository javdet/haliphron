# Your first agent run

> **This tutorial has not been executed end to end.**
>
> It needs a Kubernetes cluster, a model provider key and a git forge, none of
> which the repository can supply, so no step below has been run and had its
> output captured. Every command and every expected result is derived from the
> code and the charts. Treat the outputs as what the system is built to
> produce, not as a transcript.

In this tutorial we will install Haliphron, connect a cluster to it, and send
one agent to fix something in a repository — ending with a pull request we can
open in a browser.

It takes about thirty minutes, most of it waiting for images to pull.

## What we need first

- A Kubernetes cluster we can install into, and `kubectl` pointed at it.
- Helm 3.
- A git repository we do not mind an agent opening a pull request on, and a
  token that can push to it.
- An API key for a model provider.
- The Haliphron repository checked out, so we have the charts.

We will use the chart's bundled PostgreSQL. That is fine for a first run and
wrong for anything lasting — see [how to install the control
plane](../how-to/install-the-control-plane.md) when we do this for real.

Let us set two shell variables now, so the commands that follow can be pasted
as they are. Replace the values with our own.

```sh
export GIT_TOKEN='ghp_replace_me'
export LLM_KEY='sk-replace-me'
```

## Step 1: install the control plane

From the root of the checked-out repository:

```sh
helm dependency build deploy/charts/haliphron
```

```
Saving 2 charts
Downloading postgresql from repo oci://registry-1.docker.io/bitnamicharts
Downloading minio from repo oci://registry-1.docker.io/bitnamicharts
Deleting outdated charts
```

Now install it:

```sh
helm install haliphron deploy/charts/haliphron \
  --namespace haliphron --create-namespace \
  --set postgresql.enabled=true \
  --set agent.image=ghcr.io/automagicops/haliphron-agent:latest
```

Helm prints a summary, then release notes telling us which listeners are
running. Notice the line naming the REST listener on port 8080 — that is the
one we will talk to.

Let us wait for it to be ready:

```sh
kubectl -n haliphron rollout status deploy/haliphron
```

```
Waiting for deployment "haliphron" rollout to finish: 0 of 1 updated replicas are available...
deployment "haliphron" successfully rolled out
```

This takes a minute or two. The backend applies its database schema before it
opens a port, so a fresh install is not ready immediately. If it never becomes
ready, the database is usually the reason:

```sh
kubectl -n haliphron logs deploy/haliphron
```

## Step 2: get our first token

The API accepts nothing but a bearer token, and the install generated one for
us:

```sh
export BOOTSTRAP=$(kubectl -n haliphron get secret haliphron-bootstrap \
  -o jsonpath='{.data.token}' | base64 -d)

echo $BOOTSTRAP
```

```
hlt_9f2c4a1e8b7d3056
```

We will not expose the API to the internet for a tutorial, so let us reach it
through a port-forward. Run this in a second terminal and leave it running:

```sh
kubectl -n haliphron port-forward svc/haliphron 8080:8080
```

```
Forwarding from 127.0.0.1:8080 -> 8080
Forwarding from [::1]:8080 -> 8080
```

Back in the first terminal, let us check that the API answers:

```sh
curl -s localhost:8080/api/v1/runs -H "Authorization: Bearer $BOOTSTRAP"
```

```json
{"runs":[]}
```

An empty list. Notice that this one small response tells us three things are
working: the listener, the token store, and the database behind it.

## Step 3: mint a token of our own

The bootstrap token is meant to be used once. Let us trade it for our own:

```sh
curl -s -X POST localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $BOOTSTRAP" \
  -H 'Content-Type: application/json' \
  -d '{"name":"tutorial","scopes":["admin"]}'
```

```json
{
  "token_id": "01JQ8F3K2M9XA7VB0CDEFGHJKM",
  "name": "tutorial",
  "token": "hlt_4d81b60ac9e2f735",
  "scopes": ["admin"],
  "expires_at": null
}
```

The `token` field is shown once and never again — only its digest is stored.
Let us capture it now:

```sh
export TOKEN='hlt_4d81b60ac9e2f735'
```

Use our own value from the response, not the one printed above.

## Step 4: store the two credentials a run needs

An agent needs a model key, and — because our run will touch a repository — a
git token. Both live in the control plane, never in a prompt or a role.

```sh
curl -s -X PUT localhost:8080/api/v1/secrets/llm-api-key \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"value\":\"$LLM_KEY\"}"
```

```json
{"name":"llm-api-key","kind":"managed"}
```

```sh
curl -s -X PUT localhost:8080/api/v1/secrets/git-token \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"value\":\"$GIT_TOKEN\"}"
```

```json
{"name":"git-token","kind":"managed"}
```

Let us confirm both are there:

```sh
curl -s localhost:8080/api/v1/secrets -H "Authorization: Bearer $TOKEN"
```

```json
{
  "secrets": [
    {"name": "git-token", "kind": "managed", "updated_at": "2026-09-24T09:14:02Z"},
    {"name": "llm-api-key", "kind": "managed", "updated_at": "2026-09-24T09:13:58Z"}
  ]
}
```

Notice that the values are not shown. They never are — the API has no endpoint
that returns one.

## Step 5: issue a bootstrap token for our cluster

A cluster registers itself using a token of its own, which is a different
thing from the admin token we have been using.

```sh
curl -s -X POST localhost:8080/api/v1/clusters/bootstrap-tokens \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"tutorial-cluster"}'
```

```json
{
  "token_id": "01JQ8F4P7R2WYT5N8ZQXCVBNML",
  "name": "tutorial-cluster",
  "token": "hbt_0a7e93c15f4d2b86",
  "expires_at": "2026-09-25T09:15:41Z",
  "max_uses": 0
}
```

This token is also shown once. Let us capture it:

```sh
export CLUSTER_TOKEN='hbt_0a7e93c15f4d2b86'
```

Again, use the value from our own response.

## Step 6: install the controller

We will install it into the same cluster, in a namespace of its own. Real
installations put this in every cluster that should run agents.

```sh
kubectl create namespace haliphron-system
```

```
namespace/haliphron-system created
```

```sh
kubectl -n haliphron-system create secret generic haliphron-bootstrap \
  --from-literal=token="$CLUSTER_TOKEN"
```

```
secret/haliphron-bootstrap created
```

Now the chart. Notice that `backend.url` points at the Cluster API's Service
as seen from inside the cluster — port 8082, not the 8080 we have been using:

```sh
helm install haliphron-runtime deploy/charts/haliphron-runtime \
  --namespace haliphron-system \
  --set cluster.name=tutorial-cluster \
  --set backend.url=http://haliphron.haliphron.svc:8082 \
  --set controller.callbackURL=http://haliphron-runtime.haliphron-system.svc:8083 \
  --set backend.bootstrapTokenExistingSecret=haliphron-bootstrap
```

Let us watch it register:

```sh
kubectl -n haliphron-system logs -l app.kubernetes.io/instance=haliphron-runtime -f
```

```
level=INFO msg="generated cluster identity" secret=haliphron-cluster-identity
level=INFO msg="registered" cluster=01JQ8F5T3V8KHN4M2PQRSWXYZA name=tutorial-cluster
level=INFO msg="polling for work" capacity=8
```

Notice the middle line. Registration happens once: the controller made an
Ed25519 key pair, kept the private half in its own cluster, and sent only the
public half. From here on it signs its own tokens — see [the pull
model](../explanation/the-pull-model.md).

Press Ctrl-C to stop following the logs.

## Step 7: confirm the control plane sees the cluster

```sh
curl -s localhost:8080/api/v1/clusters -H "Authorization: Bearer $TOKEN"
```

```json
{
  "clusters": [
    {
      "cluster_id": "01JQ8F5T3V8KHN4M2PQRSWXYZA",
      "name": "tutorial-cluster",
      "status": "Active",
      "agent_namespace": "haliphron-agents",
      "controller_version": "0.1.0",
      "runtimes": ["claude-code", "codex"],
      "capacity_slots": 8,
      "free_slots": 8,
      "quota_exhausted": false,
      "registered_at": "2026-09-24T09:16:10Z",
      "last_heartbeat_at": "2026-09-24T09:16:40Z"
    }
  ]
}
```

`Active` with eight free slots. If it says `Unreachable`, the heartbeats have
stopped; if the list is empty, the controller never reached the control plane.

## Step 8: send an agent

This is the moment. Let us point one at a repository and give it something
small to do:

```sh
curl -s -X POST localhost:8080/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "Add a CONTRIBUTING.md with a short section on how to run the tests.",
    "repo": "https://github.com/our-org/our-repo.git",
    "base_branch": "main"
  }'
```

```json
{
  "run_id": "01JQ8F6W9Y3ZBC7D5EFGHJKMNP",
  "status": "Queued",
  "agent": "claude-code",
  "model": "anthropic/claude-opus-5",
  "repo": "https://github.com/our-org/our-repo.git",
  "base_branch": "main",
  "epoch": 1,
  "attempt": 1,
  "cost_usd": "0",
  "input_tokens": 0,
  "output_tokens": 0,
  "num_turns": 0,
  "depth": 0,
  "created_by": "token:tutorial",
  "created_via": "api",
  "created_at": "2026-09-24T09:18:02Z"
}
```

We got an answer immediately, and the run is `Queued` — nothing has picked it
up yet. Let us keep the identifier:

```sh
export RUN=01JQ8F6W9Y3ZBC7D5EFGHJKMNP
```

Notice `epoch` and `attempt`, both 1. We will watch them stay there.

## Step 9: watch it run

```sh
curl -s localhost:8080/api/v1/runs/$RUN -H "Authorization: Bearer $TOKEN" \
  | jq '{status, observed_phase, cluster_id, cost_usd}'
```

Run that command again every ten seconds or so. Over a few minutes we will see
the status move:

```json
{"status":"Queued","observed_phase":null,"cluster_id":null,"cost_usd":"0"}
```

```json
{"status":"Dispatched","observed_phase":null,"cluster_id":"01JQ8F5T3V8KHN4M2PQRSWXYZA","cost_usd":"0"}
```

```json
{"status":"Starting","observed_phase":"Starting","cluster_id":"01JQ8F5T3V8KHN4M2PQRSWXYZA","cost_usd":"0"}
```

```json
{"status":"Running","observed_phase":"Running","cluster_id":"01JQ8F5T3V8KHN4M2PQRSWXYZA","cost_usd":"0"}
```

```json
{"status":"Succeeded","observed_phase":"Succeeded","cluster_id":"01JQ8F5T3V8KHN4M2PQRSWXYZA","cost_usd":"0.184200"}
```

Notice that the cost stays at zero until the very end. It arrives with the
completion report, not while the agent is working.

`Starting` usually takes the longest on a first run, because the node is
pulling the agent image.

While we wait, we can watch the pod itself:

```sh
kubectl -n haliphron-agents get pods
```

```
NAME                                   READY   STATUS    RESTARTS   AGE
haliphron-01jq8f6w9y3zbc7d5efghjkmnp-0-x4k2p   1/1     Running   0          47s
```

Notice the pod name contains our run identifier, lowercased.

## Step 10: read what it did

```sh
curl -s localhost:8080/api/v1/runs/$RUN -H "Authorization: Bearer $TOKEN" \
  | jq '{status, exit_code, pr_url, commit_sha, cost_usd, num_turns}'
```

```json
{
  "status": "Succeeded",
  "exit_code": 0,
  "pr_url": "https://github.com/our-org/our-repo/pull/42",
  "commit_sha": "3f9a1c2e5b8d4706192a3c4d5e6f7089abcdef12",
  "cost_usd": "0.184200",
  "num_turns": 7
}
```

There is the pull request. Let us open `pr_url` in a browser and look at the
diff.

And the agent's own account of the work:

```sh
curl -sL localhost:8080/api/v1/runs/$RUN/result -H "Authorization: Bearer $TOKEN"
```

```
Added CONTRIBUTING.md with a "Running the tests" section covering the
three make targets found in the Makefile, and a note about the container
requirement.
```

`Succeeded` here means the container exited 0 — nothing more. Whether the
pull request is any good is for us to judge, which is why we opened it. See
[what "Succeeded" means](../explanation/what-succeeded-means.md).

## Step 11: look at the ledger

One last thing, because it is the first place to look when a run goes wrong:

```sh
curl -s localhost:8080/api/v1/runs/$RUN/attempts \
  -H "Authorization: Bearer $TOKEN" \
  | jq '.attempts[] | {attempt, epoch, exit_code, cost_usd, pod_name}'
```

```json
{
  "attempt": 1,
  "epoch": 1,
  "exit_code": 0,
  "cost_usd": "0.184200",
  "pod_name": "haliphron-01jq8f6w9y3zbc7d5efghjkmnp-0-x4k2p"
}
```

One row: one attempt, one epoch, one charge. Notice that the cost lives here
and is summed onto the run — a run retried three times has three rows and
three charges.

## What we built

We installed a control plane, connected a cluster to it, gave it the two
credentials an agent needs, and sent one agent to do a piece of work that
ended in a pull request.

Notice what we never did: we never gave the control plane credentials for the
cluster. The controller reached out, took the work, and reported back.

## Where to go next

- [How to submit a run and collect its result](../how-to/submit-a-run.md) —
  the options we skipped.
- [How to define a role](../how-to/define-a-role.md) — so we stop repeating
  ourselves.
- [How to diagnose a failed run](../how-to/diagnose-a-failed-run.md) — for the
  run that does not end like this one.
- [About the architecture](../explanation/architecture.md) — what we just
  installed, and why it is shaped that way.

## Clearing up

```sh
helm uninstall haliphron-runtime -n haliphron-system
helm uninstall haliphron -n haliphron
kubectl delete namespace haliphron-system haliphron haliphron-agents
```

Notice that the CRD and the agents' namespace survive `helm uninstall` on
purpose, which is why we delete them by hand here.
