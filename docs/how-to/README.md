# How-to guides

Task-oriented guides for people who already know what they want and need to
get it done. Each one starts and ends somewhere useful rather than covering a
subject exhaustively; where a full list of options would help, the guide links
to the [reference](../reference/) instead of reproducing it.

If you are meeting Haliphron for the first time, start with [the
tutorial](../tutorials/your-first-agent-run.md) rather than here.

## Running an installation

The path from nothing to a working control plane with a cluster attached to
it, and then keeping it running.

- [Install the control plane](install-the-control-plane.md) — the part
  installed once, with or without putting credentials in the Helm release.
- [Register a target cluster](register-a-target-cluster.md) — bootstrap
  tokens, capacity, the agents' namespace, and removing a cluster later.
- [Manage installation credentials](manage-installation-credentials.md) — the
  key encryption key and the first admin token: reading them out, replacing
  them, and what GitOps changes.
- [Upgrade an installation](upgrade-an-installation.md) — the two charts move
  independently, and the controller version range is what keeps them
  compatible.

## Running agents

What callers do, whether that caller is a person, a script or another agent.

- [Submit a run and collect its result](submit-a-run.md) — the primary act,
  over REST.
- [Call Haliphron from another agent over MCP](call-haliphron-over-mcp.md) —
  for IDEs, agents and automation platforms, including runs that start runs.
- [Define a role](define-a-role.md) — reusable configuration, so the same
  settings stop travelling with every request.
- [Give runs a git token and a model key](provide-run-credentials.md) — the
  two credentials every real run needs, and how to keep them out of prompts.

## When something has gone wrong

- [Diagnose a failed run](diagnose-a-failed-run.md) — read the exit code and
  the attempt ledger before going anywhere near the cluster.

## Not covered here

Two areas are deliberately absent, and are documented in
[`deploy/README.md`](../../deploy/README.md) instead: exposing the API through
Gateway API rather than Ingress, and building the images. Object storage is
configured through the values in [Helm
values](../reference/helm-values.md#object-storage) and explained in [artifact
modes](../explanation/artifact-modes.md).
