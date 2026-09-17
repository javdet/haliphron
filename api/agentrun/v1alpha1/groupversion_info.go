// Package v1alpha1 contains the AgentRun custom resource: the controller's
// durable record of one leased run.
//
// AgentRun is not the system of record. PostgreSQL is. The CR exists so that
// the controller survives its own restart without a local store, so that pod
// failures become an ordinary reconcile problem, so that a lease already taken
// can be played out while the backend is unreachable, and so that an operator
// can type `kubectl get agentruns`. It is deleted by TTL once the run is over.
//
// +kubebuilder:object:generate=true
// +groupName=haliphron.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group. It is deliberately not the product's DNS name in
// a customer's cluster: the group is part of the contract and must not change
// with branding.
const GroupName = "haliphron.io"

var (
	// GroupVersion is the group and version of this package's types.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeBuilder registers the types. It is built on apimachinery rather
	// than controller-runtime's helper so that the backend's tests can decode
	// an AgentRun without depending on controller-runtime.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &AgentRun{}, &AgentRunList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Resource returns a GroupResource in this group, for error messages.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
