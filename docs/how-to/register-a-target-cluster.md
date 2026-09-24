# How to register a target cluster

A target cluster is where agents actually run. It needs the runtime chart, a
one-time bootstrap token, and a route out to the control plane. It needs no
database, no UI, and no inbound access of any kind.

For a single-cluster installation, this goes in the same cluster as the
control plane, in a different namespace.

## Issue a bootstrap token

From the control plane, with an admin token. The token registers one cluster,
once, and is then exchanged for a key pair the controller keeps locally.

```sh
curl -sX POST https://haliphron.example.com/api/v1/clusters/bootstrap-tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"eu-prod"}'
```

`name` is required. The response carries the token once; only its digest is
stored.

To bound it, add `ttl_seconds` (default 86400) or `max_uses`.

## Install the runtime chart

Against the target cluster:

```sh
kubectl create namespace haliphron-system

kubectl -n haliphron-system create secret generic haliphron-bootstrap \
  --from-literal=token='...'

helm install haliphron-runtime deploy/charts/haliphron-runtime \
  --namespace haliphron-system \
  --set cluster.name=eu-prod \
  --set cluster.labels.region=eu-central-1 \
  --set backend.url=https://clusters.haliphron.example.com \
  --set controller.callbackURL=http://haliphron-runtime.haliphron-system.svc:8083 \
  --set backend.bootstrapTokenExistingSecret=haliphron-bootstrap
```

`backend.url` is the Cluster API's URL **as seen from inside this cluster**.

Pass the token through a Secret rather than `backend.bootstrapToken`. A value
lands in the release's own storage, where `helm get values` reads it.

### Keep the agents in their own namespace

The controller lands in `haliphron-system`; agent pods land in
`haliphron-agents`, which the chart creates. Do not collapse the two.

The agents' namespace carries a ResourceQuota, a LimitRange and a NetworkPolicy
that denies all ingress and most egress. Sharing the namespace applies all
three to the controller: the quota can refuse to schedule it, and the policy
cuts it off from the control plane. The chart prints a warning when it detects
this.

## Watch it register

Registration happens once, at startup. The result is an Ed25519 key pair kept
in a Secret in the controller's namespace.

```sh
kubectl -n haliphron-system logs -l app.kubernetes.io/instance=haliphron-runtime -f
```

A rejected bootstrap token is fatal and says so. Issue a new one and
`helm upgrade` with it — a spent token cannot be reused.

## Confirm the control plane sees it

```sh
curl -s https://haliphron.example.com/api/v1/clusters \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

The cluster reaches `Active` within one heartbeat interval — 10 seconds by
default. It carries its labels, its capacity and its free slots.

If it stays absent, the controller never reached the control plane. If it
appears and then goes `Unreachable`, the heartbeats stopped; check the proxy
in front of the cluster entrypoint, which must tolerate a 30-second read.

## Size the capacity

`cluster.capacitySlots` is how many runs this cluster accepts at once, 8 by
default and 256 at most. The control plane will not lease more than that.

Set it against the ResourceQuota rather than against the node pool: agent pods
run in the Guaranteed QoS class, so `agents.resourceQuota.hard` is the real
ceiling.

## Restrict what lands here

`cluster.labels` are matched by a role's `clusterSelector`. To keep a cluster
for one kind of work, label it and put the matching selector on the roles that
belong there.

`cluster.runtimes` is which agent types this cluster will run. Removing one
makes the control plane place those runs elsewhere.

## Wire up metrics

The controller serves Prometheus metrics:

```sh
helm upgrade haliphron-runtime deploy/charts/haliphron-runtime -n haliphron-system \
  --reuse-values --set metrics.serviceMonitor.enabled=true
```

Do not do the same on the control-plane chart. The backend serves no metrics
endpoint, and enabling its ServiceMonitor points Prometheus at a 404.

## Remove a cluster

```sh
helm uninstall haliphron-runtime -n haliphron-system
```

This leaves three things behind, on purpose:

- **The cluster record on the control plane.** There is no deregistration
  call. It goes `Unreachable` once heartbeats stop; revoke it from the API or
  the UI to end it.
- **The CRD**, by `helm.sh/resource-policy: keep`.
- **The agents' namespace.** Deleting it would delete every running agent Job
  with it.

To end a cluster's ability to act without touching the cluster itself:

```sh
curl -sX POST https://haliphron.example.com/api/v1/clusters/$CLUSTER_ID/revoke \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"decommissioned"}'
```

Its runs are left alone. What to do with the ones still in flight is your
decision, not a side effect.
