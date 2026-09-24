# About why the contracts are normative

Most documentation describes code. The four documents in `docs/contracts/` do
the opposite: they are the specification, and the code is judged against them.

When code and a contract document disagree, that is a bug in one of them. It
is not a stale doc.

This is an unusual claim to make about prose, and it only means anything
because tests enforce it. This page is about why the arrangement exists, what
it costs, and where its limits are.

## The four seams

The Cluster API sits between the backend and the controller. The `AgentRun`
CRD sits between the controller and Kubernetes. The agent runtime contract
sits between the controller and the pod it launches. The state store contract
sits between the backend and PostgreSQL.

Each has a schema and a document, and they do different jobs. **The schema
says what is transmitted. The document says what it means and how to behave** —
fencing, deadlines, monotonicity, idempotency, what to do when things fail.

A JSON schema can say that `leaseEpoch` is an integer. It cannot say that only
the backend may raise it, that a report under a stale one changes nothing, or
that its expiry does not oblige the controller to stop working. All of that is
the contract, and all of it lives in prose.

## Why it was done this way

The immediate reason was parallel development. Three tracks — backend,
controller, agent image — had to proceed at once, each against a fake of the
other side, before the other side existed. That is only possible if the
contract is fixed first and is precise enough to build against.

It also produced the fakes, which are a deliverable rather than test
utilities. Each implements one side faithfully enough that the other can be
written and regression-tested against it, *including the rules that only fire
on failure*: epoch fencing, report monotonicity, the two deadlines.

A fake that is merely permissive is worse than no fake. It teaches the other
side habits the real implementation will reject, and the bill arrives at
integration. So the fakes implement the refusals too, and expose them as
things a test can trigger on purpose.

The controller deliberately does not depend on the fakes, so nothing that
ships to a customer can carry a test double.

## How the prose is enforced

This is the part that makes the claim more than an aspiration.

`test/contract/runtime_test.go` **parses the tables** in
`docs/contracts/agent-runtime.md` and asserts them against the Go constants.
Editing a documented table without editing the Go fails a test. Editing the Go
without editing the table fails the same test.

`drift_test.go` compares the hand-written OpenAPI against the generated CRD,
key by key, so two descriptions of the same object cannot drift apart
unnoticed.

The generated artifacts are covered the same way. Deepcopy functions, the CRD
bases, the controller RBAC and the chart's copies of both are produced from
the Go types by `make generate` and `make chart-gen`, and `make verify` fails
if any committed artifact is stale. A forgotten regeneration is caught by CI
rather than by a cluster.

The effect is that documentation cannot rot quietly. It can still be wrong —
a table can be wrong in the code and the prose simultaneously — but it cannot
be *out of date*, which is the failure mode that makes most documentation
untrustworthy.

## What it costs

It is slower. Changing a documented constant means editing the constant, the
table, and often the OpenAPI, then regenerating and committing the artifacts.
For a one-character change this is irritating, and the irritation is the point:
the friction is proportional to the number of parties who have to agree.

It also constrains this documentation set. The four contract documents are not
reshaped into Diátaxis quadrants, even though they blur reference and
explanation quite freely — several of their sections are pure *why*. Splitting
them would break the tests that parse them. So they are left alone and linked
to, and the generated pages restate nothing from them.

That is a real compromise with the documentation system, made deliberately.
Tests that enforce a document are worth more than a tidy taxonomy.

## Where this does not reach

**`docs/architecture.md` is not enforced.** It is a design record, and it
describes a system larger than the one that exists — an OIDC identity
provider, a workflow engine, a scheduler, and gin as the HTTP stack, which was
never used. Nothing fails when it drifts, and it has drifted.

That is not a criticism of it. A dated argument about why decisions were made
is valuable precisely because it is not continuously rewritten. But it means
the document has a different standing from the contracts, and reading it as
though it were normative will mislead you.

**The compatibility rule is a convention, not a check.** Both wire contracts
say unknown fields are ignored rather than refused, so a peer written for a
newer version still works. Nothing tests every future field for this; it is a
discipline.

**A pruned field is the trap the CRD contract spends most effort on.**
Kubernetes silently drops fields not in the schema, so a spec that looks
accepted can arrive missing a setting. The controller refuses a spec the
cluster would prune rather than running it — a run with a silently lost
setting being worse than a run that did not happen — and the CRD ships in the
chart's `templates/` rather than `crds/` for the same reason. Helm installs
`crds/` once and never updates it, so the first additive schema change would
fail to reach the cluster and surface as exactly that pruning.

## The underlying idea

Two independently written implementations of the same contract will always
diverge somewhere, and the divergence will be in the failure paths, because
that is where nobody looks.

The mechanisms here all attack that. A failure carries an explicit
`Problem.action` rather than a status code, because two sides always disagree
about what a 409 means, and there is exactly one place in the controller that
reads it. The fakes implement the refusals. The tests parse the prose.

None of this is free, and on a smaller system it would be overkill. On one
where the two sides of a contract are a control plane and a controller
deployed on someone else's infrastructure, and where getting the failure path
wrong means duplicate model spend and conflicting pull requests, it is the
cheap option.

## Related

- [About the architecture](architecture.md)
- [Leases, epochs and attempts](leases-epochs-and-attempts.md)
