# Contract artifacts. Everything runs in a container: the contract must
# regenerate identically on a laptop and in CI, and neither is required to have
# a Go toolchain installed.
GO_IMAGE      ?= golang:1.26
PG_IMAGE      ?= postgres:18.1-bookworm
ENVTEST_K8S   ?= 1.34.x
AGENT_IMAGE   ?= haliphron/agent:dev
AGENT_VERSION ?= dev
CONTROLLER_VERSION ?= 0.1.0
BACKEND_VERSION    ?= 0.1.0
BACKEND_IMAGE      ?= haliphron/backend:dev
CONTROLLER_IMAGE   ?= haliphron/controller:dev
HELM               ?= helm
CHART_CP           = deploy/charts/haliphron
CHART_RT           = deploy/charts/haliphron-runtime
PG_CONTAINER   = haliphron-contract-pg
PG_NETWORK     = haliphron-contract-net
DOCKER_RUN     = docker run --rm -e GOMAXPROCS=2 \
                 -v haliphron-gomod:/go/pkg/mod \
                 -v haliphron-gocache:/root/.cache/go-build \
                 -v haliphron-gobin:/go/bin \
                 -v haliphron-envtest:/envtest \
                 -v $(PWD):/w -w /w $(GO_IMAGE)

.PHONY: generate test db-test fake-test image-test image-build controller-test controller-build backend-test backend-build verify
.PHONY: backend-image controller-image images chart-gen chart-deps chart-lint chart-template chart-package chart-verify

## generate: deepcopy functions and the AgentRun CRD, from the Go types
generate:
	$(DOCKER_RUN) sh /w/hack/gen.sh

## test: contract tests, including the CRD against a real API server
test:
	$(DOCKER_RUN) sh /w/hack/test.sh

## fake-test: the fakes' own contract tests, under the race detector
fake-test:
	$(DOCKER_RUN) sh -c 'cd /w/fake && go test -count=1 -race ./...'

## image-test: the agent image entrypoint, against FakeControlPlane
image-test:
	$(DOCKER_RUN) sh -c 'cd /w/image && go test -count=1 -race ./... && cd /w/test/image && go test -count=1 -race ./...'

## image-build: the agent image itself
image-build:
	docker build -f image/Dockerfile -t $(AGENT_IMAGE) --build-arg VERSION=$(AGENT_VERSION) .

## controller-test: the controller's own tests, and the contract tests against FakeBackend
controller-test:
	$(DOCKER_RUN) sh /w/hack/controllertest.sh

## controller-build: the controller binary
controller-build:
	$(DOCKER_RUN) sh -c 'cd /w/controller && CGO_ENABLED=0 go build -ldflags "-X github.com/automagicops/haliphron/controller/version.Version=$(CONTROLLER_VERSION)" -o /w/bin/haliphron-controller ./cmd/haliphron-controller'

## backend-test: the backend's tests, and the contract tests against FakeController
##               and a real PostgreSQL
backend-test:
	@docker network create $(PG_NETWORK) 2>/dev/null || true
	@docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	docker run -d --name $(PG_CONTAINER) --network $(PG_NETWORK) \
	  -e POSTGRES_PASSWORD=haliphron -e POSTGRES_DB=postgres $(PG_IMAGE) >/dev/null
	@trap 'docker rm -f $(PG_CONTAINER) >/dev/null 2>&1; docker network rm $(PG_NETWORK) >/dev/null 2>&1' EXIT; \
	docker run --rm --network $(PG_NETWORK) -e GOMAXPROCS=2 \
	  -e HALIPHRON_TEST_DSN=postgres://postgres:haliphron@$(PG_CONTAINER):5432/postgres?sslmode=disable \
	  -v haliphron-gomod:/go/pkg/mod \
	  -v haliphron-gocache:/root/.cache/go-build \
	  -v $(PWD):/w -w /w $(GO_IMAGE) sh /w/hack/backendtest.sh

## backend-build: the control plane binary
backend-build:
	$(DOCKER_RUN) sh -c 'cd /w/backend && CGO_ENABLED=0 go build -ldflags "-X github.com/automagicops/haliphron/backend/version.Version=$(BACKEND_VERSION)" -o /w/bin/haliphron-backend ./cmd/haliphron-backend'

## db-test: store contract tests against a real PostgreSQL
db-test:
	@docker network create $(PG_NETWORK) 2>/dev/null || true
	@docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	docker run -d --name $(PG_CONTAINER) --network $(PG_NETWORK) \
	  -e POSTGRES_PASSWORD=haliphron -e POSTGRES_DB=postgres $(PG_IMAGE) >/dev/null
	@trap 'docker rm -f $(PG_CONTAINER) >/dev/null 2>&1; docker network rm $(PG_NETWORK) >/dev/null 2>&1' EXIT; \
	docker run --rm --network $(PG_NETWORK) -e GOMAXPROCS=2 \
	  -e HALIPHRON_TEST_DSN=postgres://postgres:haliphron@$(PG_CONTAINER):5432/postgres?sslmode=disable \
	  -v haliphron-gomod:/go/pkg/mod \
	  -v haliphron-gocache:/root/.cache/go-build \
	  -v $(PWD):/w -w /w $(GO_IMAGE) sh /w/hack/dbtest.sh

## backend-image: the control plane image
backend-image:
	docker build -f backend/Dockerfile -t $(BACKEND_IMAGE) --build-arg VERSION=$(BACKEND_VERSION) .

## controller-image: the cluster controller image
controller-image:
	docker build -f controller/Dockerfile -t $(CONTROLLER_IMAGE) --build-arg VERSION=$(CONTROLLER_VERSION) .

## images: every image
images: backend-image controller-image image-build

## chart-gen: copy the generated CRD and RBAC into the runtime chart
chart-gen:
	sh hack/chartgen.sh

## chart-deps: vendor the control plane chart's subcharts. Helm resolves a
##             declared dependency whether or not its condition is met, so this
##             has to run before lint, template, package or install.
chart-deps:
	$(HELM) dependency build $(CHART_CP)

## chart-lint: both charts, against the values in each chart's ci/
chart-lint: chart-gen chart-deps
	$(HELM) lint $(CHART_CP) -f $(CHART_CP)/ci/lint-values.yaml
	$(HELM) lint $(CHART_RT) -f $(CHART_RT)/ci/lint-values.yaml

## chart-template: render both charts, which catches what lint does not
chart-template: chart-gen chart-deps
	$(HELM) template ci $(CHART_CP) -f $(CHART_CP)/ci/lint-values.yaml >/dev/null
	$(HELM) template ci $(CHART_RT) -f $(CHART_RT)/ci/lint-values.yaml >/dev/null

## chart-package: the two .tgz, into dist/
chart-package: chart-lint
	mkdir -p dist
	$(HELM) package $(CHART_CP) -d dist
	$(HELM) package $(CHART_RT) -d dist

## chart-verify: fail if the chart's copies of the CRD and the RBAC are stale
chart-verify: chart-gen
	git diff --exit-code -- $(CHART_RT)/templates/crd.yaml $(CHART_RT)/templates/rbac-controller.yaml || \
		(echo "the chart's generated templates are stale: run make chart-gen and commit" && exit 1)

## verify: fail if the committed artifacts differ from what the types produce
verify: generate chart-gen
	git diff --exit-code -- config/crd config/rbac api $(CHART_RT)/templates/crd.yaml $(CHART_RT)/templates/rbac-controller.yaml || \
		(echo "generated artifacts are stale: run make generate chart-gen and commit" && exit 1)
