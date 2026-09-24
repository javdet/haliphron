# About leases, epochs and attempts

A cluster goes quiet. Is the run finished, still running, or did it never
start?

The control plane cannot look, because it has no way in — see [the pull
model](the-pull-model.md). It has to decide from the outside what a silence
means, and get it right, because the two wrong answers are both expensive. Give
the work to another cluster when the first one is still running it, and you get
two agents pushing to the same branch and two model bills. Refuse to reassign
when the first cluster is genuinely dead, and the run hangs forever.

This page is about the machinery that resolves that, and about one distinction
that is easy to get wrong.

## Epoch and attempt have different owners

This is the single idea worth carrying away.

**The epoch is ownership of the work.** It is raised only by the backend, and
only when it takes ownership back from a cluster.

**The attempt is a retry inside one ownership.** It is raised only by the
controller, on its own authority, without asking.

An attempt is really the triple `(run_id, lease_epoch, attempt)`, and `attempt`
resets to 1 whenever the epoch rises.

They look similar — both are counters that go up when something failed — and
conflating them breaks the system in a way that is hard to see. A controller
that raised the epoch would be claiming to have taken ownership from itself. A
backend that raised the attempt would be asserting something about work
happening in a cluster it cannot observe. Each counter is only ever written by
the party that actually knows.

The practical consequence, when you are reading an attempt ledger: attempts
climbing with a flat epoch is a cluster retrying work it still holds. The epoch
climbing is the control plane having taken the work away and given it out
again, possibly to somewhere else.

## Two deadlines, not one

A lease carries two, and they answer different questions.

**The ack deadline** asks: did the cluster ever start this? A controller that
takes a lease must acknowledge it, and an unacknowledged lease past its
deadline is treated as work that definitely did not begin. It is safe to raise
the epoch and give it to someone else.

**The lease deadline** asks: is this cluster still in charge? Its expiry moves
the run to `Unknown` and stops renewing — but it does **not** trigger automatic
reassignment, because the Job may be executing right at that moment.

That asymmetry is the whole point. One expiry is safe to act on
automatically; the other is not, and a human decides. `Unknown` is
uncomfortable — it is a state that means "we do not know" and it is visible in
the UI as exactly that — but the alternative is a system that guesses, and a
system that guesses wrong here spends money and opens conflicting pull
requests.

### The lease deadline does not oblige the controller to stop

A deliberate asymmetry, and a counter-intuitive one: the lease deadline is
authoritative for the backend and merely *informative* for the controller.

A controller that has lost connectivity keeps playing out the work it took and
accumulates its reports. When the network comes back, the reports are accepted
if the epoch still matches, and an instruction to abandon arrives if it does
not.

The opposite rule — "the lease expired, kill the Job" — would turn a
ten-second network glitch into the loss of an hour of an agent's work and the
model spend that went with it. The current rule occasionally wastes work that
has already been reassigned; the alternative reliably wastes work that has not.

## Nothing starts before the acknowledgement

Because the backend reassigns an unacknowledged lease on the grounds that the
work cannot have started, the controller has to make sure that is true.

So the Job waits for the acknowledgement — recorded as an annotation on the
custom resource — and a run that never receives one is discarded rather than
launched.

Without that rule, the one case that matters plays out badly: the
acknowledgement is lost in flight, the backend concludes the work never
started and reassigns it, and two clusters run the same agent against the same
branch. The rule is not defending against a common case. It is defending
against the case that turns a lost packet into a duplicated charge.

## An attempt is a replay, not a fresh start

For "attempt 2" to mean anything, it has to be an attempt at the *same* work.

So a run's spec is rendered once, at admission, and handed out byte-for-byte on
every lease afterwards — including a lease to a different cluster after a
reassignment. It is never re-rendered.

The alternative is quietly awful. If the spec were rebuilt per attempt, editing
a role between attempt 1 and attempt 2 would change what attempt 2 executes,
and the second attempt would stop being a retry and become a different run
wearing the same identifier. Debugging that means comparing two things the
system insists are one thing.

Three mechanisms defend the property at three layers: a database trigger, a CEL
immutability rule on the custom resource, and a digest pin on the prompt inside
the pod. Three, because it is the kind of invariant that is expensive to
discover broken and cheap to enforce.

## Fencing is by epoch, and only by epoch

A report arriving under a stale epoch changes nothing. That is what stops a
"zombie" controller — one that was unreachable, had its work reassigned, and
then came back — from overwriting the state of a run that now belongs to
someone else.

This is also why there is exactly one fencing mechanism. A second one would
eventually disagree with the first.

## Why reports are ordered by rank, not by time

Reports arrive out of order, and the system has to decide which of two is
newer.

It cannot use timestamps. Cluster clocks are not synchronised, and a clock
skewed by a few seconds would silently reorder a run's history. So every phase
carries a monotonic rank — `Pending` 10, `Starting` 20, `Running` 30, terminal
40 — and reports are ordered by `(attempt, rank)`.

A phase this build has never heard of ranks 0, which makes it lose every
comparison. That is deliberate: a newer peer reporting a phase an older build
does not recognise must not be able to move a run backwards. Failing safe here
means ignoring the unknown thing, not trusting it.

## Only two failure classes retry automatically

`infra` and `git` retry in the cluster. `agent`, `config` and `budget` do not.

The asymmetry is about what a retry would discover. An infrastructure failure —
an eviction, a node going away, a spool that was briefly unreachable — has a
good chance of not recurring; the cluster can repair it by trying again. A
configuration failure will fail identically every time, and burning three
attempts to find that out helps nobody.

The `agent` case is the interesting one, and it is not about hopelessness — a
model run again might well do better. It is about side effects. An agent that
already pushed a branch and opened a pull request has done externally visible
work, and replaying it spends the budget a second time and can produce
conflicting commits. So an `agent` failure is surfaced to a human, who can
retry it deliberately through the API if that is the right call.

`git` retries despite also touching the outside world, because the git phases
are written to converge: a deterministic branch name, `--force-with-lease`,
and create-or-update on the pull request. A retried push arrives at the same
place rather than a second one.

## One live attempt, enforced in the schema

The database carries a partial unique index, `run_attempts_one_live`, that
permits exactly one open attempt row per run. It is a real constraint with
real teeth: get it wrong and the run becomes permanently unrunnable, because
nothing can open the next attempt.

Two different paths have to close an open row, and they are genuinely
different cases. An epoch rise is handled by a trigger on `runs`. An attempt
rise *inside* an epoch is handled in application code, which closes rows with a
**strictly lower** attempt number — because reports arrive reordered, and a
late observation about attempt 1 must not close attempt 2.

That the constraint lives in SQL rather than in Go is a deliberate choice about
where correctness belongs. The rule has to hold no matter which code path
wrote the row, including a path someone adds later without reading this page.

## What this does not solve

Exactly-once execution, which is not available in this problem and is not
claimed. The guarantee is at-least-once with converging side effects: a
duplicated run converges on one pull request, but the model is paid for twice.

Everything above is an effort to make that rare. None of it makes it
impossible.

## Related

- [The pull model](the-pull-model.md)
- [Statuses, phases and exit codes](../reference/statuses-and-exit-codes.md)
- [How to diagnose a failed run](../how-to/diagnose-a-failed-run.md)
