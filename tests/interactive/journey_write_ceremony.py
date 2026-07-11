"""journey_write_ceremony — THE WRITE CEREMONY, as a runnable journey.

JOURNEY CATALOGUE (tests/interactive/README.md): this file IS the "write
ceremony" journey — agent declares an issue -> human confirms on the terminal ->
the verified push lands. Journeys live in `journey_*.py`; the assertion tests
live in `test_*.py` and are what `run.sh` sweeps. A journey's job is to SHOW the
behavior end to end against reality, so its output can be pasted into a PR.

This is not an assertion test; it is the ceremony's SHOWCASE. It drives ONE real
`rein run` against the live throwaway repo and then replays the transcript as the
two views that matter, because the whole security argument of #35 lives in the
GAP between them:

  * WHAT THE AGENT SEES (in-sandbox, no tty, no token): a pre-declaration push
    denied by the proxy's synthesized ERR advertisement -> `rein declare <n>`
    (which BLOCKS while a human decides) -> a verified push to
    agent/<issue>/<nonce> that SUCCEEDS -> a non-convention ref that is REJECTED
    even though writes are unlocked.
  * WHAT THE HUMAN SEES (host tty): the Form A prompt carrying the issue title,
    state and HOME repo FETCHED from GitHub (decision E — so a wrong-but-plausible
    number is visibly wrong), and the resulting `[approved]` line.

Then it verifies on GitHub which branches actually landed, and cleans up.

WHY THIS FILE EXISTS AT ALL (the doctrine): the pexpect pty IS the human. An
autonomous agent can run this end to end with nobody at the keyboard — the write
path does NOT need a human present, only a controlling terminal. (The SANDBOXED
agent still has none, so it can never self-approve; that boundary is intact and
is exactly what the two views below demonstrate.) See tests/interactive/README.md.

SELF-CONTAINED. By default it CREATES its own throwaway issue via `gh`, and in a
`finally` it deletes every branch it made and closes the issue it opened. Pass an
existing issue with REIN_DEMO_ISSUE=<n> to reuse one (it is then NOT closed).

HARD-CONSTRAINT #1: touches ONLY $REIN_TEST_REPO_A, the throwaway.

NOT PART OF THE SWEEP. `run.sh` discovers `test_*.py`; this is `journey_*.py`, so
the default suite never picks it up (it is slow — a full sandboxed clone + four
network round-trips). Run it deliberately:

    source ./dev-env
    python3 tests/interactive/journey_write_ceremony.py          # creates its own issue
    REIN_DEMO_ISSUE=42 python3 tests/interactive/journey_write_ceremony.py
    REIN_DEMO_RAW=1 python3 tests/interactive/journey_write_ceremony.py   # keep git's progress meter

The transcript it prints is intended as a doc/screenshot source, so git's
progress meter is elided by default (REIN_DEMO_RAW=1 keeps it); nothing else is.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys

import reinharness as H

ISSUE_ENV = "REIN_DEMO_ISSUE"

# The Form A prompt block on the HOST tty runs from this header to the outcome
# marker. Used to split the one pty transcript into the two views.
PROMPT_HEADER = "=== rein: agent declares work on an issue ==="

RULE = "=" * 78


def say(s: str = "") -> None:
    print(s, flush=True)


def banner(title: str) -> None:
    say()
    say(RULE)
    say(f"  {title}")
    say(RULE)


# --------------------------------------------------------------------------
# The throwaway issue (created by us, or supplied)
# --------------------------------------------------------------------------


def create_issue(repo: str, env: dict) -> tuple[int, str]:
    """Open a real issue on the THROWAWAY so the declare has something to fetch.

    The declare fetches the issue before prompting (decision E), so the ceremony
    needs a real, open issue — an invented number would 404 and fail closed.
    """
    title = "rein demo: declare-ceremony walkthrough (safe to close)"
    out = subprocess.check_output(
        [
            "gh", "issue", "create", "--repo", repo,
            "--title", title,
            "--body",
            "Opened automatically by tests/interactive/journey_write_ceremony.py "
            "to demonstrate the #35 declare -> confirm -> verified-push ceremony. "
            "Throwaway repo only. This issue is closed again when the demo ends.",
        ],
        text=True,
        env=env,
    ).strip()
    num = int(out.rstrip("/").split("/")[-1])
    return num, title


def issue_title(repo: str, issue: int, env: dict) -> str:
    out = subprocess.check_output(
        ["gh", "issue", "view", str(issue), "--repo", repo, "--json", "title"],
        text=True,
        env=env,
    )
    return json.loads(out)["title"]


def close_issue(repo: str, issue: int, env: dict) -> None:
    subprocess.run(
        ["gh", "issue", "close", str(issue), "--repo", repo,
         "--comment", "demo complete; closing."],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )


# --------------------------------------------------------------------------
# The in-sandbox script: all four agent-visible events in ONE run
# --------------------------------------------------------------------------


def ceremony_script(repo: str, issue: int, good_branch: str, bad_branch: str) -> str:
    """clone -> push (LOCKED) -> declare -> push agent/<n>/<nonce> (OK) -> push
    a non-convention ref (REJECTED).

    `set +e` throughout, and every step echoes an explicit RC sentinel, so the
    demo reports each in-sandbox command's OWN outcome and can never hang.
    Phase markers let the printer slice the transcript into the agent's view.
    """
    return f"""
