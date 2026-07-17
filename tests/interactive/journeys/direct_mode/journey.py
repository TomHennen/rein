"""direct_mode — the #35 write ceremony UNSANDBOXED (`rein run --direct`); twin of write_ceremony.

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: `SBX| ` here tags the WRAPPED (unsandboxed) child's output — the same
convention journeys/credential_boundary/journey.py uses for its `--direct` leg — it
does NOT imply a sandbox.
"""

from __future__ import annotations

import os
import re
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"
ISSUE_ENV = "REIN_DEMO_ISSUE"


def direct_script(repo: str, issue: int, good: str) -> str:
    """A `bash -c` body run as the wrapped (UNSANDBOXED) child. Like the sandboxed
    ceremony's script it cannot be puppeted line-by-line, so each STEP emits a
    tagged `@PHASE..` sentinel and the test asserts on those IN SEQUENCE. Commands
    go through `run` (reinharness.sandbox_preamble): it echoes each one as
    `SBX| $ <command>` then tags its output, so the transcript interleaves
    command -> output -> command. `run` preserves the command's exit code via
    PIPESTATUS. `cd "$0"` enters the workdir positional rein appends.

    There is NO phase-4 non-convention push here: the ref cross-check is a PROXY
    feature (sandbox mode), and direct mode has no proxy — so the direct ceremony
    ends at the verified push landing.

    The CLONE deliberately omits `--progress` (unlike the sandboxed ceremony): a
    piped, non-tty clone with `--progress` forces the transfer chatter, whose
    `remote: Total …` line RACES the local `Receiving/Resolving …` lines and
    reorders run-to-run — genuine git nondeterminism that normalize-on-compare
    (which never reorders) cannot absorb. Without `--progress` git suppresses the
    meter entirely, leaving just `Cloning into 'repo'…`; `@CLONE_OK` still proves
    the read flowed. The PUSH keeps `--progress` (its output is stably ordered).
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"
rm -rf repo
run git clone https://github.com/{repo} repo
cd repo || {{ emit "@CLONE_FAIL"; exit 3; }}
emit "@CLONE_OK  (reads flow with no declaration — direct mode, no sandbox)"

emit "@PHASE1_START  push BEFORE declare (expect: helper placeholder -> git auth fails)"
echo "direct phase 1" >> probe-1.txt
run git add -A
run git commit -q -m "direct ceremony: pre-declaration write attempt"
run git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE1_RC=$?"

emit "@PHASE2_START  rein declare {issue} (blocks for the human on THIS terminal)"
run rein declare {issue}
emit "@PHASE2_RC=$?"

emit "@PHASE3_START  push agent/{issue}/<nonce> (expect: lands)"
run git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE3_RC=$?"
emit "@SCRIPT_DONE"
"""


def _pinned_session(repo: str) -> str:
    """A temp repo-only session, so the journey never depends on the machine's
    ambient dev-session.yaml and never writes an `issue:` (#35 retired it)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_directmode\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def _rc(step_text: str, phase: int) -> int | None:
    m = re.search(re.escape(H.SBX_TAG) + rf"@PHASE{phase}_RC=(\d+)", step_text)
    return int(m.group(1)) if m else None


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    supplied = os.getenv(ISSUE_ENV)
    ours = not supplied
    if supplied:
        issue = int(supplied)
    else:
        issue = H.create_issue(
            repo,
            "rein journey: direct-mode walkthrough (safe to close)",
            "Opened by journeys/direct_mode/journey.py to demonstrate the "
            "#35 declare -> confirm -> verified-push ceremony UNSANDBOXED "
            "(`rein run --direct`). Throwaway repo only; closed when the journey ends.",
            env,
        )

    good = f"agent/{issue}/{H.unique_branch('direct')}"
    wd = H.make_workdir()
    session = _pinned_session(repo)
    script = direct_script(repo, issue, good)

    print(f"journey: direct-mode ceremony on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'})", flush=True)

    try:
        # ONE step: the whole `rein run --direct` session. run_journey drives the
        # mid-run Form A prompt via `answers` (expect the hint, type the number),
        # then captures the COMPLETE pty session. REIN_APPROVAL=tty forces the
        # inline /dev/tty prompt (pexpect IS the human) even inside tmux;
        # GIT_TERMINAL_PROMPT=0 keeps a failed auth from ever blocking on a prompt.
        result = H.run_journey(
            [
                H.JourneyStep(
                    argv=["run", "--direct", "--", "bash", "-c", script, wd],
                    answers=[(H.PROMPT_HINT, str(issue))],
                    label="rein run --direct -- bash -c <direct-ceremony> " + wd,
                    timeout=180,
                ),
            ],
            env=env,
            extra_env={
                "REIN_SESSION_FILE": session,
                "REIN_APPROVAL": "tty",
                "GIT_TERMINAL_PROMPT": "0",
            },
        )
        text = result.transcript
        step_text = result.steps[0].text if result.steps else ""
        rc1, rc2, rc3 = (_rc(step_text, 1), _rc(step_text, 2), _rc(step_text, 3))
        prompts = step_text.count(H.PROMPT_BANNER)
        landed = H.branch_exists(repo, good, env)

        # 1) The ceremony must hold — independent of the golden (exit 2). The deny
        #    channel is pinned to DIRECT mode: the helper hint + git's own auth
        #    failure, and explicitly NOT the proxy's `remote error: rein:` ERR.
        invariants = [
            (result.reached_eof, "the run must complete (no missed prompt / timeout)"),
            (rc1 is not None and rc1 != 0, "phase 1 (pre-declaration push) must FAIL — writes locked"),
            (rc2 == 0, "phase 2 (declare) must succeed after confirmation"),
            (rc3 == 0, "phase 3 (verified push) must succeed"),
            (prompts == 1, "exactly one Form A prompt for the run"),
            (landed is True, "the agent/<issue>/<nonce> branch must LAND (verified on GitHub)"),
            ("no issue declared for this run — writes are locked" in step_text,
             "the deny is the DIRECT-MODE helper hint (printDeclareHint), naming rein declare"),
            ("Authentication failed" in step_text,
             "git's OWN auth failure fires (placeholder credential rejected by GitHub)"),
            ("remote error: rein:" not in step_text,
             "NOT the proxy pkt-line ERR — direct mode has no proxy (#45/#35 helper channel)"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("DIRECT CEREMONY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  rc1={rc1} rc2={rc2} rc3={rc3} prompts={prompts} landed={landed}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # 2) The golden IS the complete captured session; outcomes are asserted
        #    above and echoed here only (not baked into the golden).
        print()
        print(text, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        for ph, rc, meaning in ((1, rc1, "writes locked (helper placeholder)"),
                                (2, rc2, "human confirmed on /dev/tty"),
                                (3, rc3, "verified push landed")):
            print(f"  phase {ph}  rc={rc}  ({meaning})", flush=True)
        print(f"  Form A prompts fired: {prompts}", flush=True)
        print(f"  branch {good}: {'LANDED' if landed else 'ABSENT'}", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(text), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, text)  # store RAW
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN, text)  # normalizes BOTH sides
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "direct_mode.fresh.txt")
        with open(scratch, "w") as f:
            f.write(text)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        H.delete_branch(repo, good, env)
        if ours:
            H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print("cleanup: branch deleted" + ("; issue closed" if ours else ""), flush=True)


if __name__ == "__main__":
    sys.exit(main())
