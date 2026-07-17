"""scope_expansion — declare a repo OUTSIDE scope -> approve -> push to it (#69).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: the persist `[y/N]` is answered N (run-only) — the deterministic default; a
`y` would MUTATE the session file and is exercised by test_scope_expansion.py. The fixture
issue on repo B is LONG-LIVED (ensure_fixture_issue finds/reopens/creates and leaves it
OPEN) because the golden bakes its number + title RAW and un-normalized. The
install-not-present sibling is journeys/expansion_404/journey.py.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile

import pexpect

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"
ISSUE_B_ENV = "REIN_ITEST_ISSUE_B"

# The long-lived fixture issue's exact title on repo B. Stable by construction so
# the golden (which stores it RAW and un-normalized) matches verbatim across runs.
FIXTURE_TITLE = "rein journey: scope-expansion fixture (safe to close)"
FIXTURE_BODY = (
    "Long-lived fixture for journeys/scope_expansion/journey.py — the "
    "#69 scope-expansion journey declares THIS issue (which lives in repo B, "
    "OUTSIDE the session's repo-A scope) to fire the SCOPE EXPANSION prompt. Kept "
    "OPEN and reused across runs so the golden's issue number + title stay "
    "stable-real. Safe to close: the journey reopens or recreates it as needed."
)


# --------------------------------------------------------------------------
# The in-sandbox agent script — every step emits a tagged sentinel
# --------------------------------------------------------------------------


def expansion_script(repo_a: str, repo_b: str, issue_b: int, branch_b: str) -> str:
    """A `bash -c` body run as the srt child. Like the write ceremony's script it
    cannot be puppeted line-by-line (one sandboxed process), so each STEP emits a
    tagged `@PHASE..` sentinel and the test asserts on those IN SEQUENCE — the run
    reads as expect->act->expect even though the child runs once.

    Commands go through `run` (reinharness.sandbox_preamble): it echoes each one
    as `SBX| $ <command>` and then tags its output, so the transcript interleaves
    command -> output -> command like a real terminal session. The @PHASE.. lines
    (via `emit`) stay as the human-readable "what step is this" labels. `run`
    preserves the command's own exit code via PIPESTATUS. The clone/push pass
    `--progress` ON PURPOSE (git suppresses its meter once stderr is a pipe, not a
    tty); the golden keeps those lines with counts normalized.

    Repo B has NO sandbox bind mount of its own — the binds are fixed at launch
    (#64) and no mid-run approval can make a new path writable — so B is cloned
    into the child's writable TMPDIR (rein's per-run agentTmp), NOT nested inside
    A's working tree. That is exactly what rein's approve-message steers the agent
    to do (declare.expansionApprovedMessage: "clone into $HOME/ or $TMPDIR").
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"
rm -rf repo
run git clone --progress https://github.com/{repo_a} repo
cd repo || {{ emit "@CLONE_FAIL"; exit 3; }}
emit "@CLONE_OK  (session is scoped to repo A only)"

emit "@PHASE1_START  rein declare {issue_b} --repo {repo_b}  (B is OUTSIDE scope: expansion prompt, blocks)"
run rein declare {issue_b} --repo {repo_b}
emit "@PHASE1_RC=$?"

emit "@PHASE2_START  clone repo B into scratch (scope grew, binds did not: use TMPDIR, not the working tree)"
scratch="${{TMPDIR:-/tmp}}/rein-expansion-b"
rm -rf "$scratch"
run git clone --progress https://github.com/{repo_b} "$scratch"
emit "@PHASE2_RC=$?"
cd "$scratch" || {{ emit "@CLONEB_FAIL"; exit 4; }}

emit "@PHASE3_START  push agent/{issue_b}/<nonce> to repo B (expect: lands)"
echo "scope-expansion probe $(date -u +%FT%TZ)" >> expansion-probe.txt
run git add -A
run git commit -q -m "scope-expansion journey: push to repo B"
run git push --progress origin HEAD:refs/heads/{branch_b}
emit "@PHASE3_RC=$?"
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def _rc(child_match) -> int:
    return int(child_match.group(1))


