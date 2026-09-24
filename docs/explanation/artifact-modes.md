# About artifact modes

Results, logs and output envelopes have to go somewhere. There are two places
they can go, and which one an installation uses is the most consequential
operational choice it makes after the database.

**Relay mode** — the default — sends artifacts through the controller to the
backend, which writes them to a volume it holds.

**Object-store mode** sends them straight from the pod to an S3-compatible
bucket, using presigned links.

Both sit behind one port in the code, and most of the system cannot tell which
is running.

## Why the default is the awkward one

Object storage is the better answer at any serious scale, and it is not the
default. That is worth justifying.

The reason is the first five minutes. An installation should be able to run
its first agent without standing up MinIO, obtaining bucket credentials, and
deciding on a lifecycle policy. Relay mode needs none of that: a path with a
sensible default and no credentials anywhere.

A configuration format that made object storage the path of least resistance
would undo that on its own, even with relay mode nominally available. Defaults
are not neutral; they are what most installations will run forever.

## What relay mode costs

It puts every byte of every result and every log chunk through one process.

For an evaluation, or a team running tens of runs a day, this is invisible. At
volume it makes the control plane a bandwidth bottleneck for work it did not
do and has no interest in — and it makes the artifact volume a single point of
failure with a fixed size, defaulting to 50 Gi.

It also means the backend must serve every read. `GET /runs/{id}/result`
streams bytes in relay mode and answers `302` with a presigned link in
object-store mode, and that difference is the visible tip of the whole
trade-off: in object-store mode the bytes are already reachable from wherever
the caller is, because that is where the pod wrote them from inside a cluster.

## Why the pod never holds a bucket credential

In neither mode does the agent pod get a durable storage credential.

In relay mode it posts to the controller, which holds the volume. In
object-store mode it gets presigned links scoped to its own prefix with a
short life. The reason is the one that shapes everything in the pod: it is
[untrusted code](the-untrusted-agent-pod.md), and a long-lived bucket
credential in an environment that executes model-generated commands is a
credential you have given away.

This is also why artifact keys are refused rather than sanitised, and why one
allow-list serves both the controller's callback and the backend's ingest. A
presigned POST policy that accepts a key outside its prefix is a hole; a key
one side accepts and the other refuses is an object lost.

## The relay's acknowledgement is the contract

In relay mode the controller does not acknowledge an artifact until the bytes
are on its volume.

That single rule is what makes relay mode safe to use with the "persist before
git" ordering — see [what "Succeeded"
means](what-succeeded-means.md#persist-before-git). "Durable" in that rule
means whatever the mode makes it, and in relay mode it means acknowledged by
the controller. Without the rule, a pod could consider its result persisted
while the bytes were still in flight to a controller about to restart.

The spool is the other half of it. A controller whose backend is unreachable
writes artifacts to its own volume and forwards them later, which is why
`controller.spool.persistence` is on by default and why turning it off means a
controller restart during an outage loses reports.

Artifacts are forwarded **before** the completion report that names them. A
completion pointing at an object nobody has is worse than a late completion.

## Why exit 21 is `infra` in both modes

A failed artifact upload exits 21 and classifies as `infra` — retriable —
whichever mode is running. The reason is the same in both, which is a small
piece of evidence that the abstraction is real.

In relay mode, the controller's spool was unreachable or refused the bytes;
the next attempt finds a controller that has come back. In object-store mode,
the presigned bundle had expired; the controller mints a fresh one before
starting the next attempt.

Classifying it as `config` would burn a run whose result the entrypoint had
already produced — which is exactly the outcome the phase ordering exists to
prevent.

## Retention, and why there are three numbers

Logs are kept 30 days, results 180, everything else 90.

Three ages rather than one, because the three have different value. A log is
worth having while someone might still be debugging that run. A result is the
artefact of work that was paid for, and is worth half a year. Artifacts are in
between.

Where they are enforced differs by mode, and this is a real operational
difference rather than an implementation detail. In relay mode the backend
runs a reaper that walks the volume by prefix age. In object-store mode the
chart renders ILM or S3 lifecycle rules from the same values and the bucket
enforces them — which means changing retention needs the lifecycle rules
re-applied, not just a value changed.

## Choosing

Relay mode is right when you are evaluating, when runs are occasional, or when
an object store is a procurement conversation you would rather not have yet.

Object-store mode is right when the volume is filling, when the backend's
bandwidth is showing up in graphs, or when you already run MinIO or S3 and the
marginal cost is zero.

Moving between them is a configuration change, not a migration — but existing
artifacts do not move with it. Runs from before the switch keep their old
location, and a change of mode is therefore best made early.

## Related

- [Configuration](../reference/configuration.md#artifacts)
- [Helm values](../reference/helm-values.md#artifacts)
- [The untrusted agent pod](the-untrusted-agent-pod.md)
