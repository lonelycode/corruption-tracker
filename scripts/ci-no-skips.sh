#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Martin Buhr and contributors
#
# Fails when a `go test -v` log shows a skipped test. In CI every test has
# what it needs (a database included), so a skip means a test proved
# nothing: most often DATABASE_URL_TEST went missing.
#
#   go test -p 1 -count=1 -v ./... 2>&1 | tee test.log
#   scripts/ci-no-skips.sh test.log
#
# go test prints the skip reason on its own line before "--- SKIP: Name", so
# the check matches the SKIP lines themselves, never the reason text.
set -euo pipefail

log=${1:?usage: ci-no-skips.sh GO_TEST_V_LOG}

# Tests allowed to skip in CI, as anchored extended regexes over the test
# name (subtests as Parent/child). Keep it empty unless a test needs
# something CI can't provide, and say why next to the entry.
allowed=()

if ! grep -q '^=== RUN' "$log"; then
  echo "ci-no-skips: $log holds no test run (was it written with go test -v?)" >&2
  exit 1
fi

bad=()
while read -r name; do
  ok=0
  for re in "${allowed[@]+"${allowed[@]}"}"; do
    if [[ "$name" =~ ^($re)$ ]]; then ok=1; fi
  done
  [ "$ok" = 1 ] || bad+=("$name")
done < <(sed -nE 's/^[[:space:]]*--- SKIP: ([^ ]+).*/\1/p' "$log")

if [ "${#bad[@]}" -gt 0 ]; then
  echo "ci-no-skips: ${#bad[@]} test(s) skipped; a skipped test proves nothing:" >&2
  for name in "${bad[@]}"; do
    echo "  $name" >&2
  done
  echo "Skip reasons (the line above each SKIP):" >&2
  grep -B1 -E '^[[:space:]]*--- SKIP: ' "$log" | grep -vE '^(--|[[:space:]]*--- SKIP: )' | sed 's/^/  /' >&2 || true
  exit 1
fi
echo "ci-no-skips: no skipped tests"
