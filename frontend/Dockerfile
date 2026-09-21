# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
FROM node:22-alpine AS build
WORKDIR /src

# The lockfile alone first: dependencies change far less often than sources,
# and this is the layer worth keeping.
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci

COPY tsconfig.json vite.config.ts index.html ./
COPY src ./src
RUN npm run build

# ---- run ------------------------------------------------------------------
# Unprivileged: the image listens on 8080 as uid 101 and needs no root, which
# is what lets the pod run with runAsNonRoot and a read-only root filesystem.
FROM nginxinc/nginx-unprivileged:1.27-alpine

# Where /api goes, and what the page is allowed to connect to. Both are
# substituted into the server block at start by the base image's entrypoint.
ENV BACKEND_ORIGIN=http://haliphron:8080 \
    CSP_CONNECT_SRC="'self'"

COPY nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /src/dist /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD ["/bin/sh", "-c", "wget -qO- http://127.0.0.1:8080/healthz || exit 1"]
