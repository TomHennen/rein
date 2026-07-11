"""journey_app_not_installed — MISCONFIG: App not installed on a session repo.

JOURNEY CATALOGUE (tests/interactive/README.md, row 10): this file IS the
"Misconfig: App not installed on a session repo" journey. It is the live demo
that issue #68 asked for — the D4 install-coverage check (design.md:581: an
uncovered session repo must be a LOUD launch error, not a placeholder failing
inside the agent) was early-returning on the REIN_APP_* env path, so the whole
check never ran there. A unit suite stayed green; only RUNNING it caught the gap.

    catalogue row 10 flips GAP -> COVERED once this lands; this file is the
    demo the catalogue row points to. (The numbered catalogue table itself
    lives on PR #72 / branch e2e-suite-doctrine, which rewrites this README into
    the catalogue; on THIS branch, cred-boundary, the table isn't present yet.
    Reconciled at merge — see the PR body.)

Journeys live in `journey_*.py`; the assertion tests live in `test_*.py` and are
what `run.sh` sweeps. This file is NOT swept (it needs live creds). It both SHOWS
the two outcomes and ASSERTS them, so an autonomous agent can re-run it and get a
clean pass/fail plus a pasteable transcript.

Two legs, both driven through a real `rein run`:

  * MISCONFIG: a session naming an UNCOVERED repo. rein must refuse at LAUNCH,
    exit 1, and the refusal must name the repo, name the App (slug), and carry
    the App-specific `.../installations/new` deep-link. The inner command must
    NEVER run (the agent never reaches the point where a mint would fail).
  * CONTROL: a normal single-repo session on the throwaway. rein must clear the
    coverage gate and actually launch the inner command, exit 0.

WHY --direct: the coverage gate (cmd/rein/run.go: resolveAndCacheInstallID) runs
BEFORE the sandboxed/direct mode split, so it fires identically either way. Using
`--direct` lets this journey demonstrate the gate WITHOUT depending on a healthy
srt/bwrap sandbox stack or paying for a full sandboxed clone — the refusal and
the control are both about the launch-time gate, not the sandbox.

THE UNCOVERED REPO IS FICTIONAL AND SAFE. It is `<owner>/definitely-not-installed
-<unix-ts>` under the SAME owner as the throwaway. GitHub's
`GET /repos/{owner}/{repo}/installation` 404s identically for "repo does not
exist" and "App not installed on repo", so this touches NO real repo — it cannot,
it names one that does not exist. Hard-constraint #1 holds: nothing real is
touched, least of all a real repo.

NOT PART OF THE SWEEP. `run.sh` discovers `test_*.py`; this is `journey_*.py`.
Run it deliberately:

    source ./dev-env
    python3 tests/interactive/journey_app_not_installed.py
    REIN_DEMO_RAW=1 python3 tests/interactive/journey_app_not_installed.py   # no normalization

The printed transcript normalizes the volatile bits (the timestamp in the
fictional repo name, the App slug, tmp paths) so it is a stable doc/golden
source. Set REIN_DEMO_RAW=1 to see the raw transcript. NOTE: when PR #72's
golden-transcript helpers land (normalized transcripts under
tests/interactive/golden/), this file should adopt them; until then its
normalization is deliberately simple and local (see normalize()).
"""

from __future__ import annotations

import os
import re
import sys
import tempfile
import time

import reinharness as H

RULE = "=" * 78


def say(s: str = "") -> None:
    print(s, flush=True)


def banner(title: str) -> None:
    say()
    say(RULE)
    say(f"  {title}")
    say(RULE)


