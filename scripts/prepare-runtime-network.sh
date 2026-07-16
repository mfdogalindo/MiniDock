#!/usr/bin/env bash
set -euo pipefail

# Creates the only network to which MiniDock may attach managed applications.
# Compose intentionally treats it as external so stopping the control plane can
# never remove a live application network.
network="${MINIDOCK_DOCKER_NETWORK:-minidock}"
subnet="${MINIDOCK_DOCKER_NETWORK_SUBNET:-172.31.251.0/24}"

if ! docker network inspect "$network" >/dev/null 2>&1; then
  docker network create --driver bridge --internal --subnet "$subnet" "$network" >/dev/null
fi

driver="$(docker network inspect --format '{{.Driver}}' "$network")"
internal="$(docker network inspect --format '{{.Internal}}' "$network")"
actual_subnet="$(docker network inspect --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}' "$network")"
if [[ "$driver" != "bridge" || "$internal" != "true" || "$actual_subnet" != "$subnet" ]]; then
  echo "runtime network must be an internal bridge with the configured subnet" >&2
  exit 1
fi

printf 'Runtime network %s is an internal bridge on %s.\n' "$network" "$subnet"
