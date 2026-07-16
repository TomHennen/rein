"""sandbox_gh_read_staleness — the #95 regression guard (load-bearing).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: the seed MUST be a REAL A-scoped gh-read token (minted via the `seedghread`
helper, as cmd/rein/issue95_live_test.go does), NOT a garbage string: a garbage token
401s everywhere and proves nothing about SCOPE; a genuine A-only token 200s on A and 404s
on B — that asymmetry is the bug. The state dir is pinned via XDG_STATE_HOME so the seed
lands where the run's cache glob looks. The happy-path sibling that does NOT guard #95 is
journeys/multi_repo/journey.py.
"""

from __future__ import annotations

import os
import re
import shutil
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"
FIXTURE_TITLE = "rein journey: gh-read staleness fixture (safe to close)"


def agent_script(repo_a: str, issue_a: int, good_a: str,
                 repo_b: str, issue_b: int, good_b: str) -> str:
    """A single `bash -c` body run as the srt child for the WHOLE run: clone BOTH
    repos (reads flow, no declaration), then for each repo in turn `rein declare
    <issue> --repo <repo>` (BLOCKS on the host /dev/tty Form A; pexpect answers it)
    and push a real branch (via rein's per-run injecting proxy) that must LAND.

    B is the SECOND declared repo — the exact surface #95 broke: pre-fix its
    metadata fetch reuses the seeded A-only token and 404s. It cannot be puppeted
    line-by-line (one `bash -c` process), so each step emits a tagged `@PHASE..`
    sentinel and the test asserts on those in sequence. Commands go through `run`
    (reinharness.sandbox_preamble): it echoes each as `SBX| $ <command>` then tags
    its output, so the transcript reads like a real terminal. The clones omit
    `--progress` (piped, git suppresses its meter; `@READ_x_OK` proves the read);
    the pushes pass `--progress` so the golden shows the real "it landed" transfer
    chatter (counts normalized to <N> at compare time)."""
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
echo "gh-read staleness journey: real agent work in repo A" >> agent-note.txt
run git add -A
run git commit -q -m "gh-read staleness journey: real agent work (repo A)"
run git push --progress origin HEAD:refs/heads/{good_a}
emit "@PUSH_A_RC=$?"
cd "$0"

emit "@DECLARE_B_START  rein declare {issue_b} --repo {repo_b}  (2nd issue: the #95 surface — pre-fix this 404s reusing the stale A-only token)"
run rein declare {issue_b} --repo {repo_b}
emit "@DECLARE_B_RC=$?"

