// Package version is the controller's own version, as the control plane sees
// it.
//
// It is not decoration. The header travels on every Cluster API request because
// in a multi-cluster installation there is otherwise no telling which version
// sent what, and that question is only ever asked after something has gone
// wrong. The control plane also refuses versions outside the window it
// supports, so this string decides whether the cluster can talk at all.
package version

// Version is overridden at build time:
//
//	go build -ldflags "-X github.com/automagicops/haliphron/controller/version.Version=1.2.3"
//
// The default is a development build, deliberately inside any sane supported
// range so that a locally built controller can register against a locally run
// control plane.
var Version = "0.1.0"
