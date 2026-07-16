#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$project_root"

export MINIDOCK_ADDRESS="${MINIDOCK_ADDRESS:-127.0.0.1:8080}"
export MINIDOCK_DATABASE_PATH="${MINIDOCK_DATABASE_PATH:-$project_root/data/minidock-dev.db}"
export MINIDOCK_ENVIRONMENT="${MINIDOCK_ENVIRONMENT:-development}"

mkdir -p data tmp
go run github.com/a-h/templ/cmd/templ@v0.3.833 generate ./internal/app/views
go test ./...
exec go run ./cmd/devwatch
