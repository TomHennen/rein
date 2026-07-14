#!/usr/bin/env python3
"""record_demo — record the README demo: a REAL `claude`, under rein, gated by the
write-approval popup (#99).

WHAT IT RECORDS (one take, no acting):

    rein run -- claude "add a joke about credentials to jokes.md, commit, push, PR"

a real agent, sandboxed by rein, reads rein's injected contract, DECLARES the issue,
and BLOCKS. Approval routes to the tmux POPUP — Form A, overlaid on the agent's live
TUI — where a human (here, this script) types the issue number. Only then is a
write-tier token minted; the agent pushes `agent/<issue>/…` and opens a PR; `/exit`
makes rein revoke the token and SAY SO. That whole arc is rein's story, so the demo is
the product running, not a mock-up.

IT IS THE JOURNEY, RECORDED. The scenario, the pane, the popup, the cleanup are
`tests/interactive/journey_realagent_write.py` — which already drives exactly this,
live, and asserts it holds. This script reuses that harness (`reinharness`) rather
than re-implementing it, and adds only what a RECORDING needs:

  * `TmuxPaneSession.start_recording` (the opt-in, default-off asciicast hook): the
    harness already drains the attached CLIENT's pty on every poll iteration; the hook
    just TIMESTAMPS those reads. The client is the ONLY surface that carries the pane's
    content AND the popup composited on top — a tmux popup is a client-owned overlay,
    invisible to `capture-pane`/`pipe-pane` — so it is the only surface a recording of
    THIS story can come from.
  * HUMAN PACING (`pane.hold`, which keeps draining — a bare sleep would stall tmux's
    attach and record nothing). The command is TYPED, in chunks, at a clean prompt; Form
    A is held long enough to READ; the issue number is typed as DIGITS, held so it is
    visibly IN the box, and only THEN answered with Enter. The first take skipped that
    middle beat — it wrote `202\\r` in one call, so the popup echoed and closed inside a
    single repaint and NO captured frame ever showed the number being typed.
  * `/exit` rather than Ctrl-C: SIGINT is untrapped by design, so rein would die without
    printing `rein: revoked N of N write token(s) on exit` — the closing beat.

GEOMETRY — 160x32, and it is NOT arbitrary. `tmux popup` (rein passes no `-w`/`-h`,
internal/ui/grant/grant.go) defaults to HALF the client's size, so the pane must be
~2x the widest Form A line (the `session:` line, 77 cols with this session id + repo).
Measured: at 100x30 the popup is 48 cols wide, Form A wraps, and its `=== rein: …`
banner SCROLLS OUT OF THE BOX entirely; 160 is the smallest width at which Form A
renders whole and unwrapped. Anything narrower buys a smaller gif by destroying the
one frame the gif exists for.

SAFE + RE-RUNNABLE: it creates its own throwaway issue on the throwaway repo
(`resolve_throwaway_repo`; hard-constraint #1 — never a real repo) and, in a `finally`,
closes the PR, deletes the branch the agent chose, closes the issue and removes the
checkout. Re-run it until a real LLM gives you a take you like.

    python3 demo/record_demo.py                 # record + render
    python3 demo/record_demo.py --no-render     # record only (.cast)
    REIN_DEMO_ISSUE=123 python3 demo/record_demo.py   # reuse an existing issue

Renders with `agg` (asciicast -> GIF, rasterized from a font: no browser, no ffmpeg —
see demo/README.md for why not vhs/terminalizer). Re-render the SAME cast as often as
you like; only recording costs API tokens.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

DEMO_DIR = Path(__file__).resolve().parent
REPO_ROOT = DEMO_DIR.parent
sys.path.insert(0, str(REPO_ROOT / "tests" / "interactive"))

import reinharness as H  # noqa: E402

CAST = DEMO_DIR / "creds-joke.cast"
GIF = DEMO_DIR / "creds-joke.gif"

# Every scratch dir the run makes, removed in main()'s finally — a demo you re-run until
# you like the take must not leave a trail of them in /tmp.
SCRATCH: list[str] = []

# The pane the whole demo lives in. See the GEOMETRY note above: the width is set by
# the tmux popup's default 50% sizing, not by taste.
WIDTH, HEIGHT = 160, 32

# THE APPROVAL BEATS, in RECORDED seconds. The shape is deliberate and was learned from
# the first take: it held Form A for 10s with an EMPTY `>` prompt and then wrote the
# answer AND the carriage return in ONE `send()`. The popup echoed the digits and closed
# inside a single repaint, so the typed answer was never in ANY captured read — zero
# frames of the whole cast ever showed `> <issue>`. A viewer saw a long dead pause and
# then a popup that vanished with nothing in it.
#
# So the answer is typed the way a human types it — digits first, a beat, THEN Enter —
# and every beat is a DRAINING `pane.hold` (a bare sleep stalls tmux's attach and records
# nothing). The middle beat is the one that has to exist: it is what makes the echo land
# in a captured read.
PROMPT_HOLD = 1.0    # the empty prompt, before a key is pressed: the demo starts at zero
COMMAND_HOLD = 1.3   # the typed command, sitting there, readable, before Enter
FORMA_HOLD = 3.5     # Form A up, prompt still empty — long enough to read, not to bore
TYPED_HOLD = 1.8     # digits on screen, BEFORE Enter — the frames the first take lacked
DISMISS_HOLD = 2.5   # after Enter: the popup goes, claude resumes underneath

# One keystroke to the next, in CAST seconds — so ~1/SPEED of this ON SCREEN. At 0.45
# and --speed 3 the digits land ~150ms apart in the gif: a human's typing cadence, EVEN.
#
# "Even" is the whole point, and it is enforced in TWO places, because the first
# re-record got the recording right and the RENDER wrong. The take itself was already
# evenly paced (measured: 0.309s / 0.308s between digits) — and the gif still showed
# "20", a long pause, then "6". The gap was INSERTED by pace_cast, whose `forma` anchor
# matched "Form A without the COMPLETE number", which is also true of a frame showing a
# PARTIAL number. So the 7s "read Form A" beat landed between the "0" and the "6". The
# anchor is now the EMPTY prompt (`popup_answer`), and `no_beat_mid_typing` asserts no
# beat can ever land mid-number again. Pace here; PROVE there.
DIGIT_GAP = 0.45

# The task. One line, and it says nothing about declaring or about branch names: the
# agent learns THOSE from rein's injected contract — which is exactly what the demo is
# showing off.
def task_for(issue: int) -> str:
    return (
        f"Add one short joke about credentials to jokes.md, then commit it, push it, "
        f"and open a pull request. This is issue #{issue}."
    )


def session_file(repo: str) -> str:
    """A pinned repo-only session. `id: demo` is deliberate: it lands verbatim in Form
    A's `session:` line, which is the widest line in the popup — a longer id would push
    the box past the pane and wrap the money shot."""
    d = tempfile.mkdtemp(prefix="rein-demo-sess-")
    SCRATCH.append(d)
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(f"id: demo\nrole: implement\nrepos:\n  - {repo}\n")
    return path


def demo_command(issue: int) -> str:
    """THE command the viewer watches being typed. It is the whole setup for everything
    that follows, so it must be what a developer would REALLY type: a bare `rein`, the
    agent, and the task in plain English. Nothing cosmetic is hidden and nothing is
    faked — the machinery it needs (`REIN_SESSION_FILE`, the sandbox working tree, the
    `bin/` that makes `rein` a bare word) is EXPORTED INTO THE PANE'S SHELL BEFORE the
    recording starts (`prepare_pane`), which is exactly where a developer's own
    environment would already have it. rein then ECHOES the full argv under its own
    banner, so the viewer can check it against what was typed.

    `--dangerously-skip-permissions` stays: it is genuinely required (claude would
    otherwise prompt per tool call and the unattended take would block). Honesty beats
    prettiness — and rein's point is precisely that even a claude with its OWN gates
    turned off still cannot write without the human at rein's gate.
    """
    return f'rein run -- claude --dangerously-skip-permissions "{task_for(issue)}"'


def prepare_pane(pane, workdir: str, issue: int, session: str) -> None:
    """Put the pane in the state a developer's pane is already in — BEFORE t=0.

    Everything here is environment, not story: the session file, the sandbox working
    tree, `bin/` on PATH, and the cwd. Doing it in the shell (rather than wrapping it
    into the command line, as the first take did with a `bash /tmp/…/rein-run.sh`
    launcher) is what lets the recording open on the honest one-liner of `demo_command`
    instead of on a wall of exported env or an opaque path that tells the viewer nothing.

    Then C-l — readline's clear-screen, which repaints the prompt WITHOUT typing a
    visible `clear` — is sent by `record()` AFTER the recorder is armed, so the cast's
    first frame is a clean `$ ` prompt with a human's hands not yet on the keyboard.
    """
    exports = " ; ".join([
        f"export REIN_SESSION_FILE={shlex.quote(session)}",
        f"export REIN_SANDBOX_WORKDIR={shlex.quote(workdir)}",
        f"export PATH={shlex.quote(str(H.REIN_BIN.parent))}:$PATH",
        f"cd {shlex.quote(workdir)}",
    ])
    pane.run_in_pane(exports)
    pane.wait_stable(300, timeout=15)


def type_in_pane(pane, text: str, *, chunk: int = 3, gap: float = 0.06) -> None:
    """Type `text` into the pane the way a HUMAN does — in small chunks with a beat
    between them — so the gif shows a command being typed, not pasted. The gaps are
    draining holds (`pane.hold`), never `time.sleep`: an unread client pty stalls tmux's
    attach, and a stalled attach records nothing."""
    for i in range(0, len(text), chunk):
        pane.send_pane_literal(text[i:i + chunk])
        pane.hold(gap)


def await_landing(pane, repo: str, issue: int, env: dict, timeout: float = 600.0,
                  every: float = 6.0):
    """Wait for the branch + PR to appear AT GITHUB — ground truth, not claude's prose.
    Polled through `pane.until`, so every iteration also drains (and records) the
    client: an unread client pty stalls tmux's attach and freezes the pane."""
    state = {"last": 0.0, "branch": None, "prs": []}

    def landed() -> bool:
        now = time.time()
        if now - state["last"] < every:
            return False
        state["last"] = now
        if state["branch"] is None:
            refs = H.list_matching_refs(repo, f"agent/{issue}/", env)
            state["branch"] = refs[0] if refs else None
        if state["branch"] is not None:
            state["prs"] = H.list_prs_for_branch(repo, state["branch"], env)
        return bool(state["prs"])

    pane.until(landed, timeout=timeout, poll=0.5)
    return state["branch"], state["prs"]


