#!/usr/bin/env bash
set -euo pipefail

# Acceptance gate for the production-shaped path:
# Compose -> MiniDock -> Docker -> Caddy -> persisted release evidence.
#
# The script intentionally uses only a versioned local fixture and temporary
# Compose state, so the same command can run in CI and on the target Mac mini
# without credentials. It requires jq only to read and assemble the
# machine-readable evidence; fail before starting the stack if it is absent.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
report="${MINIDOCK_E2E_REPORT:-$root/tmp/md-p0-04-e2e.json}"
cookies="${MINIDOCK_E2E_COOKIES:-$(mktemp)}"
base_url="https://localhost"
app_domain="minidock-e2e.local"
password="minidock-e2e-password"
# This test-only value is never emitted. It proves delivery to the Docker
# container while guarding release evidence and deployment logs against leaks.
runtime_secret="minidock-e2e-runtime-canary"
fixture_root="$root/e2e/fixture-root"
fixture="$fixture_root/hello"
evidence_dir="$(mktemp -d)"
reports="$evidence_dir/releases.jsonl"
original_dockerfile="$evidence_dir/Dockerfile.original"
original_index="$evidence_dir/index.original"

if ! command -v jq >/dev/null 2>&1; then
  echo "scripts/e2e-compose.sh requires jq to validate and write the acceptance report" >&2
  exit 2
fi

cleanup() {
  status=$?
  # The fixture is versioned. Restore it even when an acceptance case fails so
  # running the gate never leaves a developer checkout modified.
  [ -f "$original_dockerfile" ] && cp "$original_dockerfile" "$fixture/Dockerfile" || true
  [ -f "$original_index" ] && cp "$original_index" "$fixture/index.html" || true
  docker compose -f "$root/compose.yaml" logs --no-color >"${report%.json}.log" 2>&1 || true
  if [ -s "$reports" ]; then
    jq -s . "$reports" >"$evidence_dir/releases.json" || true
  fi
  if [ "${MINIDOCK_E2E_KEEP_RUNNING:-0}" = "1" ]; then
    echo "Acceptance evidence retained in $evidence_dir" >&2
    exit "$status"
  fi
  docker compose -f "$root/compose.yaml" down --volumes --remove-orphans >/dev/null 2>&1 || true
  docker network rm "${MINIDOCK_DOCKER_NETWORK:-minidock-e2e}" >/dev/null 2>&1 || true
  rm -rf "$evidence_dir"
  rm -f "$cookies"
  exit "$status"
}
trap cleanup EXIT

mkdir -p "$(dirname "$report")"
cp "$fixture/Dockerfile" "$original_dockerfile"
cp "$fixture/index.html" "$original_index"
export MINIDOCK_DOMAIN=localhost
export MINIDOCK_ADMIN_DOMAIN=localhost
export MINIDOCK_LOCAL_REPOSITORIES_PATH_HOST="$root/e2e/fixture-root"
export MINIDOCK_DOCKER_NETWORK=minidock-e2e
export MINIDOCK_DOCKER_NETWORK_SUBNET=172.31.252.0/24
export MINIDOCK_CADDY_WORKLOAD_IP=172.31.252.2

"$root/scripts/prepare-runtime-network.sh"

docker compose -f "$root/compose.yaml" up --build --detach

# Prove the runtime boundary before exercising a deployment. The Minidock
# container must never receive the daemon socket; the private proxy must still
# be usable for its permitted API calls, while disabled endpoint families fail.
minidock_container="$(docker compose -f "$root/compose.yaml" ps -q minidock)"
[ -n "$minidock_container" ] || { echo "MiniDock container was not created" >&2; exit 1; }
if docker inspect --format '{{range .Mounts}}{{println .Source}}{{end}}' "$minidock_container" | grep -F '/var/run/docker.sock' >/dev/null; then
  echo "MiniDock received a raw Docker socket mount" >&2
  exit 1
fi
[ "$(docker network inspect --format '{{.Internal}}' "$MINIDOCK_DOCKER_NETWORK")" = "true" ] || {
  echo "workload network is not internal" >&2
  exit 1
}
docker compose -f "$root/compose.yaml" exec -T minidock docker info >/dev/null
if docker compose -f "$root/compose.yaml" exec -T minidock docker volume ls >/dev/null 2>&1; then
  echo "Docker socket proxy unexpectedly permits volume API access" >&2
  exit 1
fi

curl_caddy() {
  curl --noproxy '*' --insecure --silent --show-error --fail "$@"
}

page_csrf() {
  curl_caddy --cookie "$cookies" --cookie-jar "$cookies" "$base_url/applications/$app_id" |
    sed -n 's/.*name="csrf_token" value="\([^"]*\)".*/\1/p' | head -n 1
}

latest_deployment_id() {
  curl_caddy --cookie "$cookies" "$base_url/applications/$app_id" |
    grep -Eo "/applications/$app_id/deployments/[0-9]+/release-report" | head -n 1 | cut -d/ -f5
}

release_report() {
  local deployment_id="$1"
  curl_caddy --cookie "$cookies" "$base_url/applications/$app_id/deployments/$deployment_id/release-report"
}

