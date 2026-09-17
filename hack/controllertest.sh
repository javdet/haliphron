#!/bin/sh
# The controller's tests: its own unit tests, then the contract tests that drive
# it against FakeBackend and a real API server. The second half needs envtest —
# pruning, defaulting and CEL are enforced by the API server and by nothing else.
set -e
ENVTEST_K8S=${ENVTEST_K8S:-1.34.x}
[ -x /go/bin/setup-envtest ] || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.22
KUBEBUILDER_ASSETS=$(/go/bin/setup-envtest use ${ENVTEST_K8S} --bin-dir /envtest -p path)
export KUBEBUILDER_ASSETS
cd /w/controller && go test -count=1 -race ./...
cd /w/test/controller && go test -count=1 ./...
