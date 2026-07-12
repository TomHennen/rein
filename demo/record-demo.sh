#!/usr/bin/env bash
# record-demo.sh — record the rein "credentials joke" demo as a GIF, IN THIS VM.
#
# A real `claude` session under rein: the agent writes a joke about credentials,
# tries to push, hits the write-approval TMUX POPUP, YOU approve it, and it pushes
# and opens a PR. You drive it at a real terminal — which is exactly what makes it
# work (an automated headless capture trips over claude's trust dialog and a
# tty-detach when srt starts; a real terminal has neither).
#
# rein is LINUX-ONLY, so this runs here, not on a Mac. ZERO installs: it uses
# script(1) + agg (both already present) — NOT asciinema/vhs (vhs needs headless
# Chromium, which has no linux-arm64 build).
#
# Deps: script (util-linux), tmux, agg, gh, and rein configured (`rein init`)
# with a dev-session naming a THROWAWAY repo.  Usage: ./demo/record-demo.sh
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
raw="$here/.creds-joke.raw"; timing="$here/.creds-joke.timing"
cast="$here/creds-joke.cast"; gif="$here/creds-joke.gif"
COLS=104; ROWS=32

need(){ command -v "$1" >/dev/null 2>&1 || { echo "MISSING: $1 — $2" >&2; exit 1; }; }
need script "util-linux (already on Linux)"
need tmux   "apt install tmux"
need agg    "asciinema gif renderer — prebuilt binary at github.com/asciinema/agg/releases (no browser needed)"
need gh     "GitHub CLI, authed as you (creates the demo issue)"
need rein   "build rein and run 'rein init'"

repo="${REIN_DEMO_REPO:-}"
if [ -z "$repo" ]; then
  s="${HOME}/.config/rein/dev-session.yaml"
  [ -f "$s" ] && repo="$(awk '/^repos:/{f=1;next} f&&/^[[:space:]]*-/{sub(/^[[:space:]]*-[[:space:]]*/,"");gsub(/[[:space:]]/,"");print;exit}' "$s")"
fi
[ -n "$repo" ] || { echo "Set REIN_DEMO_REPO=owner/name (a THROWAWAY)." >&2; exit 1; }

issue="$(gh issue create --repo "$repo" --title "add a credentials joke to the repo" \
  --body "Demo issue for the rein README recording. Safe to close." | grep -oE '[0-9]+$')"
echo "==> repo=$repo  issue=#$issue"

work="$(mktemp -d)/repo"
git clone --depth 1 "https://github.com/${repo}.git" "$work" >/dev/null 2>&1
printf 'id: sess_demo\nrole: implement\nrepos:\n  - %s\n' "$repo" > "$work/.demo-session.yaml"
prompt="Please do exactly these steps in order: \
1) write a short one-line joke about credentials to a file named jokes.md; \
2) commit it with git; \
3) run 'rein declare ${issue}' to declare issue ${issue}; \
4) push the commit to a branch named agent/${issue}/joke; \
5) open a pull request with 'gh pr create --fill'."
cat > "$work/run-agent.sh" <<AGENT
#!/usr/bin/env bash
cd "$work"
REIN_SESSION_FILE="$work/.demo-session.yaml" rein run -- claude "$prompt"
AGENT
chmod +x "$work/run-agent.sh"

cat <<EOF

==> Recording starts on Enter. You'll be in tmux running \`rein run -- claude\`.
    - If claude shows "Is this a project you trust?", press  1  then Enter.
    - When the WRITE-APPROVAL POPUP appears, type  ${issue}  and press Enter.
    - When claude finishes, type  exit  to end the take. The GIF renders itself.

    (Press Enter to begin.)
EOF
read -r _

tmux -L reindemo kill-server 2>/dev/null || true
script -q -O "$raw" -T "$timing" \
  -c "tmux -L reindemo new-session 'bash \"$work/run-agent.sh\"; echo; echo [claude exited — type: exit to end]; exec bash'"
tmux -L reindemo kill-server 2>/dev/null || true

echo "==> converting + rendering ..."
python3 "$here/script2cast.py" "$raw" "$timing" "$cast" "$COLS" "$ROWS"
agg --font-size 18 --idle-time-limit 2 "$cast" "$gif"
rm -f "$raw" "$timing"; rm -rf "$(dirname "$work")"
echo "==> done:"
echo "    cast: $cast"
echo "    gif:  $gif"
echo "    (issue #$issue + its PR are on $repo — close/delete when finished.)"
