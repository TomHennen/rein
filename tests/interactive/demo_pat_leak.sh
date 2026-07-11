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

# Assertions: this demo must FAIL LOUD on a regression (not just print a leak
# that a human has to notice). FAILS accumulates; the script exits non-zero if
# any check fails. Each check prints a [PASS]/[FAIL] line.
FAILS=0
pass_fail() { # $1=ok(0/1) $2=desc
  if [ "$1" -eq 0 ]; then echo "    [PASS] $2"; else echo "    [FAIL] $2"; FAILS=$((FAILS + 1)); fi
}
assert_grep()   { grep -qx "$2" "$1" 2>/dev/null; pass_fail $? "$3"; }         # file must contain exact line
assert_nogrep() { if grep -q "$2" "$1" 2>/dev/null; then pass_fail 1 "$3"; else pass_fail 0 "$3"; fi; }  # file must NOT contain
assert_absent() { if [ -f "$1" ]; then pass_fail 1 "$2"; else pass_fail 0 "$2"; fi; }  # fake gh must NOT have run
assert_present(){ if [ -f "$1" ]; then pass_fail 0 "$2"; else pass_fail 1 "$2"; fi; }  # fake gh must have run
assert_eq()     { if [ "$1" = "$2" ]; then pass_fail 0 "$3"; else pass_fail 1 "$3 (got $1, want $2)"; fi; }

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
assert_present "$ENVOUT" "fake gh ran (deny leg execs gh with a placeholder)"
assert_grep    "$ENVOUT" "GH_TOKEN=rein-placeholder-denied" "GH_TOKEN is the fail-closed placeholder"
assert_grep    "$ENVOUT" "GITHUB_TOKEN=<unset>" "GITHUB_TOKEN stripped (not the ambient value)"
assert_nogrep  "$ENVOUT" "$FAKE_PAT" "the ambient PAT reached gh NOWHERE (no leak)"

# --- (b) HUMAN gh auth login still works ----------------------------------
rule; echo "  (b) HUMAN recovery — \`gh auth login\` (no REIN_RUN_ID -> local tier)"; rule
echo "  \$ GH_TOKEN=$FAKE_PAT  rein-gh auth login"
clear_dump
env GH_TOKEN="$FAKE_PAT" "$BIN" auth login >/tmp/demo57_authlogin.out 2>&1
hcode=$?
echo "  rein-gh exit: $hcode"
sed 's/^/    /' /tmp/demo57_authlogin.out
echo "  env the real gh received:"
dump
echo "  ^ gh RAN with NO GH_TOKEN at all (not even the placeholder — gh 2.67"
echo "    refuses \`auth login\` if GH_TOKEN is set). The ambient PAT is still"
echo "    stripped, so the human's login flow is unblocked but nothing leaks."
assert_eq      "$hcode" "0" "rein-gh exits 0 (human's auth login is not blocked)"
assert_present "$ENVOUT" "fake gh ran (local tier execs gh for the human)"
assert_grep    "$ENVOUT" "GH_TOKEN=<unset>" "no GH_TOKEN at all (so gh auth login is not refused by gh)"
assert_nogrep  "$ENVOUT" "$FAKE_PAT" "the ambient PAT reached gh NOWHERE (no leak)"

# --- (c) AGENT gh auth is refused -----------------------------------------
rule; echo "  (c) AGENT — \`gh auth ...\` REFUSED inside a rein run (REIN_RUN_ID set)"; rule
for sub in "auth login" "auth token"; do
  echo "  \$ REIN_RUN_ID=demo-run  rein-gh $sub"
  clear_dump
  env REIN_RUN_ID=demo-run GH_TOKEN="$FAKE_PAT" "$BIN" $sub >/tmp/demo57_agentauth.out 2>&1
  acode=$?
  echo "  rein-gh exit: $acode  (want non-zero: refused)"
  sed 's/^/    /' /tmp/demo57_agentauth.out
  if [ "$acode" -ne 0 ]; then pass_fail 0 "rein-gh refused \`$sub\` (non-zero exit)"; else pass_fail 1 "rein-gh refused \`$sub\` (non-zero exit)"; fi
  assert_absent "$ENVOUT" "fake gh did NOT run for \`$sub\` (refused before exec)"
  echo
done

rule
if [ "$FAILS" -eq 0 ]; then
  echo "  DONE — ALL CHECKS PASS: (a) PAT stripped on write, (b) human auth unblocked, (c) agent auth refused."
else
  echo "  FAILED — $FAILS check(s) failed. #57 does NOT hold as demonstrated."
fi
rule
exit $FAILS
