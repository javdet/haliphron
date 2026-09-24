# About credentials and the key encryption key

A fresh installation faces a small paradox and one large decision. The
paradox: the API accepts nothing but a bearer token, and the endpoint that
issues tokens is itself admin-scoped, so nobody can get in. The decision:
where the key that protects every stored secret lives.

Both are resolved on the first install, and both are resolved in ways worth
understanding before you rely on them.

## The bootstrap token, and the circle it breaks

The chart generates an admin token into a Secret, and the backend writes it to
its token store at startup. An operator reads it out once, mints a token of
their own, and revokes it.

This is the pattern Grafana uses for its first admin password, and it is
borrowed knowingly. The alternatives are worse in familiar ways: an
installation with no credential at all needs an out-of-band ritual to create
one; a fixed default credential is a back door with a changelog entry.

Three properties of it are deliberate and occasionally surprising.

**It is installed once, by digest.** Restarts and rollouts find the row and
leave it alone. `helm upgrade` does not rotate it, because the template reads
back the Secret it wrote last time rather than generating a new value. A
credential that changed on every upgrade would be a credential nobody could
write down.

**Revoking it is final.** The backend will not reinstate a revoked or expired
bootstrap row on the next start. This is the important one: a credential that
comes back whenever a node is drained is not a bootstrap, it is a back door
that reopens itself. Installing a fresh one is a deliberate act — a different
value, and a Deployment roll — and the spent one stays spent.

**It does not expire by default.** This looks like the wrong default, and the
argument for it is operational rather than theoretical: an expiry nobody
noticed is a control plane nobody can log into. Ending this credential should
be an act, not a date. `bootstrapToken.ttl` is there for installations that
disagree, and reasonable people do.

It is still a credential readable by anyone who can read Secrets in that
namespace. It is mounted as a file rather than passed as a variable — an
environment variable shows up in `kubectl describe pod` and in every crash
dump — but the namespace is the real boundary.

## The key encryption key

Managed secrets are encrypted under a data key of their own, and the data key
is wrapped by a key encryption key that **never enters the database**.

That is the entire property, and it is worth stating as a claim you can check:
*a dump of this database is not a credential leak.* Someone holding a copy of
the database holds wrapped bytes and nothing that unwraps them.

The cost of that property is the obvious one, and it is not small: losing the
key is unrecoverable rather than inconvenient. There is no recovery path, no
escrow, and nothing clever to try. Every managed secret becomes bytes nobody
can read.

### Why the chart generates one

An installation that could not store a model key until an operator had run
`openssl rand -base64 32` by hand would be an installation where the first
five minutes end in a detour. And the key the operator would have produced is
the same 32 random bytes the chart produces.

So the chart generates it, once, into a Secret of its own — and then works
quite hard to never touch it again.

**It is generated once.** Every later render reads the Secret back rather than
inventing a second key, so `helm upgrade` does not rotate it.

**Rotating it in place would be a disaster, not a chore.** A new key does not
re-wrap anything. It leaves every managed secret in the database wrapped under
bytes nobody has. Rotation here is a migration, and the chart deliberately
offers no button for it.

**`helm uninstall` leaves it behind**, by `helm.sh/resource-policy: keep`. A
reinstall under the same release name adopts it and the data is readable
again. Deleting it is a `kubectl delete secret` typed on purpose — which is
the appropriate amount of friction for an irreversible act.

### The GitOps problem

The read-back is a cluster lookup, and `helm template` cannot perform one.

Under Argo CD, Flux, or `helm template | kubectl apply`, every render would
produce a *different* key, and the last render would win. The symptom would be
managed secrets that stopped decrypting after an unrelated sync — which is a
genuinely awful thing to debug, because nothing about the failing sync would
point at the key.

So under GitOps the key must be stated: `encryption.existingSecret` or
`encryption.key`, with `autoGenerate` off. The same caveat applies to the
bootstrap token, where `helm template` and `--dry-run` render one that is
never installed.

This is a real sharp edge of the convenience, and the honest summary is that
auto-generation is a good default for `helm install` and a trap for anything
declarative.

## Managed or referenced

A secret is one of two kinds, and the distinction is about who is responsible.

**Managed** secrets are stored and encrypted here. Convenient, and they need a
key encryption key.

**Referenced** secrets are pointers — a Vault path, an External Secrets
reference — that the backend never resolves itself. Nothing sensitive is
stored, and no key is needed.

An installation whose secrets already live in Vault can run with no key at
all. The backend starts, says so, and serves referenced secrets; managed ones
are simply unavailable. Refusing to start would make the key mandatory for
installations that had deliberately chosen not to need it.

A secret cannot be both. Sending `value` and `ref` together is refused rather
than resolved, which is the same instinct that makes the chart refuse
`encryption.key` alongside `encryption.existingSecret`: both would write a
credential that nothing then reads, and the wrong value in a password manager
is worse than no value at all.

## What the API will never tell you

Listing secrets returns names, kinds, references and timestamps. Never a
value, and never a ciphertext.

An endpoint that returned ciphertext would turn a read scope into a
credential — offline, at the attacker's leisure, against whatever key material
they later obtain. The same reasoning governs tokens: a new token is returned
once, only its digest is stored, and there is no second chance to read it.

And unknown, revoked and expired tokens all produce the same 401. Telling a
caller which of the three it was tells an attacker which guess was closer.

## Related

- [How to manage installation credentials](../how-to/manage-installation-credentials.md)
- [How to give runs a git token and a model key](../how-to/provide-run-credentials.md)
- [The untrusted agent pod](the-untrusted-agent-pod.md)
