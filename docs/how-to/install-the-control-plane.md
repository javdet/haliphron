# How to install the control plane

The control plane is installed once, wherever you want it. It does not have to
sit in a cluster that runs agents, and it never connects outward into one.

## Before you start

You need:

- **PostgreSQL 16 or later.** The chart can bring its own
  (`--set postgresql.enabled=true`) for an evaluation. Do not use that for an
  installation that has to survive a year: the subchart pins no image tag safe
  to run unattended, and the database holds the system of record.
- **An agent image, pinned by digest.** The chart will not choose one.
- **`helm dependency build`**, run before install whether or not you enable a
  subchart:

```sh
helm dependency build deploy/charts/haliphron   # or: make chart-deps
```

## Install with values

The only credential you have to produce is the one only you know:

```sh
helm install haliphron deploy/charts/haliphron \
  --namespace haliphron --create-namespace \
  --set database.dsn='postgres://haliphron:...@postgres:5432/haliphron?sslmode=require' \
  --set agent.image=ghcr.io/automagicops/haliphron-agent@sha256:... \
  --set ingress.api.enabled=true --set ingress.api.host=haliphron.example.com \
  --set ingress.cluster.enabled=true --set ingress.cluster.host=clusters.haliphron.example.com
```

The defaults carry the rest: relay artifact mode on a 50 Gi PVC, no object
storage, the frontend off.

The first admin token and the key encryption key are generated into Secrets of
their own on this install, and left alone by every later `helm upgrade`. Read
both out before you do anything else — see [how to manage installation
credentials](manage-installation-credentials.md).

## Install without putting credentials in the release

Anything passed as a value ends up in a Secret the chart writes **and** in the
release's own storage, which `helm get values` reads. If that is not
acceptable, create the Secrets yourself and hand the chart their names:

```sh
kubectl create namespace haliphron

kubectl -n haliphron create secret generic haliphron-database \
  --from-literal=dsn='postgres://haliphron:...@postgres:5432/haliphron?sslmode=require'

kubectl -n haliphron create secret generic haliphron-kek \
  --from-literal=kek="$(openssl rand -base64 32)"

helm install haliphron deploy/charts/haliphron \
  --namespace haliphron \
  --set database.existingSecret=haliphron-database \
  --set encryption.existingSecret=haliphron-kek \
  --set agent.image=ghcr.io/automagicops/haliphron-agent@sha256:... \
  --set ingress.api.enabled=true --set ingress.api.host=haliphron.example.com \
  --set ingress.cluster.enabled=true --set ingress.cluster.host=clusters.haliphron.example.com
```

An `existingSecret` wins over the corresponding value, except for two pairs the
chart refuses outright rather than resolve:

- `encryption.key` with `encryption.existingSecret`
- `bootstrapToken.value` with `bootstrapToken.existingSecret`

Both would write a credential that nothing reads.

## Watch it come up

```sh
kubectl -n haliphron rollout status deploy/haliphron
```

The first start applies the schema before it binds a port, so a fresh install
is not ready immediately. Replicas serialise on an advisory lock, so expect the
first pod to take the longest.

If the rollout hangs, look at the logs before the probes:

```sh
kubectl -n haliphron logs deploy/haliphron
```

A missing or unreachable `database.dsn` fails here, loudly.

## Check the proxy in front of the Cluster API

A lease poll hangs for up to 30 seconds. Every proxy in front of the cluster
entrypoint must allow a read to take longer than that. Where one does not,
controllers get disconnects instead of empty responses, which reads as network
instability. See [the pull model](../explanation/the-pull-model.md).

The chart sets `proxy-read-timeout: 120` on the cluster Ingress and refuses to
render if you lower it below `clusterProtocol.timings.maxWaitSeconds`. It
cannot see the load balancer in front of your ingress controller. Check that
one yourself.

## Verify

```sh
kubectl -n haliphron port-forward svc/haliphron 8080:8080

curl -s localhost:8080/api/v1/runs -H "Authorization: Bearer $ADMIN_TOKEN"
```

An empty `{"runs":[]}` means the API, the token store and the database are all
working.

## If you need MCP outside the perimeter and REST inside it

`backend.mode` is `all` by default: one Deployment, four listeners. To split
them, install the chart twice against the same database:

```sh
helm install haliphron-api deploy/charts/haliphron -n haliphron \
  --set backend.mode=api ...

helm install haliphron-mcp deploy/charts/haliphron -n haliphron \
  --set backend.mode=mcp ...
```

The mode is part of the Service selector, so the two releases do not select
each other's pods.

Exactly one release must serve `cluster` or `all`. The lease and ack expiry
scanners run wherever the Cluster API does; with none, leases never expire and
a lost cluster's runs are never reassigned.

## Next

- [Register a target cluster](register-a-target-cluster.md) — nothing runs
  until at least one exists.
- [Give runs a git token and a model key](provide-run-credentials.md).
