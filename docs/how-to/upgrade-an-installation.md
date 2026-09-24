# How to upgrade an installation

The control plane and the target clusters are upgraded independently. A
version divergence between them is a normal state, not a fault — which is what
makes it possible to upgrade a control plane without coordinating an outage
window with every cluster that reports to it.

## Before you upgrade the control plane

Check the controller version range the new backend will accept:

```sh
helm show values deploy/charts/haliphron | grep -A3 ControllerVersion
```

Then check what your clusters are actually running:

```sh
curl -s https://haliphron.example.com/api/v1/clusters \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Every cluster's `controller_version` must fall inside
`clusterProtocol.minControllerVersion` to `maxControllerVersion`. A controller
outside the range is told its failure is fatal and stops taking work.

Keep at least N-1 minor supported, or rolling out a new control plane locks
out every controller of the previous release.

## Upgrade the control plane

```sh
helm dependency build deploy/charts/haliphron

helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set image.digest=sha256:...

kubectl -n haliphron rollout status deploy/haliphron
```

Migrations run at startup, before the process binds a port, with an advisory
lock serialising replicas. A rollout waits for its schema rather than starting
against half of one — which is why the Deployment has a startup probe with a
long fuse.

`helm upgrade` does not rotate the key encryption key or the bootstrap token.
Both templates read back the Secret they wrote last time.

Runs in flight are unaffected. A controller that cannot reach the control
plane during the rollout drives the work it already holds to completion and
queues its reports.

## Upgrade a target cluster

```sh
helm upgrade haliphron-runtime deploy/charts/haliphron-runtime \
  -n haliphron-system --reuse-values \
  --set image.digest=sha256:...
```

Registration is not repeated. The controller finds the identity Secret it
wrote the first time and reuses the key pair.

Agent Jobs already running are not touched by a controller restart. The
controller rebuilds its view from the CRs, which is where everything that has
to survive a restart lives.

### Drain it first if you would rather not

```sh
helm upgrade haliphron-runtime ... --reuse-values --set cluster.capacitySlots=0
```

The control plane stops leasing new work to it. Wait for its running runs to
finish, then upgrade and put the capacity back.

## Upgrade the agent image

The image decides what every run executes, so the control plane does not
choose one for you.

```sh
helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set agent.image=ghcr.io/automagicops/haliphron-agent@sha256:...
```

Pin by digest. Runs already admitted keep the image their spec was rendered
with; the new digest applies to the next admission.

## When the CRD schema changes

Nothing to do. The CRD ships in the chart's `templates/`, not `crds/`, so
`helm upgrade` applies schema changes.

Leave `crd.conversionWebhook.enabled` off. It must stay off until the
controller serves `/convert`; turning it on points the API server at a webhook
that is not there, and every read of an `AgentRun` then fails.

## Roll back

```sh
helm rollback haliphron -n haliphron
```

Check the schema first. Migrations are not reversed by a Helm rollback, and an
older backend against a newer schema is not a supported combination. If the
upgrade included a migration, restore the database alongside the rollback.

The runtime chart rolls back cleanly: it holds no schema.

## After either upgrade

```sh
curl -s https://haliphron.example.com/api/v1/clusters \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Every cluster should be `Active` within one heartbeat interval. One that has
gone `Unreachable` and stays there has been locked out by the version range,
or cannot reach the cluster entrypoint.

Then submit one run end to end. A control plane that answers `/api/v1/runs`
and a cluster that is `Active` still tell you nothing about whether work
flows.
