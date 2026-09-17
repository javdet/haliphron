# Contract artifacts. Everything runs in a container: the contract must
# regenerate identically on a laptop and in CI, and neither is required to have
# a Go toolchain installed.
GO_IMAGE      ?= golang:1.26
PG_IMAGE      ?= postgres:18.1-bookworm
ENVTEST_K8S   ?= 1.34.x
PG_CONTAINER   = haliphron-contract-pg
PG_NETWORK     = haliphron-contract-net
DOCKER_RUN     = docker run --rm -e GOMAXPROCS=2 \
                 -v haliphron-gomod:/go/pkg/mod \
                 -v haliphron-gocache:/root/.cache/go-build \
                 -v haliphron-gobin:/go/bin \
                 -v haliphron-envtest:/envtest \
                 -v $(PWD):/w -w /w $(GO_IMAGE)

.PHONY: generate test db-test verify

## generate: deepcopy functions and the AgentRun CRD, from the Go types
generate:
	$(DOCKER_RUN) sh /w/hack/gen.sh

## test: contract tests, including the CRD against a real API server
test:
	$(DOCKER_RUN) sh /w/hack/test.sh

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

## verify: fail if the committed artifacts differ from what the types produce
verify: generate
	git diff --exit-code -- config/crd api || \
		(echo "generated artifacts are stale: run make generate and commit" && exit 1)
