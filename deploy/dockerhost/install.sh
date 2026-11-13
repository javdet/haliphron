#!/bin/sh
# Installs the Haliphron Docker host controller on this machine. Run as root.
# Idempotent: running it again with the same arguments changes nothing, and
# with different ones rewrites the configuration and restarts the controller.
#
#   install.sh --backend-url https://haliphron.example.com:8082 \
#              --bootstrap-token-file /root/hlb.token \
#              --name build-box-1 --dns 10.0.0.53 \
#              --agent-image javdet/haliphron-agent:0.6.2@sha256:...
#
#   install.sh --with-control-plane --dns 10.0.0.53 --name all-in-one \
#              --agent-image ... --public-addr 203.0.113.10
#
# What it does, in order (docker-host.md §9):
#   1. checks: Linux, root, Docker Engine 25+, iptables, /run on tmpfs
#   2. directories: /var/lib/haliphron 0700, /run/haliphron 0711 (and the
#      tmpfiles rule that recreates it at boot), /etc/haliphron 0700
#   3. the agents network: a bridge with a fixed subnet and interface name,
#      inter-container traffic off, IPv6 off
#   4. the firewall: config, script, systemd unit, applied now
#   5. with --with-control-plane: PostgreSQL, the backend and the UI from
#      compose.control-plane.yaml, and a cluster bootstrap token minted from it
#   6. the controller from compose.yaml
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
PREFIX=/opt/haliphron
ETC=/etc/haliphron
STATE=/var/lib/haliphron
RUNDIR=/run/haliphron

BACKEND_URL=""
TOKEN_FILE=""
NAME=$(hostname -s 2>/dev/null || echo docker-host)
LABELS=""
CAPACITY=4
DNS=""
ALLOW=""
DENY=""
SUBNET=172.31.250.0/24
NETWORK=haliphron-agents
BRIDGE=br-haliphron
AGENT_IMAGE=""
CONTROLLER_IMAGE=javdet/haliphron-dockerhost:latest
PUBLIC_CHECK_URL=https://api.anthropic.com/
RUNTIMES=""
WITH_CP=false
PUBLIC_ADDR=0.0.0.0
MCP_PORT=8081
INSECURE=false

usage() { sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --backend-url) BACKEND_URL=$2; shift 2 ;;
    --bootstrap-token-file) TOKEN_FILE=$2; shift 2 ;;
    --name) NAME=$2; shift 2 ;;
    --labels) LABELS=$2; shift 2 ;;
    --capacity) CAPACITY=$2; shift 2 ;;
    --dns) DNS="$DNS $(echo "$2" | tr ',' ' ')"; shift 2 ;;
    --allow) ALLOW="$ALLOW $2"; shift 2 ;;
    --deny) DENY="$DENY $2"; shift 2 ;;
    --subnet) SUBNET=$2; shift 2 ;;
    --agent-image) AGENT_IMAGE=$2; shift 2 ;;
    --controller-image) CONTROLLER_IMAGE=$2; shift 2 ;;
    --public-check-url) PUBLIC_CHECK_URL=$2; shift 2 ;;
    --runtimes) RUNTIMES=$2; shift 2 ;;
    --with-control-plane) WITH_CP=true; shift ;;
    --public-addr) PUBLIC_ADDR=$2; shift 2 ;;
    --mcp-port) MCP_PORT=$2; shift 2 ;;
    --insecure-skip-netcheck) INSECURE=true; shift ;;
    -h|--help) usage 0 ;;
    *) echo "install.sh: unknown argument $1" >&2; usage 2 ;;
  esac
done

# squeeze collapses the space-separated lists the repeatable flags build up.
squeeze() { echo "$1" | tr -s ' ' | sed 's/^ //; s/ $//'; }
die() { echo "install.sh: $*" >&2; exit 1; }
say() { echo "==> $*"; }

