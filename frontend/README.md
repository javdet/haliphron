# haliphron-frontend

The control plane's web UI: a React + TypeScript bundle served by an nginx that
also proxies `/api` to the backend.

## Why the proxy

The backend sends no CORS headers. Its callers are scripts, controllers and
agents, and adding a browser-shaped authentication model to it was not part of
building this. So the UI lives on the API's own origin instead: nginx serves
`index.html` and forwards `/api` to the backend Service, and the browser makes
one same-origin request with no preflight. The Vite dev server does the same
thing with a proxy, so development and production have the same shape.

## Authentication

There is no OIDC and no session — the public API accepts one credential, a
bearer token with scopes (`runs:read`, `runs:write`, `admin`; `admin` implies
the others). The UI asks for one, verifies it against a one-row run listing,
and keeps it in `localStorage`. A 401 from anywhere puts the prompt back.

This is the part to revisit first. A browser holding an `admin` token is a
browser holding the keys to every cluster, and the honest fix is an identity
provider in front of the API. [src/auth/TokenGate.tsx](src/auth/TokenGate.tsx)
is the one component that has to change when that arrives.

## What it covers

| Page | What it does |
|---|---|
| Runs | Filter by status, role, agent and text; submit a run; page with the `before` cursor |
| Run | Status, accounting, source; result, logs and the attempt ledger |
| Clusters | Registered clusters with heartbeat and slots; revoke; mint bootstrap tokens |
| Roles | Create, edit and soft-delete role specs |
| Secrets | List, add and rotate; values are never shown because the API never returns them |
| Tokens | List, mint and revoke API tokens |

Not covered, because the backend has none of it: workflows, statistics and
audit. Logs are polled rather than streamed — the API pages over chunks and
offers no SSE.

## Working on it

```sh
npm install
npm run dev        # Vite on :5173, proxying /api to $HALIPHRON_API (:8080)
npm run typecheck  # strict tsc; there is no separate lint step
npm run build      # production bundle into dist/
```

Without a control plane to point at, there is a stand-in:

```sh
npm run build && npm run mock   # http://127.0.0.1:8099, token: dev
```

[mock/server.mjs](mock/server.mjs) answers the API's shapes from memory and
serves the built bundle from the same origin, which is the arrangement the
packaged nginx produces. It is a development convenience, not a test fake:
nothing asserts against it.

From the repository root the same steps run in a container — `make
frontend-check`, `make frontend-build`, `make frontend-image`.

## Deploying

`frontend.enabled=true` in the control-plane chart adds the Deployment and
Service and points the `api` entrypoint's Ingress or HTTPRoute at the UI, which
proxies through to the backend. Two values matter:

- `frontend.cspConnectSrc` — in object-store mode the backend answers a result
  with a redirect to a presigned link on the bucket's origin, and the page's
  `connect-src` has to allow it. `'self'` is right for relay mode.
- `backend.mode` — the UI only installs alongside an API listener. Asking for
  it on an `mcp` or `cluster` release fails the render rather than shipping a
  UI whose every request 502s.

## Contract drift

`src/api/types.ts` is transcribed by hand from `backend/restapi`, because the
backend publishes an OpenAPI document for the cluster and runtime contracts but
not for the public API. Nothing checks the two agree. Generating the client
from a published spec is the durable fix; until then, a change to a `json:` tag
in `backend/restapi` is a change here too.
