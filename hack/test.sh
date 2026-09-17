#!/bin/sh
# Contract tests. The CRD ones need a real API server: CEL cost estimation,
# structural pruning and defaulting live there and nowhere else.
set -e
ENVTEST_K8S=${ENVTEST_K8S:-1.34.x}
[ -x /go/bin/setup-envtest ] || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.22
KUBEBUILDER_ASSETS=$(/go/bin/setup-envtest use ${ENVTEST_K8S} --bin-dir /envtest -p path)
export KUBEBUILDER_ASSETS
cd /w/test/contract && go test -count=1 ./...
