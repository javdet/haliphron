// Package version carries the build's identity.
package version

// Version is set at build time with -ldflags. The default says plainly that
// this binary was not built by the release path, which is more useful in a
// support conversation than a plausible-looking number.
var Version = "dev"
