#!/usr/bin/env bash
# Issue #35 manual live gate — agent-declared + human-confirmed issue scoping.
#
# WHY MANUAL: the Form A confirmation reads the issue number via /dev/tty
# (internal/ui/grant), and the one claim unit tests CANNOT prove is that REAL
# git accepts the proxy's synthesized wire responses:
#   (a) the pre-declaration ERR advertisement -> git prints
#       `fatal: remote error: rein: writes are locked ...` and exits cleanly
#       (the §5.3 live-gate item: verify git remote-curl accepts the ERR pkt);
#   (b) the post-approval report-status `ng` -> git prints
#       `! [remote rejected] <ref> (<reason>)`.
#
# Run in your REAL terminal. THROWAWAY repo only (hard-constraint #1). Pushes
# only to DISPOSABLE agent/<n>/... branches you can delete after.
#
# PREREQ: a real (open) issue on the throwaway. Create one first, e.g.:
#   gh issue create -R "$REIN_TEST_REPO_A" -t "Wire up the widget carousel" -b "manual test"
# and pass its number:  docs/35-manual-test.sh <issue-number>
set -euo pipefail

ISSUE="${1:?usage: docs/35-manual-test.sh <real-issue-number-on-throwaway>}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
# shellcheck disable=SC1091
source ./dev-env
: "${REIN_TEST_REPO_A:?set REIN_TEST_REPO_A=<owner>/<throwaway>}"

go build -o bin/ ./...
./bin/rein install-shim >/dev/null

WORK=$(mktemp -d /tmp/35-manual-work.XXXXXX)
export REIN_SANDBOX_WORKDIR="$WORK"
NONCE="m$(date +%s)"
GOOD_BRANCH="agent/${ISSUE}/${NONCE}"
BAD_BRANCH="not-the-convention-${NONCE}"

echo "=== #35 manual live gate ==="
echo "throwaway:   $REIN_TEST_REPO_A"
echo "real issue:  #$ISSUE (must exist and be open)"
echo "good branch: $GOOD_BRANCH   bad branch: $BAD_BRANCH"
echo
echo "You will be prompted TWICE on this terminal:"
echo "  1. a declare for a BOGUS issue (should FAIL BEFORE any prompt — 404)"
echo "  2. the Form A prompt for #$ISSUE — CHECK IT SHOWS THE REAL TITLE + REPO,"
echo "     then type the displayed number to approve."
echo

./bin/rein run -- bash -c '
  set +e
  cd "$0"
  ISSUE="$1"; GOOD_BRANCH="$2"; BAD_BRANCH="$3"; REPO="$4"

  echo "=== [1] reads flow pre-declaration ==="
  # Clone via the .git URL — GitHub's DEFAULT clone shape (what `gh repo
  # clone` and the web UI hand you), so the live gate exercises the repo
  # string the proxy actually derives from a real remote ("o/r.git"), not
  # the tidy bare form. (Security review round 2, HIGH-1/HIGH-2c: a raw
  # comparison in the cross-check broke exactly this default case.)
  git clone --depth 1 "https://github.com/$REPO.git" repo || { echo "CLONE FAILED (BUG)"; exit 3; }
  cd repo
  echo "remote: $(git remote get-url origin)   (must end in .git)"

  echo
  echo "=== [2] pre-declaration push -> expect: fatal: remote error: rein: writes are locked ... ==="
  echo probe > probe.txt; git add -A; git commit -qm "35 manual probe"
  git push origin "HEAD:refs/heads/$GOOD_BRANCH"
  echo "push rc=$? (expect nonzero, clean exit, NO prompt fired)"

  echo
  echo "=== [3] pre-declaration gh write -> expect 403 naming rein declare ==="
  gh issue comment "$ISSUE" -R "$REPO" -b "should be blocked" 2>&1 | head -3
  echo "(expect: HTTP 403 ... rein: no issue declared for this run)"

  echo
  echo "=== [4] declare a BOGUS issue -> expect: not found, NO prompt ==="
  rein declare 999999
  echo "bogus declare rc=$? (expect nonzero)"

  echo
  echo "=== [5] declare the REAL issue -> Form A prompt on YOUR terminal ==="
  echo ">>> CHECK: the prompt shows the issue TITLE, [open], and the repo. <<<"
  rein declare "$ISSUE"
  echo "declare rc=$? (expect 0 after you approve)"

  echo
  echo "=== [6] non-convention ref -> expect: ! [remote rejected] ... agent/<issue>/<nonce> ==="
  git push origin "HEAD:refs/heads/$BAD_BRANCH"
  echo "bad-ref push rc=$? (expect nonzero, remote rejected, NO second prompt)"

  echo
  echo "=== [7] wrong-issue ref -> expect: rejected, names rein declare ==="
  git push origin "HEAD:refs/heads/agent/999999/x"
  echo "wrong-issue push rc=$? (expect nonzero)"

  echo
  echo "=== [8] verified push -> expect success, NO second prompt ==="
  git push origin "HEAD:refs/heads/$GOOD_BRANCH"
  echo "good push rc=$? (expect 0)"

  echo
  echo "=== [9] post-approval gh write -> expect it to PASS REIN'S GATE ==="
  echo "(the deny message must be GONE; GitHub itself may still 403 the comment"
  echo " because the sandboxed write mint is contents:write only — issues:write"
  echo " is the deferred role catalog, NOT a #35 gate failure)"
  gh issue comment "$ISSUE" -R "$REPO" -b "rein #35 manual test: declared + confirmed write ($GOOD_BRANCH)" 2>&1 | head -3
  echo "gh comment rc=$? (PASS = no 'rein: no issue declared' message)"
