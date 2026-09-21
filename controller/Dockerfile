# The controller image.
#
# Its build context is controller/ and api/ and nothing else: the component that
# holds a customer's git tokens has no business carrying the fakes, the test
# harness or the agent image in its graph, and a narrow context is how that stops
# being a matter of discipline. See Dockerfile.dockerignore beside this file,
# which BuildKit prefers over the repository's own.
#
#   docker build -f controller/Dockerfile -t haliphron/controller:dev .

# --platform=$BUILDPLATFORM: the binary is cross-compiled below through
# GOOS/GOARCH with CGO off, so building a linux/arm64 image on an amd64
# machine costs one more `go build` rather than a Go toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

# The manifests before the sources, so that editing a reconciler does not
# re-download the module graph. api's come along because `go mod download` has
# to resolve the replace target to read it.
COPY api/go.mod api/go.sum ./api/
COPY controller/go.mod controller/go.sum ./controller/

ENV CGO_ENABLED=0 GOWORK=off

# Cache mounts rather than layers: the module and build caches are large,
# change on every dependency bump, and are worth nothing inside the image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd controller && go mod download

COPY api/ ./api/
COPY controller/ ./controller/

ARG VERSION=dev
# TARGETOS/TARGETARCH are set by buildx and left empty by a plain `docker
# build`, where the host's values are the right ones anyway. They are what
# makes the cross-compile above real: without them the build stage, pinned
# to the build platform, would emit the builder's own architecture.
ARG TARGETOS
ARG TARGETARCH
# The version is not decoration: the control plane refuses to register a
# controller outside the range it declares, and this string is what it reads.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd controller && GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/automagicops/haliphron/controller/version.Version=${VERSION}" \
      -o /out/haliphron-controller ./cmd/haliphron-controller

# Distroless, non-root, no shell: the controller talks to two APIs and reads one
# Secret, and every other capability is one an attacker inherits for free.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
LABEL org.opencontainers.image.title="haliphron-controller" \
      org.opencontainers.image.description="Haliphron cluster controller: leases work, reconciles AgentRun into Jobs, reports back" \
      org.opencontainers.image.source="https://github.com/automagicops/haliphron" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/haliphron-controller /haliphron-controller

# Documentation, not a control: 8081 health, 8083 the pods' completion
# callback, 9090 metrics.
EXPOSE 8081 8083 9090

USER 65532:65532
ENTRYPOINT ["/haliphron-controller"]
