#!/usr/bin/env bash
# CP4 manual test — git AUTHOR IDENTITY + default-mode on the write path.
#
# WHY MANUAL: the write-approval prompt reads the issue number via /dev/tty
# (internal/ui/grant), so the push path can only be driven by a human at a real
# terminal. This script closes the one CP4 claim that unit tests CANNOT prove:
# that a sandboxed commit authors as rein's non-impersonating identity (not the
# developer) AND that GitHub attributes it to the rein App via the bot noreply
# email. It also exercises the CP4 default-mode flip (`rein run` = sandboxed).
#
# Run in your REAL terminal. THROWAWAY repo only (hard-constraint #1). Pushes to
# a DISPOSABLE branch you can delete after.
set -euo pipefail

REPO_ROOT="/mnt/dev/dev/rein/.claude/worktrees/modular-bubbling-hippo"
cd "$REPO_ROOT"
# shellcheck disable=SC1091
source ./dev-env
: "${REIN_TEST_REPO_A:?set REIN_TEST_REPO_A=<owner>/<throwaway>}"

go build -o bin/rein ./cmd/rein
go build -o bin/rein-git ./cmd/rein-git
go build -o bin/rein-gh ./cmd/rein-gh
./bin/rein install-shim >/dev/null

echo "=== CP4 write-path + identity manual test ==="
echo "throwaway repo: $REIN_TEST_REPO_A"
echo "HOST git identity (what the agent MUST NOT author as):"
echo "  name=$(git config --get user.name)  email=$(git config --get user.email)"
echo

# Scratch working tree OUTSIDE the repo (becomes srt's allowWrite mount).
WORK=$(mktemp -d /tmp/cp4-write-work.XXXXXX)
export REIN_SANDBOX_WORKDIR="$WORK"
BRANCH="cp4-manual-$(date +%s)"
echo "working tree: $WORK"
echo "disposable branch: $BRANCH"
echo

# NOTE: this uses `rein run` with NO mode flag — CP4 makes that SANDBOXED by
# default. (Direct/unsandboxed now requires `rein run --direct`.)
#
# The in-sandbox script deliberately does NOT set `git config user.*`: we are
# verifying that rein's GIT_AUTHOR_*/GIT_COMMITTER_* env identity is what lands.
# It also proves the leak fixes: `git config user.name` shows rein's identity
# (not the developer's), and `cat ~/.gitconfig` FAILS (deny-read).
echo ">>> When the push prompts for approval, approve it on THIS terminal. <<<"
echo
./bin/rein run -- bash -c '
  set -e
  cd "$0"
  echo "--- in-sandbox identity checks ---"
  echo "git config user.name  = $(git config user.name  || echo UNSET)"
  echo "git config user.email = $(git config user.email || echo UNSET)"
  echo "GIT_AUTHOR_NAME       = ${GIT_AUTHOR_NAME:-UNSET}"
  echo "GIT_AUTHOR_EMAIL      = ${GIT_AUTHOR_EMAIL:-UNSET}"
  echo -n "cat ~/.gitconfig      = "; (cat ~/.gitconfig >/dev/null 2>&1 && echo "READABLE (BUG!)" || echo "hidden (deny-read OK)")
  echo "-----------------------------------"
  git clone --depth 1 https://github.com/'"$REIN_TEST_REPO_A"' repo
  cd repo
  echo "cp4 identity probe $(date -u +%FT%TZ)" >> cp4-identity-probe.txt
  git add cp4-identity-probe.txt
  git commit -m "cp4: identity probe"
  echo "LOCAL commit author:    $(git log -1 --format="%an <%ae>")"
  echo "LOCAL commit committer: $(git log -1 --format="%cn <%ce>")"
  git push origin HEAD:refs/heads/'"$BRANCH"'
' "$WORK"

echo
echo "=== VERIFY (outside the sandbox) ==="
echo "Fetching the pushed commit's author/committer from GitHub…"
SHA=$(gh api "repos/$REIN_TEST_REPO_A/commits/$BRANCH" --jq .sha)
echo "commit: $SHA"
gh api "repos/$REIN_TEST_REPO_A/commits/$BRANCH" --jq '
  "author.name    = \(.commit.author.name)",
  "author.email   = \(.commit.author.email)",
  "committer.name = \(.commit.committer.name)",
  "committer.email= \(.commit.committer.email)",
  "GH-attributed author login = \(.author.login // "<none>")",
  "GH-attributed author type   = \(.author.type  // "<none>")"'
echo
echo "PASS criteria:"
echo "  - author.email is <botID>+<slug>[bot]@users.noreply.github.com (NOT $(git config --get user.email))"
echo "  - author.name  is \"$(git config --get user.name) (via rein)\" (or your REIN_GIT_AUTHOR_TEMPLATE)"
echo "  - GH-attributed author login is <slug>[bot], type Bot (GitHub linked it to the App)"
echo "  - the in-sandbox 'cat ~/.gitconfig' printed 'hidden (deny-read OK)'"
echo
echo "Cleanup: gh api -X DELETE repos/$REIN_TEST_REPO_A/git/refs/heads/$BRANCH ; rm -rf $WORK"
