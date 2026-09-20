# Deploying Haliphron

Two charts, because there are two things to install and they have almost
nothing in common.

| Chart | Where | What it installs |
|---|---|---|
| `charts/haliphron` | The control plane, once | the backend (REST, MCP, Cluster API, expiry scanners), its Service and ingresses, optionally PostgreSQL and MinIO |
| `charts/haliphron-runtime` | Every target cluster | the controller, the `AgentRun` CRD, the agents' namespace with its quota, LimitRange and default-deny NetworkPolicy, and the RBAC for both |

The split follows the direction of the arrow between them. Nothing connects
into a target cluster: the controller fetches work and reports back, so a
cluster behind NAT needs no VPN and the control plane stores no kubeconfig. A
target cluster therefore needs no database, no UI and no engine.

For a single-cluster installation both charts go into the same cluster, in
different namespaces.

## Before you start

You need:

- PostgreSQL 16 or later, and an S3-compatible bucket. Both charts can bring
  their own for an evaluation (`--set postgresql.enabled=true`,
  `--set minio.enabled=true`); neither is a good idea for an installation that
  has to survive a year — see the note in `charts/haliphron/Chart.yaml`.
- An agent image, pinned by digest. The chart will not choose one: the image
  decides what every run executes.
- `helm dependency build charts/haliphron` (or `make chart-deps`). Helm
  resolves declared dependencies whether or not their condition is met, so
  this is required even with both subcharts off.

## Images

```sh
make backend-image      # backend/Dockerfile      -> haliphron/backend:dev
make controller-image   # controller/Dockerfile   -> haliphron/controller:dev
make image-build        # image/Dockerfile        -> haliphron/agent:dev
```

The first two are static binaries on `distroless/static`: no shell, no package
manager, non-root, and a read-only root filesystem at runtime. The agent image
is Debian slim, because the agent CLIs need Node and a real userland.

## 1. The control plane

Credentials go in first, as Secrets the chart references rather than as values
it copies:

```sh
kubectl create namespace haliphron

kubectl -n haliphron create secret generic haliphron-database \
  --from-literal=dsn='postgres://haliphron:...@postgres:5432/haliphron?sslmode=require'

kubectl -n haliphron create secret generic haliphron-object-storage \
  --from-literal=accessKey=... --from-literal=secretKey=...

# The key that wraps every managed secret. It never enters the database, which
# is what makes a database dump not a credential leak — and what makes losing
# this key unrecoverable. Back it up before you store anything under it.
kubectl -n haliphron create secret generic haliphron-kek \
  --from-literal=kek="$(openssl rand -base64 32)"
```

Then:

```sh
helm install haliphron deploy/charts/haliphron \
  --namespace haliphron \
  --set database.existingSecret=haliphron-database \
  --set objectStorage.existingSecret=haliphron-object-storage \
  --set encryption.existingSecret=haliphron-kek \
  --set agent.image=ghcr.io/automagicops/haliphron-agent@sha256:... \
  --set ingress.api.enabled=true --set ingress.api.host=haliphron.example.com \
  --set ingress.cluster.enabled=true --set ingress.cluster.host=clusters.haliphron.example.com
```

The first start applies the schema before it binds a port — every replica calls
the migration, and an advisory lock makes a rollout wait for its schema instead
of starting against half of one. That is why the Deployment has a startup probe
with a long fuse and a liveness probe that does not touch the database.

**The Cluster API and long polls.** A lease poll hangs for up to 30 seconds.
Every proxy in front of `ingress.cluster` must allow a read to take longer than
that, or the controllers get disconnects instead of empty responses and it
reads as network instability. The chart sets `proxy-read-timeout: 120` and
refuses to render if you lower it below the configured wait — but it cannot see
the load balancer in front of your ingress controller. Check that one yourself.

### Gateway API instead of Ingress

Every entrypoint can be exposed as an `HTTPRoute` instead of an `Ingress`. The
choice is per entrypoint, not per chart, so an installation can move one route
at a time:

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron \
  --set ingress.api.enabled=false \
  --set httpRoute.api.enabled=true \
  --set httpRoute.api.parentRefs[0].name=external \
  --set httpRoute.api.parentRefs[0].namespace=gateway-system \
  --set httpRoute.api.parentRefs[0].sectionName=https \
  --set httpRoute.api.hostnames[0]=haliphron.example.com
