# rein demo recording

`record-demo.sh` records a **real `claude` session under rein** and renders it to
a GIF for the README: the agent writes a one-line joke about credentials, tries to
push, hits the **write-approval tmux popup**, **you approve it**, and it pushes and
opens a PR. You drive the popup yourself — a human approving the agent's writes IS
the demo.

## Run it

```sh
# deps (macOS): brew install asciinema agg tmux
# rein must be configured (`rein init`) with a dev-session naming a THROWAWAY repo
./demo/record-demo.sh
# or: REIN_DEMO_REPO=you/throwaway ./demo/record-demo.sh
```

It creates a fresh demo issue, drops you into `rein run -- claude` inside a
dedicated tmux server, and records. When the popup appears, type the issue number
+ Enter to approve; when claude is done, type `exit` to stop. The GIF renders to
`demo/creds-joke.gif` automatically.

## Why this toolchain

- **asciinema → agg**, not vhs/terminalizer: those render via headless Chromium,
  which has **no linux-arm64 build** (Google publishes no arm64 Chromium
  snapshots), so they can't render on an Apple-silicon Linux VM. `agg` rasterizes
  straight from a font — no browser, no ffmpeg — so it works everywhere.
- **The tmux popup IS captured.** A `tmux display-popup` is drawn by the tmux
  *client* to its terminal; recording that client's pty (which is what asciinema
  does when it wraps `tmux`) captures the overlay. Verified: firing a popup while
  recording the client pty puts the popup text in the stream and renders it in the
  gif. (This is the same reason rein's own tmux-popup journey can drive the popup
  by attaching a pexpect client.)
- **Dedicated tmux socket** (`-L reindemo`): never touches your real tmux server,
  and avoids nested-session weirdness if you're already in tmux.

## Fully-automated variant (harness is in `main`)

`record-demo.sh` is human-driven (you approve the popup live). To drive it
*without* a human — for a re-runnable capture — reuse the proven tmux-popup
harness (#88, now merged): `reinharness.tmux_popup_session()` stands up the
dedicated-socket tmux + attached client, and `drive_popup(pattern, answer)` waits
for the Form A and sends the approval. Run `rein run -- claude "<prompt>"` **inside
that session** (so claude's TUI and the popup share one client pty and both get
captured), record the client pty with asciicast timestamps, and render with `agg`.
The one non-deterministic part is real claude itself — re-run until you get a
clean take. `record_demo.py` (alongside this file) is that automated recorder.
