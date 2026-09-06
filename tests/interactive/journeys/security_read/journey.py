"""security_read — the App's security-status READ perms reach the sandboxed
agent (#165): Dependabot alerts + code scanning, which returned "Resource not
accessible by integration" before the App was granted vulnerability_alerts:read
+ security_events:read.

Environment-dependent by nature: the perms live on the App INSTALLATION, so the
result depends on THIS box's App state. The journey classifies the live gh-api
response:
  - the perm is PRESENT (200, or a feature-disabled reply) -> PASS + golden,
  - the perm is ABSENT ("Resource not accessible by integration") -> SKIP
    (exit 3): the App has not been upgraded, so this coverage did not run — the
    same discipline credential_boundary uses when `bagel` is absent (never a
    green pass for a path nothing exercised, #68).

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


def sandbox_script(repo: str) -> str:
    # Classify each security endpoint into a STABLE sentinel (deterministic in the
    # golden): ok / feature-off / no-perm / unknown. no-perm is the missing-grant
    # signal the journey skips on.
    def classify(path: str, name: str) -> str:
        return f"""
out=$(gh api {path} 2>&1); rc=$?
if [ $rc -eq 0 ]; then emit "@{name}=ok"
elif printf '%s' "$out" | grep -qi 'not accessible by integration'; then emit "@{name}=no-perm"
elif printf '%s' "$out" | grep -qiE 'disabled|no analysis|not enabled|not found'; then emit "@{name}=feature-off"
else emit "@{name}=unknown"; fi"""

    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@SECURITY_READ  (dependabot + code-scanning reads — perm present iff not no-perm)"
{classify(f"repos/{repo}/dependabot/alerts", "DEP")}
{classify(f"repos/{repo}/code-scanning/alerts", "SCAN")}
"""


def _val(text: str, name: str) -> str | None:
    m = re.search(rf"{re.escape(H.SBX_TAG)}@{name}=(\S+)", text)
    return m.group(1) if m else None


def _pinned_session(repo: str) -> str:
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_security_read\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def clone_checkout(repo: str, env: dict) -> str:
    d = tempfile.mkdtemp(prefix="rein-secread-")
    subprocess.run(["gh", "repo", "clone", repo, d, "--", "-q"],
                   check=True, env=env, capture_output=True, text=True)
    return d


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)
    print(f"journey: security-status read perms (dependabot + code scanning) on {repo}", flush=True)

    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        step = H.JourneyStep(
            argv=["run", "--", "bash", "-c", sandbox_script(repo), workdir],
            label=f"rein run -- bash -c <security-read probe> {workdir}",
            extra_env={"REIN_SESSION_FILE": _pinned_session(repo), "REIN_SANDBOX_WORKDIR": workdir},
            timeout=180,
        )
        result = H.run_journey([step], env=env)
        raw = result.transcript
        dep, scan = _val(raw, "DEP"), _val(raw, "SCAN")

        # SKIP (exit 3) if the App simply lacks the grant — no coverage, not a pass.
        if dep == "no-perm" or scan == "no-perm":
            print(f"SKIP: the App lacks the security read grant (DEP={dep} SCAN={scan}) — "
                  "coverage did NOT run. Add vulnerability_alerts:read + security_events:read "
                  "on the App and accept the installation update, then re-run.", flush=True)
            return 3

        # Perm present iff a real GitHub response came back (ok or feature-off).
        ok_vals = {"ok", "feature-off"}
        if dep not in ok_vals or scan not in ok_vals:
            print("CLAIM BROKE:", flush=True)
            print(f"  - dependabot read did not resolve to a granted response (DEP={dep})" if dep not in ok_vals else "", flush=True)
            print(f"  - code-scanning read did not resolve to a granted response (SCAN={scan})" if scan not in ok_vals else "", flush=True)
            print("--- transcript ---\n" + raw, flush=True)
            return 1

        print("--- outcomes (asserted; not all in the golden) ---", flush=True)
        print(f"  dependabot alerts: {dep} (vulnerability_alerts:read present)", flush=True)
        print(f"  code-scanning alerts: {scan} (security_events:read present)", flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0
        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "security_read.fresh.txt")
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
