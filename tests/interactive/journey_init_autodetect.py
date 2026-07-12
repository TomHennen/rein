"""journey_init_autodetect — `rein init`'s repo prompt DEFAULT is autodetected
from the cwd's git remote (issue #69 / PR #78, mocks §3).

This is ONE journey. For what a journey IS, the golden-transcript rule, and how
to author the next one, read tests/interactive/CLAUDE.md — none of that lives here.

WHAT #78 CHANGED. `cmd/rein/init.go` (+ the new `cmd/rein/gitremote.go`) make the
repo prompt's DEFAULT the repo the human is STANDING IN: `detectRepoFromGit` reads
the cwd's git `origin` URL and maps it to `owner/name`, and `resolveRepoForSession`
offers that as the prompt default. It is UX only — a default the human still
confirms, never a grant. Outside a GitHub checkout the default is simply absent.
The same detection also turns `rein run`'s cold "no session" dead-end into a hint
that names the repo you are standing in (`cmd/rein/gitremote.go:noSessionHint`).

TWO LEGS in the golden, both driven through a real interactive `rein init` under a
pty (NO --yes, so the prompt genuinely renders):

  * DETECTED: init runs from a checkout of the throwaway repo. The repo prompt is
    PRE-FILLED with the detected `owner/name`; the human accepts it with Enter and
    the session is scaffolded for the detected repo.
  * CONTRAST: init runs from a NON-git directory. There is no `origin` to detect,
    so the prompt has NO default (the bare prompt) — proving the default is
    cwd-derived, not hardcoded.

PLUS a PLAIN ASSERTION (not in the golden): `rein run` with no session prints a
hint that NAMES the detected repo. It is kept out of the golden deliberately —
reaching the hint means `--direct`, whose loud UNSANDBOXED-MODE warning banner
would muddy an init-focused transcript. The assertion still proves the run.go
change end to end: the hint from the CHECKOUT names `rein init --repo <repo>`,
while the hint from a NON-git dir is the generic `run \`rein init\` to create one`.

COMMAND-ECHO IS HOST-SIDE, NOT `SBX| `. `rein init` is not sandboxed, so there is
no in-sandbox script to tag. The two init invocations are echoed at the HOST level
as `$ cd <dir> && rein init …` then rein's raw output — the same command-echo
convention Tom asked for, host-side (identical to journey_app_not_installed.py). It
adopts the rest of the #78 model: `reinharness.build_raw_transcript` for the RAW
golden and `compare_golden` (normalize BOTH sides) for drift detection.

DELIVERABLE: a RAW, human-reviewable transcript at `golden/init_autodetect.txt` —
real repo name, real prompt wording, real scaffold path — so Tom SEES the
pre-filled default exactly as it renders. Determinism lives in the comparator: a
fresh run is normalized (tmp HOME/XDG/checkout paths -> <TMP>) before the diff, so
different tempdirs still match while a genuinely new or changed rein line trips
drift. The repo name is stable-by-construction (the same throwaway), so — like the
write ceremony's repo — it is kept RAW and matched verbatim.

    python3 tests/interactive/journey_init_autodetect.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_init_autodetect.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_init_autodetect.py # also print the compare lens

Exit 0 = detection behaved AND the normalized transcript matches the golden.
Exit 1 = drift (RAW fresh transcript dropped to a scratch path; NORMALIZED diff
printed). Exit 2 = the #69 detection itself misbehaved (the invariants below failed).

SAFETY (hard-constraint #1). Every init run is confined to a throwaway HOME/XDG
tempdir (reinharness.isolated_home_env), REIN_APP_* stays present so init keeps to
the env path and NEVER reaches the ~25-minute manifest/browser flow, and each run
passes --no-alias --no-symlink --skip-mint-check. The "checkout" is a bare `git
init` + `remote add origin` pointing at the throwaway — enough for the origin-URL
detection, which touches NO real repo (it only NAMES the throwaway in a local
remote). The repo is resolved the rein-init way (reinharness.resolve_throwaway_repo:
REIN_JOURNEY_REPO, else the configured dev-session, else REIN_TEST_REPO_A; #40).
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
import tempfile

import pexpect

import reinharness as H

GOLDEN_NAME = "init_autodetect.txt"

# --no-alias --no-symlink --skip-mint-check keep an init run inert; NO --yes, so
# the repo prompt genuinely renders and can be answered live.
INIT_FLAGS = ["--no-alias", "--no-symlink", "--skip-mint-check"]
REPO_PROMPT = "Which repo should the agent work on"


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


def origin_url(checkout: str) -> str:
    out = subprocess.run(
        ["git", "-C", checkout, "remote", "get-url", "origin"],
        capture_output=True,
        text=True,
        check=True,
    )
    return out.stdout.strip()


def drive_init(cwd: str, env):
    """One interactive `rein init` under a pty from `cwd`, isolated HOME/XDG.

    NO --yes, so the repo prompt renders; the human ACCEPTS the default with a
    bare Enter (the detected repo where there is one, or "" = skip scaffolding).
    Returns (raw_transcript, exit_code, home) — `home` is reused by the run-hint
    assertion (it has a shim installed but no session scaffolded).
    """
    home = H.isolated_home()
    run = H.spawn_rein(
        ["init", *INIT_FLAGS], env=env, extra_env=H.isolated_home_env(home), cwd=cwd, timeout=60
    )
    run.child.expect(REPO_PROMPT, timeout=45)
    run.answer("")  # Enter accepts the default
    run.child.expect(pexpect.EOF, timeout=45)
    run.child.close()
    return run.text(), run.child.exitstatus, home


def run_no_session_hint(cwd: str, home: str, env) -> str:
    """`rein run` with NO session, from `cwd`, reusing an init'd `home` (shim
    present). --direct is the ONLY path that carries noSessionHint; REIN_TEST_REPO_A
    is blanked so LoadOrFallback has no fallback and the cold "no session" fires.
    Returns the raw transcript (asserted, not put in the golden)."""
    extra = dict(H.isolated_home_env(home))
    extra["REIN_TEST_REPO_A"] = ""  # no fallback => the no-session hint fires
    run = H.spawn_rein(
        ["run", "--direct", "--", "echo", "hi"], env=env, extra_env=extra, cwd=cwd, timeout=40
    )
    run.child.expect(pexpect.EOF, timeout=35)
    run.child.close()
    return run.text()


def prompt_line(text: str) -> str:
    """The rendered repo-prompt line (ANSI-stripped), or "" if absent."""
    for ln in H.strip_ansi(text).replace("\r", "\n").split("\n"):
        if REPO_PROMPT in ln:
            return ln.strip()
    return ""


def build_transcript(legs) -> str:
    """Assemble the interleaved HOST transcript and hand it to build_raw_transcript.

    Each leg is (header_lines, echo_lines, raw): the `# …` labels, the `$ <command>`
    echoes (with any command stdout, e.g. the origin URL), then rein's raw output.
    build_raw_transcript then applies the exact golden shape (ANSI strip, drop
    sub-100% progress ticks, collapse blank runs) — the same machinery every other
    journey golden uses.
    """
    parts: list[str] = []
    for header, echoes, raw in legs:
        parts.extend(header)
        parts.extend(echoes)
        parts.append(raw)
        parts.append("")
    return H.build_raw_transcript("\n".join(parts))


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    print(f"journey: init repo autodetection on {repo}", flush=True)

    # ---- leg 1: DETECTED — init from a checkout of the throwaway ----------
    checkout = make_checkout(repo)
    url = origin_url(checkout)
    raw_detected, rc_detected, home_detected = drive_init(checkout, env)
    line_detected = prompt_line(raw_detected)

    # ---- leg 2: CONTRAST — init from a NON-git directory ------------------
    nongit = tempfile.mkdtemp(prefix="rein-journey-nongit-")
    raw_bare, rc_bare, home_bare = drive_init(nongit, env)
    line_bare = prompt_line(raw_bare)

    # ---- plain assertion: `rein run` no-session hint names the cwd repo ---
    # Reuse leg 2's home (shim present, session NOT scaffolded). From the CHECKOUT
    # the hint must name `rein init --repo <repo>`; from a NON-git dir it degrades
    # to the generic hint — proving the hint, like the prompt default, is cwd-derived.
    hint_detected = run_no_session_hint(checkout, home_bare, env)
    hint_bare = run_no_session_hint(nongit, home_bare, env)

    detected_default = f"[{repo}]"
    run_repo_hint = f"rein init --repo {repo}"

    # 1) The #69 detection itself must hold — independent of the golden (exit 2).
    invariants = [
        (rc_detected == 0, f"detected-leg init must exit 0; got {rc_detected}"),
        (rc_bare == 0, f"contrast-leg init must exit 0; got {rc_bare}"),
        (
            "(default detected from this dir)" in line_detected,
            "detected leg: the prompt says it detected a default from this dir",
        ),
        (
            detected_default in line_detected,
            f"detected leg: the prompt is PRE-FILLED with {detected_default}",
        ),
        (
            f"(repos: [{repo}])" in H.strip_ansi(raw_detected),
            "detected leg: accepting the default scaffolded the session for the detected repo",
        ),
        (
            "(owner/name, Enter to skip)" in line_bare,
            "contrast leg: a NON-git dir gets the bare prompt",
        ),
        (
            "[" not in line_bare,
            f"contrast leg: the bare prompt has NO pre-filled default (got: {line_bare!r})",
        ),
        (
            "not scaffolded" in H.strip_ansi(raw_bare),
            "contrast leg: a bare Enter with no default scaffolds nothing (graceful skip)",
        ),
        (
            run_repo_hint in H.strip_ansi(hint_detected),
            f"run-hint from the checkout must name `{run_repo_hint}`",
        ),
        (
            run_repo_hint not in H.strip_ansi(hint_bare)
            and "rein init` to create one" in H.strip_ansi(hint_bare),
            "run-hint from a NON-git dir is the generic hint (no --repo) — cwd-derived",
        ),
    ]
    broken = [msg for ok, msg in invariants if not ok]
    if broken:
        print("JOURNEY BROKE — the #69 cwd-autodetection did not behave:", flush=True)
        for msg in broken:
            print(f"  - {msg}", flush=True)
        print(f"  detected prompt: {line_detected!r}", flush=True)
        print(f"  contrast prompt: {line_bare!r}", flush=True)
        return 2

    # 2) Build the RAW transcript (real values) and compare NORMALIZED.
    legs = [
        (
            [
                "# leg 1 — DETECTED: `rein init` from a checkout of the throwaway repo.",
                "# The repo prompt's DEFAULT is autodetected from the cwd's git `origin`;",
                "# the human ACCEPTS it with Enter and the session is scaffolded for it.",
            ],
            [
                f"$ git -C {checkout} remote get-url origin",
                url,
                f"$ cd {checkout} && rein init {' '.join(INIT_FLAGS)}",
            ],
            raw_detected,
        ),
        (
            [
                "# leg 2 — CONTRAST: `rein init` from a NON-git directory (no `origin`).",
                "# No cwd git remote => NO detected default (the bare prompt) — proving the",
                "# default is cwd-derived, not hardcoded. Enter here is a graceful skip.",
            ],
            [f"$ cd {nongit} && rein init {' '.join(INIT_FLAGS)}"],
            raw_bare,
        ),
    ]
    raw = build_transcript(legs)
    print()
    print(raw, flush=True)  # what actually happened, real repo + real prompt
    print("--- outcomes (asserted; not in the golden) ---", flush=True)
    print(f"  detected leg  prompt: {line_detected}", flush=True)
    print(f"  contrast leg  prompt: {line_bare}", flush=True)
    print(f"  run-hint (checkout)  names: {run_repo_hint}", flush=True)

    if os.getenv("REIN_SHOW_NORMALIZED"):
        print("\n--- normalized (the comparison lens) ---", flush=True)
        print(H.normalize_for_compare(raw), flush=True)

    if os.getenv("REIN_UPDATE_GOLDEN"):
        p = H.update_golden(GOLDEN_NAME, raw)  # store RAW
        print(f"[golden UPDATED] {p} (raw)", flush=True)
        return 0

    ok, diff = H.compare_golden(GOLDEN_NAME, raw)  # normalizes BOTH sides
    if ok:
        print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
        return 0
    scratch = os.path.join(tempfile.gettempdir(), "init_autodetect.fresh.txt")
    with open(scratch, "w") as f:
        f.write(raw)
    print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
