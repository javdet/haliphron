#!/bin/sh
# The agents network's firewall: the Docker host's counterpart of the agents
# NetworkPolicy (docker-host.md §6). Run as root, by the systemd unit
# haliphron-agent-firewall.service, after Docker and again whenever Docker
# restarts. Idempotent: every run builds the chains from scratch beside the
# live ones and swaps them in, so there is no moment with no rules.
#
#   firewall.sh [apply|remove|show] [config]    (config: /etc/haliphron/firewall.env)
#
# The rules DROP and never REJECT. The controller's network check tells a
# working firewall from a missing one by exactly that: a closed port behind a
# DROP times out, and one with no rule in front answers with a reset.
#
# What it enforces, for traffic from the agents bridge:
#   to this host (INPUT):     the controller's callback port, the resolvers, the
#                             control plane's MCP port if it lives here; nothing
#                             else — not the Docker API, not PostgreSQL, not the
#                             controller's health and metrics ports
#   elsewhere (DOCKER-USER):  ALLOW entries, then the resolvers, then the MCP
#                             endpoint, then DROP the private ranges, link-local
#                             and DENY entries, then TCP 80/443 anywhere, then
#                             DROP
#   into agents:              nothing new, from anywhere
# Unlike the chart's NetworkPolicy, each ALLOW entry is a rule of its own, not a
# CIDR with the same "except" attached, so 10.20.0.5/32 can be allowed without
# opening 10.0.0.0/8.
set -eu

ACTION=${1:-apply}
CONFIG=${2:-/etc/haliphron/firewall.env}

AGENT_BRIDGE=br-haliphron
AGENT_SUBNET=172.31.250.0/24
CALLBACK_PORT=8083
DNS_SERVERS=""
# Space-separated. ALLOW entries are CIDR or CIDR:port[,port...] (TCP).
ALLOW=""
DENY=""
# host:port of the control plane's MCP endpoint as agents reach it, when the
# control plane shares this host. Empty otherwise.
CONTROL_PLANE_MCP=""
EGRESS_PORTS="80,443"
PRIVATE_RANGES="10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 100.64.0.0/10 127.0.0.0/8 224.0.0.0/4 0.0.0.0/8"
IPTABLES=${IPTABLES:-iptables}

if [ -f "$CONFIG" ]; then
  # shellcheck disable=SC1090
  . "$CONFIG"
fi

IN=HALIPHRON-AGENTS-IN
OUT=HALIPHRON-AGENTS-OUT
TO=HALIPHRON-AGENTS-TO

ipt() { "$IPTABLES" -w "$@"; }

