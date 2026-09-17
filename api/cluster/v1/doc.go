// Package v1 is the Go form of the Cluster API messages — the pull-model
// protocol between the backend and a cluster's controller.
//
// openapi.yaml in this directory is the contract the two teams read; these
// types are what they compile against. The two are joined by
// TestClusterAPIGoTypesMatchSchema rather than by agreement, for the same
// reason the CRD is generated rather than transcribed: a field that exists on
// one side and not the other surfaces as a lease the backend issued and the
// controller cannot materialise.
//
// The rules the shapes here do not express — fencing by epoch, the two
// deadlines, report monotonicity, idempotency — are in
// docs/contracts/cluster-api.md, and are implemented once in fake/backend so
// that both sides can be developed against the same reading of them.
//
// Three constraints carried over from api/run/v1:
//
//   - No Kubernetes imports. The backend speaks this protocol and must not
//     grow a dependency on apimachinery to do it.
//   - No transport. Nothing here knows about HTTP status codes; Problem.Status
//     carries the number because it is on the wire, and the mapping from a
//     Problem to a response lives in the server.
//   - The spec and the completion report are not redefined. They are
//     run/v1.RenderedRunSpec and run/v1.CompletionReport, travelling here on a
//     different carrier.
package v1