set +e
cd "$0"
rm -rf repo
git clone --depth 1 https://github.com/{repo} repo
cd repo || {{ echo "SBX_CLONE_FAIL"; exit 3; }}
echo "SBX_CLONE_OK (reads flow with no declaration at all)"

echo "SBX_PHASE1_START"
echo "phase 1" >> probe-1.txt
git add -A
git commit -q -m "demo: pre-declaration write attempt"
git push origin HEAD:refs/heads/{good_branch}
echo "SBX_PHASE1_RC=$?"

echo "SBX_PHASE2_START"
rein declare {issue}
echo "SBX_PHASE2_RC=$?"

echo "SBX_PHASE3_START"
git push origin HEAD:refs/heads/{good_branch}
echo "SBX_PHASE3_RC=$?"

echo "SBX_PHASE4_START"
git push origin HEAD:refs/heads/{bad_branch}
echo "SBX_PHASE4_RC=$?"
echo "SBX_SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# Transcript -> the two views
# --------------------------------------------------------------------------


def _lines(text: str) -> list[str]:
    return H.strip_ansi(text).replace("\r\n", "\n").replace("\r", "\n").split("\n")


# git's progress meter (`Counting objects:  42% (3/7)`) redraws a dozen times per
# operation and drowns the four events this demo exists to show. We elide the
# INTERMEDIATE ticks only — every terminal `done.` line, and every error/reject
# line, survives. Set REIN_DEMO_RAW=1 to keep the meter.
_PROGRESS = re.compile(
    r"^\s*(remote:\s*)?(Counting|Compressing|Receiving|Resolving|Writing|Enumerating|Unpacking)"
    r"\s+(objects|deltas):\s+\d+%"
)


def _is_progress_noise(ln: str) -> bool:
    if os.getenv("REIN_DEMO_RAW"):
        return False
    return bool(_PROGRESS.match(ln)) and "done." not in ln


def host_view(text: str) -> list[str]:
    """The Form A prompt block + its outcome — everything the HUMAN sees."""
    out, inside = [], False
    for ln in _lines(text):
        if PROMPT_HEADER in ln:
            inside = True
        if inside:
            out.append(ln.rstrip())
            if H.APPROVED_MARK in ln or H.DENIED_MARK in ln:
                break
    return out


def agent_view(text: str) -> list[str]:
    """The in-sandbox output, with the host-only lines removed.

    The one pty carries BOTH sides, so this view has to subtract two things to
    stay honest:

      * everything up to and including rein's `---` handoff rule. Before that
        rule rein prints its own HOST banner — which includes a `rein: running:
        <script>` echo of the script SOURCE. Anchoring here (rather than on the
        first sentinel) is what stops the script's own text being mistaken for
        the agent's output.
      * the Form A prompt block, which rendered on the host /dev/tty. The agent
        never saw it — it has no tty at all.
    """
    host = set(host_view(text))
    lines = [ln.rstrip() for ln in _lines(text)]
    start = 0
    for i, ln in enumerate(lines):
        if ln.strip() == "---":
            start = i + 1
            break
    out = []
    for ln in lines[start:]:
        if not ln.strip() or ln in host or _is_progress_noise(ln):
            continue
        if PROMPT_HEADER in ln or ln.lstrip().startswith(">"):
            continue
        out.append(ln)
        if "SBX_SCRIPT_DONE" in ln:
            break
    return out


def rc(text: str, phase: int) -> int | None:
    m = re.search(rf"SBX_PHASE{phase}_RC=(\d+)", H.strip_ansi(text))
    return int(m.group(1)) if m else None


