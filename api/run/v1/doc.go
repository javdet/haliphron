// Package v1 holds the types that cross every haliphron boundary: the Cluster
// API between backend and controller, the AgentRun CRD between controller and
// Kubernetes, and the completion report between the agent pod and the
// controller.
//
// The package is the single source for those shapes. The CRD manifest is
// generated from it, and api/cluster/v1/openapi.yaml is checked against it, so
// the three contracts cannot drift into disagreement one release at a time.
//
// Two rules keep it usable from all three sides:
//
//   - No Kubernetes imports. The backend must not grow a dependency on
//     apimachinery just to render a run spec. Quantities and durations are
//     therefore plain strings and ints validated by schema, not
//     resource.Quantity and metav1.Duration.
//   - No transport in the types. Nothing here knows about HTTP, etcd or SQL.
//
// +kubebuilder:object:generate=true
package v1