```

The chart writes routes and does not write a Gateway. A Gateway owns an
address, a certificate and a listener policy; it is shared by everything that
attaches to it and outlives any one release. Point `parentRefs` at one you
already run.

Two things this costs you that an Ingress did not:

- **The route can be accepted by the API server and attach to nothing.** A
  `parentRefs` entry naming a Gateway in another namespace needs a
  `ReferenceGrant` on the Gateway's side, or the route is `Accepted=False` with
  `NotAllowedByListeners` — and the only symptom is a 404. The chart refuses to
  render a route with no `parentRefs` at all; it cannot check the grant.
  `kubectl get httproute <name> -o jsonpath='{.status.parents[*].conditions[*]}'`
  is what tells you.
- **The long poll again.** `httpRoute.cluster.timeouts.request` is the Gateway
  API's field for what `proxy-read-timeout` was, and the chart applies the same
  rule: it must exceed `clusterProtocol.timings.maxWaitSeconds`, or be `0s` for
  no timeout. Leaving it unset is a render failure rather than a default,
  because several implementations quietly default to 15 or 30 seconds and the
  resulting disconnects read as network instability.

Enabling both for one entrypoint is allowed — it is the normal state
mid-migration — and the install notes say so, because otherwise it is a service
reachable two ways with two sets of timeouts and two TLS configurations.

`httpRoute.apiVersion` defaults to `gateway.networking.k8s.io/v1`; set
`v1beta1` for a cluster still on Gateway API 0.x.

### Split deployment

`backend.mode` is `all` by default: one Deployment, four listeners. Where MCP
has to be exposed outward while REST stays inside the perimeter, install the
chart twice against the same database:

```sh
helm install haliphron-api deploy/charts/haliphron -n haliphron --set backend.mode=api ...
helm install haliphron-mcp deploy/charts/haliphron -n haliphron --set backend.mode=mcp ...
```

The mode is part of the selector, so the two releases' Services do not select
each other's pods. Exactly one release must serve `cluster` or `all`: the
expiry scanners run wherever the Cluster API does.

## 2. Each target cluster

Issue a bootstrap token from the control plane. It registers one cluster, once,
and is then exchanged for a key pair the controller keeps locally:

```sh
curl -sX POST https://haliphron.example.com/v1/clusters/bootstrap-tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{}'
```

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
  --set backend.bootstrapTokenExistingSecret=haliphron-bootstrap
```

The controller lands in `haliphron-system`; the agent pods land in
`haliphron-agents`, which the chart creates. Keep them apart. The agents'
namespace carries a ResourceQuota, a LimitRange and a NetworkPolicy that denies
all ingress and most egress, and every one of those would apply to the
controller too if it shared the namespace — the policy in particular would cut
it off from the control plane.

### What the agents' namespace is for

The agent pod runs commands a model wrote, in a repository whose contents may
contain prompt injection. It is untrusted code, and the chart treats it that
way: a namespace of its own with Pod Security admission at `restricted`, a
ServiceAccount with no RBAC and no mounted token, a quota that bounds a burst
of runs, and a NetworkPolicy that denies ingress entirely and allows egress
only to DNS, to the controller's callback, and to the CIDRs in
`agents.networkPolicy.allowedCIDRs` — which excludes the cluster's own private
ranges, and so excludes the API server.

There is no filtering by hostname and there cannot be: NetworkPolicy matches
addresses, and FQDN policies are not available on every CNI. An agent that
reaches a permitted CIDR can reach any host inside it. This is a known and
accepted limitation.

## Observability

The controller serves Prometheus metrics on its metrics Service, and
`metrics.serviceMonitor.enabled=true` wires them up.

The backend does not. It serves `/healthz`, `/readyz` and `/version` on the
health port and nothing else, so the control-plane chart's ServiceMonitor stays
off: enabling it points Prometheus at a 404. The template ships anyway, so that
the day the endpoint lands it is a value change and not a chart change.

## Upgrades

The control plane and the runtime charts are versioned and upgraded
independently, and a version divergence between them is a normal state. The
backend declares the controller version range it accepts
(`clusterProtocol.min/maxControllerVersion`); keep at least N-1 minor, or
rolling out a new control plane locks out every controller of the previous
release.

The CRD ships in `templates/` with `helm.sh/resource-policy: keep`, not in
`crds/`. Helm installs what is in `crds/` once and never updates it, so the
first additive schema change would fail to reach the cluster and surface as
pruned fields in `spec` — runs with a silently lost setting. The resource
policy means `helm uninstall` leaves the CRD, and the agents' namespace, alone.

`crd.conversionWebhook.enabled` is off, and must stay off until the controller
serves `/convert`: turning it on points the API server at a webhook that is not
there, and every read of an `AgentRun` then fails. The chart wiring exists
early because the Service, the certificate and the CA injection are the
expensive part of adding `v1` later — not the handler.

## Uninstalling a cluster

`helm uninstall` leaves a cluster record behind, which goes to `Unreachable`
once the heartbeats stop. There is no deregistration call; delete the record by
hand from the control plane. The CRD and the agents' namespace survive by
design — deleting the namespace would delete every running agent Job with it.

## Development

```sh
make chart-gen        # regenerate the chart's copies of the CRD and the RBAC
make chart-lint       # both charts, against each chart's ci/lint-values.yaml
make chart-template   # render both, which catches what lint does not
make chart-package    # two .tgz into dist/
make verify           # fails if any generated artifact, chart copies included, is stale
```

`templates/crd.yaml` and `templates/rbac-controller.yaml` in the runtime chart
are generated from `config/crd/bases` and `config/rbac` by `hack/chartgen.sh`.
Edit the Go types or the kubebuilder markers, run `make generate chart-gen`,
and commit both.
