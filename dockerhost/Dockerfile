# The Docker host controller image.
#
# Its build context is dockerhost/, controller/ and api/ and nothing else (see
# Dockerfile.dockerignore beside this file): the component that holds a host's
# git tokens and its Docker socket has no business carrying the fakes, the test
# harness or the agent image in its graph.
#
#   docker build -f dockerhost/Dockerfile -t haliphron/dockerhost:dev .

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

COPY api/go.mod api/go.sum ./api/
COPY controller/go.mod controller/go.sum ./controller/
COPY dockerhost/go.mod dockerhost/go.sum ./dockerhost/

# CGO off: the SQLite driver is pure Go, so the binary is static and the
# runtime image needs no libc.
ENV CGO_ENABLED=0 GOWORK=off

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd dockerhost && go mod download

COPY api/ ./api/
COPY controller/ ./controller/
COPY dockerhost/ ./dockerhost/

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd dockerhost && GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/automagicops/haliphron/dockerhost/version.Version=${VERSION}" \
      -o /out/haliphron-dockerhost ./cmd/haliphron-dockerhost

# Distroless, no shell. It runs as root inside its container, deliberately and
# unlike the Kubernetes controller: it holds the Docker socket, which is
# root-equivalent on the host anyway, and it must chown each run's secret files
# to the agent's uid 1000. compose.yaml drops every capability but CHOWN and
# FOWNER and makes the root filesystem read-only.
FROM gcr.io/distroless/static-debian12

ARG VERSION=dev
LABEL org.opencontainers.image.title="haliphron-dockerhost" \
      org.opencontainers.image.description="Haliphron Docker host controller: leases work, runs agents as hardened containers on one host, reports back" \
      org.opencontainers.image.source="https://github.com/automagicops/haliphron" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/haliphron-dockerhost /haliphron-dockerhost

ENTRYPOINT ["/haliphron-dockerhost"]
CMD ["serve"]
