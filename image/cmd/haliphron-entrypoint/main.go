// Command haliphron-entrypoint is the agent image's entrypoint.
//
// It takes no arguments and reads no configuration file: everything it needs
// arrives as environment variables and as files under the two mounts the
// controller creates. That is the contract, and the reason the image can be
// verified with a single `docker run` against FakeControlPlane rather than
// against a cluster.
package main

import (
	"os"

	"github.com/automagicops/haliphron/image/entrypoint"
)

func main() { os.Exit(entrypoint.Main()) }
