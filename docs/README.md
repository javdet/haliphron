# Haliphron documentation

Haliphron runs headless coding agents — claude-code, codex — as one-shot
Kubernetes Jobs, on request from REST, MCP, Slack or a UI. It is delivered
on-prem and single-tenant, as two Helm charts.

The documentation is in four parts, and which one you want depends on what you
are doing right now.

## [Tutorial](tutorials/)

A lesson. [Your first agent run](tutorials/your-first-agent-run.md) takes you
from an empty cluster to an open pull request in about thirty minutes, so that
the rest of these pages have something to attach to.

Start here if Haliphron is new to you.

## [How-to guides](how-to/)

Recipes for people at work: installing the control plane, registering a
cluster, submitting runs, defining roles, managing credentials, and working
out why a run failed.

Start here if you know what you want and need the steps.

## [Reference](reference/)

The machinery described: the [REST API](reference/rest-api.md), the [MCP
tools](reference/mcp-tools.md), every [environment
variable](reference/configuration.md), every [Helm
value](reference/helm-values.md), the [role spec](reference/role-spec.md), and
the [statuses and exit codes](reference/statuses-and-exit-codes.md) a run is
described in.

Start here if you need to look something up.

## [Explanation](explanation/)

Why the system is shaped this way — [the pull
model](explanation/the-pull-model.md), [the untrusted agent
pod](explanation/the-untrusted-agent-pod.md), [leases, epochs and
attempts](explanation/leases-epochs-and-attempts.md), and [what "Succeeded"
means](explanation/what-succeeded-means.md).

Start here if you want to understand the design rather than operate it.

## Also in this directory

Two kinds of document sit alongside the four quadrants and follow different
rules.

**[The contracts](contracts/)** — `cluster-api.md`, `agentrun-crd.md`,
`agent-runtime.md` and `run-store.md` — are normative specifications of the
four seams between the components. Tests parse them: editing a documented
table without editing the Go fails a test. See [why the contracts are
normative](explanation/why-the-contracts-are-normative.md).

**[architecture.md](architecture.md)** is the original design record: a dated
argument for the decisions the system rests on. It is not maintained against
the code, and describes several things that do not exist. Read it for the
reasoning; read the pages above for what was built.

## Status

This is phase 1 of five: the vertical `run_agent` slice, complete through
every layer. There is no workflow engine and no scheduler — both are designed
and neither is implemented. The documentation describes what is built.
