# Reference

Descriptions of the machinery: what the endpoints accept, what the settings
default to, what the codes mean. Look things up here; learn how to use them in
the [how-to guides](../how-to/).

Everything on these pages is derived from the code and the charts. Where a
subject already has a normative specification — the wire contracts between the
components — these pages link to it rather than restating it, because a third
copy is a third thing to drift.

## Calling Haliphron

The two surfaces a caller talks to. They admit runs by the same rules; only
the transport differs.

- [REST API](rest-api.md) — the public HTTP API: endpoints, scopes, error
  codes, pagination and idempotency.
- [MCP tools](mcp-tools.md) — the same use cases as tools, for agents and
  IDEs.

## Configuring an installation

- [Configuration](configuration.md) — every environment variable the backend
  and the controller read, with its default.
- [Helm values](helm-values.md) — both charts, value by value.
- [Role spec](role-spec.md) — the fields of a role, and how they combine with
  a request and with the installation's policy ceiling.

## Reading a run

- [Statuses, phases and exit codes](statuses-and-exit-codes.md) — the four
  separate vocabularies a run is described in, and which part of the system
  owns each.

## The wire contracts

Not restated here. The four seams between the components are specified in
[`docs/contracts/`](../contracts/), and those documents are normative — tests
parse them. See [why the contracts are
normative](../explanation/why-the-contracts-are-normative.md).

| Contract | Document |
|---|---|
| Cluster API — backend to controller | [cluster-api.md](../contracts/cluster-api.md) |
| `AgentRun` CRD — controller to Kubernetes | [agentrun-crd.md](../contracts/agentrun-crd.md) |
| Agent runtime — controller to pod | [agent-runtime.md](../contracts/agent-runtime.md) |
| State store — backend to PostgreSQL | [run-store.md](../contracts/run-store.md) |
