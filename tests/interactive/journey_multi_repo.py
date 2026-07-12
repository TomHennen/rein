"""journey_multi_repo — ONE BROKERED TOKEN, REAL WORK ACROSS TWO REPOS (B10).

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

The story this journey proves is the actual MULTI-REPO user path, in a SINGLE
`rein run`: a session statically scoped to TWO same-owner, App-installed throwaway
repos (A and B) brokers ONE run that does REAL work in BOTH repos — reads from each,
declares an issue on each, and pushes a branch (via rein's brokered write token,
through the per-run credential helper) that LANDS on each. (An earlier version was
really "two single-repo runs sharing a session file": each `rein run` cloned ONE
repo, declared ONE issue, and pushed nothing — so no single run ever used >1 repo,
and the multi-repo claim went untested. Tom's review, option (b): show the real path
in ONE run. This is it.)

  * LAUNCH names the full ceiling. `rein run` under a session whose `repos:` lists
    A and B. The #68 install-coverage gate (which runs BEFORE the sandbox/direct
    split) clears for BOTH, and the launch banner space-joins the scope,
    `repos=[A B]` (Go %v) — both repos are in the static ceiling. (No separate
    launch-gate journey is needed; the banner shows the cleared gate inline.)
  * READ BOTH, no declaration. The agent clones A, then clones B. The brokered
    READ token covers every session repo, so both clones succeed with no declare —
    the ceiling is readable end to end.
  * WRITE to A. `rein declare <issueA> --repo A` blocks on the host `/dev/tty`
    Form A (the FIRST issue this run: the plain "agent declares work on an issue"
    confirm). A human approves; the verified push to A's `agent/<issueA>/<nonce>`
    LANDS on A.
  * WRITE to B — the second-issue confirm IS part of this path, shown, not routed
    around. `rein declare <issueB> --repo B` is the SECOND issue confirmed in the
    same run, so grant renders the distinct header "agent wants to ALSO work on an
    issue" (grant.go: expansion = the run already holds a confirmed issue — purely
    issue-count, NOT repo-gated). The header deliberately does NOT say "scope
    expansion": that phrase is reserved for a REPO scope-expansion. B is already in
    the static ceiling, so `resolveRepo`
    returns expansion=false and NO `SCOPE EXPANSION requested` / `agent asks to ADD
    repo` text renders (an OUT-of-scope repo would trip THAT — journey_scope_expansion
    covers it). A human approves; the verified push to B's `agent/<issueB>/<nonce>`
    LANDS on B. (A 2-repo session also REQUIRES `--repo` on declare — resolveRepo
    errors on a bare `rein declare <n>` when >1 repo is in scope — so naming the
    repo is part of the multi-repo flow.)

Both branches are then verified HOST-SIDE (the operator's own gh) to confirm each
genuinely landed on GitHub, and a `finally` deletes both branches and closes both
fixture issues.

WHY --direct (not sandboxed): the OLD journey used --direct, and it is the mode that
exercises the real one-run cross-repo WRITE path cleanly here. The push in --direct
still goes through rein's per-run credential helper — the golden's banner names the
`per-process git config` that installs it — so a landing push is rein's BROKERED
write token, not the operator's ambient gh, even though the process is unsandboxed.
Sandboxed WOULD be the stronger isolation story, but it is currently BLOCKED by a
rein bug this very journey surfaced (see the NOTE below): the sandboxed declare of a
SECOND session repo's issue 404s because the broker reuses a run-cached gh-read token
scoped narrower than the ceiling, so the second declare can't fetch its issue. That
is a real defect to fix in rein, not something to route the journey around; until
it is fixed, --direct is the honest way to show the one-run cross-repo path actually
working end to end. The clone omits `--progress` (a piped `--progress` clone's
`remote: Total` line races the local `Receiving/Resolving` lines and reorders
run-to-run); the pushes pass `--progress` so the golden shows the real "it landed"
transfer chatter (counts normalized to <N> at compare time), exactly as
write_ceremony.

NOTE (rein bug #95, surfaced by this journey): in
SANDBOXED mode the broker's declare fetch (cmd/rein/run_sandboxed.go fetchIssue) uses
the run's CACHED gh-read token for an in-ceiling repo, but that cached token is scoped
narrower than the full ceiling, so `rein declare <issueB> --repo B` — B being the
SECOND repo — returns "issue #<n> not found in B" even though the run's own read token
clones B fine. The --direct declare path (cmd/rein/declare.go) mints a FRESH
ceiling-scoped token per fetch and is immune, which is why --direct works and the old
journey did too. Flip this journey back to sandboxed (drop `--direct` from the step
argv) once the broker caches/rescopes the gh-read token to cover the whole ceiling.

CAPTURE IS STRUCTURAL (#82/#85): ONE run_journey STEP whose wrapped `bash -c` child
drives both declares and both pushes; the runner captures the COMPLETE pty session
(banner + every SBX| -tagged child line + both host Form A prompts) and returns it as
`.transcript`. The two Form A prompts are driven by the step's ordered `answers`
(each answered with its own displayed issue number). Determinism lives in the
COMPARATOR; repo names + fixed issue titles are stable-by-construction and kept RAW.
(`SBX| ` here tags the wrapped, unsandboxed child, the same convention
journey_credential_boundary.py uses for its --direct leg.)

DELIVERABLE: `golden/multi_repo.txt`.

    python3 tests/interactive/journey_multi_repo.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_multi_repo.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_multi_repo.py # also print the compare lens

Exit 0 = both repos read + declared + pushed-and-landed AND the normalized transcript
matches the golden. Exit 1 = drift. Exit 2 = the cross-repo write itself broke.

SELF-CONTAINED: creates its own throwaway issue on EACH repo and, in a `finally`,
deletes both pushed branches and closes both issues. Touches only the two throwaways
(hard-constraint #1). Repo A is resolved the rein-init way (#40); repo B is
REIN_TEST_REPO_B (same owner — the App installation is single-owner).
"""