def record(env: dict, repo: str, issue: int, workdir: str) -> tuple[str | None, list]:
    """One live take, into `CAST`. Returns (branch, prs)."""
    session = session_file(repo)
    branch, prs = None, []

    with H.tmux_pane_session(env=env, width=WIDTH, height=HEIGHT) as pane:
        # Environment, not story — and therefore BEFORE t=0 (see prepare_pane).
        prepare_pane(pane, workdir, issue, session)

        # t=0 is HERE. The cast opens on a clean prompt and a human typing the command,
        # not mid-story on rein's banner already spilling out.
        pane.start_recording(str(CAST))
        try:
            pane.send_pane("C-l")   # readline clear-screen: a clean prompt, no `clear`
            pane.hold(PROMPT_HOLD)
            type_in_pane(pane, demo_command(issue))
            pane.hold(COMMAND_HOLD)  # the typed command, readable, before it is run
            pane.send_pane("Enter")

            # claude's folder-trust dialog: PLUMBING (rein gives the agent an ephemeral
            # $HOME, so claude sees an untrusted folder and BLOCKS forever if unanswered).
            # Dismissed, not narrated — it is claude's UX, not rein's story.
            H.dismiss_claude_trust_dialog(pane, timeout=240)
            if not pane.until_pane(lambda s: "? for shortcuts" in s or "esc to interrupt" in s,
                                   timeout=120):
                raise RuntimeError("claude's TUI never came up in the pane")

            # THE MONEY SHOT. The agent ran `rein declare` and is BLOCKED; approval
            # routed to the popup, which renders over the live TUI on the attached
            # client. Wait for Form A to be FULLY painted (through its trailing `>`
            # prompt — the last thing rein writes before it blocks on input, so the
            # frame provably cannot change), then LINGER so a viewer can read it, then
            # answer on the CLIENT (`send-keys` can never reach a client-owned overlay).
            print("[rec] waiting for the approval popup…", flush=True)
            if not pane.until_client(
                lambda s: s.contains(H.PROMPT_HINT) and H.popup_forma_complete(s),
                timeout=600,
            ):
                raise RuntimeError(
                    "the approval popup's Form A never rendered on the client. If rein "
                    "fell back to the inline prompt, the usual cause is an undrained "
                    f"client pty, not a rein bug. Client screen:\n{pane.screen.text()}"
                )
            print(f"[rec] Form A is up — holding {FORMA_HOLD}s so a viewer can read it",
                  flush=True)
            pane.hold(FORMA_HOLD)   # drains + records throughout; never time.sleep

            # THE HUMAN APPROVES — as a human does: digits, a beat, THEN Enter. Splitting
            # the send is the entire reason this take was re-recorded (see the beats
            # above): with `send(answer + "\r")` the popup echoed and closed inside one
            # repaint and the digits landed in NO captured read, so the gif showed a
            # popup dismissed with nothing ever typed into it. Each beat is a DRAINING
            # hold, so the echo, the dismissal and the TUI resuming underneath are all
            # genuinely read off the client — and therefore genuinely in the cast.
            for digit in str(issue):
                pane.send_client(digit)
                pane.hold(DIGIT_GAP)
            pane.hold(TYPED_HOLD)   # `> <issue>` ON SCREEN — the frames the first take lacked
            pane.send_client("\r")
            pane.hold(DISMISS_HOLD)  # the popup goes; claude resumes underneath

            print("[rec] approved — waiting for the branch + PR to land at GitHub…",
                  flush=True)
            branch, prs = await_landing(pane, repo, issue, env, timeout=600)
            print(f"[rec] landed: branch={branch} prs={[p['number'] for p in prs]}",
                  flush=True)

            # Let the agent finish narrating what it did — the frame before the close.
            pane.wait_stable(700, timeout=60)
            pane.hold(3.0)

            # `/exit`, NOT Ctrl-C: SIGINT is untrapped by design, so rein would never run
            # its exit-revoke and never print `rein: revoked N of N write token(s) on
            # exit` — the closing beat, and the proof the credential does not outlive the
            # task.
            print("[rec] quitting with /exit…", flush=True)
            pane.send_pane("Escape")
            pane.hold(0.5)
            pane.send_pane_literal("/exit")
            pane.send_pane("Enter")
            if not pane.until_raw("revoked", timeout=60):
                print("[rec] WARNING: rein's exit token accounting never appeared",
                      flush=True)
            # The revoke line hit the PANE's stream; give the CLIENT a beat to be pumped
            # (and recorded) so it actually lands in the gif's last frame.
            pane.hold(3.0)
        finally:
            pane.stop_recording()
            # The pane's COMPLETE byte stream (`pipe-pane`), for diagnosing a take. The
            # .cast is the CLIENT's pty — what a viewer saw — and claude's TUI truncates
            # its own tool-call lines to fit the box, so the cast cannot always tell you
            # WHICH command the agent ran. The raw stream can. Off by default (it is a
            # debugging artifact, not an output of the demo).
            if dump := os.getenv("REIN_DEMO_RAWDUMP"):
                Path(dump).write_text(pane.raw_stream())
                print(f"[rec] raw pane stream -> {dump}", flush=True)
    return branch, prs


