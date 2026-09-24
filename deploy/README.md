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
## Installing

The procedures live in the documentation set rather than here:

- [How to install the control plane](../docs/how-to/install-the-control-plane.md)
- [How to register a target cluster](../docs/how-to/register-a-target-cluster.md)
- [How to manage installation credentials](../docs/how-to/manage-installation-credentials.md)
- [How to upgrade an installation](../docs/how-to/upgrade-an-installation.md)
- [How to call Haliphron over MCP](../docs/how-to/call-haliphron-over-mcp.md)
- [How to give runs a git token and a model key](../docs/how-to/provide-run-credentials.md)

Every value in both charts is listed in the [Helm values
reference](../docs/reference/helm-values.md), and the reasoning behind the
two-chart split is in [about the
architecture](../docs/explanation/architecture.md).

What remains below is what those guides do not cover: the images, where pods
land, exposing an entrypoint through Gateway API, and working on the charts
themselves.

## Images

The charts pull from Docker Hub, which is where CI publishes on every push to
`main` and on every `v*` tag:

| Image | Built from | Platforms |
|---|---|---|
| `javdet/haliphron-backend` | `backend/Dockerfile` | amd64, arm64 |
| `javdet/haliphron-controller` | `controller/Dockerfile` | amd64, arm64 |
| `javdet/haliphron-frontend` | `frontend/Dockerfile` | amd64 |
| `javdet/haliphron-agent` | `image/Dockerfile` | amd64 |

The tag is the content of `VERSION`, plus `latest` on `main` and the commit
SHA on every build. Nothing has to be built by hand to install: the charts'
defaults name these repositories, and only the agent image is passed at install
time, by digest.

The first three are static binaries on `distroless/static`: no shell, no
package manager, non-root, and a read-only root filesystem at runtime. The
agent image is Debian slim, because the agent CLIs need Node and a real
userland.

To publish from a laptop instead — a local change that must reach a cluster
before it reaches `main`:

```sh
make images-push        # all four, to $(REGISTRY), for $(PLATFORM)
make backend-push       # or one at a time
make digests            # what to pin: each image's published digest
```

`buildx`, and `--platform linux/amd64` by default, because a native build on an
Apple laptop is arm64 and the nodes are not. The wrong architecture is
discovered as a CrashLoopBackOff with `exec format error`, one layer below
where anyone looks first. `make backend-image` and friends still build for the
local architecture, which is what the local `docker run` paths want.

## Where the pods run

Three sets of pods, placed from two charts, and the split is not arbitrary:

| Pods | Set by | Where |
|---|---|---|
| backend, UI | `nodeSelector` / `tolerations`, `frontend.*` | control plane chart |
| controller | `controller.nodeSelector` / `controller.tolerations` | runtime chart |
| agents | `agent.nodeSelector` / `agent.tolerations` | **control plane** chart |

The agents are placed by the control plane because placement travels in the
lease: what the controller materialises is rendered upstream, so a node
selector the controller invented would not be in the `AgentRun` anybody reads.
A role that names its own `nodeSelector` replaces the installation's outright
rather than merging with it — two selectors merged are a conjunction nobody
wrote, discovered as a run that stays `Pending`.

A labelled node group is usually a tainted one, and a selector without the
matching toleration places nothing. Both charts take both. `deploy/values/`
holds a worked example for a node group labelled and tainted `nodegroup=…`:

```sh
helm install haliphron deploy/charts/haliphron -n haliphron \
  -f deploy/values/ai-infra-control-plane.yaml ...
helm install haliphron-runtime deploy/charts/haliphron-runtime -n haliphron-system \
  -f deploy/values/ai-infra-runtime.yaml ...
```

A bad label value is refused when the backend starts, not when the first run is
materialised: the schema that would reject it belongs to an API server in
another cluster, and one run per hour failing to create is not how anyone wants
to find a typo.

## Gateway API instead of Ingress

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
