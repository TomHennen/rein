#!/usr/bin/env python3
"""record_demo.py — AUTOMATED recording of the rein "credentials joke" demo.

A real `rein run -- claude` session (agent writes a joke about credentials, hits
the write-approval tmux popup, it's approved, then it pushes + opens a PR),
captured to an asciicast and rendered to a GIF with `agg`. No human needed.

Reuses the proven tmux-popup harness (#88): reinharness.tmux_popup_session()
stands up a dedicated-socket tmux server with a pexpect-attached client. We run
`rein run -- claude` INSIDE that session (via send-keys) so claude's TUI and the
popup share ONE client pty — captured together — and we answer the popup on that
same client (the popup grabs its keyboard). Every byte pexpect reads is
timestamped via a logfile_read shim, which IS the asciicast.

The one non-deterministic part is real claude — re-run until you get a clean take.

Deps: agg (asciinema gif renderer; prebuilt arm64/x86 binaries, no browser),
tmux, and rein configured (`rein init`) with a dev-session naming a THROWAWAY repo.

    python3 demo/record_demo.py
    REIN_DEMO_REPO=you/throwaway python3 demo/record_demo.py
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import time
import uuid

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "tests", "interactive"))
import reinharness as H  # noqa: E402
import pexpect  # noqa: E402

CAST = os.path.join(HERE, "creds-joke.cast")
GIF = os.path.join(HERE, "creds-joke.gif")
COLS, ROWS = 104, 32
HARD_SECONDS = 300


class TSWriter:
    """A logfile_read sink that records every chunk pexpect reads as a timestamped
    asciicast 'o' event. Because drive/pump both read through pexpect, this
    captures the WHOLE client session with real timing."""

    def __init__(self):
        self.events: list = []
        self.t0 = time.time()

    def write(self, s: str):
        if s:
            self.events.append([round(time.time() - self.t0, 3), "o", s])

    def flush(self):
        pass


def main() -> int:
    if not shutil.which("agg"):
        print("MISSING: agg — install the asciinema gif renderer "
              "(prebuilt binaries at github.com/asciinema/agg/releases; no browser needed).",
              file=sys.stderr)
        return 1
    if not H.tmux_available() if hasattr(H, "tmux_available") else not shutil.which("tmux"):
        print("MISSING: tmux", file=sys.stderr)
        return 1

    env = H.rein_env()
    repo = os.environ.get("REIN_DEMO_REPO") or H.resolve_throwaway_repo(env)
    H.build_binaries(env)
    rein = os.path.abspath(os.path.join(HERE, "..", "bin", "rein"))

    issue = H.create_issue(
        repo, "add a credentials joke to the repo",
        "Demo issue for the rein README recording. Safe to close.", env)
    print(f"==> repo={repo} issue=#{issue}", flush=True)

    work = os.path.join("/tmp", f"rein-demo-{uuid.uuid4().hex[:6]}", "repo")
    subprocess.run(["git", "clone", "--depth", "1", f"https://github.com/{repo}.git", work],
                   check=True, capture_output=True)
    sess_file = os.path.join(work, ".demo-session.yaml")
    open(sess_file, "w").write(f"id: sess_demo\nrole: implement\nrepos:\n  - {repo}\n")

    # Let claude FIGURE OUT the declare itself (it hits rein's write gate and works
    # it out) — don't spell out the steps.
    prompt = ("Add a short one-line joke about credentials to a new file jokes.md, "
              "then commit it, push it, and open a pull request. "
              f"This work is for issue {issue}.")
    # Put the launch in a script so send-keys never has to quote the prompt.
    # --dangerously-skip-permissions ("yolo"): the sandbox + rein's declare gate are
    # the guardrails, so claude needs no per-tool permission prompts (the point of
    # the demo); it also skips the folder-trust dialog.
    agent_sh = os.path.join(work, "run-agent.sh")
    open(agent_sh, "w").write(
        "#!/usr/bin/env bash\n"
        f'cd "{work}"\n'
        f'REIN_SESSION_FILE="{sess_file}" "{rein}" run -- claude --dangerously-skip-permissions "{prompt}"\n')
    os.chmod(agent_sh, 0o755)

    approved = pr = False
    sock = f"reindemo-{os.getpid()}-{uuid.uuid4().hex[:6]}"
    subprocess.run(["tmux", "-L", sock, "kill-server"], capture_output=True)
    # Spawn the tmux client DIRECTLY under pexpect so the client OWNS this pty from
    # birth. (Creating the session detached and then `tmux attach`-ing from pexpect
    # makes the attached client drop when srt starts its --new-session sandbox —
    # that is what truncated earlier takes.)
    client = pexpect.spawn("tmux", ["-L", sock, "new-session", f"bash {agent_sh}"],
                           env=env, encoding="utf-8", codec_errors="replace",
                           dimensions=(ROWS, COLS), timeout=None)
    tsw = TSWriter()
    client.logfile_read = tsw
    buf, hard = "", time.time() + HARD_SECONDS
    while time.time() < hard:
        try:
            d = client.read_nonblocking(4096, timeout=0.3)
            buf += H.strip_ansi(d)
        except pexpect.TIMEOUT:
            pass          # claude thinking; do NOT bail on a quiet moment
        except pexpect.EOF:
            break
        if not approved and "type the issue number" in buf:
            time.sleep(0.8)
            client.send(str(issue) + "\r")
            approved = True
            print("==> approved the popup", flush=True)
        if "/pull/" in buf:
            pr = True
        if approved and pr:
            t = time.time() + 3.0
            while time.time() < t:
                try:
                    client.read_nonblocking(4096, timeout=0.3)
                except (pexpect.TIMEOUT, pexpect.EOF):
                    pass
            break
    events = tsw.events
    try:
        client.close(force=True)
    except Exception:
        pass
    subprocess.run(["tmux", "-L", sock, "kill-server"], capture_output=True)

    with open(CAST, "w") as f:
        f.write(json.dumps({"version": 2, "width": COLS, "height": ROWS,
                            "env": {"TERM": "xterm-256color"}}) + "\n")
        for e in events:
            f.write(json.dumps(e) + "\n")

    subprocess.run(["agg", "--font-size", "18", "--idle-time-limit", "2", CAST, GIF], check=True)
    shutil.rmtree(os.path.dirname(work), ignore_errors=True)
    print(f"==> approved={approved} pr_seen={pr} events={len(events)}", flush=True)
    print(f"==> cast: {CAST}\n==> gif:  {GIF}", flush=True)
    print(f"    (issue #{issue} + its PR are on {repo} — close/delete when done.)", flush=True)
    return 0 if (approved and pr) else 2


if __name__ == "__main__":
    sys.exit(main())
