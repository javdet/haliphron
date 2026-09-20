# The haliphron control plane image.
#
# The backend is one static binary with no runtime dependencies: the schema is
# embedded (db/migrations), the contract queries are embedded (db/queries), and
# the only things it opens are a Postgres connection, an S3 endpoint and four
# listeners. So the runtime stage is distroless/static — no shell, no package
# manager, no libc — rather than the Debian slim the agent image needs. The
# agent image runs model-generated commands and requires a real userland; this
# one runs a single process and requires nothing.
#
# The build context is the repository root, not backend/: the module replaces
# ../api and ../db, and a context that cannot see them cannot resolve them.
#
#   docker build -f backend/Dockerfile -t haliphron/backend:dev .

# --- the binary --------------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# The manifests before the sources, so that editing a handler does not
# re-download the module graph. The two replaced modules' manifests come along
# because `go mod download` has to resolve the replace targets to read them.
COPY api/go.mod api/go.sum ./api/
COPY db/go.mod db/go.sum ./db/
COPY backend/go.mod backend/go.sum ./backend/

# GOWORK=off throughout: go.work lists the test modules, which are not in this
# context and are not wanted in a release build. Module mode also makes
# backend/go.sum authoritative, which is the property a reproducible build
# needs — the workspace's go.work.sum is a developer convenience.
ENV GOWORK=off CGO_ENABLED=0

# Cache mounts rather than layers: the module cache and the build cache are
# large, change on every dependency bump, and are worth nothing inside the
# published image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd backend && go mod download

COPY api/ ./api/
COPY db/ ./db/
COPY backend/ ./backend/

ARG VERSION=dev
# TARGETOS/TARGETARCH are set by buildx and left empty by a plain `docker
# build`, where the host's values are the right ones anyway.
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd backend && GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/automagicops/haliphron/backend/version.Version=${VERSION}" \
      -o /out/haliphron-backend ./cmd/haliphron-backend

# --- the runtime -------------------------------------------------------------
# static-debian12 carries ca-certificates, tzdata, /etc/passwd and a writable
# /tmp, and nothing else. The certificates are not optional: without them every
# call to S3 and to an OTLP collector fails at the TLS handshake, which is a
# confusing way to learn that an image is too small.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
LABEL org.opencontainers.image.title="haliphron-backend" \
      org.opencontainers.image.description="Haliphron control plane: REST, MCP, Cluster API and the expiry scanners" \
      org.opencontainers.image.source="https://github.com/automagicops/haliphron" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/haliphron-backend /usr/local/bin/haliphron-backend

# Documentation, not a control: 8080 REST and the BFF, 8081 MCP, 8082 the
# Cluster API, 9090 health and metrics. Which of them actually listen is
# HALIPHRON_MODE's business, and the health listener runs in every mode.
EXPOSE 8080 8081 8082 9090

# 65532:65532 is distroless's `nonroot`. Declared numerically so that a
# PodSecurityContext with runAsNonRoot can verify it without resolving a name.
USER 65532:65532

# No HEALTHCHECK: there is no shell and no curl to write one with, and the two
# probes this image answers — /healthz and /readyz on the health listener — are
# driven by the chart's livenessProbe and readinessProbe, which Kubernetes
# honours and a Dockerfile HEALTHCHECK it ignores.
#
# No tini either. The process is PID 1, it installs its own SIGINT/SIGTERM
# handler, and it forks nothing, so there is nothing to reap.
ENTRYPOINT ["/usr/local/bin/haliphron-backend"]
