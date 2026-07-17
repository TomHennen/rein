"""multi_repo — one brokered token, REAL work across two repos in ONE run (B10).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code notes:
  * The clone omits `--progress` (a piped `--progress` clone's `remote: Total` line races
    the local `Receiving/Resolving` lines and reorders run-to-run); the pushes pass
    `--progress` so the golden shows the real transfer chatter (counts normalized at
    compare). Same as journeys/write_ceremony/journey.py.
  * A 2-repo session REQUIRES `--repo` on declare (resolveRepo errors on a bare
    `rein declare <n>` when >1 repo is in scope).
  * The load-bearing #95 guard is journeys/sandbox_gh_read_staleness/journey.py; an
    OUT-of-scope repo would trip the AddRepo scope-expansion prompt —
    journeys/scope_expansion/journey.py.
"""

from __future__ import annotations

import os
import re
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"


def agent_script(repo_a: str, issue_a: int, good_a: str,
                 repo_b: str, issue_b: int, good_b: str) -> str:
    """A single `bash -c` body run as the srt child for the WHOLE run:
    clone BOTH repos (reads flow with no declaration), then for each repo in turn
    `rein declare <issue> --repo <repo>` (BLOCKS on the host /dev/tty Form A; pexpect
    answers it) and push a real branch (via rein's per-run injecting proxy) that
    must LAND.

    It cannot be puppeted line-by-line (it is one `bash -c` process), so each step
    emits a tagged `@PHASE..` sentinel and the test asserts on those in sequence.
    Commands go through `run` (reinharness.sandbox_preamble): it echoes each as
    `SBX| $ <command>` then tags its output, so the transcript reads like a real
    terminal. Commit messages / file contents carry NO per-run number (they are
    echoed by `run`, so a raw issue# there would drift); the run-varying values —
    issue number and branch nonce — appear only where a normalize rule maps them.

    The clones omit `--progress` (piped, git suppresses its meter and a plain
    `Cloning into …` stays deterministic; `@READ_x_OK` proves the read). The pushes
    pass `--progress` so the real transfer chatter shows the write landing; the
    golden keeps those lines with counts normalized to <N> at compare time.
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"

emit "@READ_A_START  clone {repo_a} (read flows with no declaration)"
rm -rf repoA
run git clone https://github.com/{repo_a} repoA
emit "@READ_A_RC=$?"
[ -d repoA/.git ] && emit "@READ_A_OK"

emit "@READ_B_START  clone {repo_b} (the SECOND session repo — reads flow too)"
rm -rf repoB
run git clone https://github.com/{repo_b} repoB
emit "@READ_B_RC=$?"
[ -d repoB/.git ] && emit "@READ_B_OK"

emit "@DECLARE_A_START  rein declare {issue_a} --repo {repo_a}  (1st issue: plain confirm, in scope)"
run rein declare {issue_a} --repo {repo_a}
emit "@DECLARE_A_RC=$?"

emit "@PUSH_A_START  push agent/{issue_a}/<nonce> -> {repo_a} (expect: LANDS on A)"
cd repoA
echo "multi-repo journey: real agent work in repo A" >> agent-note.txt
run git add -A
run git commit -q -m "multi-repo journey: real agent work (repo A)"
run git push --progress origin HEAD:refs/heads/{good_a}
emit "@PUSH_A_RC=$?"
cd "$0"

emit "@DECLARE_B_START  rein declare {issue_b} --repo {repo_b}  (2nd issue mid-run: the 'ALSO work on an issue' confirm; B is already in scope, so NOT a repo scope-expansion)"
run rein declare {issue_b} --repo {repo_b}
emit "@DECLARE_B_RC=$?"

emit "@PUSH_B_START  push agent/{issue_b}/<nonce> -> {repo_b} (expect: LANDS on B)"
cd repoB
echo "multi-repo journey: real agent work in repo B" >> agent-note.txt
run git add -A
run git commit -q -m "multi-repo journey: real agent work (repo B)"
run git push --progress origin HEAD:refs/heads/{good_b}
emit "@PUSH_B_RC=$?"
emit "@SCRIPT_DONE"
"""


