"""journey_multi_repo — STATIC MULTI-REPO HAPPY LAUNCH (B10).

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

It is the HAPPY counterpart to journey_app_not_installed.py. That journey shows a
session naming an UNCOVERED repo being REFUSED at launch (the #68 install-coverage
gate). This one shows the other side: a session hand-scoped to TWO same-owner,
App-installed throwaway repos LAUNCHES cleanly (the gate clears for BOTH), and the
agent can READ and DECLARE against EACH of them.

  * LAUNCH: `rein run` with a session whose `repos:` lists A and B. The
    install-coverage gate (which runs BEFORE the sandbox/direct split, so it fires
    identically either way) passes for both, and the launch banner names the full
    scope `repos=[A B]` — both are in the static ceiling. (The banner space-joins
    the repos, Go %v style; the Form A confirm prompt renders the same ceiling
    comma-joined as `repos=[A, B]`, which is the form the invariant matches.)
  * READ both: the agent clones A and clones B. Reads flow with no declaration
    (the brokered read token covers every session repo), so both clones succeed.
  * DECLARE against both: TWO legs, `rein declare <issueA> --repo A` (leg A) and
    `rein declare <issueB> --repo B` (leg B). Because each repo is already in the
    session (session.Contains → resolveRepo returns expansion=false,
    internal/declare/declare.go), each fires a PLAIN Form A confirm — NOT the
    SCOPE EXPANSION prompt (`SCOPE EXPANSION requested` / `agent asks to ADD
    repo`). That plain confirm on repo B is the crux: an out-of-scope B would
    instead trip the AddRepo expansion path (journey_scope_expansion covers THAT).
    Here B is statically in scope, so it is a plain declaration. (A 2-repo session
    also REQUIRES `--repo` on declare — resolveRepo errors on a bare `rein declare
    <n>` when the session scopes more than one repo — so naming the repo is part
    of the multi-repo flow.)

TWO LEGS, not two declares in ONE run: each leg is its own `rein run --direct`, so
each declare is a FIRST declaration and shows the clean "agent declares work on an
issue" header. (A second declare within ONE run would render the "agent wants to
ALSO work on an issue" header — grant derives it from the run already holding a
confirmed issue — which is about declaring a second ISSUE, not the repo scope, and
would muddy this journey's "both repos statically in scope" story.) This two-leg
shape mirrors journey_app_not_installed, whose two legs share one session too.

WHY --direct: exactly as journey_app_not_installed — the coverage gate is a
LAUNCH-time check, independent of the sandbox, so `--direct` demonstrates the
2-repo launch + scope WITHOUT depending on a healthy srt stack or paying for
sandboxed clones. The point is "a static 2-repo session launches and both repos
are in scope", not the sandbox. (`SBX| ` here tags the wrapped, unsandboxed child,
the same convention journey_credential_boundary.py uses for its `--direct` leg.)

CAPTURE IS STRUCTURAL (#82/#85): two run_journey STEPS; the runner captures the
COMPLETE pty session, driving each leg's mid-run Form A prompt via `answers`.
Determinism lives in the COMPARATOR; repo names + fixed issue titles are
stable-by-construction and kept RAW. The clone omits `--progress` so the transfer
chatter's racy `remote: Total` ordering can't drift the golden — `@READ_OK`
proves each read flowed.

DELIVERABLE: `golden/multi_repo.txt`.

    python3 tests/interactive/journey_multi_repo.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_multi_repo.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_multi_repo.py # also print the compare lens

Exit 0 = the 2-repo launch held AND the normalized transcript matches the golden.
Exit 1 = drift. Exit 2 = the launch/scope itself broke.

SELF-CONTAINED: creates its own throwaway issue on EACH repo and, in a `finally`,
closes both. No branch is pushed (declaring proves scope; a push would only
overlap the write journeys). Touches only the two throwaways (hard-constraint #1).
Repo A is resolved the rein-init way; repo B is REIN_TEST_REPO_B (same owner — the
App installation is single-owner).
"""

from __future__ import annotations

import os
import re
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "multi_repo.txt"