def run_expansion(env, repo_a, repo_b, issue_b, session_path):
    """Drive the live run; return (transcript, rcs, exp_prompts, issue_prompts, landed, branch_b)."""
    branch_b = f"agent/{issue_b}/{H.unique_branch('exp')}"
    wd = H.make_workdir()
    script = expansion_script(repo_a, repo_b, issue_b, branch_b)
    run = H.spawn_rein_run(
        ["bash", "-c", script], workdir=wd, env=env,
        # REIN_SESSION_FILE pins a repo-A-only session (so B is genuinely out of
        # scope AND cfg.SessionFile is set, which is what makes the persist [y/N]
        # question even appear). REIN_APPROVAL=tty forces the inline /dev/tty
        # prompt — pexpect IS the human — never the tmux popup (default when $TMUX).
        extra_env={"REIN_SESSION_FILE": session_path, "REIN_APPROVAL": "tty"},
    )

    rcs: dict[int, int] = {}
    try:
        # expect -> act -> expect, one step at a time. A pexpect EOF/TIMEOUT here
        # means a live step didn't happen (most often a transient clone/mint
        # failure); catch it and return PARTIAL rcs so main() reports a clean
        # "expansion broke" (exit 2) with the transcript, not a raw traceback.
        run.child.expect(r"@CLONE_OK", timeout=180)

        run.child.expect(r"@PHASE1_START", timeout=60)
        # the expansion declare BLOCKS -> the SCOPE EXPANSION prompt fires on tty
        run.expect_expansion_prompt(timeout=180)
        run.answer(str(issue_b))                     # type the DISPLAYED number
        run.expect_expansion_approved(timeout=60)
        # then the in-prompt persist question; answer N -> run-only (the golden's
        # deterministic path; a `y` would mutate the session file — plain test).
        run.expect_persist_question(timeout=30)
        run.answer("N")
        run.child.expect(r"@PHASE1_RC=(\d+)", timeout=60)
        rcs[1] = _rc(run.child.match)

        run.child.expect(r"@PHASE2_START", timeout=30)
        run.child.expect(r"@PHASE2_RC=(\d+)", timeout=180)
        rcs[2] = _rc(run.child.match)

        run.child.expect(r"@PHASE3_START", timeout=30)
        run.child.expect(r"@PHASE3_RC=(\d+)", timeout=180)
        rcs[3] = _rc(run.child.match)

        run.child.expect(r"@SCRIPT_DONE", timeout=60)
        run.wait(timeout=120)
    except (pexpect.EOF, pexpect.TIMEOUT):
        pass
    finally:
        try:
            run.child.close(force=True)
        except Exception:
            pass

    exp_prompts = run.expansion_prompt_count()
    issue_prompts = run.prompt_count()
    landed = H.branch_exists(repo_b, branch_b, env)
    return run.text(), rcs, exp_prompts, issue_prompts, landed, branch_b


