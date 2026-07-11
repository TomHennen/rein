#!/usr/bin/env bash
# run-journeys.sh — the MANUAL, on-demand journey runner.
#
# There is NO timer and NO background token-minting behind this (Tom's ruling):
# it is what a human, or a pre-merge step, invokes deliberately. It:
#   1. runs every tests/interactive/journey_*.py LIVE, regenerating each golden
#      under REIN_UPDATE_GOLDEN (real srt + real throwaway repo), and
#   2. diffs the regenerated goldens against what's committed, reporting a clear
#      PASS / DRIFT summary and exiting NON-ZERO on any drift or ceremony break.
#
# Workflow (see tests/interactive/CLAUDE.md): before a PR that changes a journey,
# run this and COMMIT the updated golden. On a clean run with no intended change,
# the goldens are rewritten byte-identical, so `git diff` stays empty = PASS.
#
# Setup is the `rein init` world (a fresh machine is already configured). This
# script sources ./dev-env only as the LEGACY this-box shortcut for the App creds.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
cd "$REPO_ROOT"

# Legacy this-box shortcut for REIN_APP_* (a rein-init machine won't need it; the
# journeys resolve their throwaway via resolve_throwaway_repo regardless).
# shellcheck disable=SC1091
[ -f ./dev-env ] && source ./dev-env

# Build up front so a compile error fails fast before any pexpect spawn.
go build -o bin/ ./...

shopt -s nullglob
journeys=("$HERE"/journey_*.py)
shopt -u nullglob
if [ "${#journeys[@]}" -eq 0 ]; then
  echo "no journey_*.py under $HERE" >&2
  exit 1
fi

fail=0
summary=()
for j in "${journeys[@]}"; do
  name="$(basename "$j")"
  echo "=== running $name  (REIN_UPDATE_GOLDEN=1, live) ==="
  if REIN_UPDATE_GOLDEN=1 python3 "$j"; then
    summary+=("RAN   $name")
  else
    rc=$?
    summary+=("ERROR $name (exit $rc — ceremony broke or setup failed)")
    fail=1
  fi
  echo
done

echo "=== golden drift vs committed ==="
if git diff --quiet -- "$HERE/golden/"; then
  echo "PASS: all goldens current (no drift)"
else
  echo "DRIFT: goldens changed — review, then commit if the change is intended"
  echo "       (or 'git checkout -- tests/interactive/golden/' to discard):"
  echo
  git --no-pager diff -- "$HERE/golden/"
  fail=1
fi

echo
echo "=== summary ==="
printf '  %s\n' "${summary[@]}"
if [ "$fail" -eq 0 ]; then
  echo "OK"
else
  echo "DRIFT/ERROR — see above"
fi
exit "$fail"
