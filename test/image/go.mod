// The agent image's contract tests.
//
// A module of their own, for the same reason test/contract and test/store are:
// they depend on FakeControlPlane, and the image must not. A `replace` in the
// image's own go.mod would put the fake in the graph of the binary that ships,
// and the Docker build would need the fake's sources in its context to resolve
// a dependency no shipped code has.
module github.com/automagicops/haliphron/test/image

go 1.25.0

require (
	github.com/automagicops/haliphron/api v0.0.0
	github.com/automagicops/haliphron/fake v0.0.0
	github.com/automagicops/haliphron/image v0.0.0
)

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/text v0.23.0 // indirect
)

replace github.com/automagicops/haliphron/api => ../../api

replace github.com/automagicops/haliphron/fake => ../../fake

replace github.com/automagicops/haliphron/image => ../../image
