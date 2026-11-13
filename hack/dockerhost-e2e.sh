#!/bin/sh
# make dockerhost-e2e: the parts of the Docker host runtime that only a real
# daemon and a real kernel can show. Linux only, opt-in, needs root.
#
#   1. a scratch agents network, made exactly as install.sh makes it, on a
#      subnet and bridge name of its own so it never touches an installation
#   2. the agent image run as an agent: read-only root, uid 1000, the four
#      named volumes writable, a 0400 secret file readable through the
#      read-only bind mount
#   3. the network check fails with no firewall — every "blocked" check reaches
#      something — and passes once firewall.sh has applied the rules
#   4. everything removed again
#
# The firewall half runs firewall.sh in a privileged container on the host's
# network namespace; the probe half runs the controller's own netcheck from
# the source tree, in the Go toolchain image, against this host's socket.
set -eu

[ "$(uname -s)" = Linux ] || { echo "dockerhost-e2e: Linux only (Docker Desktop has no host iptables)"; exit 1; }
: "${AGENT_IMAGE:?set AGENT_IMAGE to an agent image with curl}"
GO_IMAGE=${GO_IMAGE:-golang:1.26}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

NET=haliphron-e2e
BR=br-hlphe2e
SUBNET=172.31.251.0/24
GW=172.31.251.1
DNS=${E2E_DNS:-1.1.1.1}
RUNDIR=$(mktemp -d /tmp/haliphron-e2e.XXXXXX)
CFG=$RUNDIR/firewall.env
cat > "$CFG" <<EOT
AGENT_BRIDGE=$BR
AGENT_SUBNET=$SUBNET
CALLBACK_PORT=18083
DNS_SERVERS="$DNS"
EOT

firewall() {
  docker run --rm --privileged --network host -v "$ROOT/deploy/dockerhost:/fw:ro" -v "$CFG:/fw.env:ro" \
    alpine:3.20 sh -c "apk add --no-cache iptables >/dev/null && sh /fw/firewall.sh $1 /fw.env"
}

cleanup() {
  firewall remove >/dev/null 2>&1 || true
  docker ps -aq --filter label=haliphron.io/managed-by=haliphron-dockerhost --filter label=haliphron.io/e2e=1 | xargs -r docker rm -f >/dev/null
  docker rm -f hlph-e2e-agent hlph-e2e-callback >/dev/null 2>&1 || true
  docker volume rm hlph-e2e-ws >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$RUNDIR" /tmp/hlph-e2e-run
}
trap cleanup EXIT
cleanup_net_only() { docker network rm "$NET" >/dev/null 2>&1 || true; }
cleanup_net_only

echo "==> network"
docker network create --driver bridge --subnet "$SUBNET" --gateway "$GW" \
  --opt com.docker.network.bridge.name="$BR" --opt com.docker.network.bridge.enable_icc=false "$NET" >/dev/null
docker image inspect "$AGENT_IMAGE" >/dev/null 2>&1 || docker pull "$AGENT_IMAGE" >/dev/null

echo "==> the agent image runs hardened"
mkdir -p "$RUNDIR/secrets"
printf 'token' > "$RUNDIR/secrets/git-token"
chown -R 1000:1000 "$RUNDIR/secrets"
chmod 0400 "$RUNDIR/secrets/git-token"
chmod 0500 "$RUNDIR/secrets"
docker run --rm --name hlph-e2e-agent --user 1000:1000 --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true --network "$NET" --dns "$DNS" \
  --mount type=volume,source=hlph-e2e-ws,target=/workspace \
  --mount type=bind,source="$RUNDIR/secrets",target=/haliphron/secrets,readonly \
  --entrypoint /bin/sh "$AGENT_IMAGE" -c '
    set -e
    [ "$(id -u)" = 1000 ]
    touch /workspace/x && rm /workspace/x
    cat /haliphron/secrets/git-token >/dev/null
    if touch /etc/x 2>/dev/null; then echo "the root filesystem is writable"; exit 1; fi
    echo agent-ok'

echo "==> a stand-in callback on the gateway"
# busybox httpd answers 404 for /healthz, which is a completed HTTP exchange:
# what the check asks of the callback port is that it is reachable.
docker run -d --name hlph-e2e-callback --network host alpine:3.20 httpd -f -p "$GW:18083" >/dev/null

probe() {
  docker run --rm -e GOMAXPROCS=2 --network host \
    -v haliphron-gomod:/go/pkg/mod -v haliphron-gocache:/root/.cache/go-build \
    -v /var/run/docker.sock:/var/run/docker.sock -v "$ROOT":/w -w /w/dockerhost \
    -v /tmp/hlph-e2e-run:/tmp/hlph-e2e-run \
    -e HALIPHRON_BACKEND_URL=http://unused -e HALIPHRON_CLUSTER_NAME=e2e \
    -e HALIPHRON_AGENT_NETWORK="$NET" -e HALIPHRON_AGENT_SUBNET="$SUBNET" -e HALIPHRON_AGENT_BRIDGE="$BR" \
    -e HALIPHRON_AGENT_DNS="$DNS" -e HALIPHRON_CALLBACK_ADDR="$GW:18083" \
    -e HALIPHRON_NETCHECK_IMAGE="$AGENT_IMAGE" -e HALIPHRON_NETCHECK_PUBLIC_URL=https://example.com/ \
    -e HALIPHRON_RUN_DIR=/tmp/hlph-e2e-run \
    "$GO_IMAGE" go run ./cmd/haliphron-dockerhost netcheck
}

echo "==> without the firewall the check must fail"
if probe; then
  echo "dockerhost-e2e: the network check passed with no firewall installed"; exit 1
fi

echo "==> with the firewall the check must pass"
firewall apply
probe
echo "dockerhost-e2e: OK"
