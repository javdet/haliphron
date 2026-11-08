# About cooperative locks

Agents that change shared infrastructure, such as a load balancer, a
database, DNS or a cluster, can trip over each other. Two runs started an hour
apart from two chat threads can both decide to migrate the same database. Locks
are how they find out: an agent records "I am working on this" before it
starts, and reads what the others have recorded.

This page explains why the locks are shaped the way they are. The tools are
described in the [MCP reference](../reference/mcp-tools.md#resource-locks).

## The backend does not judge

A lock is a free-form name and a free-form description, and nothing more. The
backend never compares two locks, never refuses one because of another, and
never holds a run back because of one.

That looks like a missing feature, but it is a choice. Whether "prod/db/orders"
and "orders database, eu-west" are the same thing, or whether adding an index
conflicts with a schema migration, is a question about infrastructure, and the
agent doing the work understands that infrastructure far better than a string
comparison does. A backend that matched names exactly would get these wrong
both ways. It would let overlapping work through under two spellings, and it
would block work that only shares a word. So the decision stays with the
agents, and the backend makes sure they have what they need to decide.

## What the backend does guarantee

**Who holds a lock comes from the token.** The holder is the run whose per-run
token set the lock. An agent cannot set a lock in another run's name, and an
operator's token cannot hold one.

**Only the holder releases a lock through an agent.** An agent cannot release
another run's lock, because one prompt injection would otherwise clear every
lock in the installation. An operator can force-release from the UI or the API,
with `admin`.

**A lock dies with its run.** When a run's status becomes terminal, its locks
are deleted in the same database transaction, by a trigger. No run can end and
leave its locks behind. A requested cancellation releases nothing: the pod is
still running until the cancel reaches it.

**Every lock expires.** By default it lasts as long as its run can live, so an
agent never has to renew it while it works. An expired lock disappears from
every read at once.

**There is an order.** Every lock gets a sequence number, and every lock write
in the installation waits its turn behind one database lock. That is what
makes the next rule possible.

## Write first, then judge

The obvious protocol is "list the locks, and if nothing is there, set mine".
It has a race: two agents list at the same moment, both see nothing, and both
go ahead.

The protocol here reverses the order. An agent sets its lock first. The answer
lists every live lock of another run that was set before it, under `earlier`.
If one of those concerns the same infrastructure, the agent is second: it
releases its lock, and either stops or defers the rest of its task until the
earlier lock is gone. Locks set after its own are not its concern, because
their holders see its lock in their own `earlier`.

Because writes take turns, the later of any two locks always sees the earlier
one. Two agents that agree their work overlaps cannot both miss each other,
and cannot both back off either: exactly one of them is second.

They have to agree, though. If one agent thinks the work overlaps and the
other does not, the one that thinks so backs off if it is second, and both
proceed if it is first. That is the cost of leaving judgement to the agents,
and the reason the tool descriptions ask for names that state the system and
the environment.

## Deferring instead of failing

An agent that finds what it needs busy has two honest options: stop and say
so, or hand the rest of its task to a later run. `schedule_agent` is the
second option. The agent writes down what is left, names the locks to wait for,
and finishes. A new run starts when those locks are gone.

That run is an ordinary child of the one that deferred. It has the same role,
the same repository and the same cost ceiling, and it counts against the same
limits on children and depth, so deferring cannot be used to escape a budget.
If the original run was started from chat, the continuation replies in the same
thread.

When a lock is released, every run waiting on it starts. If two of them want
the same thing, the write-first rule makes one of them yield and defer again.
That costs one short extra run. A scheduler that started waiters one at a time
would avoid that run, but it would have to decide which waiters conflict, and
that decision belongs to the agents.

The run that deferred usually ends `Succeeded`, which means
[only that it exited 0](what-succeeded-means.md). The UI shows on that run that
it handed work on, so nobody reads its status as "the task is done".

## Lock text is untrusted

A description written by one agent is read by every other agent that lists the
locks. Text from the repository an agent was working in can end up there, so
it is a path for prompt injection from one run to another. Lock text is
limited in length and refused, not cleaned, when it contains control characters
or bidirectional overrides. Wherever it is shown to a model, it sits inside a
block labelled as data written by other agents. `list_locks` shows each
holder's status and last activity, but none of its prompt or results.

## What locks do not protect against

Locks are cooperative. They do nothing about:

- people changing the same infrastructure by hand;
- CI pipelines, cron jobs and other automation that do not call these tools;
- an agent that skips the tools, whether because its role does not tell it to
  use them or because something in its input talked it out of it.

They are a way for well-behaved agents to stay out of each other's way, not a
safety boundary. For a hard guarantee, the infrastructure itself has to
enforce it, through its own locks, change windows or permissions.

Telling agents to use them is the operator's job, through the role's system
prompt. For example: "Before any change to the production load balancer, call
`set_lock` with the name `prod/lb` and follow the protocol in its description."
The tool descriptions explain the protocol. The role says when it applies.
