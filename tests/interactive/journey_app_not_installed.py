"""journey_app_not_installed — MISCONFIG: App not installed on a session repo.

This is ONE journey. For what a journey IS, the golden-transcript rule, and how
to author the next one, read tests/interactive/CLAUDE.md — none of that lives here.

JOURNEY CATALOGUE (tests/interactive/README.md, row 10): this file IS the
"Misconfig: App not installed on a session repo" journey. It is the live demo
issue #68 asked for — the D4 install-coverage check (design.md:581: an uncovered
session repo must be a LOUD launch error, not a placeholder failing inside the
agent) was early-returning on the REIN_APP_* env path, so the whole check never
ran there. A unit suite stayed green; only RUNNING it caught the gap.

Two legs, both driven through a real `rein run --direct`:

  * MISCONFIG: a session naming an UNCOVERED repo. rein must refuse at LAUNCH,
    exit 1, and the refusal must name the repo, name the App (slug), and carry
    the App-specific `.../installations/new` deep-link. The inner command must
    NEVER run (the agent never reaches the point where a mint would fail).
  * CONTROL: a normal single-repo session on the throwaway. rein must clear the
    coverage gate and actually launch the inner command, exit 0.

WHY --direct: the coverage gate (cmd/rein/run.go: resolveAndCacheInstallID) runs
BEFORE the sandboxed/direct mode split, so it fires identically either way. Using
`--direct` demonstrates the gate WITHOUT depending on a healthy srt/bwrap sandbox
stack or paying for a full sandboxed clone — the refusal and the control are both
about the launch-time gate, not the sandbox.

COMMAND-ECHO IS HOST-SIDE, NOT `SBX| `. The write-ceremony exemplar runs an
in-sandbox script and tags each command `SBX| $ <cmd>` via
`reinharness.sandbox_preamble()`. THIS journey has no sandbox side: both legs are
`--direct`, and the misconfig leg refuses at LAUNCH so nothing runs in a sandbox
at all. Tagging rein's own host output `SBX| ` would be a lie ("this came from
inside the sandbox"). So the two host commands are echoed at the HOST level as
`$ rein run --direct -- …` then rein's raw output — the same command-echo
convention Tom asked for, just host-side. It DOES adopt the rest of the #78 model:
`reinharness.build_raw_transcript` for the RAW golden and `compare_golden`
(normalize BOTH sides) for drift detection.

DELIVERABLE: a RAW, human-reviewable transcript at `golden/app_not_installed.txt`
— real repo, real App slug, real refusal wording — so Tom SEES exactly what the
run produced (PR #78). Determinism does NOT live in the file: a fresh run is
compared by normalizing BOTH sides first (reinharness.compare_golden), so
different tmp paths still match while a genuinely new or changed rein line trips
drift. The repo names (throwaway + the fictional uncovered leaf) and the App slug
are stable-by-construction on a machine, so — like the write ceremony's repo —
they are kept RAW and matched verbatim.

    python3 tests/interactive/journey_app_not_installed.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_app_not_installed.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_app_not_installed.py # also print the compare lens

Exit 0 = the gate behaved AND the normalized transcript matches the golden.
Exit 1 = drift (RAW fresh transcript dropped to a scratch path; NORMALIZED diff
printed). Exit 2 = the #68 gate itself misbehaved (the invariants below failed).

THE UNCOVERED REPO IS FICTIONAL AND SAFE. It is `<owner>/definitely-not-installed`
under the SAME owner as the throwaway — a FIXED name (stable-by-construction, so
it is not normalized; #78). GitHub's `GET /repos/{owner}/{repo}/installation`
404s identically for "repo does not exist" and "App not installed on repo", so
this touches NO real repo — it cannot, it names one that does not exist.
Hard-constraint #1 holds.

SETUP is the `rein init` world; the repo is resolved the rein-init way
(reinharness.resolve_throwaway_repo): REIN_JOURNEY_REPO, else the configured
dev-session, else the legacy REIN_TEST_REPO_A shortcut (#40).
"""

from __future__ import annotations

import os
import re
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "app_not_installed.txt"

# The sentinel the inner command echoes. The whole point of the misconfig leg is
# that this NEVER appears (rein refuses before the command runs).
SENTINEL = "REIN_INNER_COMMAND_RAN"