# 1. Checks ------------------------------------------------------------------
[ "$(uname -s)" = Linux ] || die "a Linux host is required (Docker Desktop has neither the host's iptables nor a usable host network)"
[ "$(id -u)" = 0 ] || die "run as root"
command -v docker >/dev/null || die "docker is not installed"
command -v iptables >/dev/null || die "iptables is not installed"
docker compose version >/dev/null 2>&1 || die "the docker compose plugin is not installed"
server=$(docker version --format '{{.Server.Version}}' 2>/dev/null) || die "the docker daemon is not reachable"
major=${server%%.*}
[ "$major" -ge 25 ] 2>/dev/null || die "Docker Engine 25 or later is required, this is $server"
if [ "$(findmnt -n -o FSTYPE /run 2>/dev/null || echo unknown)" != tmpfs ]; then
  die "/run is not a tmpfs; per-run secret files must not reach a disk"
fi
[ -n "$AGENT_IMAGE" ] || die "--agent-image is required (the network check probes from it)"
[ -n "$DNS" ] || die "--dns is required: the resolvers agents use, which the firewall allows by address"
case "$SUBNET" in */24) ;; *) die "--subnet must be a /24" ;; esac
base=${SUBNET%/*}
GATEWAY="${base%.*}.$(( ${base##*.} + 1 ))"
if [ "$WITH_CP" = false ]; then
  [ -n "$BACKEND_URL" ] || die "--backend-url is required (or --with-control-plane)"
  [ -n "$TOKEN_FILE" ] || die "--bootstrap-token-file is required (or --with-control-plane)"
fi

# 2. Directories -------------------------------------------------------------
say "directories"
install -d -m 0700 "$STATE" "$ETC"
install -d -m 0711 "$RUNDIR"
install -d -m 0755 "$PREFIX" /usr/local/lib/haliphron
cat > /etc/tmpfiles.d/haliphron.conf <<EOT
# The Docker host controller's per-run secret files live here, on tmpfs.
d $RUNDIR 0711 root root -
EOT

# 3. The agents network ------------------------------------------------------
say "agents network $NETWORK ($SUBNET, $BRIDGE, inter-container traffic off)"
if docker network inspect "$NETWORK" >/dev/null 2>&1; then
  current=$(docker network inspect "$NETWORK" --format '{{(index .IPAM.Config 0).Subnet}} {{index .Options "com.docker.network.bridge.name"}} {{index .Options "com.docker.network.bridge.enable_icc"}}')
  [ "$current" = "$SUBNET $BRIDGE false" ] || die "network $NETWORK exists as '$current'; remove it (docker network rm $NETWORK) and rerun"
else
  docker network create --driver bridge \
    --subnet "$SUBNET" --gateway "$GATEWAY" \
    --opt com.docker.network.bridge.name="$BRIDGE" \
    --opt com.docker.network.bridge.enable_icc=false \
    --label haliphron.io/managed-by=install.sh \
    "$NETWORK" >/dev/null
fi

# 4. The firewall -------------------------------------------------------------
say "firewall"
MCP=""
if [ "$WITH_CP" = true ]; then
  MCP="$GATEWAY:$MCP_PORT"
fi
umask 077
cat > "$ETC/firewall.env" <<EOT
AGENT_BRIDGE=$BRIDGE
AGENT_SUBNET=$SUBNET
CALLBACK_PORT=8083
DNS_SERVERS="$(squeeze "$DNS")"
ALLOW="$(squeeze "$ALLOW")"
DENY="$(squeeze "$DENY")"
CONTROL_PLANE_MCP="$MCP"
EOT
install -m 0755 "$HERE/firewall.sh" /usr/local/lib/haliphron/firewall.sh
install -m 0644 "$HERE/haliphron-agent-firewall.service" /etc/systemd/system/haliphron-agent-firewall.service
systemctl daemon-reload
systemctl enable haliphron-agent-firewall.service >/dev/null
systemctl restart haliphron-agent-firewall.service

# 5. The control plane, when it shares this host ------------------------------
if [ "$WITH_CP" = true ]; then
  say "control plane (PostgreSQL, backend, UI)"
  install -m 0644 "$HERE/compose.control-plane.yaml" "$PREFIX/compose.control-plane.yaml"
  [ -s "$ETC/db-password" ] || head -c 32 /dev/urandom | base64 | tr -d '/+=' > "$ETC/db-password"
  [ -s "$ETC/kek" ] || head -c 32 /dev/urandom | base64 > "$ETC/kek"
  [ -s "$ETC/admin-token" ] || printf 'hlt_%s' "$(head -c 48 /dev/urandom | base64 | tr -d '/+=' | cut -c1-32)" > "$ETC/admin-token"
  # The backend runs as uid 65532 and reads these as files.
  chown 65532:65532 "$ETC/kek" "$ETC/admin-token"
  chmod 0400 "$ETC/kek" "$ETC/admin-token"
  cat > "$ETC/control-plane.env" <<EOT
HALIPHRON_DSN=postgres://haliphron:$(cat "$ETC/db-password")@postgres:5432/haliphron?sslmode=disable
POSTGRES_PASSWORD=$(cat "$ETC/db-password")
HALIPHRON_AGENT_IMAGE=$AGENT_IMAGE
HALIPHRON_MCP_ENDPOINT=http://$GATEWAY:$MCP_PORT/mcp
PUBLIC_ADDR=$PUBLIC_ADDR
AGENT_GATEWAY=$GATEWAY
MCP_PORT=$MCP_PORT
EOT
  docker compose -p haliphron-control-plane -f "$PREFIX/compose.control-plane.yaml" \
    --env-file "$ETC/control-plane.env" up -d --wait
  BACKEND_URL=http://127.0.0.1:8082
  if [ ! -s "$ETC/bootstrap-token" ]; then
    say "minting a cluster bootstrap token"
    curl -fsS -X POST http://127.0.0.1:8080/api/v1/clusters/bootstrap-tokens \
      -H "Authorization: Bearer $(cat "$ETC/admin-token")" -H 'Content-Type: application/json' \
      -d "{\"name\":\"$NAME\",\"max_uses\":1}" \
      | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' > "$ETC/bootstrap-token"
    [ -s "$ETC/bootstrap-token" ] || die "could not mint a bootstrap token"
  fi
  TOKEN_FILE=$ETC/bootstrap-token
  CP_HOST=$GATEWAY
else
  install -m 0400 "$TOKEN_FILE" "$ETC/bootstrap-token"
  CP_HOST=""
fi

# 6. The controller ------------------------------------------------------------
say "controller"
install -m 0644 "$HERE/compose.yaml" "$PREFIX/compose.yaml"
cat > "$ETC/dockerhost.env" <<EOT
HALIPHRON_BACKEND_URL=$BACKEND_URL
HALIPHRON_BOOTSTRAP_TOKEN_FILE=/etc/haliphron/bootstrap-token
HALIPHRON_CLUSTER_NAME=$NAME
HALIPHRON_CLUSTER_LABELS=$LABELS
HALIPHRON_CAPACITY_SLOTS=$CAPACITY
HALIPHRON_RUNTIMES=$RUNTIMES
HALIPHRON_AGENT_NETWORK=$NETWORK
HALIPHRON_AGENT_SUBNET=$SUBNET
HALIPHRON_AGENT_BRIDGE=$BRIDGE
HALIPHRON_AGENT_DNS=$(squeeze "$DNS" | tr ' ' ',')
HALIPHRON_NETCHECK_IMAGE=$AGENT_IMAGE
HALIPHRON_NETCHECK_PUBLIC_URL=$PUBLIC_CHECK_URL
HALIPHRON_CONTROL_PLANE_MCP=$MCP
HALIPHRON_CONTROL_PLANE_HOST=$CP_HOST
HALIPHRON_CONTROL_PLANE_PORTS=8080,8082,8443,9090,5432
HALIPHRON_INSECURE_SKIP_NETCHECK=$INSECURE
EOT
cat > "$ETC/compose.env" <<EOT
DOCKERHOST_IMAGE=$CONTROLLER_IMAGE
EOT
docker compose -p haliphron-dockerhost -f "$PREFIX/compose.yaml" --env-file "$ETC/compose.env" up -d

say "done. Watch it come up:  docker logs -f haliphron-dockerhost"
say "check the network:       docker exec haliphron-dockerhost /haliphron-dockerhost netcheck"
say "list runs:               docker exec haliphron-dockerhost /haliphron-dockerhost runs list"
if [ "$WITH_CP" = true ]; then
  say "UI: http://$PUBLIC_ADDR:8443   admin token (sign in, mint your own, revoke this one): $ETC/admin-token"
fi
