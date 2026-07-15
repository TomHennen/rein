#!/usr/bin/env bash
# run-journeys.sh — the MANUAL, on-demand journey runner (no timer, no background
# token-minting — Tom's ruling). It runs every tests/interactive/journey_*.py
# LIVE and each one COMPARES its fresh run to the committed RAW golden by
# normalizing BOTH sides (PR #78), so a different issue number / nonce / object
# count still passes but a genuinely new or changed line trips drift.
#
# Two KINDS of check, kept clearly separated in the output:
#   [A] GOLDEN-DRIFT  — do the journey transcripts still match their goldens?
#   [B] SANDBOX-HOLDS — do the live sandbox INVARIANTS still hold? (opt-in via
#                       --sandbox; the gated E2E Go tests + the interactive
#                       .git-hardening / agent-contract tests). This is the
#                       on-demand "prove the sandbox still holds" entry point.
#
# Modes:
#   run-journeys.sh                 # [A] compare each journey to its golden; DRIFT is
#                                   # reported (normalized diff + a scratch path to
#                                   # the raw fresh transcript) and exits non-zero.
#   run-journeys.sh --sandbox       # [A] then [B]: also run the live sandbox suites
#                                   # (REIN_SANDBOX_E2E Go tests + .git hardening +
#                                   # agent contract + real-agent sandbox startup).
#                                   # PASS/FAIL per suite; non-zero exit if ANY suite
#                                   # (or any journey) fails.
#   REIN_UPDATE_GOLDEN=1 run-journeys.sh
#                                   # ADOPT: rewrite each RAW golden from a live run.
#                                   # `git diff` then shows the new raw golden to commit.
#   run-journeys.sh --normalized    # also print each journey's normalized transcript
#                                   # (the comparison lens) to eyeball what changed.
#
# Flags may be combined and given in any order (e.g. --sandbox --normalized).
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

RUN_SANDBOX=0
for arg in "$@"; do
  case "$arg" in
    --normalized) export REIN_SHOW_NORMALIZED=1 ;;
    --sandbox)    RUN_SANDBOX=1 ;;
    *) echo "unknown flag: $arg (accepted: --sandbox, --normalized)" >&2; exit 2 ;;
  esac
done

# Legacy this-box shortcut for REIN_APP_* (a rein-init machine won't need it; the
# journeys resolve their throwaway via resolve_throwaway_repo regardless).
# shellcheck disable=SC1091
[ -f ./dev-env ] && source ./dev-env

# Build up front so a compile error fails fast before any pexpect spawn.
go build -o bin/ ./...

# ==========================================================================
# [A] GOLDEN-DRIFT — the journey transcripts vs their committed goldens
# ==========================================================================
echo "########## [A] GOLDEN-DRIFT: journeys vs goldens ##########"
shopt -s nullglob
journeys=("$HERE"/journey_*.py)
shopt -u nullglob
if [ "${#journeys[@]}" -eq 0 ]; then
  echo "no journey_*.py under $HERE" >&2
  exit 1
fi

fail=0
skipped=0
summary=()
for j in "${journeys[@]}"; do
  name="$(basename "$j")"
  echo "=== $name (live) ==="
  # The journey itself does the normalize-both compare and sets the exit code:
  #   0 = match (or golden updated)   1 = drift   2 = ceremony/boundary broke
  #   3 = SKIPPED — a prerequisite is missing, so the journey did NOT run.
  # 3 is its own status ON PURPOSE: a skip that reports PASS is the #68 footgun
  # (a green suite hiding an untested path). A skip is not a failure, but it must
  # never look like coverage.
  python3 "$j"
  rc=$?
  case "$rc" in
    0) summary+=("PASS  $name") ;;
    1) summary+=("DRIFT $name (normalized diff above; raw fresh at \$TMPDIR)"); fail=1 ;;
    2) summary+=("BROKE $name (invariant failed)"); fail=1 ;;
    3) summary+=("SKIP  $name (prerequisite missing — this journey did NOT run; see above)"); skipped=1 ;;
    *) summary+=("ERROR $name (exit $rc)"); fail=1 ;;
  esac
  echo
done

echo "=== [A] golden-drift summary ==="
printf '  %s\n' "${summary[@]}"
if [ "$skipped" -eq 1 ]; then
  echo
  echo "  WARNING: one or more journeys SKIPPED — that coverage did NOT run."
  echo "  Install the missing prerequisite and re-run, or you are reading a green"
  echo "  summary for a path nothing exercised (#68)."
fi
if [ -n "${REIN_UPDATE_GOLDEN:-}" ]; then
  echo
  echo "REIN_UPDATE_GOLDEN was set: the RAW goldens were rewritten from live runs."
  echo "Review and commit them:"
  git --no-pager diff --stat -- "$HERE/golden/" || true
fi
echo

