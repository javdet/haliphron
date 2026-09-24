# About the untrusted agent pod

There is one security assumption in this system, and everything else follows
from it:

**The agent pod is untrusted code.**

Not "code from a vendor we have not audited". Untrusted in the strong sense: it
executes commands a language model generated, in a repository whose contents
it did not choose, and those contents may contain instructions aimed at the
model. Prompt injection is not an exotic threat here — it is the expected
condition of the input.

Once you accept that, a series of otherwise-annoying decisions become
obviously correct.

## What the pod does not have

**No ServiceAccount token.** The pod cannot talk to the Kubernetes API,
because there is nothing mounted that would let it authenticate. An agent that
talks its way into `kubectl` finds nothing to authenticate with.

**No storage credentials.** In relay mode the pod posts artifacts to the
controller, which holds the volume. In object-store mode the pod gets
presigned links scoped to its own prefix, with a short life. It never holds a
bucket credential.

**No network, by default.** The agents' namespace carries a NetworkPolicy that
denies all ingress and permits egress only to DNS, to the controller's
callback, and to explicitly allowed CIDRs — which exclude the cluster's own
private ranges, and therefore the API server and every internal service.

**A namespace of its own**, with Pod Security admission at `restricted`, a
ResourceQuota that bounds a burst of runs, and a LimitRange. Keeping the
controller out of that namespace is not tidiness: the quota could refuse to
schedule it, and the NetworkPolicy would cut it off from the control plane.

**Nothing the control plane accepts directly.** The pod reports to the
controller, which reports to the backend. The control plane accepts no input
from a pod, ever. This is why the callback exists at all rather than the pod
posting its result straight to the API.

## What it does have, and for how long

The pod needs a model key and, when it has a repository, a git token. Both
arrive in a per-run Kubernetes Secret with an owner reference, so they are
deleted with the run's objects.

The git token lives about an hour. It is never written into `.git/config` — it
goes through a credential helper instead — and it is never logged. Neither is
the lease body that carried it: that message is the one place in the system
where secret material travels in the clear, and there is no debug mode that
prints it. A test asserts so, which is the only way a rule like that survives
contact with someone debugging at two in the morning.

Everything the pod uploads goes through a redactor first. The agent's own
child process gets an environment that was **built** rather than inherited:
the model key and the MCP headers, and nothing else. A process that inherits
its parent's environment inherits whatever happened to be in it.

## Why artifact keys are refused rather than sanitised

This is the sharpest example of the mindset, and it is worth walking through.

An agent asks to upload an artifact under a key. The key might be
`../escaped.md`. The tempting response is to sanitise it:
`filepath.Clean("/" + key)`.

That produces `/escaped.md`. Nothing has escaped the volume — the sanitisation
worked, in the sense it was written to work — and the object has silently left
the run's prefix. It is now somewhere it does not belong, under a name nobody
will connect to the run that wrote it.

So keys are **refused**, never repaired. There is one allow-list,
`runv1.ArtifactKey`, and it is called by both the controller's callback
handler and the backend's ingest.

One allow-list, deliberately, because two would be worse than one. A key that
one side accepts and the other refuses produces an object that is spooled,
forwarded, rejected and dropped — with the pod already gone and the result
unrecoverable. The shared constant is not code reuse for its own sake; it is
the thing that makes the two sides unable to disagree.

## The limits of this

An honest account has to name what the model does not cover.

**There is no filtering by hostname, and there cannot be.** NetworkPolicy
matches IP addresses, and FQDN-based policies are not available on every CNI.
An agent that can reach a permitted CIDR can reach any host inside it. The
default allows the public internet minus the private ranges — which is a real
boundary, and a coarse one. This is a known and accepted limitation, not an
oversight.

**The model provider and the forge are reachable by definition.** An agent
that can open a pull request can open a bad pull request. The control here is
the token's scope, not the network.

**A wide-open token undoes most of this.** Everything above constrains what a
pod can reach. None of it constrains what a git token with organisation-wide
write access can do once the pod has it.

**Resource exhaustion is bounded, not prevented.** The quota and LimitRange
stop one run from taking a cluster down. They do not stop an agent from
burning its whole budget doing something useless. A pod with no
`ephemeralStorage` limit is bounded only by the LimitRange default, and one
that fills a node's disk shows up as unrelated pods being evicted — which is
a genuinely confusing failure to debug.

## Why failing loudly is part of the model

The `mcp-verify` phase proves the MCP servers came up before the agent starts.
If they did not, the run fails with exit 30.

It would be friendlier to continue without them. It would also be worse. An
agent missing the tools it was promised does not stop and say so — it
cheerfully does the wrong thing with what it has, and you pay for a full run
to discover that. An explicit refusal costs one pod start.

The same instinct runs through the whole entrypoint. `validate` fails before
anything costs money. A prompt whose digest does not match never reaches the
model. A spec the cluster would silently prune is refused rather than run,
because a run with a quietly dropped setting is worse than a run that did not
happen.

## Related

- [About the architecture](architecture.md)
- [How to give runs a git token and a model key](../how-to/provide-run-credentials.md)
- [Credentials and the key encryption key](credentials-and-the-key-encryption-key.md)
