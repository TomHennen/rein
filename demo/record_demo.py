#!/usr/bin/env python3
"""record_demo — record the README demo (#99): a real `claude`, sandboxed by rein,
declares an issue, blocks, is approved at the tmux popup, pushes a PR, and `/exit`s so
rein revokes the token and says so. It IS journey_realagent_write.py recorded — it
reuses `reinharness` and adds only what a recording needs.

    python3 demo/record_demo.py                 # record + render
    python3 demo/record_demo.py --no-render     # record only (.cast)
    python3 demo/record_demo.py --render-only    # re-render an existing .cast (no claude)
    REIN_DEMO_ISSUE=123 python3 demo/record_demo.py   # reuse an existing issue

Non-obvious constraints, each explained where it bites below: the popup is a
client-owned overlay (so the recording comes from the attached client's pty, not
capture-pane); pacing uses draining `pane.hold`, never `time.sleep`; the approval is
typed digits-then-Enter, not in one send; and the pane is 160x32 for the popup to fit.
Renders with `agg` (see demo/README.md). Re-rendering is free; only recording costs API
tokens.
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

SCRATCH: list[str] = []  # every scratch dir the run makes; removed in main()'s finally

# `tmux popup` defaults to 50% of the client, so the pane must be ~2x the widest Form A
# line (the `session:` line, 77 cols). At 100x30 the popup wraps and the banner scrolls
# out of the box; 160 is the narrowest width that renders Form A whole.
WIDTH, HEIGHT = 160, 32

# Holds during the live take, in recorded seconds. Each is a draining `pane.hold` (a bare
# sleep stalls tmux's attach and records nothing).
PROMPT_HOLD = 1.0    # empty prompt before any key
COMMAND_HOLD = 1.3   # the typed command, readable, before Enter
FORMA_HOLD = 3.5     # Form A up, prompt empty — long enough to read
TYPED_HOLD = 1.8     # digits on screen before Enter
DISMISS_HOLD = 2.5   # after Enter: popup gone, claude resumes

# Gap between digits (cast seconds); ~150ms apart on screen at --speed 3, an even human
# cadence. Evenness is also enforced in pace_cast (no beat may land mid-number).
DIGIT_GAP = 0.45


def task_for(issue: int) -> str:
    # Says nothing about declaring or branch names — the agent learns those from rein's
    # injected contract, which is what the demo shows off.
    return (
        f"Add one short joke about credentials to jokes.md, then commit it, push it, "
        f"and open a pull request. This is issue #{issue}."
    )


def session_file(repo: str) -> str:
    # `id: demo` is deliberately short: it lands in Form A's widest line; a longer id
    # would wrap the popup box.
    d = tempfile.mkdtemp(prefix="rein-demo-sess-")
    SCRATCH.append(d)
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(f"id: demo\nrole: implement\nrepos:\n  - {repo}\n")
    return path


def demo_command(issue: int) -> str:
    # The command the viewer watches typed: a bare `rein`, the agent, the task in plain
    # English. --dangerously-skip-permissions is genuinely required (claude would prompt
    # per tool call and block the unattended take); it also makes rein's point — even a
    # claude with its own gates off cannot write without the human at rein's gate.
    return f'rein run -- claude --dangerously-skip-permissions "{task_for(issue)}"'


def prepare_pane(pane, workdir: str, issue: int, session: str) -> None:
    # Put the pane in the state a developer's already is, BEFORE t=0: session file,
    # sandbox working tree, `bin/` on PATH, cwd. Done in the shell (not folded into the
    # command line) so the recording opens on the honest one-liner, not a wall of env.
    exports = " ; ".join([
        f"export REIN_SESSION_FILE={shlex.quote(session)}",
        f"export REIN_SANDBOX_WORKDIR={shlex.quote(workdir)}",
        f"export PATH={shlex.quote(str(H.REIN_BIN.parent))}:$PATH",
        f"cd {shlex.quote(workdir)}",
    ])
    pane.run_in_pane(exports)
    pane.wait_stable(300, timeout=15)


def type_in_pane(pane, text: str, *, chunk: int = 3, gap: float = 0.06) -> None:
    # Type in small chunks so the gif shows typing, not a paste. Gaps are draining holds,
    # never time.sleep (an unread client pty stalls tmux's attach).
    for i in range(0, len(text), chunk):
        pane.send_pane_literal(text[i:i + chunk])
        pane.hold(gap)


def await_landing(pane, repo: str, issue: int, env: dict, timeout: float = 600.0,
                  every: float = 6.0):
    # Wait for the branch + PR to appear AT GITHUB (ground truth, not claude's prose).
    # Polled via pane.until so every iteration also drains + records the client.
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
    """One live take into CAST. Returns (branch, prs)."""
    session = session_file(repo)
    branch, prs = None, []

    with H.tmux_pane_session(env=env, width=WIDTH, height=HEIGHT) as pane:
        prepare_pane(pane, workdir, issue, session)  # environment, before t=0

        # t=0. C-l (readline clear-screen, no visible `clear`) gives a clean opening frame.
        pane.start_recording(str(CAST))
        try:
            pane.send_pane("C-l")
            pane.hold(PROMPT_HOLD)
            type_in_pane(pane, demo_command(issue))
            pane.hold(COMMAND_HOLD)
            pane.send_pane("Enter")

            # claude's folder-trust dialog blocks forever if unanswered (rein gives an
            # ephemeral $HOME, so claude sees an untrusted folder). Dismissed, not narrated.
            H.dismiss_claude_trust_dialog(pane, timeout=240)
            if not pane.until_pane(lambda s: "? for shortcuts" in s or "esc to interrupt" in s,
                                   timeout=120):
                raise RuntimeError("claude's TUI never came up in the pane")

            # The money shot: the agent declared and blocked; approval renders in the popup
            # over the live TUI on the client. Wait for Form A fully painted (through its
            # trailing `>` prompt, the last thing rein writes before blocking), linger,
            # then answer on the CLIENT (send-keys cannot reach a client-owned overlay).
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
            pane.hold(FORMA_HOLD)

            # Approve as a human does: digits, a beat, THEN Enter. Splitting the send is
            # why this take was re-recorded — `send(answer + "\r")` echoed and closed the
            # popup inside one repaint, so the digits landed in no captured read.
            for digit in str(issue):
                pane.send_client(digit)
                pane.hold(DIGIT_GAP)
            pane.hold(TYPED_HOLD)
            pane.send_client("\r")
            pane.hold(DISMISS_HOLD)

            print("[rec] approved — waiting for the branch + PR to land at GitHub…",
                  flush=True)
            branch, prs = await_landing(pane, repo, issue, env, timeout=600)
            print(f"[rec] landed: branch={branch} prs={[p['number'] for p in prs]}",
                  flush=True)

            pane.wait_stable(700, timeout=60)  # let the agent finish narrating
            pane.hold(3.0)

            # /exit, not Ctrl-C: SIGINT is untrapped, so rein would never run its exit-
            # revoke or print `rein: revoked N of N write token(s) on exit` — the closing
            # beat and the proof the credential does not outlive the task.
            print("[rec] quitting with /exit…", flush=True)
            pane.send_pane("Escape")
            pane.hold(0.5)
            pane.send_pane_literal("/exit")
            pane.send_pane("Enter")
            if not pane.until_raw("revoked", timeout=60):
                print("[rec] WARNING: rein's exit token accounting never appeared",
                      flush=True)
            pane.hold(3.0)  # let the client be pumped so the revoke line lands in the gif
        finally:
            pane.stop_recording()
            # Optional full pane byte stream (pipe-pane) for diagnosing a take: the .cast
            # is the client pty and claude truncates its tool-call lines to fit the box,
            # so it can't always show which command ran. Off by default.
            if dump := os.getenv("REIN_DEMO_RAWDUMP"):
                Path(dump).write_text(pane.raw_stream())
                print(f"[rec] raw pane stream -> {dump}", flush=True)
    return branch, prs


# --------------------------------------------------------------------------
# Pacing + render (free, local — iterate HERE, never by re-running claude)
# --------------------------------------------------------------------------

# The five viewer beats the run can't give, in cast seconds (divided by --speed on screen).
COMMAND_BEAT = 4.5  # typed command at the prompt, before Enter
BANNER_BEAT = 9.0   # rein's launch banner: what it sandboxed, locked, injected
FORMA_BEAT = 7.0    # Form A up, prompt EMPTY
TYPED_BEAT = 6.0    # the issue number visibly in the box, before Enter
DISMISS_BEAT = 3.0  # popup gone, claude resuming

# Form A's `>` prompt row as the client renders it (`│> 206      │`); group 1 is the
# digits echoed so far ("" while empty). Read off the render, not by searching for
# `> <issue>` — `"> 206" not in frame` is also true of `> 20`, which mis-fired before.
_POPUP_PROMPT_ROW = re.compile(r"[│|]\s*>\s*(\d*)\s*[│|]")


def popup_answer(frame: str) -> str | None:
    # Digits echoed in Form A's `>` prompt on this frame; None if Form A is not up, ""
    # if up and empty (the only state the `forma` beat may hold on).
    m = _POPUP_PROMPT_ROW.search(frame)
    return m.group(1) if m else None


def pace_cast(src: Path, dst: Path) -> float:
    """Lengthen five pauses in the cast. Timing only — no screen byte is added, removed
    or reordered.

    `agg --idle-time-limit` can't do it: claude's spinner and the popup repaint animate
    throughout, so there's no dead air to trim, and --speed divides the good pauses with
    the boring ones. Anchored on the RENDERED screen (via pyte), because every popup
    repaint carries the whole Form A box, so "prompt empty" and "number typed" are the
    same bytes in different frames — only the render tells them apart. Each beat is
    inserted after the last frame painting that state (command / banner / Form A empty /
    number visible / dismissed). Returns the paced duration in cast seconds.
    """
    lines = src.read_text().splitlines()
    header, events = lines[0], [json.loads(l) for l in lines[1:]]
    hdr = json.loads(header)

    # Cumulative replay: frames[i] is the screen after event i.
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

    # Read the issue number back off Form A, so pacing needs no argument (--render-only
    # can re-pace an old cast).
    m = next((m for f in frames if (m := re.search(r"issue:\s+#(\d+)", f))), None)
    if m is None:
        raise RuntimeError(f"{src}: no Form A anywhere in the cast — not a usable take")
    issue = m.group(1)
    forma_up = "To approve, type the issue"

    # Hold the dismissal on the first frame the popup is GONE (one past the last Form A
    # frame, which still shows the answer).
    last_forma = last_frame(lambda f: forma_up in f)
    gone = (None if last_forma is None else
            first_frame_after(frames, last_forma, lambda f: forma_up not in f))

    beats: dict[int, float] = {}
    anchors = {
        "command": (last_frame(lambda f: f'issue #{issue}."' in f and "rein:" not in f),
                    COMMAND_BEAT),
        # `rein: running:` lands twice (launch echo, then again when the TUI tears down);
        # take the first.
        "banner": (first_frame(lambda f: "rein: running:" in f), BANNER_BEAT),
        # prompt genuinely empty, NOT merely "complete number absent" (also true of `> 20`).
        "forma": (last_frame(lambda f: forma_up in f and popup_answer(f) == ""),
                  FORMA_BEAT),
        "typed": (last_frame(lambda f: forma_up in f and popup_answer(f) == issue),
                  TYPED_BEAT),
        "dismiss": (gone, DISMISS_BEAT),
    }
    missing = [name for name, (i, _) in anchors.items() if i is None]
    if missing:
        # Hard, never a warning: a missing `typed` anchor silently reproduces the defect
        # this re-record fixes (a popup that pauses on nothing then vanishes), and a GIF
        # that looks fine is the worst outcome.
        raise RuntimeError(
            f"{src}: pacing anchors not found on the rendered screen: {missing}. "
            f"The take is unusable as-is — re-record (see the beats in record())."
        )
    for name, (i, beat) in anchors.items():
        beats[i] = beats.get(i, 0.0) + beat

    # The typing is one gesture — no beat may land inside it. Checked, not trusted: an
    # evenly-paced take still stuttered once because a beat landed between two digits.
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
    """asciicast -> GIF via agg (see demo/README.md for why agg). Free and local.

    --speed 3 compresses the animated LLM-thinking stretches (the inserted beats are in
    cast seconds so they divide too); --idle-time-limit 12 is a safety valve larger than
    every beat; --last-frame-duration holds the ending (merged PR + revoke line).
    """
    if not H.pyte_available():  # pace_cast anchors on the render
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
        # Short on purpose: the title lands in Form A's `issue:` line; a long one wraps.
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
        # suffix, so discover whatever is under agent/<issue>/ rather than assuming.
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
        for d in SCRATCH:
            shutil.rmtree(d, ignore_errors=True)
        if ours:
            H.close_issue(repo, issue, env, comment="demo recorded; closing.")
        print(f"cleanup: {len(to_close)} PR(s) closed; branches deleted "
              f"({sorted(branches)}); checkout removed"
              + ("; issue closed" if ours else ""), flush=True)


if __name__ == "__main__":
    sys.exit(main())
