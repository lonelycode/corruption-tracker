#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Martin Buhr and contributors
#
# Smoke test for the compose stack (CI, and before a release):
#
#   1. builds the image and brings the stack up from nothing but a
#      generated POSTGRES_PASSWORD, waiting for every service to be healthy;
#   2. completes the setup wizard over HTTP and checks the tracker renders;
#   3. saves a secret setting, then `down` + `up -d` and checks the
#      account and the secret survived (database and data volumes);
#   4. prints memory use, then removes the stack, its volumes and the image.
#
# Runs under a throwaway project name and port, so it never touches a real
# deployment on the same machine. Needs docker compose v2.23+, curl and openssl.
#
#   scripts/compose-smoke.sh            # port 18080
#   SMOKE_PORT=19080 scripts/compose-smoke.sh
#   SMOKE_LIMITS=1 scripts/compose-smoke.sh   # as on a 1 vCPU / 1 GB VPS:
#                                             # tracker and db get 0.5 CPU
#                                             # and 512 MB each
set -euo pipefail

cd "$(dirname "$0")/.."
port=${SMOKE_PORT:-18080}
project=ct-smoke-$$
image=corruption-tracker:smoke-$$
work=$(mktemp -d)
base=http://127.0.0.1:$port

printf 'POSTGRES_PASSWORD=%s\nTRACKER_IMAGE=%s\nTRACKER_PORT=%s\n' \
  "$(openssl rand -hex 24)" "$image" "$port" > "$work/env"
files=(-f docker-compose.yml)
if [ -n "${SMOKE_LIMITS:-}" ]; then
  printf 'services:\n  db: {cpus: 0.5, mem_limit: 512m}\n  tracker: {cpus: 0.5, mem_limit: 512m}\n' > "$work/limits.yml"
  files+=(-f "$work/limits.yml")
fi
dc() { docker compose -p "$project" --env-file "$work/env" "${files[@]}" "$@"; }

cleanup() {
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  docker image rm -f "$image" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() { echo "SMOKE FAIL: $*" >&2; dc logs --tail 50 tracker >&2 || true; exit 1; }
csrf() { grep -o 'name="csrf_token" value="[^"]*"' "$1" | head -1 | sed 's/.*value="//; s/"$//'; }

echo "== build and start"
dc build --quiet tracker
started=$(date +%s)
dc up -d --wait --wait-timeout 180 || fail "stack did not become healthy"
echo "healthy after $(( $(date +%s) - started ))s"

echo "== setup wizard"
curl -fsS -c "$work/jar" -b "$work/jar" -o "$work/setup.html" "$base/" -L
grep -q 'name="password_confirm"' "$work/setup.html" || fail "a fresh instance did not open the setup wizard"
password=$(openssl rand -hex 12)
code=$(curl -sS -c "$work/jar" -b "$work/jar" -o /dev/null -w '%{http_code}' "$base/setup" \
  --data-urlencode "csrf_token=$(csrf "$work/setup.html")" --data-urlencode username=smoke-admin \
  --data-urlencode "password=$password" --data-urlencode "password_confirm=$password")
[ "$code" = 303 ] || fail "setup POST returned $code"

echo "== tracker renders"
curl -fsS -b "$work/jar" -o "$work/tracker.html" "$base/tracker/corruption" -L || fail "tracker page"
grep -qi 'source code' "$work/tracker.html" || fail "tracker page has no AGPL source link"

echo "== save a secret setting"
curl -fsS -c "$work/jar" -b "$work/jar" -o "$work/settings.html" "$base/admin/settings/"
code=$(curl -sS -c "$work/jar" -b "$work/jar" -o /dev/null -w '%{http_code}' "$base/admin/settings/" \
  --data-urlencode "csrf_token=$(csrf "$work/settings.html")" --data-urlencode "llm.api_key=smoke-value-0000-WXYZ")
[ "$code" = 303 ] || fail "settings save returned $code"

echo "== down and up again"
dc down
dc up -d --wait --wait-timeout 180 || fail "stack did not come back healthy"
rm -f "$work/jar"
curl -fsS -c "$work/jar" -b "$work/jar" -o "$work/login.html" "$base/login"
code=$(curl -sS -c "$work/jar" -b "$work/jar" -o /dev/null -w '%{http_code}' "$base/login" \
  --data-urlencode "csrf_token=$(csrf "$work/login.html")" --data-urlencode username=smoke-admin \
  --data-urlencode "password=$password")
[ "$code" = 303 ] || fail "the admin account did not survive a restart (login returned $code)"
curl -fsS -b "$work/jar" -o "$work/settings.html" "$base/admin/settings/"
grep -q 'set (…WXYZ)' "$work/settings.html" || fail "the stored secret did not survive a restart"
grep -q 'unreadable' "$work/settings.html" && fail "the secret key on the data volume was lost"

echo "== memory at idle"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}} cpu={{.CPUPerc}}' \
  $(dc ps -q)

echo "SMOKE OK"
