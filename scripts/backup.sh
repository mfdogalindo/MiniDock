#!/usr/bin/env bash
set -euo pipefail

source_path="${MINIDOCK_DATA_PATH:-$HOME/minidock}"
destination="${MINIDOCK_BACKUP_PATH:?Set MINIDOCK_BACKUP_PATH to a mounted backup volume}"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$destination"
archive="$destination/minidock-$timestamp.tgz"
tar -C "$source_path" -czf "$archive" .
sha256sum "$archive" > "$archive.sha256"
echo "Backup created: $archive"
