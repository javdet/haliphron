#!/bin/sh
# Code generation for the api module. Runs in a container because the contract
# must regenerate identically on a laptop and in CI, and neither is required to
# have a Go toolchain installed.
set -e
CONTROLLER_GEN_VERSION=v0.19.0
if [ ! -x /go/bin/controller-gen ]; then
  go install sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION}
fi
cd /w/api
/go/bin/controller-gen object paths=./...
/go/bin/controller-gen crd:crdVersions=v1 paths=./agentrun/... output:crd:artifacts:config=/w/config/crd/bases
# The controller's RBAC, from the markers on the reconciler. Section 12 of the
# CRD contract is a list of permissions with a reason attached to each; keeping
# it generated is what stops the chart and the reasons drifting apart.
cd /w/controller
/go/bin/controller-gen rbac:roleName=haliphron-controller paths=./... \
  output:rbac:artifacts:config=/w/config/rbac

cd /w/api && gofmt -l . && go build ./... && go vet ./...
cd /w/controller && gofmt -l . && go build ./... && go vet ./...
echo GEN_OK
