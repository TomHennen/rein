#!/usr/bin/env bash
# demo_pat_leak.sh — issue #57, LIVE: rein-gh must never leak the developer's
# ambient PAT past the scope ceiling.
#
# rein-gh is the `gh` shim rein puts at the front of $PATH. Before #57 it built
# every `exec` from a raw os.Environ(), so on any mint-failure / deny leg an
# inherited GH_TOKEN from the developer's shell reached the real `gh` — a WRITE
# executing with their full-scope PAT, the exact thing rein exists to prevent.
# (The exposure was the shim on PATH OUTSIDE `rein run`; sandboxed runs and
# `rein run` children were already safe.)
#
# This demo drives the REAL rein-gh binary against a FAKE gh that just dumps the
# credential-relevant env vars it received. It shows three legs:
#
#   (a) the WRITE leg strips the ambient PAT      (fail-closed placeholder)
#   (b) `gh auth login` still works for the HUMAN  (no REIN_RUN_ID -> local tier)
#   (c) `gh auth login` / `gh auth token` is REFUSED for the AGENT (REIN_RUN_ID set)
#
# WHY IT TOUCHES NOTHING REAL (hard-constraint #1): the write leg runs OUTSIDE a
# `rein run` (no REIN_RUN_ID), so rein-gh's write gate denies BEFORE it ever
# calls GitHub to mint (cmd/rein-gh/main.go: runWrite returns execGhWithoutToken
# on an empty confirmed-issue set). No token is minted, no repo is touched, and
# the "gh" it execs is our fake env-dumper, never the real GitHub CLI. The PAT
# used is the literal fake string `ghp_FAKE_NOT_REAL_...` — not a real credential.
#
# Run:  source ./dev-env && tests/interactive/demo_pat_leak.sh
set -u

# --- locate repo root + build the real rein-gh -----------------------------
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
cd "$ROOT" || exit 1

BIN="$(mktemp -d)/rein-gh"
echo "building rein-gh -> $BIN"
go build -o "$BIN" ./cmd/rein-gh || { echo "build failed"; exit 1; }

# --- a fake gh that dumps the credential env it was handed -----------------
FAKE_DIR="$(mktemp -d)"
FAKE_GH="$FAKE_DIR/gh"
ENVOUT="$FAKE_DIR/env-out"
cat > "$FAKE_GH" <<'EOF'
#!/bin/sh
{
  printf 'GH_TOKEN=%s\n'                "${GH_TOKEN-<unset>}"
  printf 'GITHUB_TOKEN=%s\n'            "${GITHUB_TOKEN-<unset>}"
  printf 'GH_ENTERPRISE_TOKEN=%s\n'     "${GH_ENTERPRISE_TOKEN-<unset>}"
  printf 'GITHUB_ENTERPRISE_TOKEN=%s\n' "${GITHUB_ENTERPRISE_TOKEN-<unset>}"
} > "$REIN_TEST_GH_ENVOUT"
echo "[fake gh] RAN with args: $*"
exit 0
EOF
chmod +x "$FAKE_GH"

# The literal fake PAT we poison the shell with. NOT a real credential.
FAKE_PAT="ghp_FAKE_NOT_REAL_0000000000000000000000"

# rein-gh resolves the real gh via REIN_REAL_GH when set — point it at the fake.
export REIN_REAL_GH="$FAKE_GH"
export REIN_TEST_GH_ENVOUT="$ENVOUT"

rule() { printf '%s\n' "=============================================================================="; }
dump() { if [ -f "$ENVOUT" ]; then sed 's/^/    /' "$ENVOUT"; else echo "    (fake gh did NOT run — no env dump)"; fi; }
clear_dump() { rm -f "$ENVOUT"; }

# --------------------------------------------------------------------------
rule; echo "  #57 DEMO — rein-gh must not leak the developer's ambient PAT"; rule
echo "  poisoning the shell with GH_TOKEN=$FAKE_PAT (a fake, non-real PAT)"
echo "  real gh replaced by a fake env-dumper at: $FAKE_GH"

# --- (a) WRITE leg: BEFORE vs AFTER ---------------------------------------
rule; echo "  (a) WRITE leg — \`gh issue comment\` outside \`rein run\` (mint-failure/deny leg)"; rule

echo
echo "  BEFORE #57 (what the RAW passthrough did — exec real gh with os.Environ()):"
echo "  \$ GH_TOKEN=$FAKE_PAT  gh issue comment 1 --body hi"
clear_dump
env GH_TOKEN="$FAKE_PAT" GITHUB_TOKEN="$FAKE_PAT" REIN_TEST_GH_ENVOUT="$ENVOUT" \
    "$FAKE_GH" issue comment 1 --body hi >/dev/null 2>&1
dump
echo "  ^ the developer's full-scope PAT reached gh. A write would run as THEM."

echo
echo "  AFTER #57 (rein-gh, no REIN_RUN_ID -> write gate denies -> fail-closed env):"
echo "  \$ GH_TOKEN=$FAKE_PAT  rein-gh issue comment 1 --body hi"
clear_dump
env GH_TOKEN="$FAKE_PAT" GITHUB_TOKEN="$FAKE_PAT" \
    "$BIN" issue comment 1 --body hi >/dev/null 2>&1
dump
echo "  ^ GH_TOKEN=rein-placeholder-denied, GITHUB_TOKEN stripped. gh fails to"
echo "    auth rather than silently using the developer's PAT."

# --- (b) HUMAN gh auth login still works ----------------------------------
rule; echo "  (b) HUMAN recovery — \`gh auth login\` (no REIN_RUN_ID -> local tier)"; rule
echo "  \$ GH_TOKEN=$FAKE_PAT  rein-gh auth login"
clear_dump
env GH_TOKEN="$FAKE_PAT" "$BIN" auth login >/tmp/demo57_authlogin.out 2>&1
echo "  rein-gh exit: $?"
sed 's/^/    /' /tmp/demo57_authlogin.out
echo "  env the real gh received:"
dump
echo "  ^ gh RAN with NO GH_TOKEN at all (not even the placeholder — gh 2.67"
echo "    refuses \`auth login\` if GH_TOKEN is set). The ambient PAT is still"
echo "    stripped, so the human's login flow is unblocked but nothing leaks."

# --- (c) AGENT gh auth is refused -----------------------------------------
rule; echo "  (c) AGENT — \`gh auth ...\` REFUSED inside a rein run (REIN_RUN_ID set)"; rule
for sub in "auth login" "auth token"; do
  echo "  \$ REIN_RUN_ID=demo-run  rein-gh $sub"
  clear_dump
  env REIN_RUN_ID=demo-run GH_TOKEN="$FAKE_PAT" "$BIN" $sub >/tmp/demo57_agentauth.out 2>&1
  echo "  rein-gh exit: $?  (want non-zero: refused)"
  sed 's/^/    /' /tmp/demo57_agentauth.out
  if [ -f "$ENVOUT" ]; then echo "    LEAK: fake gh RAN (should not have)"; else echo "    fake gh did NOT run (refused before exec) — correct"; fi
  echo
done

rule; echo "  DONE — (a) PAT stripped on write, (b) human auth unblocked, (c) agent auth refused."; rule
