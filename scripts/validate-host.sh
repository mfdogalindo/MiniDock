#!/usr/bin/env bash
set -euo pipefail

failures=0
check() {
  if "$@" >/dev/null 2>&1; then
    printf 'ok   %s\n' "$*"
  else
    printf 'FAIL %s\n' "$*" >&2
    failures=$((failures + 1))
  fi
}

check git --version
check docker version
check docker compose version
check docker info
check docker run --rm hello-world
check scripts/prepare-runtime-network.sh

if [[ "$(uname -s)" == "Linux" ]]; then
  check scripts/harden-runtime-firewall.sh check
else
  echo "WARN Linux iptables runtime hardening is not verifiable on $(uname -s); configure the Docker VM equivalent." >&2
fi

for directory in "${MINIDOCK_DATA_PATH:-$HOME/minidock}"/{apps,logs,backups}; do
  mkdir -p "$directory"
  chmod 700 "$directory"
  test "$(stat -f '%Lp' "$directory")" = 700 || { echo "FAIL permissions $directory" >&2; failures=$((failures + 1)); }
done

if [[ $failures -gt 0 ]]; then
  echo "Host validation failed: $failures check(s)." >&2
  exit 1
fi
echo "Host validation passed. Complete the SSH, DNS and backup checks in docs/OPERACION.md."