# --------------------------------------------------------------------------
# Pacing + render (free, local, and where you should iterate — NOT on claude)
# --------------------------------------------------------------------------

# The five beats a VIEWER needs and the RUN cannot give them, in CAST seconds (they are
# divided by --speed on screen, so at 3x a 9.0 beat is 3s of viewing). See pace_cast.
COMMAND_BEAT = 4.5  # the typed command, sitting at the prompt, before Enter
BANNER_BEAT = 9.0   # rein's launch banner: what it sandboxed, locked, injected
FORMA_BEAT = 7.0    # Form A up, prompt still EMPTY — the "read it" beat
TYPED_BEAT = 6.0    # the issue number VISIBLY in the box, before Enter — the fixed beat
DISMISS_BEAT = 3.0  # the popup gone, claude resuming underneath

# Form A's `>` prompt row, as the CLIENT renders it: `│> 206      │`. The digits echoed
# so far are group 1 — "" while the prompt is still empty. Read off the RENDER (the box
# is drawn with box characters, so the row is unambiguous) rather than by searching the
# frame for `> <issue>`, which is what the previous anchor did and why it mis-fired:
# `"> 206" not in frame` is TRUE of a frame showing `> 20`, so "Form A, prompt empty"
# silently included "Form A, half the number typed".
_POPUP_PROMPT_ROW = re.compile(r"[│|]\s*>\s*(\d*)\s*[│|]")


