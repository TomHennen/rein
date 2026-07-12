"""journey_onboarding — THE FIRST-RUN `rein init` GUIDED FLOW (onboarding slices).

This is ONE journey. For what a journey IS, the golden-transcript rule, and how
to author the next one, read tests/interactive/CLAUDE.md — none of that lives
here.

WHAT THIS WALKS (onboarding-ux-design.md §3): the interactive `rein init` a new
user runs, exercising the two onboarding slices this journey exists to
demonstrate —

  * §4/§8.1 MACHINE-LABEL PROMPT — init asks "Name this machine", PRE-FILLED with
    the detected hostname and editable; the label is woven into the App name
    (rein-<role>-<label>-<shortrand>). The golden SHOWS the pre-filled prompt.
  * §5 INSTALL-ON-REPO — after scaffolding the session, init prints the install
    deep-link (no ssh -L needed), degrading to the generic installations URL
    when it doesn't yet know the App slug. The golden SHOWS that link.

THE UNDRIVEABLE SEAM (marked, not faked): the App-CREATION step (§3 step 4) is a
real browser/OAuth-callback flow (~25 minutes) and CANNOT run in a suite. This
journey stays on the ENV path by keeping REIN_APP_* present, so init NEVER routes
into RunManifestFlow — App creation is bypassed, and the driveable segments
(machine-label prompt, session scaffold, install-on-repo link) are captured
around that seam. The golden therefore shows the env-managed install URL, not a
per-App deep-link; the manifest-path deep-link is exercised by the separate,
genuinely browser-bound manifest walkthrough (scripts/cp5-manifest-manual-test.sh).

DETERMINISM (portable golden): the run is fully self-contained and uses
ILLUSTRATIVE fixed inputs — no live GitHub interaction on this path (no mint, no
push). REIN_MACHINE_HOSTNAME pins the pre-filled hostname; the repo answer is a
fixed demo slug; the operator's real client_id/installation_id are normalized at
compare time (reinharness._NORMALIZE_RULES). So the golden reproduces on any
box. Every write is confined to a throwaway HOME/XDG tempdir
(reinharness.isolated_home_env) — hard-constraint #1: nothing touches the dev's
real home or any real repo.

    python3 tests/interactive/journey_onboarding.py            # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_onboarding.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_onboarding.py  # also print the compare lens

Exit 0 = the flow ran AND the normalized transcript matches the golden. Exit 1 =
drift (RAW fresh transcript dropped to a scratch path; NORMALIZED diff printed).
Exit 2 = the flow itself broke.
"""

from __future__ import annotations

import os
import sys
import tempfile

import pexpect

import reinharness as H

GOLDEN_NAME = "onboarding.txt"

# Fixed, illustrative inputs so the golden is deterministic and portable.
DEMO_HOSTNAME = "demo-laptop"          # pins the pre-filled "Name this machine" default
DEMO_REPO = "octo-example/demo-repo"   # the repo the user "picks"; scaffolds the session

# init flags that keep the run inert (no ~/.local/bin symlink, no network mint).
# --no-alias is DROPPED on purpose: we drive the alias [y/N] prompt live and
# decline it, so the golden shows the opt-in default holding. The isolated HOME
# keeps that safe. --yes is NOT passed: it would suppress every prompt, and the
# prompts ARE the artifact.
INIT_FLAGS = ["--no-symlink", "--skip-mint-check", "--shell", "bash"]

REPO_PROMPT = "Which repo should the agent work on"
LABEL_PROMPT = r"(?i)name this machine"
ALIAS_PROMPT = r"\[y/N\]"


def drive_init(env, home):
    """Drive `rein init` through its three interactive prompts on a live pty;
    return (transcript_text, exit_status). expect -> act -> expect, one prompt
    at a time — each answer sent only AFTER its prompt is seen."""
    extra = dict(H.isolated_home_env(home))
    extra["REIN_MACHINE_HOSTNAME"] = DEMO_HOSTNAME  # deterministic pre-fill (test seam)

    run = H.spawn_rein(["init", *INIT_FLAGS], env=env, extra_env=extra, timeout=60)
    try:
        # 1) machine-label prompt, pre-filled with the hostname -> accept default.
        run.child.expect(LABEL_PROMPT, timeout=30)
        run.answer("")  # bare Enter keeps the pre-filled "demo-laptop"
        # 2) repo prompt -> pick the demo repo (scaffolds the session).
        run.child.expect(REPO_PROMPT, timeout=30)
        run.answer(DEMO_REPO)
        # 3) alias [y/N] -> decline (opt-in default holds).
        run.child.expect(ALIAS_PROMPT, timeout=30)
        run.answer("")  # bare Enter = N
        run.child.expect(pexpect.EOF, timeout=30)
    except (pexpect.EOF, pexpect.TIMEOUT):
        # A prompt that never arrived -> the flow broke; return the partial
        # transcript so main() reports a clean "flow broke" (exit 2) rather than
        # a pexpect traceback a runner would mislabel as drift.
        pass
    finally:
        try:
            run.child.close()
        except Exception:
            pass
    rc = run.child.exitstatus if run.child.exitstatus is not None else 1
    return run.text(), rc


