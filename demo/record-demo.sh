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
# Match the asciicast grid to YOUR ACTUAL terminal, so a smaller window => a
# smaller gif. Override with REIN_DEMO_COLS/ROWS; falls back to 104x32.
sz="$(stty size </dev/tty 2>/dev/null || true)"   # "rows cols"
ROWS="${REIN_DEMO_ROWS:-${sz%% *}}"; COLS="${REIN_DEMO_COLS:-${sz##* }}"
case "$ROWS" in ''|*[!0-9]*) ROWS=32;; esac
case "$COLS" in ''|*[!0-9]*) COLS=104;; esac

need(){ command -v "$1" >/dev/null 2>&1 || { echo "MISSING: $1 — $2" >&2; exit 1; }; }
need script "util-linux (already on Linux)"
need tmux   "apt install tmux"
need agg    "asciinema gif renderer — prebuilt binary at github.com/asciinema/agg/releases (no browser needed)"
need gh     "GitHub CLI, authed as you (creates the demo issue)"
need go     "to build a fresh rein"

# Build a FRESH rein from THIS checkout — never trust a possibly-stale installed
# binary. A pre-#91 rein injects a contents-only token, so `git push` lands but
# `gh pr create` 403s on pull_requests:write ("Resource not accessible by
# integration"). rein init (App + dev-session) must already be done.
root="$(cd "$here/.." && pwd)"
echo "==> building rein from $(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo local) ..."
( cd "$root" && go build -o bin/rein ./cmd/rein )
REIN="$root/bin/rein"
[ -f "${HOME}/.config/rein/state.json" ] || { echo "rein not configured — run 'rein init' first." >&2; exit 1; }

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
# Let claude FIGURE OUT the declare itself — it'll try to push, hit rein's
# "writes are locked until you declare" gate, and work it out. Don't spell it out.
prompt="Add a short one-line joke about credentials to a new file jokes.md, then commit it, push it, and open a pull request. This work is for issue ${issue}."
cat > "$work/run-agent.sh" <<AGENT
#!/usr/bin/env bash
cd "$work"
# --dangerously-skip-permissions ("yolo"): the sandbox + rein's declare gate ARE
# the guardrails, so claude doesn't need its own per-tool permission prompts —
# and this is the whole point of the demo. It also skips the folder-trust dialog.
REIN_SESSION_FILE="$work/.demo-session.yaml" "$REIN" run -- claude --dangerously-skip-permissions "$prompt"
AGENT
chmod +x "$work/run-agent.sh"

cat <<EOF

==> Recording starts on Enter. You'll be in tmux running \`rein run -- claude\`
    in yolo mode (no claude permission prompts — the sandbox is the guardrail).
    - claude will write the joke and try to push; when it hits the write gate and
      declares, the WRITE-APPROVAL POPUP appears — type  ${issue}  and press Enter.
    - When claude finishes, type  exit  to end the take. The GIF renders itself.

    Recording at ${COLS}x${ROWS} (your terminal). A smaller window => a smaller gif.
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
