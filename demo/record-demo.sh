#!/usr/bin/env bash
# record-demo.sh — record the rein "credentials joke" demo as a GIF.
#
# A REAL `claude` session under rein: the agent writes a one-line joke about
# credentials, tries to push, hits the write-approval TMUX POPUP, YOU approve it
# on your terminal, and it pushes + opens a PR. You drive the popup yourself —
# that IS the demo (a human approves the agent's writes).
#
# Produces demo/creds-joke.cast (asciicast) and demo/creds-joke.gif.
#
# Deps (all no-browser, no-ffmpeg):
#   asciinema  — records the terminal      (macOS: brew install asciinema)
#   agg        — renders asciicast -> gif   (macOS: brew install agg)
#   tmux, gh   — the popup surface + issue creation
#   rein       — configured via `rein init`, with a dev-session naming a THROWAWAY repo
#
# Why asciinema+agg (not vhs): vhs renders via headless Chromium, which has no
# linux-arm64 build — agg rasterizes directly from a font, so it works everywhere.
#
# Usage:  ./demo/record-demo.sh            # resolves the repo from your dev-session
#         REIN_DEMO_REPO=you/throwaway ./demo/record-demo.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cast="$here/creds-joke.cast"
gif="$here/creds-joke.gif"

need() { command -v "$1" >/dev/null 2>&1 || { echo "MISSING: $1 — $2" >&2; exit 1; }; }
need asciinema "brew install asciinema  (or pipx install asciinema)"
need agg        "brew install agg  (asciinema gif generator; or: cargo install --git https://github.com/asciinema/agg)"
need tmux       "brew install tmux / apt install tmux"
need gh         "the GitHub CLI (authenticated as YOU — used only to create the demo issue)"
need rein       "build rein and run 'rein init' first"

# --- resolve a THROWAWAY repo (never a real one) ------------------------------
repo="${REIN_DEMO_REPO:-}"
if [ -z "$repo" ]; then
  sess="${HOME}/.config/rein/dev-session.yaml"
  [ -f "$sess" ] && repo="$(awk '/^repos:/{f=1;next} f&&/^[[:space:]]*-/{gsub(/[[:space:]-]/,"");print;exit}' "$sess")"
fi
[ -n "$repo" ] || { echo "No repo. Set REIN_DEMO_REPO=owner/name (a THROWAWAY), or configure a dev-session." >&2; exit 1; }
echo "==> demo repo (throwaway): $repo"

# --- a fresh demo issue (declare fetches a REAL issue) ------------------------
issue="$(gh issue create --repo "$repo" \
  --title "add a credentials joke to the repo" \
  --body "Demo issue for the rein README recording. Safe to close." \
  | grep -oE '[0-9]+$')"
echo "==> created issue #$issue"

# --- a scratch clone + a repo-only session (no issue: line) -------------------
work="$(mktemp -d)/repo"
git clone --depth 1 "https://github.com/${repo}.git" "$work" >/dev/null 2>&1
sess="$work/.demo-session.yaml"
printf 'id: sess_demo\nrole: implement\nrepos:\n  - %s\n' "$repo" > "$sess"

prompt="Please do exactly these steps in order: \
1) write a short one-line joke about credentials to a file named jokes.md; \
2) commit it with git; \
3) run 'rein declare ${issue}' to declare issue ${issue}; \
4) push the commit to a branch named agent/${issue}/joke; \
5) open a pull request with 'gh pr create --fill'."

cat <<EOF

==> When the recording starts you'll be inside tmux running \`rein run -- claude\`.
    Claude will work through the steps; when the WRITE-APPROVAL POPUP appears,
    type  ${issue}  and press Enter to approve. When claude is done, type \`exit\`
    to leave tmux and stop recording — the GIF renders automatically.

    (Press Enter to begin.)
EOF
read -r _

# asciinema records the pty; tmux (so rein fires the popup) runs the agent inside
# it. The popup is drawn to this same pty, so it IS captured. `exec bash` leaves
# you a shell after claude exits so you can type \`exit\` to end the take.
REIN_SESSION_FILE="$sess" asciinema rec "$cast" --overwrite --cols 104 --rows 32 \
  -c "tmux -L reindemo new-session 'cd \"$work\" && rein run -- claude \"$prompt\"; echo; echo [claude exited — type: exit to end]; exec bash'"

echo "==> rendering gif ..."
agg --font-size 18 --idle-time-limit 2 "$cast" "$gif"
echo "==> done:"
echo "    cast: $cast"
echo "    gif:  $gif"
echo "    (issue #$issue and its PR are on $repo — close/delete them when finished.)"
