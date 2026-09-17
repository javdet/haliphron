# The haliphron agent image.
#
# One image, two runtimes. Git, pull requests, uploads, notification, the
# checkpoint and log redaction are the same work either way; what differs is
# authentication, the MCP configuration, the launch command and the output
# parser, and all four live in the entrypoint rather than in a second image.
#
# Debian slim and not Alpine: Claude Code needs Node, and a glibc curl and GNU
# coreutils are required. The saving from musl is not worth discovering that in
# production.

# --- the entrypoint ----------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src
# The api module is a dependency of the entrypoint, so both go into the build
# context. Copied before the sources so that a change to the entrypoint does not
# re-download the module graph.
COPY api/go.mod api/go.sum ./api/
COPY image/go.mod image/go.sum ./image/
RUN cd image && go mod download

COPY api/ ./api/
COPY image/ ./image/

ARG VERSION=dev
RUN cd image && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/haliphron-entrypoint ./cmd/haliphron-entrypoint

# --- the runtime -------------------------------------------------------------
FROM debian:bookworm-slim

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=22
ARG GH_VERSION=2.63.2
ARG GLAB_VERSION=1.51.0
ARG TARGETARCH

# git-lfs and the two forge CLIs are here rather than installed at runtime for
# the reason the contract gives against "upstream image plus an entrypoint from
# a ConfigMap": versions that drift uncontrolled are versions nobody can
# reproduce on the second attempt.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git git-lfs jq openssh-client ripgrep tini \
 && curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz" \
    | tar -xz -C /tmp \
 && mv /tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh /usr/local/bin/gh \
 && curl -fsSL "https://gitlab.com/gitlab-org/cli/-/releases/v${GLAB_VERSION}/downloads/glab_${GLAB_VERSION}_linux_${TARGETARCH}.tar.gz" \
    | tar -xz -C /tmp \
 && mv /tmp/bin/glab /usr/local/bin/glab \
 && apt-get purge -y --auto-remove \
 && rm -rf /var/lib/apt/lists/* /tmp/*

# The agent CLIs. Installed globally so that nothing has to be written under
# $HOME at first run — which is what a read-only root would refuse.
RUN npm install -g --no-fund --no-audit \
      @anthropic-ai/claude-code \
      @openai/codex \
 && npm cache clean --force \
 && rm -rf /root/.npm

# Non-root, and the image requires no write outside the four emptyDirs.
RUN useradd --uid 1000 --create-home --home-dir /home/agent --shell /bin/bash agent \
 && mkdir -p /workspace /haliphron/run /haliphron/role /haliphron/secrets \
 && chown -R 1000:1000 /workspace /haliphron/run /home/agent

COPY --from=build /out/haliphron-entrypoint /usr/local/bin/haliphron-entrypoint

# Every cache the CLIs would put under a read-only root is redirected into
# $HOME, which is an emptyDir. This is the whole of what makes
# readOnlyRootFilesystem achievable rather than aspirational, and it is verified
# by `docker run --read-only` with tmpfs on the four directories — not by
# argument.
ENV HOME=/home/agent \
    XDG_CACHE_HOME=/home/agent/.cache \
    XDG_CONFIG_HOME=/home/agent/.config \
    XDG_DATA_HOME=/home/agent/.local/share \
    npm_config_cache=/home/agent/.npm \
    CLAUDE_CONFIG_DIR=/home/agent/.claude \
    CODEX_HOME=/home/agent/.codex \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_TERMINAL_PROMPT=0

ARG VERSION=dev
ENV HALIPHRON_IMAGE_VERSION=${VERSION}

USER 1000:1000
WORKDIR /workspace

# tini reaps what an agent leaves behind. An MCP server started as a child and
# never waited for becomes a zombie, and a pod full of zombies hits the pid
# limit halfway through a long run.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/haliphron-entrypoint"]
