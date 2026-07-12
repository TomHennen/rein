# rein demo recording

Records a **real `claude` session under rein** and renders it to a GIF for the
README: the agent writes a one-line joke about credentials, tries to push, hits
the **write-approval tmux popup**, **you approve it**, and it pushes + opens a PR.
A human approving the agent's writes IS the demo.

## Run it (in this Linux VM — rein is Linux-only)

`record-demo.sh` is **zero-install**: it uses `script(1)` (util-linux) + `agg`,
both already present. You drive the approval at a real terminal — which is what
makes it reliable (see "Why not automated?" below).

```sh
./demo/record-demo.sh
# or:  REIN_DEMO_REPO=you/throwaway ./demo/record-demo.sh
```

It creates a fresh demo issue, drops you into `rein run -- claude` inside a
dedicated tmux server, and records with `script(1)`. During the take:

- if claude asks **"Is this a project you trust?"**, press `1` then Enter;
- when the **popup** appears, type the issue number + Enter to approve;
- when claude is done, type `exit` to end — the GIF renders to
  `demo/creds-joke.gif` automatically (`script(1)` → `script2cast.py` → `agg`).

## Why this toolchain

- **`agg`, not vhs/terminalizer.** Those render via headless Chromium, which has
  **no linux-arm64 build** (Google publishes no arm64 Chromium snapshots) — so
  they cannot render on an Apple-silicon Linux VM (e.g. Tart). `agg` rasterizes
  straight from a font: no browser, no ffmpeg.
- **`script(1)`, not asciinema.** No pip on the VM, and none needed — util-linux
  `script` records the pty; `script2cast.py` converts its `--log-out`/`--log-timing`
  to asciicast v2 for `agg`.
- **The tmux popup IS captured.** A `tmux display-popup` is drawn by the tmux
  *client* to its terminal; recording that client's pty (what `script` wraps)
  captures the overlay. Verified: firing a popup while recording puts its text in
  the stream and renders it in the gif.
- **Dedicated tmux socket** (`-L reindemo`): never touches your real tmux server.

## Why not automated? (`record_demo.py`)

`record_demo.py` is an automated variant (pexpect + the #88 tmux-popup harness,
answers the popup itself). It launches the real flow correctly but does **not**
finish a clean take in a *headless* VM: (1) claude's folder-trust dialog blocks a
fresh scratch dir, and (2) the pexpect-owned tmux client hits EOF the moment srt
starts its `--new-session` sandbox, truncating the capture at the banner. Both are
terminal-control edges a **real attached terminal doesn't have** — which is why the
runnable path (`record-demo.sh`) is human-driven. It's kept as the starting point
for a future fully-automated take, and it's a concrete motivator for #100 (assert
on a rendered screen via pyte/`capture-pane` instead of scraping the pty).