def main() -> int:
    env = H.rein_env()
    repo_a = H.resolve_throwaway_repo(env)   # rein-init way first; #40
    repo_b = H.throwaway_repo_b(env)         # REIN_TEST_REPO_B (same owner as A)
    H.build_binaries(env)

    issue_b, title = ensure_fixture_issue(repo_b, env)
    session_path = _a_only_session(repo_a)

    print(f"journey: scope expansion  A={repo_a}  ->  B={repo_b}  "
          f"(fixture issue #{issue_b} {title!r})", flush=True)

    branch_b: str | None = None
    try:
        text, rcs, exp_prompts, issue_prompts, landed, branch_b = run_expansion(
            env, repo_a, repo_b, issue_b, session_path
        )

        # 1) The expansion itself must hold — independent of the golden.
        with open(session_path) as f:
            session_after = f.read()
        invariants = [
            (rcs.get(1) == 0, "phase 1 (expansion declare) must succeed after approval"),
            (rcs.get(2) == 0, "phase 2 (clone repo B) must succeed once B is in scope"),
            (rcs.get(3) == 0, "phase 3 (push to repo B) must succeed"),
            (exp_prompts == 1, "exactly one SCOPE EXPANSION prompt for the run"),
            (issue_prompts == 0, "no plain issue-declaration prompt fires (only the expansion one)"),
            (landed is True, "the agent's branch must LAND on repo B (verified on GitHub)"),
            (repo_b not in session_after, "persist=N must leave the session file unchanged (repo A only)"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("EXPANSION BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  rcs={rcs} exp_prompts={exp_prompts} issue_prompts={issue_prompts} "
                  f"landed={landed}", flush=True)
            return 2

        # 2) Build the RAW transcript (real values) and compare NORMALIZED.
        raw = H.build_raw_transcript(text)
        print()
        print(raw, flush=True)  # what actually happened, real names and numbers
        # The GitHub ground truth is asserted above (exit 2) and echoed here for
        # the human; it is NOT part of the golden (the terminal capture is).
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        for ph, meaning in ((1, "expansion declared + approved"),
                            (2, "repo B cloned (now in scope)"),
                            (3, "verified push to repo B")):
            print(f"  phase {ph}  rc={rcs[ph]}  ({meaning})", flush=True)
        print(f"  SCOPE EXPANSION prompts fired: {exp_prompts}  (plain issue prompts: {issue_prompts})", flush=True)
        print(f"  branch {branch_b} on {repo_b}: {'LANDED' if landed else 'ABSENT'}", flush=True)
        print(f"  persist=N: repo B {'NOT ' if repo_b not in session_after else ''}saved to the session file", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)   # store RAW
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN, raw)   # normalizes BOTH sides
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "scope_expansion.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        if branch_b:
            H.delete_branch(repo_b, branch_b, env)
        # The fixture issue is LONG-LIVED (stable-real number + title in the
        # golden), so it is deliberately NOT closed here.
        print(f"cleanup: repo B branch deleted; fixture issue #{issue_b} left OPEN (long-lived)", flush=True)


# --------------------------------------------------------------------------
# Fixture issue + session helpers
# --------------------------------------------------------------------------


def ensure_fixture_issue(repo_b: str, env: dict) -> tuple[int, str]:
    """Return (number, title) of a LONG-LIVED, OPEN fixture issue on repo B.

    The golden bakes the number + title RAW and un-normalized, so the fixture must
    be stable-real AND reliably OPEN (the prompt renders `[open]`; a `[closed]`
    would drift the golden). Resolution:

      1. REIN_ITEST_ISSUE_B (an explicit override) — used as-is (its real title
         is fetched; the operator owns keeping it open).
      2. an existing OPEN issue titled FIXTURE_TITLE — reused.
      3. an existing CLOSED fixture — REOPENED and reused.
      4. none — a new one is CREATED (and left open).
    """
    supplied = os.getenv(ISSUE_B_ENV)
    if supplied and supplied.isdigit():
        n = int(supplied)
        return n, H.issue_title(repo_b, n, env)

    out = subprocess.check_output(
        ["gh", "issue", "list", "--repo", repo_b, "--state", "all",
         "--search", FIXTURE_TITLE, "--json", "number,title,state", "--limit", "50"],
        text=True, env=env,
    )
    exact = [it for it in json.loads(out) if it.get("title") == FIXTURE_TITLE]
    open_ones = [it for it in exact if str(it.get("state", "")).upper() == "OPEN"]
    if open_ones:
        return min(it["number"] for it in open_ones), FIXTURE_TITLE
    if exact:
        n = min(it["number"] for it in exact)
        subprocess.run(
            ["gh", "issue", "reopen", str(n), "--repo", repo_b],
            env=env, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        return n, FIXTURE_TITLE
    n = H.create_issue(repo_b, FIXTURE_TITLE, FIXTURE_BODY, env)
    return n, FIXTURE_TITLE


def _a_only_session(repo_a: str) -> str:
    """A temp session scoped to repo A ONLY, so repo B is genuinely out of scope
    and the declare must trigger the expansion path (not a plain confirm). Written
    to a temp file selected via REIN_SESSION_FILE; never the machine's ambient
    dev-session.yaml. persist=N must leave THIS file unchanged."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-b-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(
            "id: sess_journey_expansion\n"
            "role: implement\n"
            "repos:\n"
            f"  - {repo_a}\n"
        )
    return path


if __name__ == "__main__":
    sys.exit(main())
