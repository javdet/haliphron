# About the pull model

Why does a control plane that orchestrates Kubernetes workloads hold no
kubeconfig?

Almost every system in this category works the other way round. The
orchestrator is given credentials for each cluster, connects in, creates
objects, and watches them. It is the obvious design, it is what the original
specification for this system described, and it is what Haliphron does not do.

Instead, the controller in each cluster **fetches** work over an outbound HTTPS
connection, drives it to completion on its own, and reports back. The control
plane never initiates anything.

## What buys this

Three things, in descending order of how often they matter.

**A cluster behind NAT needs no VPN.** This is the commercial argument, and it
is the one that decides. Haliphron is sold on-prem to platform teams, and "give
our SaaS inbound network access to your production cluster" is a conversation
that ends deals. Outbound HTTPS to one host is a conversation that ends in
five minutes.

**The control plane stores no cluster credentials.** There is nothing in the
database that grants access to a customer's cluster, because there is nothing
to store. A compromised backend is a serious problem; it is not a compromised
fleet.

**Availability decouples.** A controller that has taken work can finish it
without the backend. This is not a small property — a ten-minute control-plane
deploy would otherwise be a ten-minute outage for every run in flight — and it
is the property that most of the protocol's complexity exists to preserve.

## What it costs

A fair account has to include the bill, because it is substantial and it is
paid in awkwardness spread across the whole system.

**The long poll.** Work has to reach a cluster somehow, and with no inbound
connection the only options are polling and something like a persistent
stream. Haliphron long-polls: a lease request hangs for up to 30 seconds and
returns 204 if nothing arrived.

This is simple and it works, and it leaks into places it has no business being.
Every proxy in front of the Cluster API has to tolerate a read that takes
longer than 30 seconds. Nginx defaults to 60 and is fine; several Gateway API
implementations quietly default to 15 or 30 and are not. The symptom is
disconnects that read as network instability, and the chart goes to some
trouble to refuse a configuration that would cause them — including refusing to
render an `HTTPRoute` with no explicit timeout at all, because a default that
varies by implementation is worse than an error.

**Fencing has to be explicit.** When the orchestrator connects in, it knows
what is running because it can look. When it does not, a silent cluster is
ambiguous: the work may be finished, may be running, may never have started.
Resolving that ambiguity correctly is what the epoch, the two deadlines and
the acknowledgement are for. See [leases, epochs and
attempts](leases-epochs-and-attempts.md).

**Nothing can be pushed.** Cancellation is the clearest example. There is no
way to tell a cluster to stop a run; the instruction is recorded and travels
on the next heartbeat, within ten seconds by default. Callers are told this
plainly rather than being given an API that pretends to be synchronous. A run
that finishes in those ten seconds finishes.

**The controller authenticates itself.** The controller generates an Ed25519
key pair on first start, keeps the private half in a Secret in its own cluster,
and sends only the public half at registration. From then on it signs its own
short-lived tokens.

The obvious alternative — a shared secret, or a refresh token the backend
exchanges for access tokens — would give the control plane the ability to mint
a valid token on behalf of any cluster. For a system whose entire threat model
rests on the control plane having no access inside a cluster, that is a direct
contradiction: a compromised backend would get the clusters back. Self-signing
means compromising the backend gives an attacker what the backend already knew,
and no more.

## Why a CRD, then

With a pull model, the usual argument for a CRD evaporates. "The backend makes
one POST instead of N watches" is irrelevant when the backend does nothing in
the cluster at all. So why is the controller writing custom resources rather
than just creating Jobs?

Four reasons, and they hold up:

1. **The CR is the controller's durable state.** The controller must survive
   its own restart without losing work it accepted. Without a CR it would need
   local storage of its own; with one, etcd is that storage, for free.
2. **Pod failure is a native reconciliation problem.** Eviction,
   `ImagePullBackOff`, OOMKill, node drain — this is exactly what a reconcile
   loop is good at, and exactly what a bespoke watcher gets subtly wrong.
3. **Availability decoupling again.** Work already taken is played out while
   the backend is unreachable, and the CR is what makes "already taken"
   survive a controller restart.
4. **`kubectl get agentruns`.** For an on-prem product bought by a platform
   team, this is real value and not a nicety. It is also the seam through
   which a manually applied CR could later be adopted.

There is one CRD, not three. The specification named `RunAgent`,
`RunAgentWorkflow` and `RunAgentJob`; only the first became a custom resource.
A workflow is a long-lived transactional state machine with fan-in, timers and
versioning, and etcd has no cross-object transactions, no queries and no
history. Making it a CRD would also create two writers for one piece of state.
The engine belongs in the backend, and so does the scheduler.

## The honest guarantee

It is worth stating what this protocol actually promises, because it is not
exactly-once and nothing in this problem could be.

**At-least-once execution with converging side effects.**

A double execution is possible. The mitigations are that the attempt
checkpoint, the deterministic branch name and create-or-update on the pull
request all converge on one pull request rather than two. What does not
converge is the token spend: a run executed twice is paid for twice.

The system is built to make that rare, not impossible. The acknowledgement
rule exists precisely to close the case that would otherwise make it common —
see [nothing starts before the
acknowledgement](leases-epochs-and-attempts.md#nothing-starts-before-the-acknowledgement).

## A reasonable objection

Polling is wasteful, and a long poll is a connection held open doing nothing.
At ten clusters this is invisible. At a thousand it is a real number of open
connections on the control plane, and the design would want revisiting — a
push channel, or a message broker, or gRPC streaming.

The counter-argument is that a thousand clusters is not this product, and
buying scale you do not have with complexity you cannot remove is a bad trade.
The long poll is replaceable behind the Cluster API contract, which is the
sense in which the decision is reversible.

## Related

- [Leases, epochs and attempts](leases-epochs-and-attempts.md)
- [About the architecture](architecture.md)