def drive_doctor(env, home):
    """Drive `rein doctor` in the just-onboarded HOME and return a one-line
    verdict. NOT part of the golden: doctor's table carries per-box values (srt
    path, App-key path, mint expiry, sandbox health) that vary by operator, so
    it is reported as an outcome, not a reviewable transcript. This exists to
    show the flow continues past init into verification (design §2)."""
    extra = dict(H.isolated_home_env(home))
    run = H.spawn_rein(["doctor"], env=env, extra_env=extra, timeout=60)
    try:
        run.child.expect(pexpect.EOF, timeout=45)
    except (pexpect.EOF, pexpect.TIMEOUT):
        pass
    try:
        run.child.close()
    except Exception:
        pass
    text = run.text()
    verdict = "ok" if "rein doctor: ok" in text else "some checks not green (expected: box-dependent)"
    return run.child.exitstatus, verdict


def main() -> int:
    env = H.rein_env()  # REIN_APP_* present => env path, never the browser flow
    H.build_binaries(env)

    home = H.isolated_home()
    text, rc = drive_init(env, home)

    # 1) The flow itself must hold — independent of the golden. These are the
    #    driveable segments around the undriveable App-creation seam.
    checks = [
        ("Name this machine [%s]" % DEMO_HOSTNAME in text,
         "the machine-label prompt must be pre-filled with the detected hostname"),
        ("machine:    %s" % DEMO_HOSTNAME in text,
         "the resolved machine label must be displayed"),
        ("session:    scaffolded" in text and DEMO_REPO in text,
         "the session must scaffold against the picked repo"),
        ("install on repo:" in text and "visit this URL" in text,
         "the install-on-repo link (§5) must be printed"),
        ("rein init: done." in text,
         "init must run to completion"),
        (rc == 0, "init must exit 0 on the happy env path"),
    ]
    broken = [msg for ok, msg in checks if not ok]
    if broken:
        print("ONBOARDING FLOW BROKE:", flush=True)
        for m in broken:
            print(f"  - {m}", flush=True)
        print(f"  rc={rc}", flush=True)
        print("--- transcript ---", flush=True)
        print(H.build_raw_transcript(text), flush=True)
        return 2

    # 2) Build the RAW transcript (real values) and compare NORMALIZED.
    raw = H.build_raw_transcript(text)
    print()
    print(raw, flush=True)
    print("--- driveable seam (asserted; not a golden claim) ---", flush=True)
    print("  App CREATION (browser/OAuth) is UNDRIVEABLE and was bypassed "
          "(REIN_APP_* present => env path).", flush=True)
    print("  Driven: machine-label prompt, session scaffold, install-on-repo link.", flush=True)

    # Continue past init into verification (design §2). doctor's table is
    # box-dependent, so it is reported here, NOT baked into the golden.
    drc, dverdict = drive_doctor(env, home)
    print(f"  Then `rein doctor` (post-onboarding verify): exit={drc} — {dverdict}", flush=True)

    if os.getenv("REIN_SHOW_NORMALIZED"):
        print("\n--- normalized (the comparison lens) ---", flush=True)
        print(H.normalize_for_compare(raw), flush=True)

    if os.getenv("REIN_UPDATE_GOLDEN"):
        p = H.update_golden(GOLDEN_NAME, raw)
        print(f"[golden UPDATED] {p} (raw)", flush=True)
        return 0

    ok, diff = H.compare_golden(GOLDEN_NAME, raw)
    if ok:
        print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
        return 0
    scratch = os.path.join(tempfile.gettempdir(), "onboarding.fresh.txt")
    with open(scratch, "w") as f:
        f.write(raw)
    print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
