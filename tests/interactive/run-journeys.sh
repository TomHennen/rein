#!/usr/bin/env bash
# run-journeys.sh — the MANUAL, on-demand journey runner (no timer, no background
# token-minting — Tom's ruling). It runs every tests/interactive/journey_*.py
# LIVE and each one COMPARES its fresh run to the committed RAW golden by
# normalizing BOTH sides (PR #78), so a different issue number / nonce / object
# count still passes but a genuinely new or changed line trips drift.
#
# Modes:
#   run-journeys.sh                 # compare each journey to its golden; DRIFT is
#                                   # reported (normalized diff + a scratch path to
#                                   # the raw fresh transcript) and exits non-zero.
#   REIN_UPDATE_GOLDEN=1 run-journeys.sh
#                                   # ADOPT: rewrite each RAW golden from a live run.
#                                   # `git diff` then shows the new raw golden to commit.
#   run-journeys.sh --normalized    # also print each journey's normalized transcript
#                                   # (the comparison lens) to eyeball what changed.
#
# Workflow (see tests/interactive/CLAUDE.md): before a PR that changes a journey,
# run with REIN_UPDATE_GOLDEN=1 and COMMIT the regenerated raw golden.
#
# Setup is the `rein init` world (a fresh machine is already configured). This
# script sources ./dev-env only as the LEGACY this-box shortcut for the App creds.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
cd "$REPO_ROOT"

if [ "${1:-}" = "--normalized" ]; then
  export REIN_SHOW_NORMALIZED=1
  shift
fi

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
  echo "=== $name (live) ==="
  # The journey itself does the normalize-both compare and sets the exit code:
  #   0 = match (or golden updated)   1 = drift   2 = ceremony broke
  python3 "$j"
  rc=$?
  case "$rc" in
    0) summary+=("PASS  $name") ;;
    1) summary+=("DRIFT $name (normalized diff above; raw fresh at \$TMPDIR)"); fail=1 ;;
    2) summary+=("BROKE $name (ceremony invariant failed)"); fail=1 ;;
    *) summary+=("ERROR $name (exit $rc)"); fail=1 ;;
  esac
  echo
done

echo "=== summary ==="
printf '  %s\n' "${summary[@]}"
if [ -n "${REIN_UPDATE_GOLDEN:-}" ]; then
  echo
  echo "REIN_UPDATE_GOLDEN was set: the RAW goldens were rewritten from live runs."
  echo "Review and commit them:"
  git --no-pager diff --stat -- "$HERE/golden/" || true
fi
if [ "$fail" -eq 0 ]; then
  echo "OK"
else
  echo "DRIFT/BROKE — see above"
fi
exit "$fail"