# A FIXED fictional leaf under the throwaway's owner. Stable-by-construction (so
# NOT normalized, #78), does not exist, 404s identically to "App not installed".
UNCOVERED_LEAF = "definitely-not-installed"


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
    """Drive one `rein run --direct` to completion.

    Returns (exit_code, raw_text, command_shown). `command_shown` is the exact
    host command spawn_rein_run runs — `rein run --direct -- <inner> <workdir>` —
    so the transcript can echo it `$ …` before rein's own output.
    """
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
    command_shown = "rein run --direct -- " + " ".join([*inner_argv, wd])
    return code, run.text(), command_shown


def build_transcript(legs) -> str:
    """Assemble the interleaved HOST transcript and hand it to build_raw_transcript.

    For each leg: a `# …` label, the `$ <command>` echo, then rein's raw output.
    build_raw_transcript then applies the exact golden shape (ANSI strip, drop
    sub-100% progress ticks, collapse blank runs) — the same machinery the write
    ceremony's golden uses, so both journeys' goldens are built identically.
    """
    parts: list[str] = []
    for label, command_shown, raw in legs:
        parts.append(label)
        parts.append(f"$ {command_shown}")
        parts.append(raw)
        parts.append("")
    return H.build_raw_transcript("\n".join(parts))


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    owner = repo.split("/", 1)[0]
    uncovered = f"{owner}/{UNCOVERED_LEAF}"

    print(f"journey: app-not-installed on {repo} "
          f"(fictional uncovered repo: {uncovered})", flush=True)

    # ---- run both legs live ----------------------------------------------
    sess_bad = write_session(uncovered, "sess_journey_uncovered")
    code_bad, raw_bad, cmd_bad = run_leg(["echo", SENTINEL], sess_bad, env)

    sess_ok = write_session(repo, "sess_journey_control")
    code_ok, raw_ok, cmd_ok = run_leg(["echo", SENTINEL], sess_ok, env)

    # 1) The #68 gate itself must hold — independent of the golden (exit 2).
    m = re.search(r"App (\S+) is not installed", raw_bad)
    slug = m.group(1) if m else None
    invariants = [
        (code_bad == 1, f"misconfig leg must exit 1 (loud refusal); got exit {code_bad}"),
        ("is not installed on" in raw_bad, "refusal says 'is not installed on'"),
        (uncovered in raw_bad, f"refusal names the uncovered repo ({uncovered})"),
        (slug is not None, "refusal names the App (slug)"),
        ("installations/new" in raw_bad, "refusal carries the install deep-link"),
        (slug is not None and f"github.com/apps/{slug}/installations/new" in raw_bad,
         "deep-link is App-specific (github.com/apps/<slug>/installations/new)"),
        (SENTINEL not in raw_bad,
         "inner command NEVER ran on the misconfig leg (refused before launch)"),
        (code_ok == 0, f"control leg must exit 0 (launched); got exit {code_ok}"),
        ("is not installed" not in raw_ok, "control leg has no coverage refusal (gate cleared)"),
        (SENTINEL in raw_ok, f"control leg RAN the inner command ({SENTINEL} printed)"),
    ]
    broken = [msg for ok, msg in invariants if not ok]
    if broken:
        print("JOURNEY BROKE — the #68 install-coverage gate did not behave:", flush=True)
        for msg in broken:
            print(f"  - {msg}", flush=True)
        print(f"  misconfig exit={code_bad}  control exit={code_ok}", flush=True)
        return 2

    # 2) Build the RAW transcript (real values) and compare NORMALIZED.
    legs = [
        (f"# leg 1 — MISCONFIG: session names the UNCOVERED repo {uncovered}", cmd_bad, raw_bad),
        (f"# leg 2 — CONTROL: session names the COVERED throwaway {repo}", cmd_ok, raw_ok),
    ]
    raw = build_transcript(legs)
    print()
    print(raw, flush=True)  # what actually happened, real repo + real App slug
    print("--- outcomes (asserted; not in the golden) ---", flush=True)
    print(f"  misconfig leg  exit={code_bad}  (loud refusal; inner command never ran)", flush=True)
    print(f"  control leg    exit={code_ok}  (coverage gate cleared; inner command ran)", flush=True)

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
    scratch = os.path.join(tempfile.gettempdir(), "app_not_installed.fresh.txt")
    with open(scratch, "w") as f:
        f.write(raw)
    print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
