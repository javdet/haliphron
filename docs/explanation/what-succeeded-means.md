# About what "Succeeded" means

`Succeeded` means the container exited 0.

That is the whole definition. It does not mean the bug was fixed, the test was
added, the refactor was correct, or the pull request is worth reviewing. It
means a process ran to completion and returned zero.

This page is about why the system refuses to claim more than that, and what
follows from the refusal.

## The temptation

Every part of this system could plausibly assert something stronger. The exit
code is there; the result is there; the pull request is there. A status field
called `Succeeded` invites a reading like "the task was done", and a UI that
displayed it that way would feel better to use.

It would also be a lie, told systematically, by a system with no way to check.

Haliphron runs an agent. It does not evaluate the agent's work, and nothing in
the design gives it the means to. Verifying that a task was actually solved is
listed as an explicit non-goal — and not because it is unimportant, but
because the honest place for it is a separate step that a human or a test suite
performs. The customer's existing CI takes it from there; Haliphron opens a
pull request and stops.

## Why the wording matters more than it looks

A status is read far more often than it is defined. It appears in a run list,
a Slack message, a dashboard tile, an agent's summary to another agent. In
none of those places is the definition beside it.

So if the wording implies verification, the system claims success it did not
have — thousands of times, silently, in exactly the contexts where someone is
deciding whether to look more closely. The cost is not one misleading label. It
is a steady erosion of the signal, until `Succeeded` means nothing and
everybody checks manually anyway.

This is why the rule is stated as a constraint on any UI or report built on
top of this system, and not merely as a note about a constant.

## `CompletedWithoutResult`

There is a second status that exists for the same reason, and it is the
clearest evidence that the principle is taken seriously.

A run can reach a terminal phase and never have its completion report
collected. The work happened; the report did not arrive. Such a run has a
recorded cost of zero and no result.

It would be easy to call it `Succeeded` — the pod did exit 0 — and let the
zero cost sit there looking like a fact. Instead the API reports
`CompletedWithoutResult`, which says precisely what is known: the run ended,
and we do not have its report.

The zero in the cost column is not a bargain. It is missing data wearing a
number.

## Persist before git

The same discipline shapes the order of the entrypoint's phases, and this is
the single most load-bearing ordering decision in the runtime contract.

`persist` runs immediately after the result exists, and **before** the git
phases. What has been paid for is made durable before anything allowed to fail.

The consequence is that a failed push does not lose a result. The agent ran,
the model was paid, `result.md` and `output.json` are in storage — and then
the push hit a protected branch and returned 403. The run fails with exit 20
and failure class `git`, and the result is still there to read.

Reverse the order and a retry pays for the model a second time to produce
something it already had. A test named
`TestAFailedPushLeavesTheResultAlreadyInStorage` is what keeps this true, and
it is one of the tests worth knowing by name.

"Durable" here means whatever the mode makes it — the controller's spool,
which does not acknowledge until the bytes are on its volume, or the object
store. The rule forbids a paid-for result existing only in the filesystem of a
pod about to be deleted. It does not require a bucket.

## What "skipped" means, while we are here

The runtime reports each phase as `ok`, `skipped` or `failed`, and `skipped`
is a first-class outcome rather than a polite word for failure.

A run without a repository skips four git phases; that is not a degraded run,
it is a different shape of run. A resumed attempt skips everything up to and
including `run`, because the checkpoint says that work is already done — which
is precisely how a retry avoids paying for the model twice.

Collapsing `skipped` into `failed` would make both meaningless.

## Where this leaves you

If you need to know whether the work was actually good, you need a step that
checks — a test suite, a review, a second agent with a verification prompt.
Haliphron gives you the material to judge: the result, the logs, the diff, the
pull request, the cost, the attempt ledger.

It does not give you the judgement, and it will not pretend to.

## Related

- [Statuses, phases and exit codes](../reference/statuses-and-exit-codes.md)
- [How to diagnose a failed run](../how-to/diagnose-a-failed-run.md)
- [Artifact modes](artifact-modes.md)