wait_for_status() {
  local deployment_id="$1" wanted="$2" current
  for attempt in $(seq 1 120); do
    current="$(release_report "$deployment_id")"
    if [ "$(printf '%s' "$current" | jq -r '.release.status')" = "$wanted" ]; then
      printf '%s\n' "$current" | tee -a "$reports" >/dev/null
      return 0
    fi
    sleep 1
  done
  echo "deployment $deployment_id did not reach $wanted" >&2
  return 1
}

queue_action() {
  local action="$1" csrf
  csrf="$(page_csrf)"
  [ -n "$csrf" ] || { echo "Could not obtain CSRF token for $action" >&2; return 1; }
  if [ "$action" = "deploy" ]; then
    curl_caddy --cookie "$cookies" --cookie-jar "$cookies" --output /dev/null \
      --data-urlencode "csrf_token=$csrf" \
      --data-urlencode "confirm_production=fixture" \
      "$base_url/applications/$app_id/$action"
  else
    curl_caddy --cookie "$cookies" --cookie-jar "$cookies" --output /dev/null \
      --data-urlencode "csrf_token=$csrf" \
      "$base_url/applications/$app_id/$action"
  fi
}

assert_route() {
  local expected="$1" body
  for attempt in $(seq 1 90); do
    if body="$(curl_caddy --resolve "$app_domain:443:127.0.0.1" "https://$app_domain/" 2>/dev/null)" && [ "$body" = "$expected" ]; then
      return 0
    fi
    sleep 1
  done
  echo "Caddy did not serve expected fixture body: $expected" >&2
  return 1
}

validate_success_evidence() {
  local deployment_id="$1" expected_action="$2" data
  data="$(release_report "$deployment_id")"
  printf '%s' "$data" | jq -e --arg action "$expected_action" '
    .version == 1 and .application.domain == "minidock-e2e.local" and
    .release.action == $action and .release.status == "successful" and
    # A non-Git local folder has no commit SHA; its content-addressed source
    # fingerprint is the reproducibility evidence. Git sources populate both.
    (.release.source_revision | type == "string") and
    (.release.source_fingerprint | startswith("sha256:")) and
    (.release.artifact_digest | startswith("sha256:")) and
    (.release.configuration_digest | startswith("sha256:")) and
    (.release.runtime == "docker") and (.release.manifest | length > 0) and
    (.release.current_stage | length > 0) and
    ([.release.queue_duration_ms, .release.source_duration_ms,
      .release.build_duration_ms, .release.start_duration_ms,
      .release.health_duration_ms, .release.route_duration_ms] | all(. >= 0))
  ' >/dev/null
  curl_caddy --cookie "$cookies" "$base_url/applications/$app_id/deployments/$deployment_id/logs" | grep -q .
}

for attempt in $(seq 1 60); do
  if curl_caddy "$base_url/healthz" >/dev/null 2>&1 && curl_caddy "$base_url/runtimez" >/dev/null 2>&1; then
    break
  fi
  if [ "$attempt" = 60 ]; then
    echo "MiniDock did not become runtime-ready; inspect ${report%.json}.log" >&2
    exit 1
  fi
  sleep 1
done

# Initialize the empty database through the same setup form an operator uses.
curl_caddy --cookie-jar "$cookies" --output /dev/null \
  --data-urlencode "password=$password" \
  --data-urlencode "password_confirmation=$password" \
  "$base_url/setup"

