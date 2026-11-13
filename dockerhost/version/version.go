// Package version is the build's version, stamped by the linker.
package version

// Version is set with -ldflags "-X .../dockerhost/version.Version=...". The
// default is what a local build reports, and it is a valid SemVer on purpose:
// the control plane checks the controller version against its supported window
// on every request.
var Version = "0.1.0"