emit "@PUSH_B_START  push agent/{issue_b}/<nonce> -> {repo_b} (expect: LANDS on B)"
cd repoB
echo "gh-read staleness journey: real agent work in repo B" >> agent-note.txt
run git add -A
run git commit -q -m "gh-read staleness journey: real agent work (repo B)"
run git push --progress origin HEAD:refs/heads/{good_b}
emit "@PUSH_B_RC=$?"
emit "@SCRIPT_DONE"
"""


def _two_repo_session(repo_a: str, repo_b: str) -> str:
    """A temp session scoping BOTH repos, selected via REIN_SESSION_FILE so the
    journey never depends on the machine's ambient dev-session.yaml. The static
    2-repo `repos:` list makes B in-ceiling (so declare B is the plain 'ALSO work'
    confirm, NOT a scope expansion) — exactly the shape #95 broke."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(
            "id: sess_journey_gh_read_staleness\n"
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
    env = dict(H.rein_env())
    repo_a = H.resolve_throwaway_repo(env)   # rein-init way first; #40
    repo_b = H.throwaway_repo_b(env)         # REIN_TEST_REPO_B (same owner as A)
    H.build_binaries(env)                    # also builds bin/seedghread

    # A per-run temp state dir, pinned via XDG_STATE_HOME so (a) the run starts
    # clean (only the seed lives in its gh-read cache) and (b) we KNOW where the
    # legacy untagged cache path is, to plant the stale token where the run's
    # ReadCacheGlob looks. Its /tmp/rein-... path normalizes to <TMP>, so the
    # golden stays deterministic. Torn down in `finally`.
    state_home = tempfile.mkdtemp(prefix="rein-journey-state-")
    env["XDG_STATE_HOME"] = state_home

    good_a = f"agent/{{issue}}/{H.unique_branch('stale-a')}"
    good_b = f"agent/{{issue}}/{H.unique_branch('stale-b')}"

    created: list[tuple[str, int]] = []
    branches: list[tuple[str, str]] = []
    wd: str | None = None
    session: str | None = None
    try:
        issue_a = H.create_issue(
            repo_a, FIXTURE_TITLE,
            "Opened by journeys/sandbox_gh_read_staleness/journey.py — the "
            "#95 regression guard (this is repo A). Throwaway; closed when done.", env,
        )
        created.append((repo_a, issue_a))
        issue_b = H.create_issue(
            repo_b, FIXTURE_TITLE,
            "Opened by journeys/sandbox_gh_read_staleness/journey.py — the "
            "#95 regression guard (this is repo B, the SECOND declare). Throwaway; "
            "closed when done.", env,
        )
        created.append((repo_b, issue_b))

        good_a = good_a.format(issue=issue_a)
        good_b = good_b.format(issue=issue_b)
        branches = [(repo_a, good_a), (repo_b, good_b)]

        # SEED THE STALE, REPO-A-ONLY, REAL gh-read token at the LEGACY untagged
        # cache path — the leftover a prior single-repo-A run wrote. Pre-fix the
        # broker serves this for declare B and 404s; post-fix it MISSES (the run
        # reads a scope-tagged sibling) and re-mints at [A,B].
        seed_path = H.legacy_gh_read_cache_path(env)
        H.seed_legacy_gh_read_token(repo_a, seed_path, env)
        print(f"seeded stale A-only gh-read token at legacy path {seed_path}", flush=True)

        wd = H.make_workdir()
        session = _two_repo_session(repo_a, repo_b)

        print(f"journey: #95 guard — seeded A-only token, then one sandboxed [A,B] run  "
              f"A={repo_a} (#{issue_a})  B={repo_b} (#{issue_b})", flush=True)

        result = H.run_journey(
            [
                H.JourneyStep(
                    argv=["run", "--", "bash", "-c",
                          agent_script(repo_a, issue_a, good_a, repo_b, issue_b, good_b), wd],
                    # ordered: the 1st Form A is issue A, the 2nd (the ALSO-work
                    # confirm) is issue B — each answered with its OWN displayed
                    # number. Pre-fix the 2nd never renders (declare B 404s at
                    # fetch); run_journey then hits EOF and reports reached_eof
                    # False, and @DECLARE_B_RC is non-zero — the guard trips.
                    answers=[(H.PROMPT_HINT, str(issue_a)), (H.PROMPT_HINT, str(issue_b))],
                    label=f"rein run -- bash -c <clone+declare+push A and B> {wd}",
                    extra_env={
                        "REIN_SANDBOX_WORKDIR": wd,
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
        also_prompts = text.count("wants to ALSO work on an issue") # 2nd declare header (needs B fetched)
        repo_expansions = text.count(H.EXPANSION_BANNER)           # AddRepo prompt (must be 0)
        # The pre-fix #95 failure message the broker returns when the stale A-only
        # token 404s on B (internal/declare declare.go): "issue #<n> not found in B".
        not_found_b = f"not found in {repo_b}"

        landed_a = H.branch_exists(repo_a, good_a, env)
        landed_b = H.branch_exists(repo_b, good_b, env)

        # 1) THE #95 GUARD (independent of the golden). These are exactly the
        # surfaces the bug breaks; pre-fix they fail and the journey goes red.
        invariants = [
            (result.reached_eof, "the run must complete (both Form A prompts answered, no timeout) — pre-fix, declare B 404s and the 2nd never renders"),
            (read_a == 0, "leg A: clone of repo A succeeds (reads flow with no declaration)"),
            (read_b == 0, "leg B: clone of repo B succeeds (the 2nd session repo is readable too)"),
            (decl_a == 0, "declare against repo A succeeds (1st issue, plain confirm)"),
            (decl_b == 0, "GUARD: declare against repo B succeeds — B's issue metadata was FETCHED (pre-fix this 404s on the stale A-only token)"),
            (also_prompts == 1, "GUARD: the SECOND Form A rendered (the 'wants to ALSO work on an issue' header carrying issue B — only reachable once B's issue is fetched)"),
            (push_b == 0, "verified push to repo B succeeds"),
            (landed_b is True, "GUARD: the branch pushed to repo B actually LANDED on GitHub"),
            (push_a == 0, "verified push to repo A succeeds"),
            (landed_a is True, "the branch pushed to repo A actually LANDED on GitHub"),
            (plain_prompts == 1, "exactly ONE plain 'agent declares work on an issue' prompt (the 1st issue)"),
            (repo_expansions == 0, "NO 'SCOPE EXPANSION requested' (AddRepo) prompt — BOTH repos are STATICALLY in scope"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("#95 GUARD TRIPPED (declare B / second Form A / push-to-B failed — a #95 regression, or a broken seed):", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  read_a={read_a} read_b={read_b} decl_a={decl_a} decl_b={decl_b} "
                  f"push_a={push_a} push_b={push_b} landed_a={landed_a} landed_b={landed_b} "
                  f"plain={plain_prompts} also={also_prompts} repo_expansions={repo_expansions} "
                  f"seed_404_on_B={'yes' if not_found_b in text else 'no'}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # 2) The golden IS the complete captured session. Outcomes asserted above.
        print()
        print(text, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  seeded stale token: A-only gh-read at the legacy untagged cache path", flush=True)
        print(f"  reads:  A rc={read_a}  B rc={read_b}", flush=True)
        print(f"  declares: A rc={decl_a} (plain)  B rc={decl_b} (ALSO-work, #95 surface)", flush=True)
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
        scratch = os.path.join(tempfile.gettempdir(), "sandbox_gh_read_staleness.fresh.txt")
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
        shutil.rmtree(state_home, ignore_errors=True)
        if wd:
            shutil.rmtree(wd, ignore_errors=True)
        if session:
            shutil.rmtree(os.path.dirname(session), ignore_errors=True)
        print(f"cleanup: {len(branches)} branch(es) deleted; {len(created)} issue(s) closed; "
              f"temp state dir + workdir + session dir removed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
