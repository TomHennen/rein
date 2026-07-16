"""session_commands — the human-side `rein session show` / `add-repo` (#69, mocks §2).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: an isolated HOME/XDG points the state dir at a FRESH tree, so `live runs: none`
is stable and no ambient run leaks in; init_app_env() supplies the env-path App so the
install-coverage probe runs against the real App without the browser flow. Live-run deltas render here only
WHEN a run is live — see journeys/scope_expansion/journey.py.
"""

from __future__ import annotations

import os
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"


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
    env = H.rein_env()
    repo_a = H.resolve_throwaway_repo(env)   # rein-init way first; #40
    repo_b = H.throwaway_repo_b(env)         # -a/-b sibling of A (same owner)
    H.build_binaries(env)

    # An isolated HOME/XDG confines every write AND points the state dir at a fresh
    # tree, so `live runs: none` is stable and no ambient run leaks into the golden.
    # init_app_env() supplies the env-path App so the install-coverage probe runs
    # against a REAL App without the browser flow.
    home = H.isolated_home()
    session_path = _a_only_session(repo_a)
    extra = {**H.isolated_home_env(home), **H.init_app_env()}
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
        p = H.update_golden(GOLDEN, text)
        print(f"[golden UPDATED] {p} (raw)", flush=True)
        return 0

    ok, diff = H.compare_golden(GOLDEN, text)
    if ok:
        print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
        return 0
    scratch = os.path.join(tempfile.gettempdir(), "session_commands.fresh.txt")
    with open(scratch, "w") as f:
        f.write(text)
    print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
