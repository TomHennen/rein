"""egress_preset — the "dev" egress preset opens package registries, nothing more
(#163). srt 0.0.63 can't express true allow-all, so a session opts into a curated
bundle by name; this walks it LIVE in-sandbox.

Invariants (a break is exit 1, independent of the golden):
  - a preset host is REACHABLE (pypi.org -> 200),
  - a NON-preset host stays blocked (example.com -> 000, the srt allowlist deny),
  - the preset banner names the bundle.

Line-oriented (no tmux/pyte): one `rein run --sandbox -- bash -c` step.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"


def sandbox_script() -> str:
    def code(host: str, name: str) -> str:
        return f'emit "@{name}=$(curl -sS -o /dev/null -w %{{http_code}} --max-time 20 https://{host}/ 2>&1 | tail -c 3)"'

    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@EGRESS_PRESET  (dev preset: package registries reachable, other hosts blocked)"
{code("pypi.org", "PRESET_HOST")}
{code("example.com", "OTHER_HOST")}
"""


def _val(text: str, name: str) -> str | None:
    m = re.search(rf"{re.escape(H.SBX_TAG)}@{name}=(\S+)", text)
    return m.group(1) if m else None


def _pinned_session(repo: str) -> str:
    """Repo-only session that opts into the dev egress preset."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_egress_preset\nrole: implement\n"
                f"repos:\n  - {repo}\negress_preset: dev\n")
    return path


def clone_checkout(repo: str, env: dict) -> str:
    d = tempfile.mkdtemp(prefix="rein-preset-")
    subprocess.run(["gh", "repo", "clone", repo, d, "--", "-q"],
                   check=True, env=env, capture_output=True, text=True)
    return d


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)
    print(f"journey: dev egress preset on {repo}", flush=True)

    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        step = H.JourneyStep(
            argv=["run", "--", "bash", "-c", sandbox_script(), workdir],
            label=f"rein run -- bash -c <egress-preset probe> {workdir}",
            extra_env={"REIN_SESSION_FILE": _pinned_session(repo), "REIN_SANDBOX_WORKDIR": workdir},
            timeout=180,
        )
        result = H.run_journey([step], env=env)
        raw = result.transcript

        checks = {
            "preset host pypi.org is reachable (200)": _val(raw, "PRESET_HOST") == "200",
            "non-preset host example.com is blocked (000)": _val(raw, "OTHER_HOST") == "000",
            "the preset banner names the bundle": 'egress preset "dev" active' in raw,
        }
        broken = [name for name, ok in checks.items() if not ok]
        if broken:
            print("CLAIM BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print("--- transcript ---\n" + raw, flush=True)
            return 1

        print("--- outcomes (asserted; not all in the golden) ---", flush=True)
        print("  pypi.org 200 (preset), example.com 000 (blocked) — the dev preset opens registries only", flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0
        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "egress_preset.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        return 1
    finally:
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        print("cleanup: checkout removed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
