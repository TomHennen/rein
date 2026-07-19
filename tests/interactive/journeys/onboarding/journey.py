"""onboarding — the first-run `rein init` guided flow, then `rein doctor` (onboarding-ux-design.md §3).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: REIN_MACHINE_HOSTNAME pins the pre-filled hostname; init_app_env() supplies
the env-path REIN_APP_* so init keeps to the env path and NEVER routes into
RunManifestFlow (the ~25-min browser App-creation seam, exercised by
scripts/cp5-manifest-manual-test.sh).
"""

from __future__ import annotations

import os
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"


def main() -> int:
    env = H.rein_env()
    H.build_binaries(env)

    home = H.isolated_home()
    # One throwaway HOME/XDG world, shared by both commands: `rein init` sets it
    # up, `rein doctor` inspects it. REIN_MACHINE_HOSTNAME pins the pre-filled
    # label so the golden is deterministic. init_app_env() supplies the env-path
    # App so init stays off the manifest flow and the post-init `rein doctor`
    # mint check runs against a real App.
    extra = {**H.isolated_home_env(home), **H.init_app_env()}
    extra["REIN_MACHINE_HOSTNAME"] = "demo-box"

    # DECLARE STEPS ONLY — argv + the ordered answers to each prompt. The runner
    # captures the COMPLETE session (issue #82); this journey never slices it.
    # --no-alias is DROPPED on purpose so the alias [y/N] prompt fires live and
    # we decline it (the golden then shows the opt-in default holding). --yes is
    # NOT passed: it would suppress every prompt, and the prompts ARE the artifact.
    result = H.run_journey(
        [
            H.JourneyStep(
                argv=["init", "--no-symlink", "--skip-mint-check", "--shell", "bash"],
                answers=[
                    (r"(?i)name this machine", ""),               # accept the pre-filled "demo-box"
                    ("Which repo should the agent work on", "octo-example/demo-repo"),
                    (r"\[y/N\]", ""),                             # decline the alias (bare Enter = N)
                ],
            ),
            H.JourneyStep(argv=["doctor"]),                       # post-onboarding verify; no prompts
        ],
        env=env,
        extra_env=extra,
    )
    text = result.transcript

    # 1) The flow must hold — independent of the golden. Expected values are
    #    INLINE LITERALS (issue #82 review: a reviewer reads what's expected here,
    #    not a chased-down constant).
    checks = [
        (result.reached_eof, "every driven command must run to completion (no missed prompt)"),
        ("Name this machine [demo-box]" in text, "machine-label prompt must be pre-filled with the hostname"),
        ("machine:    demo-box" in text, "the resolved machine label must be displayed"),
        ("session:    scaffolded" in text and "octo-example/demo-repo" in text,
         "the session must scaffold against the picked repo"),
        ("install on repo:" in text and "visit this URL" in text, "the install-on-repo link (§5) must be printed"),
        ("rein init: done." in text, "init must run to completion"),
        (result.steps[0].exitstatus == 0, "init must exit 0 on the happy env path"),
        # doctor's output is now IN the captured session (not dropped).
        ("rein doctor: ok" in text, "`rein doctor` output must be captured in the session"),
        ("nono present" in text, "doctor's full check table must be present (complete capture)"),
    ]
    broken = [msg for ok, msg in checks if not ok]
    if broken:
        print("ONBOARDING FLOW BROKE:", flush=True)
        for m in broken:
            print(f"  - {m}", flush=True)
        print("--- transcript ---", flush=True)
        print(text, flush=True)
        return 2

    print()
    print(text, flush=True)
    print("--- driveable seam (asserted; not a golden claim) ---", flush=True)
    print("  App CREATION (browser/OAuth) is UNDRIVEABLE and was bypassed "
          "(REIN_APP_* present => env path).", flush=True)
    print("  Driven + captured whole: `rein init` (machine-label prompt, session, "
          "install link) and `rein doctor`.", flush=True)

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
    scratch = os.path.join(tempfile.gettempdir(), "onboarding.fresh.txt")
    with open(scratch, "w") as f:
        f.write(text)
    print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
