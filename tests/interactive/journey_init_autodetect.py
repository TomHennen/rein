"""journey_init_autodetect — `rein init`'s repo prompt DEFAULT (and `rein run`'s
no-session hint) are autodetected from the cwd's git remote (issue #69 / PR #78,
mocks §3).

This is ONE journey. For what a journey IS, the golden-transcript rule, the
shared runner, and how to author the next one, read tests/interactive/CLAUDE.md
— none of that lives here.

WHAT #78 CHANGED. `cmd/rein/init.go` (+ the new `cmd/rein/gitremote.go`) make the
repo prompt's DEFAULT the repo the human is STANDING IN: `detectRepoFromGit` reads
the cwd's git `origin` URL and maps it to `owner/name`, and `resolveRepoForSession`
offers that as the prompt default. It is UX only — a default the human still
confirms, never a grant. Outside a GitHub checkout the default is simply absent.
The same detection also turns `rein run`'s cold "no session" dead-end into a hint
that names the repo you are standing in (`cmd/rein/gitremote.go:noSessionHint`).

FOUR STEPS, all in the golden, each driven through a real interactive `rein`
under a pty (NO --yes, so the prompt genuinely renders):

  * init, DETECTED: from a checkout of the throwaway repo. The repo prompt is
    PRE-FILLED with the detected `owner/name`; the human accepts it with Enter and
    the session is scaffolded for the detected repo.
  * init, CONTRAST: from a NON-git directory. There is no `origin` to detect, so
    the prompt has NO default (the bare prompt) — proving the default is
    cwd-derived, not hardcoded.
  * run, DETECTED: `rein run` with no session, FROM the checkout. Its cold
    "no session" failure now carries a hint that NAMES the cwd's repo
    (`rein init --repo <repo>`).
  * run, CONTRAST: `rein run` with no session, from the NON-git dir. The hint
    degrades to the generic `run `rein init` to create one` — again proving the
    hint is cwd-derived, not hardcoded.

CAPTURE IS STRUCTURAL (issue #82). This journey uses reinharness.run_journey: it
declares STEPS (each command's argv + cwd + the ordered answers to its prompts)
and the runner captures the COMPLETE pty session of everything it drove. The
`rein run` no-session hint is therefore IN the golden AUTOMATICALLY — not
asserted-and-excluded, the exact gap #82 closed. There is no hand-assembly and no
slicing: a section is in the golden because its command ran. Volatiles (tmp
HOME/XDG/checkout paths, the running binary, home, per-run ids) are handled by
normalize-on-compare, never by dropping output. The `rein run --direct` legs DO
print the loud UNSANDBOXED-MODE banner — and that is fine: it is captured whole,
the same as every other line the flow produced.

DELIVERABLE: a RAW, human-reviewable transcript at `golden/init_autodetect.txt` —
real repo name, real prompt wording, real scaffold path, real hint — so Tom SEES
the pre-filled default and the cwd-named hint exactly as they render. Determinism
lives in the comparator: a fresh run is normalized (tmp HOME/XDG/checkout paths ->
<TMP>, running binary -> <REIN_BIN>, home -> <HOME>) before the diff, so different
tempdirs still match while a genuinely new or changed rein line trips drift. The
repo name is stable-by-construction (the same throwaway), so — like the write
ceremony's repo — it is kept RAW and matched verbatim.

    python3 tests/interactive/journey_init_autodetect.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_init_autodetect.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_init_autodetect.py # also print the compare lens

Exit 0 = detection behaved AND the normalized transcript matches the golden.
Exit 1 = drift (RAW fresh transcript dropped to a scratch path; NORMALIZED diff
printed). Exit 2 = the #69 detection itself misbehaved (the invariants below failed).

SAFETY (hard-constraint #1). Every step is confined to a throwaway HOME/XDG
tempdir (reinharness.isolated_home_env), REIN_APP_* stays present so init keeps to
the env path and NEVER reaches the ~25-minute manifest/browser flow, and each init
run passes --no-alias --no-symlink --skip-mint-check. The "checkout" is a bare `git
init` + `remote add origin` pointing at the throwaway — enough for the origin-URL
detection, which touches NO real repo (it only NAMES the throwaway in a local
remote). The repo is resolved the rein-init way (reinharness.resolve_throwaway_repo:
REIN_JOURNEY_REPO, else the configured dev-session, else REIN_TEST_REPO_A; #40).
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "init_autodetect.txt"


def make_checkout(repo: str) -> str:
    """A real git work tree whose `origin` is the throwaway repo.

    `git init` + `remote add origin` (no network clone) is all detectRepoFromGit
    needs — it only runs `git remote get-url origin`. This touches NO real repo:
    it merely NAMES the throwaway in a local remote (hard-constraint #1).
    """
    d = tempfile.mkdtemp(prefix="rein-journey-checkout-")
    subprocess.run(["git", "init", "-q", d], check=True)
    subprocess.run(
        ["git", "-C", d, "remote", "add", "origin", f"https://github.com/{repo}.git"],
        check=True,
    )
    return d


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    print(f"journey: init/run repo autodetection on {repo}", flush=True)

    # Two dirs the steps run FROM: a checkout of the throwaway (has an origin to
    # detect) and a NON-git dir (no origin => no default). Two isolated HOME/XDG
    # worlds: `home_detected` for the detected init leg, `home_bare` for the
    # contrast init leg AND the two `rein run` hint legs (which reuse it: the
    # nongit init installs the shim there but scaffolds NO session, exactly the
    # cold "no session" state the hint needs).
    checkout = make_checkout(repo)
    nongit = tempfile.mkdtemp(prefix="rein-journey-nongit-")
    home_detected = H.isolated_home()
    home_bare = H.isolated_home()

    # Init legs pin the machine label to a stable "demo-box" so the #82
    # machine-label prompt renders a deterministic default (the raw hostname is
    # machine-variable and un-normalized). Same knob the onboarding journey uses.
    # init_app_env() supplies the env-path App so these --skip-mint-check init
    # runs stay on the env path instead of the 25-minute manifest flow.
    init_env_detected = {**H.isolated_home_env(home_detected), **H.init_app_env()}
    init_env_detected["REIN_MACHINE_HOSTNAME"] = "demo-box"
    init_env_bare = {**H.isolated_home_env(home_bare), **H.init_app_env()}
    init_env_bare["REIN_MACHINE_HOSTNAME"] = "demo-box"

    # The run legs blank REIN_TEST_REPO_A so LoadOrFallback has NO fallback and
    # the cold "no session" (and thus noSessionHint) actually fires; --direct is
    # the only path that carries the hint.
    #
    # REIN_TEST_REPO_A is a PRODUCTION special-case: cmd/rein reads it via
    # session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A")) as a last-resort repo
    # fallback, so blanking it here is what forces the genuine no-session state.
    # That production special-casing is tracked for removal by #40 — once #40
    # lands and LoadOrFallback no longer consults this env var, this blanking
    # becomes unnecessary and can be dropped.
    # init_app_env() gives the run legs the env-path App so `rein run` doesn't warn
    # about missing App config; REIN_TEST_REPO_A is then re-blanked (init_app_env
    # sets it) to keep forcing the no-session hint.
    run_env = {**H.isolated_home_env(home_bare), **H.init_app_env()}
    run_env["REIN_TEST_REPO_A"] = ""

    # DECLARE STEPS ONLY — argv + cwd + the ordered answers. run_journey captures
    # the COMPLETE session of all four; nothing is sliced or hand-assembled. The
    # two init legs run FIRST so the nongit init populates `home_bare`'s shim
    # before the run legs reuse it. The init flags (--no-alias --no-symlink
    # --skip-mint-check) keep an init run inert; NO --yes, so the repo prompt
    # genuinely renders and can be answered live.
    result = H.run_journey(
        [
            H.JourneyStep(
                argv=["init", "--no-alias", "--no-symlink", "--skip-mint-check"],
                answers=[
                    (r"(?i)name this machine", ""),   # accept the pinned "demo-box" label
                    ("Which repo should the agent work on", ""),  # Enter accepts the detected default
                ],
                cwd=checkout,
                extra_env=init_env_detected,
                label=f"cd {checkout} && rein init --no-alias --no-symlink --skip-mint-check",
            ),
            H.JourneyStep(
                argv=["init", "--no-alias", "--no-symlink", "--skip-mint-check"],
                answers=[
                    (r"(?i)name this machine", ""),   # accept the pinned "demo-box" label
                    ("Which repo should the agent work on", ""),  # Enter with no default = graceful skip
                ],
                cwd=nongit,
                extra_env=init_env_bare,
                label=f"cd {nongit} && rein init --no-alias --no-symlink --skip-mint-check",
            ),
            H.JourneyStep(
                argv=["run", "--direct", "--", "echo", "hi"],
                cwd=checkout,
                extra_env=run_env,
                label=f"cd {checkout} && rein run --direct -- echo hi",
            ),
            H.JourneyStep(
                argv=["run", "--direct", "--", "echo", "hi"],
                cwd=nongit,
                extra_env=run_env,
                label=f"cd {nongit} && rein run --direct -- echo hi",
            ),
        ],
        env=env,
    )
    text = result.transcript
    init_detected, init_bare, run_detected, run_bare = result.steps

    # 1) The #69 detection itself must hold — independent of the golden (exit 2).
    #    Expected values are INLINE LITERALS (a reviewer reads what's expected
    #    here, not a chased-down constant); `repo` is the one runtime input.
    invariants = [
        (result.reached_eof, "every driven command must run to completion (no missed prompt)"),
        (init_detected.exitstatus == 0, f"detected-leg init must exit 0; got {init_detected.exitstatus}"),
        (init_bare.exitstatus == 0, f"contrast-leg init must exit 0; got {init_bare.exitstatus}"),
        (
            "(default detected from this dir)" in text and f"[{repo}]" in text,
            f"detected init leg: the prompt is PRE-FILLED with [{repo}]",
        ),
        (
            f"(repos: [{repo}])" in text,
            "detected init leg: accepting the default scaffolded the session for the detected repo",
        ),
        (
            "(owner/name, Enter to skip)" in init_bare.text,
            "contrast init leg: a NON-git dir gets the bare prompt",
        ),
        (
            "(default detected from this dir)" not in init_bare.text
            and f"[{repo}]" not in init_bare.text,
            "contrast init leg: the bare prompt has NO pre-filled default (cwd-derived)",
        ),
        (
            "not scaffolded" in init_bare.text,
            "contrast init leg: a bare Enter with no default scaffolds nothing (graceful skip)",
        ),
        (
            f"rein init --repo {repo}" in run_detected.text,
            f"run hint from the checkout must name `rein init --repo {repo}`",
        ),
        (
            f"rein init --repo {repo}" not in run_bare.text
            and "rein init` to create one" in run_bare.text,
            "run hint from a NON-git dir is the generic hint (no --repo) — cwd-derived",
        ),
    ]
    broken = [msg for ok, msg in invariants if not ok]
    if broken:
        print("JOURNEY BROKE — the #69 cwd-autodetection did not behave:", flush=True)
        for msg in broken:
            print(f"  - {msg}", flush=True)
        print("--- transcript ---", flush=True)
        print(text, flush=True)
        return 2

    print()
    print(text, flush=True)  # what actually happened, real repo + real prompt + real hint
    print("--- outcomes (asserted; the hint is now IN the golden, not excluded) ---", flush=True)
    print(f"  detected init prompt pre-filled with: [{repo}]", flush=True)
    print(f"  run hint (checkout) names:            rein init --repo {repo}", flush=True)
    print("  run hint (non-git) degrades to:       run `rein init` to create one", flush=True)

    if os.getenv("REIN_SHOW_NORMALIZED"):
        print("\n--- normalized (the comparison lens) ---", flush=True)
        print(H.normalize_for_compare(text), flush=True)

    if os.getenv("REIN_UPDATE_GOLDEN"):
        p = H.update_golden(GOLDEN_NAME, text)  # store RAW
        print(f"[golden UPDATED] {p} (raw)", flush=True)
        return 0

    ok, diff = H.compare_golden(GOLDEN_NAME, text)  # normalizes BOTH sides
    if ok:
        print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
        return 0
    scratch = os.path.join(tempfile.gettempdir(), "init_autodetect.fresh.txt")
    with open(scratch, "w") as f:
        f.write(text)
    print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
