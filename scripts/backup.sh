#!/usr/bin/env bash
set -euo pipefail

source_path="${MINIDOCK_DATA_PATH:-$HOME/minidock}"
destination="${MINIDOCK_BACKUP_PATH:?Set MINIDOCK_BACKUP_PATH to a mounted backup volume}"
database_path="${MINIDOCK_DATABASE_PATH:-$source_path/minidock.db}"
minidock_bin="${MINIDOCK_BINARY:-minidock}"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$destination"
test -d "$source_path"
test -f "$database_path"

stage="$(mktemp -d "${TMPDIR:-/tmp}/minidock-backup.XXXXXX")"
archive="$stage/minidock-files.tar"
output="$destination/minidock-$timestamp.mdbk"
cleanup() { rm -rf "$stage"; unset MINIDOCK_KMS_PASSWORD; }
trap cleanup EXIT HUP INT TERM

read -r -s -p "KMS password: " MINIDOCK_KMS_PASSWORD
printf '\n'
printf '%s' "$MINIDOCK_KMS_PASSWORD" | "$minidock_bin" backup --database "$database_path" --file "$stage/database.mdbk" --password-stdin
"$minidock_bin" kms-export --database "$database_path" --file "$output.kms.json"
# The raw SQLite file is intentionally excluded: the archive contains only its
# authenticated .mdbk representation plus persistent application files.
tar -C "$source_path" --exclude="$(basename "$database_path")" -cf "$archive" .
tar -C "$stage" -rf "$archive" database.mdbk
gzip -f "$archive"
printf '%s' "$MINIDOCK_KMS_PASSWORD" | "$minidock_bin" seal --database "$database_path" --input "$archive.gz" --file "$output" --password-stdin
shasum -a 256 "$output" > "$output.sha256"
echo "Encrypted full backup created: $output"