def popup_answer(frame: str) -> str | None:
    """The digits currently echoed in Form A's `>` prompt on this rendered frame, or
    None when Form A is not up. `""` means the prompt is up and EMPTY — which is the
    state the `forma` beat is allowed to hold on, and the ONLY one."""
    m = _POPUP_PROMPT_ROW.search(frame)
    return m.group(1) if m else None


def pace_cast(src: Path, dst: Path) -> float:
    """Lengthen FIVE PAUSES in the recording. Timing only — not one byte of screen
    content is added, removed or reordered; the take is exactly the take.

    WHY IT IS NEEDED (and why `agg --idle-time-limit` cannot do it): the cast has NO
    dead air to trim. claude's TUI animates a spinner throughout and tmux repaints the
    popup about once a second, so `--idle-time-limit` has nothing to bite on, and the
    ONLY lever left for the long LLM-thinking stretches is `--speed` — which divides the
    good pauses along with the boring ones. At 3x, every beat below is a third of what
    the take recorded: readable becomes unreadable.

    ANCHORED ON THE RENDERED SCREEN, NOT ON BYTES (tests/interactive/CLAUDE.md's rule:
    a REDRAWING surface is asserted on the render). Replaying the cast through pyte is
    the only way to tell the two popup beats APART: every popup repaint carries the whole
    Form A box, so "Form A with an EMPTY prompt" and "Form A with the issue number typed
    into it" are the SAME BYTES in different frames. A substring anchor cannot see the
    difference — and the difference is the entire point of this pass.

    The beats, each inserted AFTER the last event that paints that state (so the screen
    simply STAYS on the frame it was already showing):
      1. the typed command, complete, before Enter — the setup for everything after: a
         human typing `rein run -- claude "…"`, not a gif that opens mid-story.
      2. rein's `running:` echo, the last line of its launch banner — sandbox, egress,
         write-lock, injected contract: its whole security posture in one frame.
      3. Form A, prompt still empty — long enough to READ, and no longer (the first take
         sat here for 10s of dead air, which is exactly what the maintainer flagged).
      4. the issue number VISIBLE in the box, before Enter. This beat could not be paced
         into the first take at all: it wrote the digits and the Enter in one call, so no
         captured frame EVER showed the number. That is why the demo was re-recorded.
      5. the dismissal — the popup gone, claude live underneath, unblocked.
    Returns the paced duration in cast seconds.
    """
    lines = src.read_text().splitlines()
    header, events = lines[0], [json.loads(l) for l in lines[1:]]
    hdr = json.loads(header)

    # Cumulative replay: frames[i] is the SCREEN after event i (see the docstring —
    # bytes cannot tell the two popup beats apart; the render can).
    screen = H.RenderedScreen(hdr["width"], hdr["height"])
    frames = []
    for _, _, data in events:
        screen.feed(data)
        frames.append(screen.text())

    def last_frame(pred) -> int | None:
        hits = [i for i, f in enumerate(frames) if pred(f)]
        return hits[-1] if hits else None

    def first_frame(pred) -> int | None:
        return next((i for i, f in enumerate(frames) if pred(f)), None)

    def first_frame_after(fs, after: int, pred) -> int | None:
        return next((i for i in range(after + 1, len(fs)) if pred(fs[i])), None)

    # The issue number is READ BACK off Form A itself, so pacing needs no argument from
    # the recording (and `--render-only` can re-pace an old cast years later).
    m = next((m for f in frames if (m := re.search(r"issue:\s+#(\d+)", f))), None)
    if m is None:
        raise RuntimeError(f"{src}: no Form A anywhere in the cast — not a usable take")
    issue = m.group(1)
    forma_up = "To approve, type the issue"

    # The dismissal is held on the FIRST frame the popup is GONE from — one past the last
    # Form A frame, not the last Form A frame itself (that one still shows the answer).
    last_forma = last_frame(lambda f: forma_up in f)
    gone = (None if last_forma is None else
            first_frame_after(frames, last_forma, lambda f: forma_up not in f))

    beats: dict[int, float] = {}
    anchors = {
        # the complete command on screen, at the prompt, rein not yet started
        "command": (last_frame(lambda f: f'issue #{issue}."' in f and "rein:" not in f),
                    COMMAND_BEAT),
        # rein's `running:` echo lands TWICE (the launch echo, and again when the TUI
        # tears down and the scrollback under it is exposed) — take the FIRST.
        "banner": (first_frame(lambda f: "rein: running:" in f), BANNER_BEAT),
        # Form A up and the prompt genuinely EMPTY — NOT merely "the complete number is
        # absent", which is also true of `> 20` and is what put a 2.3s pause between the
        # "0" and the "6" (see DIGIT_GAP).
        "forma": (last_frame(lambda f: forma_up in f and popup_answer(f) == ""),
                  FORMA_BEAT),
        "typed": (last_frame(lambda f: forma_up in f and popup_answer(f) == issue),
                  TYPED_BEAT),
        "dismiss": (gone, DISMISS_BEAT),
    }
    missing = [name for name, (i, _) in anchors.items() if i is None]
    if missing:
        # HARD, never a warning. A missing `typed` anchor silently reproduces the exact
        # defect this re-record exists to fix (a popup that pauses on nothing, then
        # vanishes with the answer never seen), and a GIF that LOOKS fine is the worst
        # possible outcome.
        raise RuntimeError(
            f"{src}: pacing anchors not found on the rendered screen: {missing}. "
            f"The take is unusable as-is — re-record (see the beats in record())."
        )
    for name, (i, beat) in anchors.items():
        beats[i] = beats.get(i, 0.0) + beat

    # THE TYPING IS ONE GESTURE — nothing may be inserted INSIDE it. This is the
    # invariant the whole re-record exists to establish, so it is CHECKED, not trusted:
    # the take was already evenly paced and the gif still stuttered, because a beat was
    # inserted between two digits. Anchoring on the empty prompt fixes that; this proves
    # it, and would catch any future beat (or a re-tuned anchor) landing mid-number.
    typing_start = first_frame(lambda f: forma_up in f and (popup_answer(f) or "") != "")
    typing_end = first_frame(lambda f: forma_up in f and popup_answer(f) == issue)
    if typing_start is None or typing_end is None:
        raise RuntimeError(
            f"{src}: the issue number is never seen being typed into Form A — the take "
            f"is unusable (this is the exact defect the demo was re-recorded to fix)."
        )
    inside = [i for i in beats if typing_start <= i < typing_end]
    if inside:
        raise RuntimeError(
            f"{src}: a pacing beat lands INSIDE the typing of the issue number "
            f"(frames {typing_start}..{typing_end}, offending {inside}). That is what "
            f"made the digits read as '20' … long pause … '6'. Fix the anchor; do not "
            f"paper over it by shortening the beat."
        )

    shift = 0.0
    out = [header]
    for i, (t, kind, data) in enumerate(events):
        out.append(json.dumps([round(t + shift, 6), kind, data]))
        shift += beats.get(i, 0.0)
    dst.write_text("\n".join(out) + "\n")
    return json.loads(out[-1])[0]


