"""journey_write_ceremony — THE WRITE CEREMONY (declare -> confirm -> verified push).

This is ONE journey. For what a journey IS, the golden-transcript rule, and how to
author the next one, read tests/interactive/CLAUDE.md — none of that lives here.

The write ceremony is rein's #35 core loop:

    agent declares an issue  ->  a human confirms it on the terminal  ->  the
    verified push to agent/<issue>/<nonce> lands (and nothing else does).

The whole security argument lives in the GAP between two views of one terminal:

  * the AGENT's view (in-sandbox: no tty, no token) — a pre-declaration push
    denied by the proxy, `rein declare <n>`, a verified push that SUCCEEDS, and a
    non-convention ref that is REJECTED even though writes are now unlocked; and
  * the HUMAN's view (host tty) — the Form A prompt carrying the issue title,
    state and HOME repo FETCHED from GitHub (decision E), then `[approved]`.

DELIVERABLE: a normalized, human-reviewable transcript checked in at
`golden/write_ceremony.txt`. Running this journey rebuilds that transcript from a
LIVE run and compares:

    python3 tests/interactive/journey_write_ceremony.py          # exit 0 == matches golden
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_write_ceremony.py   # regenerate
    REIN_DEMO_ISSUE=<n>  python3 tests/interactive/journey_write_ceremony.py   # reuse an issue

Exit 0 = ceremony held AND transcript matches the golden. Exit 1 = golden drift
(re-review the journey). Exit 2 = the ceremony itself broke.

HOW THE TWO VIEWS ARE SPLIT (the #72 fix): the in-sandbox script tags EVERY line
it emits with `SBX| ` (reinharness.SBX_TAG); git's own output is piped through
`tr '\r' '\n' | ...tag` so even progress redraws stay tagged. reinharness.get_views
then splits by the tag alone — no content heuristics. The host prompt is rein's
own untagged output on the tty.

SELF-CONTAINED: creates its own throwaway issue via gh, and in a `finally`
deletes both branches and closes the issue. Touches only the throwaway
(hard-constraint #1). The repo is resolved the rein-init way
(reinharness.resolve_throwaway_repo): REIN_JOURNEY_REPO, else the configured
dev-session, else the legacy REIN_TEST_REPO_A shortcut.
"""

from __future__ import annotations

import os
import sys

import reinharness as H

GOLDEN_NAME = "write_ceremony.txt"
ISSUE_ENV = "REIN_DEMO_ISSUE"

# --------------------------------------------------------------------------
# The in-sandbox agent script — every step emits a tagged sentinel
# --------------------------------------------------------------------------


