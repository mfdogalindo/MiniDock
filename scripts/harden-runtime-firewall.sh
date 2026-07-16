#!/usr/bin/env bash
set -euo pipefail

# Host firewall companion for the internal workload bridge. It intentionally
# manages only its own chain below DOCKER-USER, leaving Docker-owned chains
# untouched. Supported target: Linux Docker Engine with iptables available.
action="${1:-check}"
network="${MINIDOCK_DOCKER_NETWORK:-minidock}"
subnet="${MINIDOCK_DOCKER_NETWORK_SUBNET:-172.31.251.0/24}"
chain="MINIDOCK-HARDENING"

require_linux() {
  [[ "$(uname -s)" == "Linux" ]] || { echo "Linux Docker Engine is required; Docker Desktop/OrbStack need equivalent VM firewall rules." >&2; exit 2; }
  command -v iptables >/dev/null || { echo "iptables is required" >&2; exit 2; }
}

bridge="$(docker network inspect --format '{{index .Options "com.docker.network.bridge.name"}}' "$network")"
if [[ -z "$bridge" || "$bridge" == "<no value>" ]]; then
  network_id="$(docker network inspect --format '{{.Id}}' "$network")"
  bridge="br-${network_id:0:12}"
fi
caddy_id="$(docker compose ps -q caddy)"
[[ -n "$caddy_id" ]] || { echo "Caddy must be running before applying workload firewall rules." >&2; exit 2; }
caddy_ip="$(docker inspect --format "{{with index .NetworkSettings.Networks \"$network\"}}{{.IPAddress}}{{end}}" "$caddy_id")"
[[ -n "$caddy_ip" ]] || { echo "Caddy has no address on workload network $network." >&2; exit 2; }

rule_exists() { iptables -C "$@" >/dev/null 2>&1; }
install() {
  iptables -N "$chain" 2>/dev/null || true
  rule_exists DOCKER-USER -j "$chain" || iptables -I DOCKER-USER 1 -j "$chain"
  iptables -F "$chain"
  iptables -A "$chain" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
  # Caddy is the sole initiator allowed to reach an application. Responses use
  # the established-flow rule above; lateral workload requests are denied.
  iptables -A "$chain" -i "$bridge" -s "$caddy_ip" -d "$subnet" -p tcp -j RETURN
  iptables -A "$chain" -i "$bridge" -d 127.0.0.0/8 -j DROP
  iptables -A "$chain" -i "$bridge" -d 10.0.0.0/8 -j DROP
  iptables -A "$chain" -i "$bridge" -d 169.254.0.0/16 -j DROP
  iptables -A "$chain" -i "$bridge" -d 172.16.0.0/12 -j DROP
  iptables -A "$chain" -i "$bridge" -d 192.168.0.0/16 -j DROP
  iptables -A "$chain" -i "$bridge" -j DROP
  echo "Installed $chain for $network ($bridge); Caddy $caddy_ip is the only permitted workload initiator."
}
remove() {
  iptables -D DOCKER-USER -j "$chain" 2>/dev/null || true
  iptables -F "$chain" 2>/dev/null || true
  iptables -X "$chain" 2>/dev/null || true
  echo "Removed $chain."
}
check() {
  rule_exists DOCKER-USER -j "$chain" || { echo "Missing $chain jump in DOCKER-USER" >&2; exit 1; }
  rule_exists "$chain" -i "$bridge" -s "$caddy_ip" -d "$subnet" -p tcp -j RETURN || { echo "Missing Caddy allow rule" >&2; exit 1; }
  rule_exists "$chain" -i "$bridge" -j DROP || { echo "Missing default workload drop rule" >&2; exit 1; }
  echo "Firewall hardening is active for $network."
}

require_linux
case "$action" in
  install) install ;;
  remove) remove ;;
  check) check ;;
  *) echo "Usage: $0 {install|check|remove}" >&2; exit 2 ;;
esac
