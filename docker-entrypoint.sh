#!/bin/sh
set -eu

# A named Docker volume is mounted after image build and is initially owned by
# root. Fix the dedicated data directory before dropping privileges so SQLite,
# logs and workspaces remain writable by the service user.
chown -R minidock:minidock /var/lib/minidock

# Docker is reached exclusively through the ACL-protected TCP proxy. Refuse a
# raw socket even if a future Compose override accidentally mounts one.
if [ -S /var/run/docker.sock ]; then
  echo "refusing raw Docker socket; configure DOCKER_HOST to docker-socket-proxy" >&2
  exit 1
fi

case "${DOCKER_HOST:-}" in
  tcp://*) ;;
  *) echo "DOCKER_HOST must reference the Docker socket proxy over TCP" >&2; exit 1 ;;
esac

exec su-exec minidock "$@"
