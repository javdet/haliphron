# How to manage installation credentials

An installation has two credentials nobody has to invent, and both are
generated on the first install: the key encryption key, and the first admin
token. This is how to read them out, replace them, and end them.

## Read them out after installing

Do this before anything else. Both are readable by anyone who can read Secrets
in the release namespace.

```sh
# the key encryption key
kubectl -n haliphron get secret haliphron-kek \
  -o jsonpath='{.data.kek}' | base64 -d; echo

# the first admin token
kubectl -n haliphron get secret haliphron-bootstrap \
  -o jsonpath='{.data.token}' | base64 -d; echo
```

Put the key somewhere that is not this cluster.

## Mint your own admin token, then revoke the bootstrap one

Use the bootstrap token once, to mint the token you will actually work with.
See [credentials and the key encryption
key](../explanation/credentials-and-the-key-encryption-key.md).

```sh
curl -sX POST https://haliphron.example.com/api/v1/tokens \
  -H "Authorization: Bearer $BOOTSTRAP" \
  -H 'Content-Type: application/json' \
  -d '{"name":"ops","scopes":["admin"]}'
```

The token is returned once. Only its digest is stored.

Then find the bootstrap row and revoke it:

```sh
curl -s https://haliphron.example.com/api/v1/tokens \
  -H "Authorization: Bearer $NEW_ADMIN_TOKEN"

curl -sX DELETE https://haliphron.example.com/api/v1/tokens/$BOOTSTRAP_TOKEN_ID \
  -H "Authorization: Bearer $NEW_ADMIN_TOKEN"
```

**Revoking is final.** The backend will not reinstate a revoked or expired
bootstrap row on the next start. If you revoke it and have no other admin
token, nothing can call the endpoint that issues one.

## Issue narrower tokens

Give each caller the least it needs. `admin` implies the other two scopes.

```sh
# a CI job that only submits work
curl -sX POST https://haliphron.example.com/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"ci","scopes":["runs:write"],"ttl_seconds":7776000}'

# a dashboard that only reads
curl -sX POST https://haliphron.example.com/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"grafana","scopes":["runs:read"]}'
```

Omitting `ttl_seconds` means the token does not expire.

## Install a fresh bootstrap token

A spent token stays spent, and `helm upgrade` does not rotate one: the
template reads back the Secret it wrote last time. To install a different one,
give it a different value and roll the Deployment.

```sh
kubectl -n haliphron create secret generic haliphron-admin \
  --from-literal=token="hlt_$(openssl rand -hex 16)"

helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set bootstrapToken.existingSecret=haliphron-admin
```

A different token is a new row. The old one stays revoked.

To install none at all — right for an installation whose tokens already exist,
wrong for a first install:

```sh
helm upgrade haliphron ... --set bootstrapToken.enabled=false
```

## Put a clock on the bootstrap token

`bootstrapToken.ttl` expires it. It is unset by default; prefer revoking the
token to dating it.

```sh
helm upgrade haliphron ... --set bootstrapToken.ttl=72h
```

## Supply your own key encryption key

The key wraps the data key of every managed secret, and it is not stored in the
database. Losing it is unrecoverable, not inconvenient — see [credentials and
the key encryption key](../explanation/credentials-and-the-key-encryption-key.md).

```sh
kubectl -n haliphron create secret generic haliphron-kek \
  --from-literal=kek="$(openssl rand -base64 32)"

helm upgrade haliphron deploy/charts/haliphron -n haliphron --reuse-values \
  --set encryption.existingSecret=haliphron-kek \
  --set encryption.autoGenerate=false
```

Setting both `encryption.key` and `encryption.existingSecret` is a render
failure, not a precedence question.

**Do not rotate this key in place.** A new key does not re-wrap anything; it
leaves every managed secret in the database wrapped under bytes nobody has.

## Under GitOps, state the key explicitly

The auto-generated key is read back from the cluster on every later render, and
`helm template` cannot perform a cluster lookup. Under Argo CD, Flux, or
`helm template | kubectl apply`, every render would produce a different key and
the last one would win.

Set `encryption.existingSecret` (or `encryption.key`) there, and leave
`encryption.autoGenerate` off.

The same applies to the bootstrap token: `helm template` and `--dry-run`
render one that is never installed.

## Run without a key encryption key

```sh
helm upgrade haliphron ... \
  --set encryption.autoGenerate=false
```

With no key and no `existingSecret`, the backend starts and says so. Managed
secrets are unavailable; [referenced
secrets](provide-run-credentials.md#reference-a-secret-you-already-manage)
still work. This is the right shape for an installation whose secrets all live
in Vault.

## What survives an uninstall

`helm uninstall` leaves the KEK Secret behind, by
`helm.sh/resource-policy: keep`. A reinstall under the same release name
adopts it and the managed secrets are readable again.

Deleting it is a `kubectl delete secret` typed on purpose.
