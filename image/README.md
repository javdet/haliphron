# The agent image

The last twelve inches: what the controller puts into the container, what the
image is obliged to do with it, what it produces and how it reports the result.
The contract is [docs/contracts/agent-runtime.md](../docs/contracts/agent-runtime.md);
this directory is its implementation.

The pod is the least trusted component in the system. It executes text that came
from outside, with tools that text selects, in a repository whose contents it
did not choose. Nothing here assumes otherwise.

## Layout

| Path | What |
|---|---|
| `entrypoint/` | the eighteen phases, as a package |
| `cmd/haliphron-entrypoint/` | the binary the image runs |
| `Dockerfile` | Debian slim, Node, both agent CLIs, non-root, read-only root |

Inside `entrypoint/`, the files follow the pipeline rather than the type
hierarchy: `phases.go` is init through role, `agent.go` and `runtimes.go` are the
model and the two CLIs, `persist.go` is output through finalize, `git.go` is the
four git phases, `report.go` is the completion webhook. `runner.go` is the spine
and the only place that decides what happens after something fails.

## The three rules

**A failure is classified by whoever has the cause, not the effect.** The
controller sees `exit 20`; this process saw "the forge answered 403 to a push
into a protected branch". Every error carries an exit code, a reason and a
message, and [failure.go](entrypoint/failure.go) is where they travel together.
The distinction is not academic: a 403 classed as retryable spends three pod
runs discovering the same answer.

**What has been paid for is made durable before anything allowed to fail.**
`persist` runs immediately after the result exists and before the git phases, so
a retry never pays for the model twice. This is the single most load-bearing
ordering decision in the contract, and
[TestAFailedPushLeavesTheResultAlreadyInStorage](entrypoint/contract_test.go) is
what keeps it true.

**Nothing secret leaves.** Every byte uploaded goes through the redactor, and
the agent's child process gets an environment that was built rather than
inherited — the model key and the MCP headers, and nothing else.

## Running the tests

```sh
make image-test        # the phases, exit codes and output envelope, race-checked
make image-build       # the image itself
```

The tests run against `fake/controlplane` in-process: every phase boundary,
every exit code and every branch of the resume rule, in under twenty seconds
and without a cluster, a backend or MinIO.

Three rows of the contract's checklist are not reachable from there and belong
to `docker run` — that the container starts as non-root, that it needs no write
outside an emptyDir, and that it completes a run under `--read-only` with tmpfs
on the four writable directories. `fake-controlplane -prepare` stages a run for
exactly that; see the header of
[fake/cmd/fake-controlplane](../fake/cmd/fake-controlplane/main.go) for the
command.

## What is deliberately not here

- **A repository cache.** Cloning a monorepo every run costs minutes; a PVC with
  a bare mirror and `--reference` is the answer, and it waits until clone time
  is a noticeable share of the duration.
- **Live log streaming.** Chunks work identically for a cluster on a laptop and
  one behind NAT, and they outlive the pod.
- **Cost verification.** The pod is an interested party and its arithmetic is
  untrustworthy by construction. Metering belongs at a proxy in front of the
  model.