def leg_script(repo: str, issue: int) -> str:
    """A `bash -c` body for ONE leg (`rein run --direct`) of the 2-repo session:
    clone `repo` (READ flows with no declaration), then `rein declare <issue>
    --repo <repo>` — which BLOCKS on the host `/dev/tty` Form A prompt (pexpect
    answers it). Both are tagged `@..` sentinels.

    The clone omits `--progress`: a piped `--progress` clone's `remote: Total`
    line races the local `Receiving/Resolving` lines and reorders run-to-run (git
    nondeterminism normalize-on-compare can't reorder away). Plain clone prints
    just `Cloning into …`; `@READ_OK` proves the read.
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"

emit "@READ_START  clone {repo} (read flows with no declaration)"
rm -rf repo
run git clone https://github.com/{repo} repo
emit "@READ_RC=$?"
[ -d repo/.git ] && emit "@READ_OK"

emit "@DECLARE_START  rein declare {issue} --repo {repo}  (in scope: plain confirm, NOT expansion)"
run rein declare {issue} --repo {repo}
emit "@DECLARE_RC=$?"
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


def _named(step_text: str, name: str) -> int | None:
    m = re.search(re.escape(H.SBX_TAG) + rf"@{name}=(\d+)", step_text)
    return int(m.group(1)) if m else None


def main() -> int:
    env = H.rein_env()
    repo_a = H.resolve_throwaway_repo(env)   # rein-init way first; #40
    repo_b = H.throwaway_repo_b(env)         # REIN_TEST_REPO_B (same owner as A)
    H.build_binaries(env)

    # Track every throwaway issue we open so the `finally` closes whatever got
    # created — even if the SECOND create_issue throws (which would otherwise
    # skip the try and leak the first, already-open, issue). Hard-constraint #1.
    created: list[tuple[str, int]] = []
    try:
        issue_a = H.create_issue(
            repo_a, "rein journey: multi-repo A fixture (safe to close)",
            "Opened by tests/interactive/journey_multi_repo.py to demonstrate a static "
            "2-repo session declaring against repo A. Throwaway; closed when done.", env,
        )
        created.append((repo_a, issue_a))
        issue_b = H.create_issue(
            repo_b, "rein journey: multi-repo B fixture (safe to close)",
            "Opened by tests/interactive/journey_multi_repo.py to demonstrate a static "
            "2-repo session declaring against repo B. Throwaway; closed when done.", env,
        )
        created.append((repo_b, issue_b))

        wd_a, wd_b = H.make_workdir(), H.make_workdir()
        session = _two_repo_session(repo_a, repo_b)

        print(f"journey: static multi-repo launch  A={repo_a} (#{issue_a})  "
              f"B={repo_b} (#{issue_b})", flush=True)

        result = H.run_journey(
            [
                H.JourneyStep(
                    argv=["run", "--direct", "--", "bash", "-c", leg_script(repo_a, issue_a), wd_a],
                    answers=[(H.PROMPT_HINT, str(issue_a))],
                    label=f"rein run --direct -- bash -c <read+declare {repo_a}> " + wd_a,
                    timeout=120,
                ),
                H.JourneyStep(
                    argv=["run", "--direct", "--", "bash", "-c", leg_script(repo_b, issue_b), wd_b],
                    answers=[(H.PROMPT_HINT, str(issue_b))],
                    label=f"rein run --direct -- bash -c <read+declare {repo_b}> " + wd_b,
                    timeout=120,
                ),
            ],
            env=env,
            extra_env={"REIN_SESSION_FILE": session, "REIN_APPROVAL": "tty"},
        )
        text = result.transcript
        text_a = result.steps[0].text if len(result.steps) > 0 else ""
        text_b = result.steps[1].text if len(result.steps) > 1 else ""
        read_a, decl_a = _named(text_a, "READ_RC"), _named(text_a, "DECLARE_RC")
        read_b, decl_b = _named(text_b, "READ_RC"), _named(text_b, "DECLARE_RC")
        prompts = text.count(H.PROMPT_BANNER)
        expansions = text.count(H.EXPANSION_BANNER)
        scope = f"repos=[{repo_a}, {repo_b}]"

        # 1) The 2-repo launch + scope must hold — independent of the golden.
        invariants = [
            (result.reached_eof, "both legs must complete (each prompt answered, no timeout)"),
            (text_a.count(scope) > 0 and text_b.count(scope) > 0,
             f"BOTH legs run under the full static scope {scope} (named in each confirm prompt)"),
            (read_a == 0, "leg A: read of repo A (clone) succeeds"),
            (read_b == 0, "leg B: read of repo B (clone) succeeds — the 2nd static repo is in scope for reads"),
            ("@READ_OK" in text_a and "@READ_OK" in text_b,
             "both legs' clones produced a real checkout (.git present)"),
            (decl_a == 0, "declare against repo A succeeds (in scope, plain confirm)"),
            (decl_b == 0, "declare against repo B succeeds (in scope, plain confirm)"),
            (prompts == 2, "exactly TWO plain 'declares work on an issue' prompts fired (one per leg)"),
            (expansions == 0,
             "NO AddRepo SCOPE-EXPANSION prompt fired — both repos are STATICALLY in scope"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("MULTI-REPO LAUNCH BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  read_a={read_a} read_b={read_b} decl_a={decl_a} decl_b={decl_b} "
                  f"prompts={prompts} expansions={expansions}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # 2) The golden IS the complete captured session. Outcomes asserted above.
        print()
        print(text, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  launch: session scoped to BOTH {repo_a} and {repo_b} (coverage gate cleared)", flush=True)
        print(f"  reads:  A rc={read_a}  B rc={read_b}", flush=True)
        print(f"  declares: A rc={decl_a}  B rc={decl_b}", flush=True)
        print(f"  plain Form A prompts: {prompts}   scope-expansion prompts: {expansions}", flush=True)

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
        for r, n in created:
            H.close_issue(r, n, env, comment="journey complete; closing.")
        print(f"cleanup: {len(created)} throwaway issue(s) closed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