# --------------------------------------------------------------------------
# The ceremony
# --------------------------------------------------------------------------


def main() -> int:
    env = H.rein_env()
    repo = H.throwaway_repo(env)  # hard-constraint #1: the throwaway, only
    H.build_binaries(env)

    supplied = os.getenv(ISSUE_ENV)
    ours = not supplied
    if supplied:
        issue = int(supplied)
        title = issue_title(repo, issue, env)
    else:
        issue, title = create_issue(repo, env)

    good = f"agent/{issue}/{H.unique_branch('demo')}"   # follows the convention
    bad = H.unique_branch("demo-nonconvention")         # deliberately does NOT
    branches = [good, bad]
    run = None

    banner("JOURNEY: the write ceremony (declare -> confirm -> verified push)")
    say(f"  repo   : {repo}  (throwaway)")
    say(f"  issue  : #{issue} {title!r}" + ("  (created by this demo)" if ours else "  (supplied)"))
    say(f"  refs   : {good}   <- convention-following, should LAND")
    say(f"           {bad}   <- non-convention, should be REJECTED")
    say()
    say("  Nobody is at the keyboard. pexpect owns the pty and answers the")
    say("  Form A prompt exactly as the developer would.")

    try:
        wd = H.make_workdir()
        script = ceremony_script(repo, issue, good, bad)
        run = H.spawn_rein_run(
            ["bash", "-c", script],
            workdir=wd,
            env=env,
            extra_env={"REIN_SESSION_FILE": _pinned_session(repo)},
        )

        # Block until the declare fires the prompt on the host tty, then answer
        # it the way a human does: type the DISPLAYED issue number.
        run.expect_prompt(timeout=180)
        run.answer(str(issue))
        run.expect_approved(timeout=60)
        run.child.expect(r"SBX_SCRIPT_DONE", timeout=240)
        run.wait(timeout=120)

        text = run.text()

        banner("(a) WHAT THE AGENT SEES  — in-sandbox: no tty, no token, no prompt")
        for ln in agent_view(text):
            say(f"  | {ln}")

        banner("(b) WHAT THE HUMAN SEES  — host tty: the Form A prompt")
        for ln in host_view(text):
            say(f"  | {ln}")
        say()
        say("  ^ title/state/home-repo are FETCHED from GitHub before the prompt")
        say("    renders (decision E) — that is what makes a wrong-but-plausible")
        say("    issue number visibly wrong to the human.")

        banner("OUTCOMES  (in-sandbox exit codes, then GitHub ground truth)")
        say(f"  phase 1  push BEFORE declare        rc={rc(text, 1)}  (nonzero: writes locked)")
        say(f"  phase 2  rein declare {issue:<15} rc={rc(text, 2)}  (zero: human confirmed)")
        say(f"  phase 3  push agent/{issue}/<nonce>{'':<7} rc={rc(text, 3)}  (zero: verified push)")
        say(f"  phase 4  push non-convention ref     rc={rc(text, 4)}  (nonzero: ref cross-check)")
        say()
        say(f"  Form A prompts fired this run: {run.prompt_count()} "
            "(one confirmation covers the whole run)")
        say()
        say("  On GitHub right now:")
        for br in branches:
            exists = H.branch_exists(repo, br, env)
            verdict = "LANDED " if exists else "ABSENT "
            say(f"    [{verdict}] {br}")

        say()
        say("  The push that landed is the one that BOTH the human confirmed AND")
        say("  matched agent/<issue>/<nonce>. Everything else was denied at the")
        say("  proxy — the agent held no credential at any point.")
        return 0

    finally:
        if run is not None:
            try:
                run.child.close(force=True)
            except Exception:
                pass
        banner("cleanup")
        for br in branches:
            ok = H.delete_branch(repo, br, env)
            say(f"  branch {br}: {'deleted' if ok else 'nothing to delete'}")
        if ours:
            close_issue(repo, issue, env)
            say(f"  issue  #{issue}: closed")
        else:
            say(f"  issue  #{issue}: left open (supplied via {ISSUE_ENV})")


def _pinned_session(repo: str) -> str:
    """A temp repo-only session, so the demo never depends on the machine's
    ambient ~/.config/rein/dev-session.yaml (and never writes an `issue:` — #35
    retired that field; the issue is agent-declared at runtime)."""
    import tempfile

    d = tempfile.mkdtemp(prefix="rein-demo-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_demo_ceremony\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


if __name__ == "__main__":
    sys.exit(main())
