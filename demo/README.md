# demo/ — the recorded README demo (#99)

`creds-joke.gif` is one **real** take: a real `claude`, sandboxed by `rein`, is asked to
add a joke about credentials to `jokes.md`. It reads rein's injected contract, sees that
writes are locked, and runs `rein declare 202` — which **blocks**. Approval routes to
the **tmux popup**: Form A, overlaid on the agent's live TUI, on the terminal the
*developer* owns. The sandboxed agent has no tty and cannot answer it. A human types the
issue number; only then is a write-tier token minted. The agent pushes
`agent/202/creds-joke`, opens PR #203, and on `/exit` rein prints
`rein: revoked 1 of 1 write token(s) on exit`.

Nothing in it is staged. It is the product, running.

| file | what it is |
|---|---|
| `creds-joke.cast` | the raw take, [asciicast v2](https://docs.asciinema.org/manual/asciicast/v2/) — the source of truth |
| `creds-joke.gif` | what the cast renders to (`agg`) |
| `record_demo.py` | records a fresh take, then renders |

## Re-record

```sh
python3 demo/record_demo.py                      # record a new take + render
python3 demo/record_demo.py --render-only        # re-render the EXISTING cast (free)
python3 demo/record_demo.py --no-render          # record only
REIN_DEMO_ISSUE=123 python3 demo/record_demo.py  # reuse an issue instead of creating one
```

**Iterate on the render, not on the recording.** Recording drives a live LLM: it costs
minutes and real API tokens, and a real agent phrases things differently every time.
Rendering is free and local, so tune `agg`'s knobs against the cast you already have
(`--render-only`) and only re-record if you want a *different take*. Re-running is safe:
the script creates its own throwaway issue and, in a `finally`, closes the PR, deletes
the branch the agent chose, closes the issue and removes the checkout — throwaway repo
only (hard-constraint #1).

### Prereqs

- **`tmux`** — rein's approval popup is a tmux popup; the demo runs in a real pane.
- **`claude`** on `PATH` — the demo IS a real agent run.
- **`python3-pyte`** (`sudo apt install python3-pyte`) — a tmux popup is a *client-owned
  overlay*, so it exists only on the attached client's pty and has to be read back
  through a terminal emulator.
- **`agg`** (`cargo install --git https://github.com/asciinema/agg`) — to render.
- A working rein setup (see `HANDOFF.md`) and a throwaway repo (`resolve_throwaway_repo`).

## How it is recorded (and why that way)

It reuses the harness that already drives this exact scenario live —
`tests/interactive/journey_realagent_write.py` and `tests/interactive/reinharness.py`.
The demo is that journey, *recorded*, not a second implementation of it.

**The recording surface is the attached tmux CLIENT's pty.** That is the only surface
that carries the pane's content *and* the popup composited on top of it: a tmux popup is
an overlay owned by the client (no `window_pane`), so `capture-pane` and `pipe-pane`
structurally cannot see it. A recording made from either would show Form A nowhere at
all — i.e. it would omit the one frame the demo exists for. `reinharness` already reads
that pty on every poll iteration (it *must*: an undrained client pty stalls tmux's
attach). `TmuxPaneSession.start_recording()` — **opt-in and default-off**, so no journey
changes behavior — simply timestamps those reads into asciicast v2.

**Geometry is 160x32, and that is forced, not chosen.** rein launches the popup with no
`-w`/`-h` (`internal/ui/grant/grant.go`), so tmux sizes it at **half the client**. At
100x30 the popup is 48 columns: Form A wraps, and its `=== rein: … ===` banner scrolls
out of the box entirely. 160 is the narrowest pane at which Form A renders whole and
unwrapped (its widest line, `session:  demo (role=…, repos=[…])`, is 77 columns). The
demo's session id (`demo`) and issue title are short for the same reason.

**The human beats are RECORDED; five of them are then LENGTHENED at render time**
(`pace_cast` in `record_demo.py`): the typed command before Enter, rein's launch banner,
Form A with an empty prompt, **the issue number visibly typed into Form A**, and the
dismissal. Lengthening is *timing only* — no screen content is added, removed or
reordered. It is needed because the take has **no dead air** to trim: claude animates a
spinner throughout and tmux repaints the popup about once a second, so `agg
--idle-time-limit` has nothing to bite on, and the only lever left for the long
LLM-thinking stretches is `--speed` — which divides the good pauses along with the boring
ones (at 3x every beat is a third of what was recorded: readable becomes unreadable).

But a beat can only be lengthened if it was **captured**, and that is the lesson of the
first take: it wrote the issue number and the Enter in **one** `send()`, so the popup
echoed the digits and closed inside a single repaint — **no frame of that cast ever showed
the number being typed**, and no amount of re-pacing could conjure one. The recorder now
types the digits, **holds while draining**, and only then sends Enter. Same rule for the
command: it is typed in chunks at a clean prompt and held, so the gif opens on a human
starting the run rather than mid-story on rein's banner.

`pace_cast` therefore anchors on the **rendered screen** (pyte), not on bytes: every popup
repaint carries the whole Form A box, so "Form A empty" and "Form A with the number typed
in" are the *same bytes* in different frames, and only the render can tell them apart. A
missing anchor is a **hard error** — a silently-skipped beat would reproduce the exact
defect this pass fixed.

### An anchor must be EXACT, or it lands mid-word

The second take typed evenly (measured on the cast: 0.309s / 0.308s between digits) and the
gif *still* showed the number going in as **"20" … long pause … "6"**. The recording was not
at fault; the **pacing** was. `forma` — the "Form A is up, read it" beat — was anchored on
*"Form A without the COMPLETE number"*, and that predicate is **also true of a frame showing
`> 20`**. So the 7-second beat was inserted between the "0" and the "6".

The anchor is now the **empty prompt**, read off the render by `popup_answer()` (the box's
`>` row: `""` while nothing is typed). And because "the take was even and the render still
stuttered" is exactly the kind of bug that silently comes back, `pace_cast` now **asserts**
it: **no beat may land between the first digit appearing and the number being complete** —
a hard error, like a missing anchor. Pace in the recorder, *prove* it in the pacer.

`DIGIT_GAP` is sized in **cast** seconds and divided by `--speed`, so 0.45 lands the digits
~150ms apart **on screen** — a human's typing cadence.

**`/exit`, never Ctrl-C.** Terminal SIGINT is untrapped by design, so rein would die
without running its exit-revoke and never print the accounting line — and that line is
the closing beat: the credential does not outlive the task.

## Why `agg`, not vhs / terminalizer / asciinema-player

`agg` rasterizes frames straight from a font: **no browser, no ffmpeg**. `vhs` and
`terminalizer` render through **headless Chromium**, which has **no linux-arm64 build** —
they cannot render on this box at all. `asciinema-player` is a web player, not a GIF
renderer; and the `asciinema` *binary* is not needed here either — nothing shells out to
`asciinema rec`, because the harness already owns the pty it would have wrapped, and it
is the only one that sees the popup.

**`python3 demo/record_demo.py --render-only` is the only command that reproduces the
checked-in GIF** — because `agg` runs on the **paced** cast (`pace_cast`, above), not on
`creds-joke.cast` directly. Running the `agg` line below against the raw cast yourself
gives you a *different*, worse gif: every beat collapses to a third of itself. It is shown
only so the knobs are visible; tune them in `record_demo.py`'s `render()`.

```
# what render() actually runs, on the PACED cast (a temp file, not the checked-in one)
agg --font-size 14 --theme asciinema --speed 3 --idle-time-limit 12 \
    --fps-cap 15 --last-frame-duration 5  <paced>.cast  creds-joke.gif
```

If the GIF grows past ~8 MB (README-friendly), the big levers, in order: `--fps-cap`,
`--font-size`, `--speed`.

## Verifying a new take

You cannot eyeball a GIF from a terminal, so extract frames and look at them (an agent can
READ a PNG). A good take shows, legibly, in order: a **clean prompt** and the command being **typed**
(`rein run -- claude "…"`); rein's launch banner (what it sandboxed, that the agent sees
**no real token**, that writes are **LOCKED** until it declares); claude's TUI doing the
work; **Form A, whole, on screen long enough to read**, with the live TUI underneath it
blocked on `rein declare`; **the issue number visibly typed into Form A** before Enter (if
no frame shows it, the take is a re-record, not a re-pace — see above); the dismissal; the
push to `agent/<issue>/…` and the PR; and the final `rein: revoked 1 of 1 write token(s)
on exit`.

**Known, and NOT fixable by re-recording (issue #119 — open; shipping the take as-is is the
maintainer's call, not a settled decision):** the agent narrates a `.git/config` write error
("the `.git/config` write errors are sandbox noise — the push itself succeeded").
`git push -u` writes `branch.<x>.remote` + `.merge`
into the sandbox's read-only `.git/config`. #103 strips `-u` in the **`rein-git` shim** —
but that shim is installed under `$HOME/.local/state/rein/shim`, and **$HOME is hidden in
the sandbox**: the only dir prepended to the sandboxed agent's PATH is the run tmpdir, which
stages `rein` and **not** `git`. So in-sandbox `git` is the real `/usr/bin/git` and the strip
never runs (it only takes effect under `rein run --direct`). This is a **product** bug, not a
recording defect — no take can avoid it until #119 lands.

`ffmpeg -ss` seeks a GIF by its *average* frame rate and will land on the wrong beat.
Dump every frame and index it by its real timestamp instead:

```sh
ffmpeg -i demo/creds-joke.gif -vsync 0 /tmp/frames/f_%03d.png     # every frame
ffmpeg -i demo/creds-joke.gif -vf showinfo -f null - 2>&1 | grep -o 'pts_time:[0-9.]*'
```
