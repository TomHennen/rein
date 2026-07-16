#!/usr/bin/env bash
# Run the rein INTERACTIVE (pexpect) test suite.
#
# This suite is SEPARATE from `go test ./...`: it's Python + pexpect and drives
# the real `rein` binary against a LIVE throwaway repo + a working GitHub App.
# It is NEVER run by `go test` (there are no .go files under tests/interactive).
#
# PREREQUISITES (see README.md):
#   - A machine set up via `rein init` (#126): the tests resolve the App from
#     state.json + the managed keystore (via reinharness.init_app_env) and their
#     THROWAWAY repo from the dev-session (hard-constraint #1: only that repo).
#     No dev-env is sourced — its committed dead App used to shadow the real one.
#   - The sandbox stack healthy: srt, bwrap, socat, ripgrep, unprivileged userns.
#   - python3 + pexpect (4.9.0). Host `gh` authed (branch verify/cleanup).
#     NOTE: the suite uses the STDLIB `unittest` — NO pytest needed (this VM has
#     no pip; installing pytest would need a privileged apt, which we avoid).
#
# Usage:
#   tests/interactive/run.sh                              # whole suite
#   tests/interactive/run.sh test_write_approval          # one module
#   tests/interactive/run.sh test_write_approval.WriteApprovalLoop.test_...  # one test
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
cd "$REPO_ROOT"

# Build up front so a compile error fails fast, before pexpect spawns anything.
go build -o bin/ ./...

# unittest discovery from the REPO ROOT so the `tests.interactive` package resolves
# (imports are package-absolute; no sys.path hacks). The `test_*.py` pattern collects
# the flat plain tests only — journeys are journeys/<name>/journey.py, so they are NOT
# swept here.
cd "$REPO_ROOT"
if [ "$#" -gt 0 ]; then
  # Explicit module/test target(s), e.g. `run.sh test_write_approval` -> the package path.
  targets=()
  for t in "$@"; do targets+=("tests.interactive.$t"); done
  exec python3 -m unittest -v "${targets[@]}"
fi
exec python3 -m unittest discover -s tests/interactive -t . -p 'test_*.py' -v