# Obtain the real CSRF token from the registration form rather than bypassing
# the browser contract.
form="$(curl_caddy --cookie "$cookies" --cookie-jar "$cookies" "$base_url/applications/new")"
csrf="$(printf '%s' "$form" | sed -n 's/.*name="csrf_token" value="\([^"]*\)".*/\1/p' | head -n 1)"
if [ -z "$csrf" ]; then
  echo "Could not obtain CSRF token from registration form" >&2
  exit 1
fi

curl_caddy --cookie "$cookies" --cookie-jar "$cookies" --output /dev/null \
  --data-urlencode "csrf_token=$csrf" \
  --data-urlencode "name=fixture" \
  --data-urlencode "repository=file:///repos/hello" \
  --data-urlencode "branch=" \
  --data-urlencode "work_dir=." \
  --data-urlencode "type=custom" \
  --data-urlencode "runtime=docker" \
  --data-urlencode "internal_port=80" \
  --data-urlencode "health_endpoint=/" \
  --data-urlencode "domain=$app_domain" \
  "$base_url/applications"

dashboard="$(curl_caddy --cookie "$cookies" "$base_url/")"
app_id="$(printf '%s' "$dashboard" | grep -Eo '/applications/[0-9]+' | head -n 1 | cut -d/ -f3)"
if [ -z "$app_id" ]; then
  echo "Registered fixture was not present on the dashboard" >&2
  exit 1
fi

# Store a runtime secret through the same CSRF-protected panel flow an operator
# uses. The following deploy verifies that Docker receives it without placing
# its value in CLI arguments, release evidence or the captured deploy log.
secrets_page="$(curl_caddy --cookie "$cookies" "$base_url/applications/$app_id/secrets?environment=production&target=runtime")"
csrf="$(printf '%s' "$secrets_page" | sed -n 's/.*name="csrf_token" value="\([^"]*\)".*/\1/p' | head -n 1)"
[ -n "$csrf" ] || { echo "Could not obtain CSRF token for runtime secret" >&2; exit 1; }
curl_caddy --cookie "$cookies" --cookie-jar "$cookies" --output /dev/null \
  --data-urlencode "csrf_token=$csrf" \
  --data-urlencode "environment=production" \
  --data-urlencode "target=runtime" \
  --data-urlencode "name=MINIDOCK_E2E_RUNTIME_SECRET" \
  --data-urlencode "value=$runtime_secret" \
  "$base_url/applications/$app_id/secrets"

# First deploy.
queue_action deploy
first_id="$(latest_deployment_id)"
[ -n "$first_id" ] || { echo "first deployment was not queued" >&2; exit 1; }
wait_for_status "$first_id" successful
assert_route "minidock-e2e-fixture-ok"
validate_success_evidence "$first_id" deploy
fixture_container="$(docker ps --filter "label=minidock.application=$app_id" --format '{{.ID}}' | head -n 1)"
[ -n "$fixture_container" ] || { echo "fixture container was not found" >&2; exit 1; }
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$fixture_container" |
  grep -Fqx "MINIDOCK_E2E_RUNTIME_SECRET=$runtime_secret" || {
  echo "runtime secret did not reach the fixture container" >&2
  exit 1
}
if release_report "$first_id" | grep -Fq "$runtime_secret" ||
  curl_caddy --cookie "$cookies" "$base_url/applications/$app_id/deployments/$first_id/logs" | grep -Fq "$runtime_secret"; then
  echo "runtime secret leaked into release evidence or deployment log" >&2
  exit 1
fi

# Redeploy a changed fixture, proving that a new release reaches Caddy.
printf 'minidock-e2e-fixture-v2\n' >"$fixture/index.html"
queue_action deploy
second_id="$(latest_deployment_id)"
[ "$second_id" != "$first_id" ] || { echo "redeploy did not create a new release" >&2; exit 1; }
wait_for_status "$second_id" successful
assert_route "minidock-e2e-fixture-v2"
validate_success_evidence "$second_id" deploy

# A deliberately invalid Dockerfile must fail and leave the current Caddy route
# intact. Keep it in place until the local-folder worker has consumed it; the
# EXIT trap still restores the versioned fixture if this assertion fails.
cp "$fixture/Dockerfile" "$evidence_dir/Dockerfile.good"
printf 'THIS IS NOT A DOCKERFILE\n' >"$fixture/Dockerfile"
queue_action deploy
failed_id="$(latest_deployment_id)"
wait_for_status "$failed_id" failed
cp "$evidence_dir/Dockerfile.good" "$fixture/Dockerfile"
release_report "$failed_id" | jq -e '.release.failure_stage != "" and .release.failure_code != ""' >/dev/null
curl_caddy --cookie "$cookies" "$base_url/applications/$app_id/deployments/$failed_id/logs" | grep -q .
assert_route "minidock-e2e-fixture-v2"

# Queue a build that cannot finish before cancellation. A changed COPY input
# prevents Docker from reusing the previous build layer.
printf 'FROM nginx:1.27-alpine\nCOPY index.html /usr/share/nginx/html/index.html\nRUN sleep 30\n' >"$fixture/Dockerfile"
printf 'minidock-e2e-fixture-cancelled\n' >"$fixture/index.html"
queue_action deploy
cancelled_id="$(latest_deployment_id)"
for attempt in $(seq 1 30); do
  status="$(release_report "$cancelled_id" | jq -r '.release.status')"
  if [ "$status" = "queued" ] || [ "$status" = "running" ]; then
    break
  fi
  if [ "$attempt" = 30 ]; then
    echo "deployment $cancelled_id became terminal before it could be cancelled" >&2
    exit 1
  fi
  sleep 1
done
csrf="$(page_csrf)"
curl_caddy --cookie "$cookies" --cookie-jar "$cookies" --output /dev/null \
  --data-urlencode "csrf_token=$csrf" \
  "$base_url/applications/$app_id/deployments/$cancelled_id/cancel"
cp "$evidence_dir/Dockerfile.good" "$fixture/Dockerfile"
printf 'minidock-e2e-fixture-v2\n' >"$fixture/index.html"
wait_for_status "$cancelled_id" cancelled
assert_route "minidock-e2e-fixture-v2"

# Rollback must select the previous successful release and publish it through
# Caddy, with the same evidence contract as a normal release.
queue_action rollback
rollback_id="$(latest_deployment_id)"
wait_for_status "$rollback_id" successful
assert_route "minidock-e2e-fixture-ok"
validate_success_evidence "$rollback_id" rollback

jq -n --slurpfile releases "$reports" \
  --arg package "MD-P0-04" --arg domain "$app_domain" \
  '{package:$package,result:"passed",application:"fixture",domain:$domain,releases:$releases}' >"$report"
echo "MD-P0-04 Compose acceptance passed: $app_domain (report: $report)"