def _two_repo_session(repo_a: str, repo_b: str) -> str:
    """A temp session scoping BOTH repos, selected via REIN_SESSION_FILE so the
    journey never depends on the machine's ambient dev-session.yaml. This static
    2-repo `repos:` list is the whole subject of the journey."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(
            "id: sess_journey_multirepo\n"
            "role: implement\n"
            "repos:\n"
            f"  - {repo_a}\n"
            f"  - {repo_b}\n"
        )
    return path


def _named(text: str, name: str) -> int | None:
    m = re.search(re.escape(H.SBX_TAG) + rf"@{name}=(\d+)", text)
    return int(m.group(1)) if m else None


def main() -> int:
    env = H.rein_env()
    repo_a = H.resolve_throwaway_repo(env)   # rein-init way first; #40
    repo_b = H.throwaway_repo_b(env)         # REIN_TEST_REPO_B (same owner as A)
    H.build_binaries(env)

    good_a = f"agent/{{issue}}/{H.unique_branch('mr-a')}"  # filled once we have the issue#
    good_b = f"agent/{{issue}}/{H.unique_branch('mr-b')}"

    # Track every throwaway issue we open so the `finally` closes whatever got
    # created — even if the SECOND create_issue throws (which would otherwise
    # skip the try and leak the first, already-open, issue). Hard-constraint #1.
    created: list[tuple[str, int]] = []
    branches: list[tuple[str, str]] = []
    try:
        issue_a = H.create_issue(
            repo_a, "rein journey: multi-repo fixture (safe to close)",
            "Opened by journeys/multi_repo/journey.py to demonstrate ONE "
            "brokered token doing real work across two repos (this is repo A). "
            "Throwaway; closed when done.", env,
        )
        created.append((repo_a, issue_a))
        issue_b = H.create_issue(
            repo_b, "rein journey: multi-repo fixture (safe to close)",
            "Opened by journeys/multi_repo/journey.py to demonstrate ONE "
            "brokered token doing real work across two repos (this is repo B). "
            "Throwaway; closed when done.", env,
        )
        created.append((repo_b, issue_b))

        good_a = good_a.format(issue=issue_a)
        good_b = good_b.format(issue=issue_b)
        branches = [(repo_a, good_a), (repo_b, good_b)]

        wd = H.make_workdir()
        session = _two_repo_session(repo_a, repo_b)

        print(f"journey: one run, real work across BOTH repos  "
              f"A={repo_a} (#{issue_a})  B={repo_b} (#{issue_b})", flush=True)

        result = H.run_journey(
            [
                H.JourneyStep(
                    argv=["run", "--", "bash", "-c",
                          agent_script(repo_a, issue_a, good_a, repo_b, issue_b, good_b), wd],
                    # ordered: the 1st Form A is issue A, the 2nd (the ALSO-work
                    # confirm) is issue B — each answered with its OWN displayed
                    # number (the sole approval token, per repo).
                    answers=[(H.PROMPT_HINT, str(issue_a)), (H.PROMPT_HINT, str(issue_b))],
                    label=f"rein run -- bash -c <clone+declare+push A and B> {wd}",
                    extra_env={
                        "REIN_SANDBOX_WORKDIR": wd,  # bind the writable tree the child clones into
                        "REIN_SESSION_FILE": session,
                        "REIN_APPROVAL": "tty",  # force the inline /dev/tty prompt pexpect drives
                    },
                    timeout=300,  # srt launch + self-test + two clones + two declares + two pushes
                ),
            ],
            env=env,
        )
        text = result.transcript
        step_text = result.steps[0].text if result.steps else ""

        read_a, read_b = _named(step_text, "READ_A_RC"), _named(step_text, "READ_B_RC")
        decl_a, decl_b = _named(step_text, "DECLARE_A_RC"), _named(step_text, "DECLARE_B_RC")
        push_a, push_b = _named(step_text, "PUSH_A_RC"), _named(step_text, "PUSH_B_RC")

        plain_prompts = text.count(H.PROMPT_BANNER)                 # 1st declare header
        also_prompts = text.count("wants to ALSO work on an issue") # 2nd declare header
        repo_expansions = text.count(H.EXPANSION_BANNER)           # AddRepo prompt (must be 0)
        banner_scope = f"repos=[{repo_a} {repo_b}]"   # launch banner: Go %v, space-joined
        confirm_scope = f"repos=[{repo_a}, {repo_b}]" # Form A ceiling: comma-joined

        # HOST-SIDE ground truth: did each branch actually land on GitHub? (the
        # operator's own gh, verifying independently of the run's brokered path.)
        landed_a = H.branch_exists(repo_a, good_a, env)
        landed_b = H.branch_exists(repo_b, good_b, env)

        # 1) The cross-repo write must hold — independent of the golden.
        invariants = [
            (result.reached_eof, "the run must complete (both Form A prompts answered, no timeout)"),
            (text.count(banner_scope) > 0,
             f"the launch banner names the FULL static ceiling {banner_scope} (#68 gate cleared BOTH)"),
            (text.count(confirm_scope) >= 2,
             f"BOTH Form A confirms render the full ceiling {confirm_scope}"),
            (read_a == 0, "leg A: clone of repo A succeeds (reads flow with no declaration)"),
            (read_b == 0, "leg B: clone of repo B succeeds (the 2nd session repo is readable too)"),
            ("@READ_A_OK" in step_text and "@READ_B_OK" in step_text,
             "both clones produced a real checkout (.git present)"),
            (decl_a == 0, "declare against repo A succeeds (1st issue, plain confirm)"),
            (decl_b == 0, "declare against repo B succeeds (2nd issue, ALSO-work confirm)"),
            (push_a == 0, "verified push to repo A succeeds"),
            (push_b == 0, "verified push to repo B succeeds"),
            (landed_a is True, "the branch pushed to repo A actually LANDED on GitHub"),
            (landed_b is True, "the branch pushed to repo B actually LANDED on GitHub"),
            (plain_prompts == 1, "exactly ONE plain 'agent declares work on an issue' prompt (the 1st issue)"),
            (also_prompts == 1, "exactly ONE 'wants to ALSO work on an issue' prompt (the 2nd issue, same run)"),
            (repo_expansions == 0,
             "NO 'SCOPE EXPANSION requested' (AddRepo) prompt — BOTH repos are STATICALLY in scope"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("CROSS-REPO WRITE BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  read_a={read_a} read_b={read_b} decl_a={decl_a} decl_b={decl_b} "
                  f"push_a={push_a} push_b={push_b} landed_a={landed_a} landed_b={landed_b} "
                  f"plain={plain_prompts} also={also_prompts} repo_expansions={repo_expansions}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # 2) The golden IS the complete captured session. Outcomes asserted above.
        print()
        print(text, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  launch: session scoped to BOTH {repo_a} and {repo_b} (coverage gate cleared)", flush=True)
        print(f"  reads:  A rc={read_a}  B rc={read_b}", flush=True)
        print(f"  declares: A rc={decl_a} (plain)  B rc={decl_b} (ALSO-work, in-scope)", flush=True)
        print(f"  pushes: A rc={push_a}  B rc={push_b}", flush=True)
        print(f"  LANDED on GitHub: A {good_a} -> {'YES' if landed_a else 'NO'}   "
              f"B {good_b} -> {'YES' if landed_b else 'NO'}", flush=True)
        print(f"  Form A prompts: plain={plain_prompts}  also-work={also_prompts}  "
              f"repo-scope-expansion={repo_expansions}", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(text), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, text)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN, text)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "multi_repo.fresh.txt")
        with open(scratch, "w") as f:
            f.write(text)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        for r, br in branches:
            H.delete_branch(r, br, env)
        for r, n in created:
            H.close_issue(r, n, env, comment="journey complete; closing.")
        print(f"cleanup: {len(branches)} branch(es) deleted; {len(created)} issue(s) closed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