# ==========================================================================
# [B] SANDBOX-HOLDS — the live sandbox invariants (opt-in: --sandbox)
# ==========================================================================
# Distinct from golden drift: these do NOT compare a transcript, they assert the
# sandbox still ENFORCES its filesystem boundary — the same claims the
# sandbox_filesystem journey narrates, but as pass/fail invariants (a hidden path
# stays hidden, the .git host-exec escape stays closed, $HOME stays ephemeral, the
# contract still reaches the agent). Run on demand because they are slow (each
# spins a real srt sandbox).
sbx_fail=0
sbx_summary=()

run_suite() {  # run_suite "<label>" "<workdir>" <cmd...>
  local label="$1" wd="$2"; shift 2
  echo "=== $label ==="
  if ( cd "$wd" && "$@" ); then
    sbx_summary+=("PASS  $label")
  else
    sbx_summary+=("FAIL  $label"); sbx_fail=1
  fi
  echo
}

if [ "$RUN_SANDBOX" -eq 1 ]; then
  echo "########## [B] SANDBOX-HOLDS: live sandbox invariants ##########"

  # Gated Go E2E: deny-read + home-deny + home-write-semantics under real srt.
  run_suite "srt deny/home E2E (Go: internal/srt -run E2E)" "$REPO_ROOT" \
    env REIN_SANDBOX_E2E=1 go test ./internal/srt/ -run E2E -count=1

  # Gated Go E2E: a working tree UNDER an allow-back stays writable (cmd/rein).
  run_suite "sandbox_home work-tree-under-allow-back E2E (Go: cmd/rein -run E2E)" "$REPO_ROOT" \
    env REIN_SANDBOX_E2E=1 go test ./cmd/rein/ -run E2E -count=1

  # Interactive (run from HERE so `import reinharness`/`import itest_base` resolve):
  # the .git host-exec escape is CLOSED (live srt, no tty needed).
  run_suite ".git hardening (interactive: test_git_hardening.py)" "$HERE" \
    python3 -m unittest -v test_git_hardening

  # Interactive: the injected contract reaches a REAL claude (needs claude on PATH;
  # skip gracefully if absent — LLM phrasing is not golden material, so this is an
  # invariant check, not a transcript compare).
  if command -v claude >/dev/null 2>&1; then
    run_suite "agent contract read-back (interactive, real claude: test_agent_contract.py)" "$HERE" \
      python3 -m unittest -v test_agent_contract
  else
    sbx_summary+=("SKIP  agent contract read-back (no 'claude' on PATH — real-agent test skipped)")
    echo "=== agent contract read-back: SKIPPED (no 'claude' on PATH) ==="
    echo
  fi

  # Interactive: a REAL claude actually STARTS in the sandbox and answers (the
  # egress/EROFS/MCP-startup floor). SELECTED here, never self-skipped in run.sh's
  # sweep. Missing 'claude' is a graceful SKIP (external dep + quota); but 'claude'
  # present with pyte MISSING is a HARD FAIL — a real agent's TUI can only be read
  # off a pyte render (#100), so its absence in an opted-in env is a misconfig, not
  # coverage that quietly vanishes.
  if command -v claude >/dev/null 2>&1; then
    if python3 -c 'import pyte' >/dev/null 2>&1; then
      run_suite "real agent starts in sandbox (interactive, real claude: realagent_e2e.py)" "$HERE" \
        python3 -m unittest -v realagent_e2e
    else
      sbx_summary+=("FAIL  real agent in sandbox (claude present but pyte MISSING — apt install python3-pyte)")
      sbx_fail=1
      echo "=== real agent in sandbox: FAIL (claude present, pyte missing — apt install python3-pyte) ==="
      echo
    fi
  else
    sbx_summary+=("SKIP  real agent in sandbox (no 'claude' on PATH — real-agent test skipped)")
    echo "=== real agent in sandbox: SKIPPED (no 'claude' on PATH) ==="
    echo
  fi

  echo "=== [B] sandbox-holds summary ==="
  printf '  %s\n' "${sbx_summary[@]}"
  echo
fi

# ==========================================================================
# Verdict
# ==========================================================================
total_fail=$(( fail | sbx_fail ))
echo "########## VERDICT ##########"
if [ "$fail" -eq 0 ]; then echo "[A] golden-drift: OK"; else echo "[A] golden-drift: DRIFT/BROKE — see above"; fi
if [ "$RUN_SANDBOX" -eq 1 ]; then
  if [ "$sbx_fail" -eq 0 ]; then echo "[B] sandbox-holds: OK"; else echo "[B] sandbox-holds: FAIL — see above"; fi
else
  echo "[B] sandbox-holds: not run (pass --sandbox to prove the sandbox invariants)"
fi
[ "$total_fail" -eq 0 ] && echo "ALL OK" || echo "FAILURES — see above"
exit "$total_fail"