gateway() {
  # The first host address of the subnet, which is what install.sh gives the
  # bridge and what the callback listens on.
  base=${AGENT_SUBNET%/*}
  last=${base##*.}
  echo "${base%.*}.$((last + 1))"
}

chain_reset() { ipt -N "$1" 2>/dev/null || ipt -F "$1"; }

unhook() {
  # Delete every copy of a jump; a crashed earlier run may have left two.
  while ipt -D "$1" "$2" "$3" -j "$4" 2>/dev/null; do :; done
}

drop_chain() {
  if ipt -nL "$1" >/dev/null 2>&1; then
    ipt -F "$1"
    ipt -X "$1"
  fi
}

build() {
  suffix=$1
  gw=$(gateway)
  mcp_host=""
  mcp_port=""
  if [ -n "$CONTROL_PLANE_MCP" ]; then
    mcp_host=${CONTROL_PLANE_MCP%:*}
    mcp_port=${CONTROL_PLANE_MCP##*:}
  fi

  # Agent -> this host.
  chain_reset "$IN$suffix"
  ipt -A "$IN$suffix" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  ipt -A "$IN$suffix" -p tcp -d "$gw" --dport "$CALLBACK_PORT" -j ACCEPT
  for r in $DNS_SERVERS; do
    ipt -A "$IN$suffix" -p udp -d "$r" --dport 53 -j ACCEPT
    ipt -A "$IN$suffix" -p tcp -d "$r" --dport 53 -j ACCEPT
  done
  if [ -n "$mcp_host" ]; then
    # An MCP server bound in the host's own namespace is reached through INPUT.
    ipt -A "$IN$suffix" -p tcp -d "$mcp_host" --dport "$mcp_port" -j ACCEPT
  fi
  ipt -A "$IN$suffix" -j DROP

  # Agent -> anywhere else.
  chain_reset "$OUT$suffix"
  ipt -A "$OUT$suffix" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  for entry in $ALLOW; do
    cidr=${entry%%:*}
    if [ "$cidr" != "$entry" ]; then
      ipt -A "$OUT$suffix" -p tcp -d "$cidr" -m multiport --dports "${entry#*:}" -j ACCEPT
    else
      ipt -A "$OUT$suffix" -d "$cidr" -j ACCEPT
    fi
  done
  for r in $DNS_SERVERS; do
    ipt -A "$OUT$suffix" -p udp -d "$r" --dport 53 -j ACCEPT
    ipt -A "$OUT$suffix" -p tcp -d "$r" --dport 53 -j ACCEPT
  done
  if [ -n "$mcp_host" ]; then
    # A published port is DNATed before FORWARD, so the destination here is
    # already the backend container's address. The original destination is
    # what identifies the MCP endpoint, and it is the only control-plane
    # port an agent may reach.
    ipt -A "$OUT$suffix" -p tcp -m conntrack --ctorigdst "$mcp_host" --ctorigdstport "$mcp_port" -j ACCEPT
  fi
  for cidr in $DENY $PRIVATE_RANGES; do
    ipt -A "$OUT$suffix" -d "$cidr" -j DROP
  done
  ipt -A "$OUT$suffix" -p tcp -m multiport --dports "$EGRESS_PORTS" -j ACCEPT
  ipt -A "$OUT$suffix" -j DROP

  # Anything -> agents: replies only.
  chain_reset "$TO$suffix"
  ipt -A "$TO$suffix" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  ipt -A "$TO$suffix" -j DROP
}

hook() {
  suffix=$1
  ipt -I INPUT 1 -i "$AGENT_BRIDGE" -j "$IN$suffix"
  ipt -N DOCKER-USER 2>/dev/null || true
  ipt -I DOCKER-USER 1 -i "$AGENT_BRIDGE" -j "$OUT$suffix"
  ipt -I DOCKER-USER 1 -o "$AGENT_BRIDGE" -j "$TO$suffix"
}

remove() {
  suffix=${1:-}
  unhook INPUT -i "$AGENT_BRIDGE" "$IN$suffix"
  unhook DOCKER-USER -i "$AGENT_BRIDGE" "$OUT$suffix"
  unhook DOCKER-USER -o "$AGENT_BRIDGE" "$TO$suffix"
  drop_chain "$IN$suffix"
  drop_chain "$OUT$suffix"
  drop_chain "$TO$suffix"
}

apply() {
  if [ -z "$DNS_SERVERS" ]; then
    echo "firewall.sh: DNS_SERVERS is empty; agents would resolve nothing" >&2
    exit 1
  fi
  # Build beside the live chains, hook the new ones in front, unhook and drop
  # the old ones, then take over the names. At no moment is the bridge without
  # rules.
  remove -NEW
  build -NEW
  hook -NEW
  remove ""
  ipt -E "$IN-NEW" "$IN"
  ipt -E "$OUT-NEW" "$OUT"
  ipt -E "$TO-NEW" "$TO"
  echo "firewall.sh: rules applied for $AGENT_BRIDGE ($AGENT_SUBNET)"
}

case "$ACTION" in
  apply) apply ;;
  remove) remove ""; remove -NEW; echo "firewall.sh: rules removed" ;;
  show)
    for c in "$IN" "$OUT" "$TO"; do ipt -S "$c" 2>/dev/null || echo "# $c: absent"; done
    ipt -S INPUT | grep -- "$AGENT_BRIDGE" || true
    ipt -S DOCKER-USER | grep -- "$AGENT_BRIDGE" || true
    ;;
  *) echo "usage: firewall.sh [apply|remove|show] [config]" >&2; exit 2 ;;
esac
