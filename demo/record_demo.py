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
  * a deliberate HOLD on Form A (`pane.hold`, which keeps draining — a bare sleep would
    stall tmux's attach), so a viewer can READ the approval screen. That screen is the
    whole point of rein; the gif must linger on it.
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

# How long Form A sits on screen before the "human" answers. Generous ON PURPOSE:
# `agg --speed`/`--idle-time-limit` can always SHORTEN a pause at render time, but
# nothing can lengthen one that was never recorded. This is the money shot.
FORMA_HOLD = 10.0

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


def launcher(workdir: str, issue: int, session: str) -> str:
    """The one command a developer types in their pane, as a tiny shell file — so the
    gif opens on `$ rein run -- claude …` and not on a wall of exported env. rein ECHOES
    the full command line under its own banner anyway, so nothing is hidden."""
    d = tempfile.mkdtemp(prefix="rein-demo-launch-")
    SCRATCH.append(d)
    path = os.path.join(d, "rein-run.sh")
    env = {"REIN_SESSION_FILE": session, "REIN_SANDBOX_WORKDIR": workdir}
    lines = [f"cd {shlex.quote(workdir)}"]
    lines += [f"export {k}={shlex.quote(v)}" for k, v in env.items()]
    lines.append("exec " + " ".join(shlex.quote(a) for a in [
        str(H.REIN_BIN), "run", "--", "claude",
        "--dangerously-skip-permissions", task_for(issue),
    ]))
    with open(path, "w") as f:
        f.write("\n".join(lines) + "\n")
    return path


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
    launch = launcher(workdir, issue, session_file(repo))
    branch, prs = None, []

    with H.tmux_pane_session(env=env, width=WIDTH, height=HEIGHT) as pane:
        # t=0 is HERE — the cast opens on the command being typed, not on the harness's
        # attach/priming keystrokes.
        pane.start_recording(str(CAST))
        try:
            pane.run_in_pane(f"bash {launch}")

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
            pane.hold(FORMA_HOLD)          # drains + records throughout; never time.sleep
            pane.send_client(f"{issue}\r")  # the human approves

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
    return branch, prs


# --------------------------------------------------------------------------
# Pacing + render (free, local, and where you should iterate — NOT on claude)
# --------------------------------------------------------------------------

# The two beats a VIEWER needs and the RUN cannot give them, in cast seconds (they are
# then divided by --speed on screen). See pace_cast for why they have to be inserted.
BANNER_BEAT = 9.0   # hold on rein's launch banner: what it sandboxed, locked, injected
FORMA_BEAT = 8.0    # extra hold on Form A, ON TOP of the 10s actually recorded


def pace_cast(src: Path, dst: Path) -> float:
    """Lengthen TWO PAUSES in the recording. Timing only — not one byte of screen
    content is added, removed or reordered; the take is exactly the take.

    WHY IT IS NEEDED (and why `agg --idle-time-limit` cannot do it): the cast has NO
    dead air to trim. claude's TUI animates a spinner throughout, and tmux repaints the
    popup about once a second, so the longest real gap in a 120s take is 1.3s — even the
    10s Form A hold arrives as ~10 one-second repaints. `--idle-time-limit` therefore
    has nothing to bite on, and the ONLY lever left for the long LLM-thinking stretches
    is `--speed` — which divides the good pauses along with the boring ones. At 3x the
    banner flashes past in 1.5s and Form A in 3.3s: not readable.

    Neither beat can be recorded instead. rein prints its banner and immediately execs
    claude, which repaints over it — nothing in the run can pause there. And a longer
    Form A hold trades an API-token-spending re-run for something that is purely a
    presentation choice.

    So: insert idle AFTER two anchor events, shifting everything later by the same
    amount — the screen simply STAYS on the frame it was already showing.
      1. rein's `running:` echo — the last line of its launch banner. Holds on the
         sandbox/egress/write-lock/contract summary: rein's whole security posture, in
         one frame.
      2. the last repaint of the popup's Form A, i.e. the moment before the human
         answers. THE money shot.
    Returns the paced duration in cast seconds.
    """
    lines = src.read_text().splitlines()
    header, events = lines[0], [json.loads(l) for l in lines[1:]]

    def last_index(needle: str, default: int | None = None) -> int | None:
        hits = [i for i, e in enumerate(events) if needle in e[2]]
        return hits[-1] if hits else default

    beats: dict[int, float] = {}
    # `rein: running:` reaches the screen twice (the launch echo, and again when the TUI
    # tears down and the scrollback under it is exposed) — take the FIRST, the launch
    # one, not one that lands after the agent has already finished.
    banner = next((i for i, e in enumerate(events) if "rein: running:" in e[2]), None)
    if banner is not None:
        beats[banner] = BANNER_BEAT
    forma = last_index("To approve, type the issue")
    if forma is not None:
        beats[forma] = FORMA_BEAT
    if len(beats) < 2:
        print(f"[pace] WARNING: only found {len(beats)}/2 anchors — is this a full take?",
              flush=True)

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
    speed touches them; the two beats pace_cast inserts are divided by it too, which is
    why they are sized in cast seconds). `--idle-time-limit 12` is a safety valve for a
    future take that DOES stall — it is deliberately larger than the inserted beats, so
    it never eats them. `--last-frame-duration` holds the ending — the merged-in PR and
    rein's `revoked 1 of 1 write token(s) on exit` — long enough to read.
    """
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
