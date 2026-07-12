"""journey_session_commands — THE HUMAN-SIDE SESSION COMMANDS (`rein session show | add-repo`).

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

WHAT THIS WALKS (issue #69, session mocks §2): the two HOST-side session commands a
human runs OUTSIDE the sandbox to inspect and widen the STANDING scope ceiling. The
sandboxed agent can never reach these — its only way to widen scope is `rein declare
--repo`, which always goes through the approval prompt. This journey demonstrates:

  * `rein session show` — the standing ceiling: the session id + file, its role, and
    each repo annotated with its LIVE install-coverage (`[App installed]` — the #53
    failure class made visible BEFORE launch, not as a mid-run 404). With no live run
    in the isolated state dir, it reports `live runs: none` (the confirmed-issue /
    scope-expansion deltas render here WHEN a run is live — see journey_scope_expansion
    for that path).
  * `rein session add-repo <owner/name>` — the VALIDATED add: the same-owner structural
    check, then a live install-coverage probe, then the write. Repo B
    (agentcreds-validation-b) is added to a session scoped to repo A only, and the
    following `rein session show` shows B now standing in the ceiling.
  * the CROSS-OWNER reject — a plain assertion (exit != 0, single-owner message), run
    off to the side so the structural refusal is proven WITHOUT landing in the golden
    (edge-case invariants are plain assertions, per the authoring rules).

This ALSO fills the hole that `rein session show` had NO test of any kind: the journey
exercises it live, twice (before and after the add).

CAPTURE IS STRUCTURAL: uses reinharness.run_journey — it declares STEPS (the commands;
these two take no prompts) and the runner captures the COMPLETE pty session. The golden
is the whole thing; volatiles are handled by normalize-on-compare.

DETERMINISM: an isolated HOME/XDG confines every read/write and — crucially — points
the state dir at a FRESH tree, so `live runs: none` is stable and no ambient run leaks
in. The session id is a fixed literal; the session FILE lives under /tmp/rein-… so its
path normalizes to <TMP>/session.yaml. The repo names are stable by construction (the
throwaways) so they match verbatim. REIN_APP_* stays present (the env path), so the
install-coverage probe runs against the real App without touching the browser flow.

    python3 tests/interactive/journey_session_commands.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_session_commands.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_session_commands.py # also print the compare lens

Exit 0 = the flow ran AND the normalized transcript matches the golden. Exit 1 = drift.
Exit 2 = the flow itself broke.

SELF-CONTAINED: every write lands in a throwaway HOME/XDG tempdir + a throwaway session
file; no real repo is touched (hard-constraint #1). Repo A is resolved the rein-init
way (reinharness.resolve_throwaway_repo); repo B is REIN_TEST_REPO_B (same owner — the
App installation is single-owner).
"""

from __future__ import annotations

import os
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "session_commands.txt"


def _a_only_session(repo_a: str) -> str:
    """A temp session scoped to repo A ONLY, with a FIXED id (so the golden is
    deterministic) and NO `issue:`/`created:` keys (so `show` prints neither the
    retired-issue warning nor a per-run timestamp). Written under /tmp/rein-… so
    its path normalizes to <TMP>/session.yaml at compare time."""
    d = tempfile.mkdtemp(prefix="rein-journey-sesscmds-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(
            "id: sess_journey_sesscmds\n"
            "role: implement\n"
            "repos:\n"
            f"  - {repo_a}\n"
        )
    return path


def main() -> int:
    env = H.rein_env()  # REIN_APP_* present => env path, real install-coverage probe
    repo_a = H.resolve_throwaway_repo(env)   # rein-init way first; #40
    repo_b = H.throwaway_repo_b(env)         # REIN_TEST_REPO_B (same owner as A)
    H.build_binaries(env)

    # An isolated HOME/XDG confines every write AND points the state dir at a fresh
    # tree, so `live runs: none` is stable and no ambient run leaks into the golden.
    home = H.isolated_home()
    session_path = _a_only_session(repo_a)
    extra = dict(H.isolated_home_env(home))
    extra["REIN_SESSION_FILE"] = session_path

    print(f"journey: session commands  A={repo_a}  add B={repo_b}  session={session_path}", flush=True)

    # DECLARE STEPS ONLY — argv + (no) answers. show, add-repo, show: the runner
    # captures the COMPLETE session; this journey never slices it.
    result = H.run_journey(
        [
            H.JourneyStep(argv=["session", "show"]),                 # ceiling: repo A + coverage
            H.JourneyStep(argv=["session", "add-repo", repo_b]),     # validated add of repo B
            H.JourneyStep(argv=["session", "show"]),                 # ceiling now shows A + B
        ],
        env=env,
        extra_env=extra,
        timeout=60,
    )
    text = result.transcript

    # 1) The flow must hold — independent of the golden. Expected values are INLINE
    #    LITERALS (a reviewer reads what's expected here, not a chased-down constant).
    checks = [
        (result.reached_eof, "every driven command must run to completion (no missed prompt)"),
        ("session: sess_journey_sesscmds" in text, "`session show` must name the scaffolded session"),
        (repo_a in text and "[App installed]" in text,
         "`session show` must annotate repo A with its live install-coverage"),
        ("live runs: none" in text, "`session show` must report no live run in the isolated state dir"),
        (f"rein: checking {repo_b}..." in text, "`add-repo` must announce the repo it validates"),
        ("matches session owner" in text, "`add-repo` must pass the same-owner structural check"),
        ("covers it  OK" in text, "`add-repo` must confirm the install-coverage probe"),
        ("rein: added. Session repos are now:" in text, "`add-repo` must confirm the write"),
        (result.steps[1].exitstatus == 0, "`add-repo` must exit 0 on the happy path"),
        (text.count(repo_b) >= 2, "the second `session show` must list repo B now in the ceiling"),
    ]
    broken = [msg for ok, msg in checks if not ok]
    if broken:
        print("SESSION-COMMANDS FLOW BROKE:", flush=True)
        for m in broken:
            print(f"  - {m}", flush=True)
        print("--- transcript ---", flush=True)
        print(text, flush=True)
        return 2

    # 2) CROSS-OWNER reject — a plain assertion, run OFF TO THE SIDE so the structural
    #    refusal is proven but does NOT land in the golden (edge-case invariant). The
    #    owner differs, so CheckAddRepo refuses before any network probe.
    cross = H.spawn_rein(["session", "add-repo", "octocat/hello-world"], env=env, extra_env=extra)
    cross_rc = cross.wait(timeout=30)
    cross_text = cross.text()
    cross_ok = cross_rc != 0 and "single-owner" in cross_text
    if not cross_ok:
        print("CROSS-OWNER REJECT BROKE:", flush=True)
        print(f"  rc={cross_rc} (want != 0), 'single-owner' in output={'single-owner' in cross_text}", flush=True)
        print(cross_text, flush=True)
        return 2

    print()
    print(text, flush=True)
    print("--- outcomes (asserted; not in the golden) ---", flush=True)
    print(f"  `rein session show` exercised LIVE (twice: before + after the add)", flush=True)
    print(f"  add-repo {repo_b}: validated (same-owner + install probe) and landed in the ceiling", flush=True)
    print(f"  cross-owner add (octocat/hello-world): REJECTED rc={cross_rc} (single-owner rule)", flush=True)

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
    scratch = os.path.join(tempfile.gettempdir(), "session_commands.fresh.txt")
    with open(scratch, "w") as f:
        f.write(text)
    print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
