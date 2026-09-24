# Explanation

Why the system is shaped the way it is. These pages are for reading away from
the keyboard — they carry no instructions and no lookup tables, and none of
them is needed to get work done.

They are worth reading anyway, because several of Haliphron's mechanisms look
arbitrary until you know what they are defending against, and the cost of
misreading them is measured in duplicate model spend.

## Start here

- [About the architecture](architecture.md) — the pieces, how they are
  deployed, and what is deliberately not built yet. Carries the C4 System
  Context and Container diagrams.

## The two ideas that shape everything

Almost every awkward mechanism in the system is downstream of one of these.

- [About the pull model](the-pull-model.md) — the control plane never connects
  to a cluster. What that buys, and the substantial bill it comes with.
- [About the untrusted agent pod](the-untrusted-agent-pod.md) — the pod runs
  model-generated commands against possibly hostile input, and is treated
  accordingly. Includes where the model stops working.

## Getting correctness right across a network

- [About leases, epochs and attempts](leases-epochs-and-attempts.md) — how the
  system decides what a cluster's silence means, and why two counters that
  look alike must never be confused.
- [About why the contracts are normative](why-the-contracts-are-normative.md)
  — the documents in `docs/contracts/` are the specification, and tests
  enforce the prose.

## Two decisions you will meet in practice

- [About what "Succeeded" means](what-succeeded-means.md) — exit code 0, and
  deliberately nothing more.
- [About artifact modes](artifact-modes.md) — relay or object storage, why the
  awkward one is the default, and how to choose.

## A note on `docs/architecture.md`

The [design document](../architecture.md) at the root of `docs/` is the
original dated argument for these decisions, and is the source most of this
section draws on. It is not maintained against the code: it describes an
identity provider, a workflow engine and a scheduler that do not exist, and an
HTTP framework that was never used. Read it for the reasoning, and read these
pages for what was built.
