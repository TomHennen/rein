"""write_ceremony — the #35 write ceremony (declare -> confirm -> verified push).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: the in-sandbox script tags EVERY line it emits with `SBX| `
(reinharness.SBX_TAG) — git's own output is piped through `tr` so even progress redraws
stay tagged — and a reader/assertion splits agent-vs-host views by the tag alone, no
content heuristics (the #72 fix). The host prompt is rein's own untagged tty output.
"""

from __future__ import annotations

import os
import sys
import tempfile

import pexpect

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"
ISSUE_ENV = "REIN_DEMO_ISSUE"

# --------------------------------------------------------------------------
# The in-sandbox agent script — every step emits a tagged sentinel
# --------------------------------------------------------------------------


def ceremony_script(repo: str, issue: int, good: str, bad: str) -> str:
    """A `bash -c` body run as the nono-sandboxed child. It cannot be puppeted
    line-by-line (it is one sandboxed process), so instead each STEP emits a
    tagged `@PHASE..` sentinel and the test asserts on those IN SEQUENCE — the run
    reads as expect->act->expect even though the child runs once.

    Commands go through `run` (reinharness.sandbox_preamble): it echoes each one
    as `SBX| $ <command>` and then tags its output, so the transcript interleaves
    command -> output -> command like a real terminal session. The @PHASE.. lines
    (via `emit`) stay as the human-readable "what step is this" labels.

    The clone and pushes pass `--progress` ON PURPOSE. `run` pipes git's stderr
    (`2>&1 |`) so it can tag each line, which means git no longer sees a tty and
    would auto-SUPPRESS its transfer meter; `--progress` forces the real chatter
    (remote: Enumerating/Counting/Compressing/Total, Receiving/Resolving objects)
    out anyway. The golden KEEPS those lines with counts normalized to <N> at
    compare time; only the sub-100% redraw ticks are dropped. A full (non-shallow)
    clone is used so the chatter is representative, not a trivial `Total 3`.
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"
rm -rf repo
run git clone --progress https://github.com/{repo} repo
cd repo || {{ emit "@CLONE_FAIL"; exit 3; }}
emit "@CLONE_OK  (reads flow with no declaration at all)"

emit "@PHASE1_START  push BEFORE declare (expect: locked, no prompt)"
echo "phase 1" >> probe-1.txt
run git add -A
run git commit -q -m "ceremony: pre-declaration write attempt"
run git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE1_RC=$?"

emit "@PHASE2_START  rein declare {issue} (blocks for the human)"
run rein declare {issue}
emit "@PHASE2_RC=$?"

emit "@PHASE3_START  push agent/{issue}/<nonce> (expect: lands)"
run git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE3_RC=$?"

emit "@PHASE4_START  push a non-convention ref (expect: rejected)"
run git push --progress origin HEAD:refs/heads/{bad}
emit "@PHASE4_RC=$?"
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def _rc(child_match) -> int:
    return int(child_match.group(1))


def run_ceremony(env, repo, issue, title):
    """Drive the live run; return (transcript_text, rcs, prompts, landed, branches).

    Runs on the default (nono) sandbox — `spawn_rein_run` invokes bare
    `rein run -- …`, which is nono since the P3 cutover."""
    good = f"agent/{issue}/{H.unique_branch('cerem')}"
    bad = H.unique_branch("cerem-nonconvention")
    branches = [good, bad]

    wd = H.make_workdir()
    script = ceremony_script(repo, issue, good, bad)
    session = _pinned_session(repo)
    # REIN_APPROVAL=tty forces the inline /dev/tty Form A prompt pexpect drives —
    # and, when the journey is run from inside a tmux session, keeps the host-side
    # approval OFF the tmux-popup surface (which would otherwise open a popup in the
    # operator's live tmux). The golden is the inline prompt, so this is a no-op on
    # the transcript and pure robustness.
    extra_env = {"REIN_SESSION_FILE": session, "REIN_APPROVAL": "tty"}
    run = H.spawn_rein_run(
        ["bash", "-c", script], workdir=wd, env=env,
        extra_env=extra_env,
    )

    rcs: dict[int, int] = {}
    try:
        # expect -> act -> expect, one step at a time (issue #72 review). A
        # pexpect EOF/TIMEOUT here means a live step didn't happen — most often a
        # transient clone failure (GitHub's installation-token mint has secondary
        # rate limits; see CLAUDE.md). Catch it and return the PARTIAL rcs so
        # main() reports a clean "ceremony broke" (exit 2) with the transcript,
        # instead of crashing with a pexpect traceback that a runner would
        # mislabel as golden drift.
        run.child.expect(r"@CLONE_OK", timeout=180)

        run.child.expect(r"@PHASE1_START", timeout=60)
        run.child.expect(r"@PHASE1_RC=(\d+)", timeout=120)
        rcs[1] = _rc(run.child.match)

        run.child.expect(r"@PHASE2_START", timeout=30)
        # the declare BLOCKS -> the Form A prompt fires on the host tty
        run.expect_prompt(timeout=120)
        run.answer(str(issue))                       # type the DISPLAYED number
        run.expect_approved(timeout=60)
        run.child.expect(r"@PHASE2_RC=(\d+)", timeout=60)
        rcs[2] = _rc(run.child.match)

        run.child.expect(r"@PHASE3_START", timeout=30)
        run.child.expect(r"@PHASE3_RC=(\d+)", timeout=120)
        rcs[3] = _rc(run.child.match)

        run.child.expect(r"@PHASE4_START", timeout=30)
        run.child.expect(r"@PHASE4_RC=(\d+)", timeout=120)
        rcs[4] = _rc(run.child.match)

        run.child.expect(r"@SCRIPT_DONE", timeout=60)
        run.wait(timeout=120)
    except (pexpect.EOF, pexpect.TIMEOUT):
        # Partial rcs -> main()'s invariant check fails -> exit 2. The captured
        # transcript (which will show e.g. @CLONE_FAIL) is still returned.
        pass
    finally:
        try:
            run.child.close(force=True)
        except Exception:
            pass

    prompts = run.prompt_count()
    landed = {br: H.branch_exists(repo, br, env) for br in branches}
    return run.text(), rcs, prompts, landed, branches


def main(golden=GOLDEN) -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    supplied = os.getenv(ISSUE_ENV)
    ours = not supplied
    if supplied:
        issue = int(supplied)
        title = H.issue_title(repo, issue, env)
    else:
        title = "rein journey: write-ceremony walkthrough (safe to close)"
        issue = H.create_issue(
            repo, title,
            "Opened by journeys/write_ceremony/journey.py to demonstrate the "
            "#35 declare -> confirm -> verified-push ceremony. Throwaway repo only; "
            "closed again when the journey ends.",
            env,
        )

    print(f"journey: write ceremony on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'})", flush=True)

    branches: list[str] = []
    try:
        text, rcs, prompts, landed, branches = run_ceremony(env, repo, issue, title)

        # 1) The ceremony itself must hold — independent of the golden.
        good = next(b for b in branches if b.startswith("agent/"))
        bad = next(b for b in branches if not b.startswith("agent/"))
        invariants = [
            (rcs.get(1, 0) != 0, "phase 1 (pre-declaration push) must FAIL — writes locked"),
            (rcs.get(2) == 0, "phase 2 (declare) must succeed after confirmation"),
            (rcs.get(3) == 0, "phase 3 (verified push) must succeed"),
            (rcs.get(4, 0) != 0, "phase 4 (non-convention ref) must be REJECTED"),
            (prompts == 1, "exactly one Form A prompt for the run"),
            (landed.get(good) is True, "the convention-following branch must LAND"),
            (landed.get(bad) is False, "the non-convention branch must NOT land"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("CEREMONY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  rcs={rcs} prompts={prompts} landed={landed}", flush=True)
            return 2

        # 2) Build the RAW transcript (real values) and compare NORMALIZED.
        raw = H.build_raw_transcript(text)
        print()
        print(raw, flush=True)  # what actually happened, real numbers and all
        # The GitHub ground truth is asserted above (exit 2) and echoed here for
        # the human; it is NOT part of the golden, which is the terminal capture.
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        for ph, meaning in ((1, "writes locked"), (2, "human confirmed"),
                            (3, "verified push"), (4, "ref cross-check")):
            print(f"  phase {ph}  rc={rcs[ph]}  ({meaning})", flush=True)
        print(f"  Form A prompts fired: {prompts}", flush=True)
        for br, ok in landed.items():
            print(f"  branch {br}: {'LANDED' if ok else 'ABSENT'}", flush=True)

        # The normalization LENS, on demand (Tom's option): see the form the
        # comparator actually diffs, to spot anything unexpected.
        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(golden, raw)  # store RAW
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(golden, raw)  # normalizes BOTH sides
        if ok:
            print(f"[golden OK] fresh run matches {golden} (normalized)", flush=True)
            return 0
        # Show the NORMALIZED diff (meaningful change, not noise), and drop the
        # RAW fresh transcript to a scratch path so the human can eyeball reality
        # and, if intended, adopt it with REIN_UPDATE_GOLDEN=1.
        scratch = os.path.join(tempfile.gettempdir(), "write_ceremony.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {golden} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        for br in branches:
            H.delete_branch(repo, br, env)
        if ours:
            H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print("cleanup: branches deleted" + ("; issue closed" if ours else ""), flush=True)


def _pinned_session(repo: str) -> str:
    """A temp repo-only session, so the journey never depends on the machine's
    ambient dev-session.yaml and never writes an `issue:` (#35 retired it)."""
    import tempfile

    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_ceremony\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


if __name__ == "__main__":
    sys.exit(main())
