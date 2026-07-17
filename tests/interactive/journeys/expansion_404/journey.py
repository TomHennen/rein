"""expansion_404 — the 404-at-expansion install NOTICE (#69, mocks §1.4/§5.2).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: REIN_APPROVAL=tty forces the inline /dev/tty NOTICE (pexpect IS the human),
never the tmux popup; the journey answers 's' (skip) — the deterministic path, since
nothing is installed mid-journey. The happy sibling is journeys/scope_expansion/journey.py.
"""

from __future__ import annotations

import os
import re
import shutil
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"

# A FIXED fictional leaf under the throwaway's owner. Stable-by-construction (so NOT
# normalized, #78), does not exist, 404s identically to "App not installed". Same
# owner as the session so the same-owner check passes and we REACH the coverage probe.
UNCOVERED_LEAF = "definitely-not-installed"

# The issue number the agent declares. Never fetched (the coverage probe 404s before
# any fetch), so any positive int works; it renders in the NOTICE as `(for issue #N)`
# and normalizes to #<ISSUE>.
DECLARED_ISSUE = 1


def sandbox_script(uncovered: str) -> str:
    """A `bash -c` body run as the srt child. One step: declare an expansion to the
    uncovered repo. It BLOCKS on the host-tty NOTICE (no approval prompt — nothing is
    approvable), then returns REFUSED. `run` (reinharness.sandbox_preamble) echoes the
    command `SBX| $ <cmd>` and tags its output; the @DECLARE_RC sentinel carries the
    declare's own exit code so the test asserts it refused (rc != 0)."""
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@DECLARE_START  rein declare {DECLARED_ISSUE} --repo {uncovered}  (App NOT installed: NOTICE, no approval)"
run rein declare {DECLARED_ISSUE} --repo {uncovered}
emit "@DECLARE_RC=$?"
emit "@SCRIPT_DONE"
"""


def _pinned_session(repo: str) -> str:
    """A temp session scoped to repo A ONLY (so the uncovered repo is genuinely an
    out-of-scope EXPANSION, which is what routes the declare through the coverage
    probe). Fixed id; no `issue:`/`created:` keys. Selected via REIN_SESSION_FILE."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-404-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_expansion_404\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def _rc(text: str, name: str) -> int | None:
    m = re.search(rf"@{name}_RC=(\d+)", text)
    return int(m.group(1)) if m else None


def main() -> int:
    env = H.rein_env()
    repo_a = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    owner = repo_a.split("/", 1)[0]
    uncovered = f"{owner}/{UNCOVERED_LEAF}"

    print(f"journey: 404-at-expansion NOTICE  scope={repo_a}  "
          f"declare --repo {uncovered} (fictional, uncovered)", flush=True)

    workdir = None
    try:
        workdir = H.make_workdir()
        # ONE sandbox step. Its only prompt is the NOTICE acknowledgement — answer 's'
        # (skip): nothing is installed mid-journey, so ENTER-when-done would loop. The
        # 's' path is the deterministic one and still refuses the declare.
        step = H.JourneyStep(
            argv=["run", "--", "bash", "-c", sandbox_script(uncovered), workdir],
            label=f"rein run -- bash -c <declare-uninstalled agent script> {workdir}",
            answers=[(r"or 's' to skip", "s")],
            extra_env={
                "REIN_SESSION_FILE": _pinned_session(repo_a),
                "REIN_SANDBOX_WORKDIR": workdir,
                # Force the inline /dev/tty NOTICE (pexpect IS the human); never the
                # tmux popup (the default when $TMUX is set).
                "REIN_APPROVAL": "tty",
            },
            timeout=180,
        )
        result = H.run_journey([step], env=env)
        text = result.transcript
        step_text = result.steps[0].text

        # 1) The NOTICE + refusal path must hold — independent of the golden (exit 2).
        declare_rc = _rc(step_text, "DECLARE")
        invariants = [
            (result.reached_eof, "the sandbox step must run to EOF (the NOTICE prompt was answered)"),
            ("NOTICE — App not installed (nothing to approve)" in text,
             "the install NOTICE must fire on the host tty (no approval prompt)"),
            (uncovered in text, f"the NOTICE must name the uncovered repo ({uncovered})"),
            ("there is no approval to give" in text,
             "the NOTICE must state it carries NO approval authority"),
            ("installations/new" in text, "the NOTICE must carry the install deep-link"),
            (declare_rc not in (None, 0), f"the declare must REFUSE (rc != 0); got rc={declare_rc}"),
            ("is not installed on it" in text,
             "the agent must receive the 404 refusal ('is not installed on it')"),
            ("The human must install it first" in text,
             "the agent's refusal must tell the human to install first"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("404-NOTICE PATH BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  declare_rc={declare_rc}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # 2) Compare the WHOLE captured session NORMALIZED.
        raw = result.transcript
        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  install NOTICE fired on the host tty (no approval prompt); named {uncovered}", flush=True)
        print(f"  declare refused: rc={declare_rc} (agent told to install-then-retry)", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "expansion_404.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        print("cleanup: working tree removed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