from __future__ import annotations

import os
import re
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "multi_repo.txt"


def agent_script(repo_a: str, issue_a: int, good_a: str,
                 repo_b: str, issue_b: int, good_b: str) -> str:
    """A single `bash -c` body run as the wrapped --direct child for the WHOLE run:
    clone BOTH repos (reads flow with no declaration), then for each repo in turn
    `rein declare <issue> --repo <repo>` (BLOCKS on the host /dev/tty Form A; pexpect
    answers it) and push a real branch (via rein's per-run credential helper) that
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
            "Opened by tests/interactive/journey_multi_repo.py to demonstrate ONE "
            "brokered token doing real work across two repos (this is repo A). "
            "Throwaway; closed when done.", env,
        )
        created.append((repo_a, issue_a))
        issue_b = H.create_issue(
            repo_b, "rein journey: multi-repo fixture (safe to close)",
            "Opened by tests/interactive/journey_multi_repo.py to demonstrate ONE "
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
                    argv=["run", "--direct", "--", "bash", "-c",
                          agent_script(repo_a, issue_a, good_a, repo_b, issue_b, good_b), wd],
                    # ordered: the 1st Form A is issue A, the 2nd (the ALSO-work
                    # confirm) is issue B — each answered with its OWN displayed
                    # number (the sole approval token, per repo).
                    answers=[(H.PROMPT_HINT, str(issue_a)), (H.PROMPT_HINT, str(issue_b))],
                    label=f"rein run --direct -- bash -c <clone+declare+push A and B> {wd}",
                    extra_env={
                        "REIN_SESSION_FILE": session,
                        "REIN_APPROVAL": "tty",  # force the inline /dev/tty prompt pexpect drives
                    },
                    timeout=240,  # two clones + two declares + two pushes (unsandboxed)
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
            p = H.update_golden(GOLDEN_NAME, text)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, text)
        if ok:
            print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "multi_repo.fresh.txt")
        with open(scratch, "w") as f:
            f.write(text)
        print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
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
