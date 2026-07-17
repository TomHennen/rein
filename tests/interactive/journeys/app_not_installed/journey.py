"""app_not_installed — MISCONFIG: App not installed on a session repo (#68).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: both legs are `rein run --direct`, so there is no sandbox side and command
echo is HOST-level (`$ rein run --direct -- …` + rein's raw output), NOT `SBX| `-tagged
— tagging host output would falsely claim it came from inside the sandbox. Uses
build_raw_transcript + compare_golden like every RAW-golden journey.
"""

from __future__ import annotations

import os
import re
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"

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
        p = H.update_golden(GOLDEN, raw)  # store RAW
        print(f"[golden UPDATED] {p} (raw)", flush=True)
        return 0

    ok, diff = H.compare_golden(GOLDEN, raw)  # normalizes BOTH sides
    if ok:
        print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
        return 0
    scratch = os.path.join(tempfile.gettempdir(), "app_not_installed.fresh.txt")
    with open(scratch, "w") as f:
        f.write(raw)
    print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