def write_session(repo: str, sess_id: str) -> str:
    """A temp repo-only session naming `repo`, selected via REIN_SESSION_FILE so
    the journey never depends on the machine's ambient dev-session.yaml (and
    writes no `issue:` — #35 retired that field)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(f"id: {sess_id}\nrole: implement\nrepos:\n  - {repo}\n")
    return path


def run_leg(inner_argv, session_file, env):
    """Drive one `rein run --direct` to completion; return (exit_code, text)."""
    wd = H.make_workdir()
    run = H.spawn_rein_run(
        inner_argv,
        workdir=wd,
        env=env,
        extra_env={"REIN_SESSION_FILE": session_file},
        direct=True,
        timeout=90,
    )
    try:
        code = run.wait(timeout=90)
    except Exception:
        try:
            run.child.close(force=True)
        except Exception:
            pass
        code = run.wait(timeout=5)
    return code, H.strip_ansi(run.text())


def normalize(text: str, uncovered_repo: str) -> str:
    """Replace the volatile bits so the transcript is a stable golden source.

    Local and simple on purpose (see module docstring re: PR #72). Normalizes:
      * the unix timestamp in the fictional repo name -> <TS>
      * the App slug (varies per developer's App) -> <APP-SLUG>
      * tmp paths (session file, per-run gitconfig, workdir) -> <TMP>
    """
    if os.getenv("REIN_DEMO_RAW"):
        return text
    out = text
    # <owner>/definitely-not-installed-1783800473 -> <owner>/definitely-not-installed-<TS>
    out = re.sub(r"(definitely-not-installed)-\d+", r"\1-<TS>", out)
    # App <slug> is not installed ...  and  github.com/apps/<slug>/installations/new
    m = re.search(r"App (\S+) is not installed", out)
    if m:
        out = out.replace(m.group(1), "<APP-SLUG>")
    # tmp paths
    out = re.sub(r"/tmp/[^\s,)]+", "<TMP>", out)
    return out


def block(text: str) -> None:
    for ln in text.replace("\r\n", "\n").replace("\r", "\n").split("\n"):
        say(f"  | {ln.rstrip()}")


# The sentinel the inner command echoes. The whole point of the misconfig leg is
# that this NEVER appears (rein refuses before the command runs).
SENTINEL = "REIN_INNER_COMMAND_RAN"


def main() -> int:
    env = H.rein_env()
    repo = H.throwaway_repo(env)  # hard-constraint #1: the throwaway, only
    H.build_binaries(env)

    owner = repo.split("/", 1)[0]
    ts = int(time.time())
    uncovered = f"{owner}/definitely-not-installed-{ts}"

    banner("JOURNEY (row 10): Misconfig — App not installed on a session repo")
    say(f"  throwaway (covered)  : {repo}")
    say(f"  fictional (uncovered): {uncovered}")
    say("                         ^ does not exist; 404s identically to")
    say("                           'App not installed on repo'. Touches nothing.")
    say()
    say("  #68: the D4 install-coverage check early-returned on the REIN_APP_*")
    say("  env path, so an uncovered repo launched happily and the mint failed")
    say("  INSIDE the agent. The fix makes it a loud LAUNCH refusal. Both legs")
    say("  below run a real `rein run --direct` (the gate runs before the mode")
    say("  split, so --direct exercises it without the sandbox stack).")

    failures: list[str] = []

    # ---- Leg 1: MISCONFIG -------------------------------------------------
    sess_bad = write_session(uncovered, "sess_journey_uncovered")
    code, raw = run_leg(["echo", SENTINEL], sess_bad, env)

    banner("(1) MISCONFIG leg — session names the uncovered repo")
    # Elide the static direct-mode warning header; show from the refusal.
    shown_bad = normalize(raw, uncovered)
    say("  (direct-mode warning header elided; showing from the refusal)")
    idx_bad = shown_bad.find("rein run: App")
    block(shown_bad[idx_bad:] if idx_bad != -1 else shown_bad)
    say()
    say(f"  rein run exit code: {code}")

    checks = [
        (code == 1, f"exit 1 (loud refusal)              got exit {code}"),
        ("is not installed on" in raw, "refusal says 'is not installed on'"),
        (uncovered in raw, f"refusal names the repo ({uncovered})"),
        (bool(re.search(r"App \S+ is not installed", raw)),
         "refusal names the App (slug)"),
        ("installations/new" in raw, "refusal carries the install deep-link"),
        (SENTINEL not in raw,
         "inner command NEVER ran (refused before launch)"),
    ]
    say()
    for ok, desc in checks:
        say(f"    [{'PASS' if ok else 'FAIL'}] {desc}")
        if not ok:
            failures.append(f"misconfig: {desc}")

    # Deep-link must be App-SPECIFIC (the #68 review found the "we can't know the
    # slug on the env path" premise false; it now deep-links the App's own page).
    m = re.search(r"App (\S+) is not installed", raw)
    if m:
        slug = m.group(1)
        app_specific = f"github.com/apps/{slug}/installations/new" in raw
        say(f"    [{'PASS' if app_specific else 'FAIL'}] "
            f"deep-link is App-specific (github.com/apps/{slug}/installations/new)")
        if not app_specific:
            failures.append("misconfig: deep-link is not App-specific")

    # ---- Leg 2: CONTROL ---------------------------------------------------
    sess_ok = write_session(repo, "sess_journey_control")
    code2, raw2 = run_leg(["echo", SENTINEL], sess_ok, env)

    banner("(2) CONTROL leg — session names the covered throwaway repo")
    # Trim the direct-mode warning header for readability; show from the launch.
    shown = normalize(raw2, uncovered)
    say("  (direct-mode warning header elided; showing from the launch)")
    idx = shown.find("rein: launching")
    block(shown[idx:] if idx != -1 else shown)
    say()
    say(f"  rein run exit code: {code2}")

    checks2 = [
        (code2 == 0, f"exit 0 (launched)                  got exit {code2}"),
        ("is not installed" not in raw2,
         "no coverage refusal (gate cleared)"),
        (SENTINEL in raw2, f"inner command RAN ({SENTINEL} printed)"),
    ]
    say()
    for ok, desc in checks2:
        say(f"    [{'PASS' if ok else 'FAIL'}] {desc}")
        if not ok:
            failures.append(f"control: {desc}")

    # ---- Verdict ----------------------------------------------------------
    banner("VERDICT")
    if failures:
        say(f"  FAILED ({len(failures)}):")
        for fdesc in failures:
            say(f"    - {fdesc}")
        say()
        say("  The install-coverage gate did not behave as #68 requires.")
        return 1
    say("  PASS — an uncovered session repo is a loud launch refusal (exit 1,")
    say("  names the repo + App + App-specific install deep-link), and a covered")
    say("  session still launches (exit 0). #68 holds.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