def render() -> None:
    """asciicast -> GIF, via agg (see demo/README.md for why agg and not vhs). Free and
    local: iterate HERE, never by re-running claude.

    `--speed 3` compresses the long LLM-thinking stretches (which are ANIMATED, so only
    speed touches them; the beats pace_cast inserts are divided by it too, which is why
    they are sized in cast seconds). `--idle-time-limit 12` is a safety valve for a
    future take that DOES stall — it is deliberately larger than every inserted beat, so
    it never eats one. `--last-frame-duration` holds the ending — the merged-in PR and
    rein's `revoked 1 of 1 write token(s) on exit` — long enough to read.
    """
    if not H.pyte_available():  # pace_cast anchors on the RENDER, so rendering needs it
        print(f"cannot render: pyte is not installed. {H.PYTE_INSTALL_HINT}", flush=True)
        return
    if shutil.which("agg") is None:
        print("agg is not on PATH — install it to render (see demo/README.md); the "
              f".cast is at {CAST}", flush=True)
        return
    paced = DEMO_DIR / "creds-joke.paced.cast"
    dur = pace_cast(CAST, paced)
    try:
        subprocess.run([
            "agg",
            "--font-size", "14",
            "--theme", "asciinema",
            "--speed", "3",
            "--idle-time-limit", "12",
            "--fps-cap", "15",
            "--last-frame-duration", "5",
            str(paced), str(GIF),
        ], check=True)
    finally:
        paced.unlink(missing_ok=True)
    print(f"rendered {GIF} ({GIF.stat().st_size / 1e6:.1f} MB; ~{dur / 3 + 5:.0f}s of "
          f"playback)", flush=True)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--no-render", action="store_true",
                    help="record the .cast only; skip the agg render")
    ap.add_argument("--render-only", action="store_true",
                    help="re-render the existing .cast (no claude, no API tokens)")
    args = ap.parse_args()

    if args.render_only:
        render()
        return 0

    for tool, why in (("claude", "the demo IS a real agent run"),
                      ("tmux", "rein's approval popup lives in a tmux pane")):
        if shutil.which(tool) is None:
            print(f"cannot record: `{tool}` is not on PATH — {why}.", flush=True)
            return 3
    if not H.pyte_available():
        print(f"cannot record: pyte is not installed — the popup exists only on the "
              f"attached client's pty and is read back through a terminal emulator. "
              f"{H.PYTE_INSTALL_HINT}.", flush=True)
        return 3

    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # hard-constraint #1: throwaway ONLY
    H.build_binaries(env)

    supplied = os.getenv("REIN_DEMO_ISSUE")
    ours = not supplied
    issue = int(supplied) if supplied else H.create_issue(
        repo,
        # Short ON PURPOSE: the title lands in Form A's `issue:` line, and a long one
        # would wrap the popup.
        "demo: add a joke about credentials",
        "Opened by demo/record_demo.py to record the README demo (#99): a real `claude`, "
        "sandboxed by rein, declares this issue, is approved at the tmux popup, and "
        "pushes a PR. Throwaway repo only; closed again when the recording ends.",
        env,
    )
    print(f"demo: recording on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'})", flush=True)

    workdir, branch, prs = None, None, []
    try:
        workdir = tempfile.mkdtemp(prefix="rein-demo-")
        SCRATCH.append(workdir)
        subprocess.run(["gh", "repo", "clone", repo, workdir, "--", "-q"],
                       check=True, env=env, capture_output=True, text=True)
        branch, prs = record(env, repo, issue, workdir)
        print(f"recorded {CAST} ({CAST.stat().st_size / 1e6:.1f} MB)", flush=True)
        if not prs:
            print("WARNING: no PR landed — the take is probably not usable", flush=True)
        if not args.no_render:
            render()
        return 0 if prs else 1
    finally:
        # Leave the throwaway clean (hard-constraint #1). The agent picks its own branch
        # suffix, so DISCOVER whatever is under agent/<issue>/ rather than assuming.
        branches = {branch} if branch else set()
        try:
            branches |= set(H.list_matching_refs(repo, f"agent/{issue}/", env))
        except Exception:
            pass
        to_close = {p["number"] for p in prs}
        for br in branches:
            try:
                to_close |= {p["number"] for p in H.list_prs_for_branch(repo, br, env)
                             if p["state"] == "OPEN"}
            except Exception:
                pass
        for pn in to_close:
            H.close_pr(repo, pn, env)
        for br in branches:
            H.delete_branch(repo, br, env)
        for d in SCRATCH:  # the checkout, the session file, the launcher
            shutil.rmtree(d, ignore_errors=True)
        if ours:
            H.close_issue(repo, issue, env, comment="demo recorded; closing.")
        print(f"cleanup: {len(to_close)} PR(s) closed; branches deleted "
              f"({sorted(branches)}); checkout removed"
              + ("; issue closed" if ours else ""), flush=True)


if __name__ == "__main__":
    sys.exit(main())
