#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Martin Buhr and contributors
#
# Upgrade, backup and restore test for the compose stack (CI on every
# release):
#
#   1. version N: builds the image from OLD_REF, brings the stack up,
#      completes setup and sets a site name;
#   2. version N+1: switches the stack to an image of this checkout and
#      runs `up -d`, which is what `docker compose pull && docker compose
#      up -d` does with published images. Checks the admin account and the
#      site name survived, the schema is at this checkout's latest
#      migration, and a pre-migrate backup was taken when the schema moved;
#   3. takes a backup in the running stack (`corruption-tracker backup`);
#   4. restores it into a second, empty stack (`corruption-tracker restore`)
#      and checks the account and the site name are there.
#
# Everything runs under throwaway project names, ports and image tags, and
# is removed at the end. Needs git, docker compose v2.23+, curl, openssl.
#
#   OLD_REF=v0.1.0 scripts/compose-upgrade-test.sh
#
# OLD_REF defaults to the newest tag. A ref from before the Dockerfile
# existed is built with this checkout's Dockerfile.
set -euo pipefail

cd "$(dirname "$0")/.."
old_ref=${OLD_REF:-$(git describe --tags --abbrev=0 2>/dev/null || true)}
[ -n "$old_ref" ] || { echo "set OLD_REF to the version to upgrade from" >&2; exit 2; }
port=${SMOKE_PORT:-18090}
tag=$$
old_image=corruption-tracker:upgrade-old-$tag
new_image=corruption-tracker:upgrade-new-$tag
work=$(mktemp -d)
base=http://127.0.0.1:$port
base2=http://127.0.0.1:$((port + 1))

env_file() { # project-env-file image port
  printf 'POSTGRES_PASSWORD=%s\nTRACKER_IMAGE=%s\nTRACKER_PORT=%s\n' "$4" "$2" "$3" > "$1"
}
pw1=$(openssl rand -hex 24)
pw2=$(openssl rand -hex 24)
env_file "$work/env1" "$old_image" "$port" "$pw1"
env_file "$work/env2" "$new_image" "$((port + 1))" "$pw2"
dc1() { docker compose -p "ct-up1-$tag" --env-file "$work/env1" -f docker-compose.yml "$@"; }
dc2() { docker compose -p "ct-up2-$tag" --env-file "$work/env2" -f docker-compose.yml "$@"; }

cleanup() {
  dc1 down -v --remove-orphans >/dev/null 2>&1 || true
  dc2 down -v --remove-orphans >/dev/null 2>&1 || true
  docker image rm -f "$old_image" "$new_image" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() { echo "UPGRADE TEST FAIL: $*" >&2; dc1 logs --tail 40 tracker >&2 || true; exit 1; }
csrf() { grep -o 'name="csrf_token" value="[^"]*"' "$1" | head -1 | sed 's/.*value="//; s/"$//'; }
login() { # base jar -> http code
  rm -f "$2"
  curl -fsS -c "$2" -b "$2" -o "$work/login.html" "$1/login"
  curl -sS -c "$2" -b "$2" -o /dev/null -w '%{http_code}' "$1/login" \
    --data-urlencode "csrf_token=$(csrf "$work/login.html")" --data-urlencode username=upgrade-admin \
    --data-urlencode "password=$password"
}
has_site_name() { # base jar
  curl -fsS -b "$2" "$1/admin/settings/" | grep -q 'value="Upgrade Test Watch"'
}
latest=$(ls migrations/sql/*.up.sql | sed 's#.*/##; s/_.*//' | sort -n | tail -1 | sed 's/^0*//')

echo "== version N: $old_ref"
prefix=$(git rev-parse --show-prefix)
git -C "$(git rev-parse --show-toplevel)" archive "$old_ref" "${prefix:-.}" | tar -x -C "$work"
old_ctx=$work/${prefix:-.}
[ -f "$old_ctx/Dockerfile" ] || cp Dockerfile .dockerignore "$old_ctx/"
old_latest=$(ls "$old_ctx"/migrations/sql/*.up.sql | sed 's#.*/##; s/_.*//' | sort -n | tail -1 | sed 's/^0*//')
docker build -q -t "$old_image" "$old_ctx" >/dev/null
docker build -q -t "$new_image" . >/dev/null
dc1 up -d --wait --wait-timeout 180 || fail "version N did not become healthy"

password=$(openssl rand -hex 12)
curl -fsS -c "$work/jar" -b "$work/jar" -o "$work/setup.html" "$base/setup"
code=$(curl -sS -c "$work/jar" -b "$work/jar" -o /dev/null -w '%{http_code}' "$base/setup" \
  --data-urlencode "csrf_token=$(csrf "$work/setup.html")" --data-urlencode username=upgrade-admin \
  --data-urlencode "password=$password" --data-urlencode "password_confirm=$password")
[ "$code" = 303 ] || fail "setup on version N returned $code"
curl -fsS -c "$work/jar" -b "$work/jar" -o "$work/settings.html" "$base/admin/settings/" ||
  fail "version N has no Settings page; use an OLD_REF that has the Settings page"
code=$(curl -sS -c "$work/jar" -b "$work/jar" -o /dev/null -w '%{http_code}' "$base/admin/settings/" \
  --data-urlencode "csrf_token=$(csrf "$work/settings.html")" --data-urlencode "site.name=Upgrade Test Watch")
[ "$code" = 303 ] || fail "saving the site name on version N returned $code"

echo "== version N+1: this checkout (schema $old_latest -> $latest)"
sed -i.bak "s#^TRACKER_IMAGE=.*#TRACKER_IMAGE=$new_image#" "$work/env1"
dc1 up -d --wait --wait-timeout 180 || fail "version N+1 did not become healthy"
[ "$(login "$base" "$work/jar")" = 303 ] || fail "the admin account did not survive the upgrade"
has_site_name "$base" "$work/jar" || fail "the site name did not survive the upgrade"
schema=$(dc1 exec -T db psql -U tracker -d tracker -tAc 'SELECT version FROM tracker_schema_migrations')
[ "$schema" = "$latest" ] || fail "schema is $schema after the upgrade, want $latest"
backups=$(dc1 exec -T tracker ls /data/backups)
if [ "$old_latest" != "$latest" ]; then
  echo "$backups" | grep -q -- '-pre-migrate$' || fail "no pre-migrate backup after a schema change (have: $backups)"
fi

echo "== backup"
name=$(dc1 exec -T tracker corruption-tracker backup | sed -n 's/^backup written: //p')
[ -n "$name" ] || fail "corruption-tracker backup printed no backup name"
mkdir -p "$work/restore"
dc1 cp "tracker:/data/backups/$name" "$work/restore/$name"
chmod -R a+rX "$work/restore" # the container user reads it on Linux too

echo "== restore into an empty stack"
dc2 up -d --wait --wait-timeout 120 db || fail "the second database did not start"
dc2 run --rm --no-deps -v "$work/restore:/restore:ro" tracker restore "/restore/$name" ||
  fail "restore failed"
dc2 up -d --wait --wait-timeout 180 || fail "the restored stack did not become healthy"
[ "$(login "$base2" "$work/jar2")" = 303 ] || fail "the admin account is not in the restored stack"
has_site_name "$base2" "$work/jar2" || fail "the site name is not in the restored stack"

echo "UPGRADE TEST OK ($old_ref -> this checkout, schema $old_latest -> $latest, restored $name)"
