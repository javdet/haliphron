// The agent image's entrypoint.
//
// Its only haliphron dependency is api: the shapes it writes and the constants
// the controller sets. Nothing else belongs in the graph of a binary that runs
// inside the least trusted component in the system — and the Docker build's
// context is the proof, since it contains api and image and nothing more.
//
// The contract tests live in test/image, which does depend on the fake.
module github.com/automagicops/haliphron/image

go 1.25.0

require (
	github.com/automagicops/haliphron/api v0.0.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	golang.org/x/text v0.23.0
)

replace github.com/automagicops/haliphron/api => ../api
