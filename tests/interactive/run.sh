#!/usr/bin/env bash
# Run the rein INTERACTIVE (pexpect) test suite.
#
# This suite is SEPARATE from `go test ./...`: it's Python + pexpect and drives
# the real `rein` binary against a LIVE throwaway repo + a working GitHub App.
# It is NEVER run by `go test` (there are no .go files under tests/interactive).
#
# PREREQUISITES (see README.md):
#   - ./dev-env sourced (this script does it) => REIN_* incl. REIN_TEST_REPO_A,
#     a THROWAWAY repo (hard-constraint #1: the suite touches ONLY this repo).
#   - A working App (REIN_APP_* + the private key) — the same setup `rein doctor`
#     validates.
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

# shellcheck disable=SC1091
source ./dev-env

: "${REIN_TEST_REPO_A:?set REIN_TEST_REPO_A=<owner>/<throwaway> (dev-env should)}"

# Build up front so a compile error fails fast, before pexpect spawns anything.
go build -o bin/ ./...

# unittest discovery: run from the suite dir so `import reinharness` resolves.
cd "$HERE"
if [ "$#" -gt 0 ]; then
  # Explicit module/test target(s), e.g. `run.sh test_write_approval`.
  exec python3 -m unittest -v "$@"
fi
exec python3 -m unittest discover -s "$HERE" -t "$HERE" -p 'test_*.py' -v