' "$WORK" "$ISSUE" "$GOOD_BRANCH" "$BAD_BRANCH" "$REIN_TEST_REPO_A"

echo
echo "=== [10] DIRECT-mode leg (same model, no ref check — §2 deltas) ==="
echo "Expect: push blocked with the declare hint; declare prompts HERE; retry succeeds."
DIRECT_WORK=$(mktemp -d /tmp/35-manual-direct.XXXXXX)
./bin/rein run --direct -- bash -c '
  set +e
  cd "$0"
  ISSUE="$1"; REPO="$2"; NONCE="$3"
  git clone --depth 1 "https://github.com/$REPO.git" repo && cd repo
  echo probe-direct > probe.txt; git add -A; git commit -qm "35 direct probe"
  echo "=== direct pre-declaration push -> expect failure + stderr hint naming rein declare ==="
  git push origin "HEAD:refs/heads/agent/$ISSUE/direct-$NONCE"
  echo "push rc=$? (expect nonzero)"
  echo "=== direct declare -> prompt on YOUR terminal ==="
  rein declare "$ISSUE"
  echo "declare rc=$? (expect 0 after approve)"
  echo "=== direct retry -> expect success ==="
  git push origin "HEAD:refs/heads/agent/$ISSUE/direct-$NONCE"
  echo "push rc=$? (expect 0)"
' "$DIRECT_WORK" "$ISSUE" "$REIN_TEST_REPO_A" "$NONCE"

echo
echo "=== verify + cleanup (host-side, your own gh auth) ==="
gh api "repos/$REIN_TEST_REPO_A/git/refs/heads/$GOOD_BRANCH" >/dev/null && echo "OK: $GOOD_BRANCH exists"
gh api "repos/$REIN_TEST_REPO_A/git/refs/heads/$BAD_BRANCH" >/dev/null 2>&1 && echo "BUG: $BAD_BRANCH exists (convention deny failed!)" || echo "OK: $BAD_BRANCH absent"
echo "cleanup:"
gh api -X DELETE "repos/$REIN_TEST_REPO_A/git/refs/heads/$GOOD_BRANCH" && echo "  deleted $GOOD_BRANCH"
gh api -X DELETE "repos/$REIN_TEST_REPO_A/git/refs/heads/agent/$ISSUE/direct-$NONCE" 2>/dev/null && echo "  deleted direct branch" || true
rm -rf "$WORK" "$DIRECT_WORK"
echo
echo "=== PASS CRITERIA ==="
echo "  [2] clean 'fatal: remote error: rein: writes are locked' (ERR-pkt accepted by git)"
echo "  [3] 403 naming rein declare;  [4] bogus declare 404s with no prompt"
echo "  [5] Form A shows the REAL fetched title + repo"
echo "  [6]/[7] '! [remote rejected]' with instructive reasons; no second prompt"
echo "  [8] verified push succeeds OVER THE .git REMOTE (the default clone shape);"
echo "      [9] gh write passes rein's gate after ONE ceremony"
echo "  [10] direct mode: same declare model (no ref check — documented delta)"
