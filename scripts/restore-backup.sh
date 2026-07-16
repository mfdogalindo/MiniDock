#!/usr/bin/env bash
set -euo pipefail

archive="${1:?Usage: restore-backup.sh BACKUP.mdbk DESTINATION DATABASE_FOR_KMS}"
destination="${2:?Usage: restore-backup.sh BACKUP.mdbk DESTINATION DATABASE_FOR_KMS}"
kms_config="${3:-$archive.kms.json}"
minidock_bin="${MINIDOCK_BINARY:-minidock}"
test ! -e "$destination" || { echo "Destination must not exist: $destination" >&2; exit 1; }
stage="$(mktemp -d "${TMPDIR:-/tmp}/minidock-restore.XXXXXX")"
cleanup() { rm -rf "$stage"; unset MINIDOCK_KMS_PASSWORD; }
trap cleanup EXIT HUP INT TERM
read -r -s -p "KMS password: " MINIDOCK_KMS_PASSWORD
printf '\n'
test -f "$kms_config" || { echo "KMS configuration missing: $kms_config" >&2; exit 1; }
printf '%s' "$MINIDOCK_KMS_PASSWORD" | "$minidock_bin" open --kms-config "$kms_config" --file "$archive" --output "$stage/files.tgz" --password-stdin
tar -tzf "$stage/files.tgz" >/dev/null
mkdir -m 0700 "$destination"
tar -C "$destination" -xzf "$stage/files.tgz"
printf '%s' "$MINIDOCK_KMS_PASSWORD" | "$minidock_bin" restore --kms-config "$kms_config" --file "$destination/database.mdbk" --destination "$destination/minidock.db" --password-stdin
rm -f "$destination/database.mdbk"
echo "Restore completed in: $destination"
