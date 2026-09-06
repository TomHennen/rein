"""egress_preset — the "dev" egress preset is ON BY DEFAULT and opens package
registries, nothing more (#163); `egress_preset: none` turns it off. srt 0.0.63
can't express true allow-all, so a curated bundle is the substitute; this walks
both the default and the opt-out LIVE in-sandbox.

Invariants (a break is exit 1, independent of the golden):
  - with NO preset configured, a preset host is REACHABLE (pypi.org -> 200) and
    so is a host behind a *.suffix wildcard (files.pythonhosted.org -> 200: the
    hosts real installs hit are all wildcard-covered, so the contract's
    "dependency installs work" rests on this),
  - a NON-preset host stays blocked (example.com -> 000, the srt allowlist deny),
  - the launch banner names the default bundle,
  - with `egress_preset: none`, the same preset host is BLOCKED (pypi.org -> 000).

Line-oriented (no tmux/pyte): two `rein run --sandbox -- bash -c` steps.
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


def _code(host: str, name: str) -> str:
    return f'emit "@{name}=$(curl -sS -o /dev/null -w %{{http_code}} --max-time 20 https://{host}/ 2>&1 | tail -c 3)"'


def default_script() -> str:
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@EGRESS_PRESET  (default dev preset: package registries reachable, other hosts blocked)"
{_code("pypi.org", "PRESET_HOST")}
{_code("files.pythonhosted.org", "PRESET_WILDCARD_HOST")}
{_code("example.com", "OTHER_HOST")}
"""


def none_script() -> str:
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@EGRESS_PRESET_NONE  (egress_preset: none — the registries are blocked again)"
{_code("pypi.org", "NONE_PRESET_HOST")}
"""


def _val(text: str, name: str) -> str | None:
    m = re.search(rf"{re.escape(H.SBX_TAG)}@{name}=(\S+)", text)
    return m.group(1) if m else None


def _pinned_session(repo: str, preset: str | None) -> str:
    """Repo-only session; preset=None leaves egress_preset unset (the default)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(f"id: sess_journey_egress_preset\nrole: implement\nrepos:\n  - {repo}\n")
        if preset is not None:
            f.write(f"egress_preset: {preset}\n")
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
    print(f"journey: default dev egress preset + none opt-out on {repo}", flush=True)

    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        # Pin the machine-wide egress knobs to empty: the default step must see
        # rein's default, not an operator's exported REIN_EGRESS_PRESET / extra
        # allow-list, or the probe flips for reasons unrelated to the code.
        pinned = {"REIN_SANDBOX_WORKDIR": workdir, "REIN_EGRESS_PRESET": "", "REIN_ALLOW_DOMAINS": ""}
        steps = [
            H.JourneyStep(
                argv=["run", "--", "bash", "-c", default_script(), workdir],
                label=f"rein run -- bash -c <default egress-preset probe> {workdir}",
                extra_env={"REIN_SESSION_FILE": _pinned_session(repo, None), **pinned},
                timeout=180,
            ),
            H.JourneyStep(
                argv=["run", "--", "bash", "-c", none_script(), workdir],
                label=f"rein run -- bash -c <egress_preset: none probe> {workdir}",
                extra_env={"REIN_SESSION_FILE": _pinned_session(repo, "none"), **pinned},
                timeout=180,
            ),
        ]
        result = H.run_journey(steps, env=env)
        raw = result.transcript

        checks = {
            "default: preset host pypi.org is reachable (200)": _val(raw, "PRESET_HOST") == "200",
            "default: wildcard preset host files.pythonhosted.org is reachable (200)": _val(raw, "PRESET_WILDCARD_HOST") == "200",
            "default: non-preset host example.com is blocked (000)": _val(raw, "OTHER_HOST") == "000",
            "default: the launch banner names the dev bundle": 'egress preset "dev" active' in raw,
            "none: preset host pypi.org is blocked (000)": _val(raw, "NONE_PRESET_HOST") == "000",
            "none: the launch banner says no registry hosts": 'egress preset "none"' in raw,
        }
        broken = [name for name, ok in checks.items() if not ok]
        if broken:
            print("CLAIM BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print("--- transcript ---\n" + raw, flush=True)
            return 1

        print("--- outcomes (asserted; not all in the golden) ---", flush=True)
        print("  default: pypi.org 200, files.pythonhosted.org 200 (wildcard), example.com 000 — registries only", flush=True)
        print("  egress_preset: none: pypi.org 000 — the opt-out closes them again", flush=True)

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
