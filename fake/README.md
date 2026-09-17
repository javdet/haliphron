# Fakes

The three fakes named in the phasing plan: they are part of the contract
deliverable, not test utilities owned by one of the teams. Each implements one
side of a contract faithfully enough that the other side can be written,
reviewed and regression-tested against it without the real thing existing.

| Fake | Stands in for | Used by | State |
|---|---|---|---|
| `backend` | the control plane's Cluster API | the controller track | done |
| `controller` | a cluster and the objects a controller builds in it | the backend track | done |
| `controlplane` | object storage plus the controller's callback | the agent image track | done |

`controlplane` stands in for the pair the pod actually talks to: presigned
object storage and the controller's completion webhook. It is not MinIO — objects
live in a map and the links are signed with HMAC — because what the image must
get right is the shape of the exchange and the answers it gets when things go
wrong: 403 on an expired signature, 404 on a checkpoint nobody has written yet,
a POST policy that refuses a key outside its prefix. Every one of those is a row
in the runtime contract's checklist, and every one of them is implemented here.

`controller` is an HTTP client of the Cluster API, so it runs against the real
backend and against `backend` alike. Pointing the two fakes at each other is not
a curiosity: it is the only place where the two independent readings of one
contract are checked against each other, and `test/contract` goes one further by
applying what `controller` builds to a real API server.

A fake that is merely permissive is worse than no fake: it teaches the other
side habits the real implementation will reject. These implement the rules from
`docs/contracts/`, including the ones that only ever fire in failure —
fencing by epoch, report monotonicity, the two deadlines — and expose them as
things a test can trigger on purpose.
