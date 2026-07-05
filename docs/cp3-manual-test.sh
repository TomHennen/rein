#!/usr/bin/env bash
# CP3 manual test — the WRITE path through the sandbox.
#
# WHY THIS IS MANUAL: the write-approval prompt reads the issue number via
# /dev/tty (internal/ui/grant), so there is NO non-interactive grant path — the
# push path can only be verified by a human at a real terminal. The READ path
# (clone, gh api, ls-remote) is already verified live and headless by the agent;
# this script closes the remaining push path.
#
# Run this in your REAL terminal (not through a pipe/tmux-less context — the
# approval prompt needs a tty). It uses the THROWAWAY repo only (hard-constraint
# #1). It clones the throwaway into a scratch working tree, makes a trivial
# commit, and pushes to a DISPOSABLE branch through `rein run --sandbox`.
set -euo pipefail

cd "$(dirname "$0")/../.." 2>/dev/null || true
REPO_ROOT="/mnt/dev/dev/rein/.claude/worktrees/modular-bubbling-hippo"
cd "$REPO_ROOT"

# shellcheck disable=SC1091
source ./dev-env

: "${REIN_TEST_REPO_A:?set REIN_TEST_REPO_A=<owner>/<throwaway> (dev-env should)}"

echo "=== CP3 write-path manual test ==="
echo "throwaway repo: $REIN_TEST_REPO_A"
echo

# 1) Ensure the session binds an issue, or the sandbox will DENY writes by
#    design (reads still flow). Point REIN_SESSION_FILE at a session with an
#    `issue:` field. The default dev-session.yaml (id=sess_cp6_phase0) binds
#    issue #1 — good enough. Confirm:
./bin/rein doctor 2>&1 | grep -E "session:|sandbox:" || true
echo

# 2) Scratch working tree OUTSIDE the repo (becomes srt's allowWrite mount).
WORK=$(mktemp -d /tmp/cp3-write-work.XXXXXX)
export REIN_SANDBOX_WORKDIR="$WORK"
BRANCH="cp3-manual-$(date +%s)"
echo "working tree: $WORK"
echo "disposable branch: $BRANCH"
echo

# 3) Everything below runs INSIDE the sandbox. git clone (CDN passthrough),
#    commit, then `git push` — which trips the write-approval prompt on THIS
#    terminal (rein hosts the broker OUT of the sandbox). When prompted:
#      - answer the issue-number confirmation on the tty, OR
#      - from ANOTHER terminal run: ./bin/rein approval grant --run-id <id>
#        (the run-id is printed in the 'write blocked' message).
echo ">>> When the push prompts for approval, approve it on this terminal. <<<"
echo
./bin/rein run --sandbox -- bash -c '
  set -e
  cd "$0"
  git clone --depth 1 https://github.com/'"$REIN_TEST_REPO_A"' repo
  cd repo
  git config user.email cp3-test@example.com
  git config user.name  "cp3 manual test"
  echo "cp3 write-path probe $(date -u +%FT%TZ)" >> cp3-write-probe.txt
  git add cp3-write-probe.txt
  git commit -m "cp3: write-path manual test"
  git push origin HEAD:refs/heads/'"$BRANCH"'
' "$WORK"

echo
echo "=== PUSH SUCCEEDED ==="
echo "Verify the branch exists, then delete it to keep the throwaway clean:"
echo "  git ls-remote https://github.com/$REIN_TEST_REPO_A $BRANCH"
echo "  gh api -X DELETE repos/$REIN_TEST_REPO_A/git/refs/heads/$BRANCH   # after sourcing dev-env"
echo
echo "Also confirm on GitHub that the commit shows the rein App as the pusher"
echo "(the agent never held a token — the write token was minted + injected at"
echo "the proxy, and revoked on rein run exit; see the 'revoked N write token'"
echo "line above)."