def ceremony_script(repo: str, issue: int, good: str, bad: str) -> str:
    """A `bash -c` body run as the srt child. It cannot be puppeted line-by-line
    (it is one sandboxed process), so instead each STEP emits a tagged `@PHASE..`
    sentinel and the test asserts on those IN SEQUENCE — the run reads as
    expect->act->expect even though the child runs once.

    `runtagged` pipes a command's combined output through `tr '\\r' '\\n'` (so
    git's carriage-return progress redraws become real lines) and prefixes every
    line with SBX_TAG, preserving the command's own exit code via PIPESTATUS.

    The clone and pushes pass `--progress` ON PURPOSE. `runtagged` pipes git's
    stderr (`2>&1 |`) so it can tag each line, which means git no longer sees a
    tty and would auto-SUPPRESS its transfer meter; `--progress` forces the real
    chatter (remote: Enumerating/Counting/Compressing/Total, Receiving/Resolving
    objects) out anyway. The golden KEEPS those lines with counts normalized to
    <N> (PR #72, Tom's line-53 note); only the sub-100% redraw ticks are dropped.
    A full (non-shallow) clone is used so the chatter is representative, not a
    trivial `Total 3`.
    """
    tag = H.SBX_TAG
    return f"""
set +e
emit() {{ printf '%s%s\\n' '{tag}' "$*"; }}
runtagged() {{
  "$@" 2>&1 | tr '\\r' '\\n' | while IFS= read -r l; do printf '%s%s\\n' '{tag}' "$l"; done
  return ${{PIPESTATUS[0]}}
}}
cd "$0"
rm -rf repo
runtagged git clone --progress https://github.com/{repo} repo
cd repo || {{ emit "@CLONE_FAIL"; exit 3; }}
emit "@CLONE_OK  (reads flow with no declaration at all)"

emit "@PHASE1_START  push BEFORE declare (expect: locked, no prompt)"
echo "phase 1" >> probe-1.txt
runtagged git add -A
runtagged git commit -q -m "ceremony: pre-declaration write attempt"
runtagged git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE1_RC=$?"

emit "@PHASE2_START  rein declare {issue} (blocks for the human)"
runtagged rein declare {issue}
emit "@PHASE2_RC=$?"

emit "@PHASE3_START  push agent/{issue}/<nonce> (expect: lands)"
runtagged git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE3_RC=$?"

emit "@PHASE4_START  push a non-convention ref (expect: rejected)"
runtagged git push --progress origin HEAD:refs/heads/{bad}
emit "@PHASE4_RC=$?"
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# Golden transcript assembly (FULL-NORMALIZED, default-keep — PR #72 ruling)
# --------------------------------------------------------------------------


def journey_subs(repo: str, issue: int, title: str) -> list[tuple[str, str]]:
    """Exact-string swaps for THIS run's known volatiles, applied before the
    generic rules. Every issue rule is BOUNDED (#, agent/, declare, (), > ) so
    there is deliberately no bare `{issue}` swap — `rc=128` etc. are never hit.
    """
    return [
        (title, "<TITLE>"),
        (repo, "<REPO>"),
        (f"agent/{issue}/", "agent/<ISSUE>/"),
        (f"#{issue}", "#<ISSUE>"),
        (f"declare {issue}", "declare <ISSUE>"),
        (f"({issue})", "(<ISSUE>)"),
        (f"> {issue}", "> <ISSUE>"),
    ]


def build_transcript(text: str, *, repo: str, issue: int, title: str) -> str:
    """The golden: the FULL terminal transcript, default-keep + normalize-noise.

    Every line that appeared on the pty survives EXCEPT normalized volatile
    tokens and dropped progress ticks (H.build_golden_transcript). So a brand-new
    rein line — e.g. a new `rein: …` security notice — lands in the diff and
    trips drift on re-review. The interleaved `SBX| `-tagged agent output and
    rein's untagged host prompt ARE the two views, inline; no separate curation.
    """
    return H.build_golden_transcript(text, journey_subs(repo, issue, title))


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def _rc(child_match) -> int:
    return int(child_match.group(1))


def run_ceremony(env, repo, issue, title):
    """Drive the live run; return (transcript_text, rcs, prompts, landed, branches)."""
    good = f"agent/{issue}/{H.unique_branch('cerem')}"
    bad = H.unique_branch("cerem-nonconvention")
    branches = [good, bad]

    wd = H.make_workdir()
    script = ceremony_script(repo, issue, good, bad)
    session = _pinned_session(repo)
    run = H.spawn_rein_run(
        ["bash", "-c", script], workdir=wd, env=env,
        extra_env={"REIN_SESSION_FILE": session},
    )

    rcs: dict[int, int] = {}
    try:
        # expect -> act -> expect, one step at a time (issue #72 review).
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
    finally:
        try:
            run.child.close(force=True)
        except Exception:
            pass

    prompts = run.prompt_count()
    landed = {br: H.branch_exists(repo, br, env) for br in branches}
    return run.text(), rcs, prompts, landed, branches


def main() -> int:
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
            "Opened by tests/interactive/journey_write_ceremony.py to demonstrate the "
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

        # 2) Build the full-normalized transcript (the golden) + compare.
        transcript = build_transcript(text, repo=repo, issue=issue, title=title)
        print()
        print(transcript, flush=True)
        # The GitHub ground truth is asserted above (exit 2) and echoed here for
        # the human; it is NOT part of the golden, which is the terminal capture.
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        for ph, meaning in ((1, "writes locked"), (2, "human confirmed"),
                            (3, "verified push"), (4, "ref cross-check")):
            print(f"  phase {ph}  rc={rcs[ph]}  ({meaning})", flush=True)
        print(f"  Form A prompts fired: {prompts}", flush=True)
        for br, ok in landed.items():
            print(f"  branch {br}: {'LANDED' if ok else 'ABSENT'}", flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN_NAME, transcript)
            print(f"[golden UPDATED] {p}", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, transcript)
        if ok:
            print("[golden OK] live run matches golden/write_ceremony.txt", flush=True)
            return 0
        print("[golden DRIFT] live run != golden/write_ceremony.txt — re-review this journey:", flush=True)
        print(diff, flush=True)
        print("(regenerate with REIN_UPDATE_GOLDEN=1 once you've confirmed the change is intended)", flush=True)
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
