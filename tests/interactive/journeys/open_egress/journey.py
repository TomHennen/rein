"""open_egress — `open_egress: true` lets the sandboxed agent reach any PUBLIC
host on 443, enforced by REIN as the sandbox's egress proxy (#185: srt's
external-proxy shape), while loopback / private targets, other ports and plain
http stay refused even in open mode. Walked LIVE in-sandbox.

Invariants (a break is exit 1, independent of the golden):
  - open mode: an unlisted public host (example.com) answers 200,
  - open mode still refuses, with rein's 403 after CONNECT: a loopback target
    and a private (RFC1918) target when FORCED through the proxy (curl is told
    to ignore NO_PROXY, which srt sets for those ranges), a non-443 port, and
    plain http://,
  - the launch banner carries the non-suppressible EGRESS IS OPEN block and the
    injected contract's NETWORK section says egress is open,
  - the launch self-test line says egress was answered by rein,
  - the run's audit log records the allowed host (`allowed-egress-open`) and the
    refusals (`refused-egress-*`): every contacted host is on the record.

Line-oriented (no tmux/pyte): one `rein run -- bash -c` step.
"""

from __future__ import annotations

import glob
import os
import re
import shutil
import subprocess
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"


def _probe(name: str, url: str, forced: bool = False) -> str:
    # --noproxy '' overrides srt's NO_PROXY so loopback/private targets are
    # sent to the proxy (where rein must refuse them) instead of dialled
    # directly (where the unshared namespace refuses them for a different reason).
    extra = "--noproxy '' " if forced else ""
    return f'emit "@{name}=$(curl -sS {extra}-o /dev/null -w %{{http_code}} --max-time 20 {url} 2>&1 | tr -d \'\\n\' | tail -c 60)"'


def script() -> str:
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@OPEN_EGRESS  (open_egress: true — any public host on 443 through rein; never-route + ports still refused)"
{_probe("PUBLIC", "https://example.com/")}
{_probe("LOOPBACK", "https://127.0.0.1/", forced=True)}
{_probe("PRIVATE", "https://10.0.0.1/", forced=True)}
{_probe("OTHER_PORT", "https://example.com:8443/")}
{_probe("PLAINTEXT", "http://example.com/")}
"""


def _val(text: str, name: str) -> str | None:
    m = re.search(rf"{re.escape(H.SBX_TAG)}@{name}=(.*)", text)
    return m.group(1).strip() if m else None


def _pinned_session(repo: str) -> str:
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(f"id: sess_journey_open_egress\nrole: implement\nrepos:\n  - {repo}\nopen_egress: true\n")
    return path


def clone_checkout(repo: str, env: dict) -> str:
    d = tempfile.mkdtemp(prefix="rein-open-")
    subprocess.run(["gh", "repo", "clone", repo, d, "--", "-q"],
                   check=True, env=env, capture_output=True, text=True)
    return d


def _newest_audit_log() -> str:
    state = os.environ.get("XDG_STATE_HOME") or os.path.join(os.path.expanduser("~"), ".local", "state")
    logs = glob.glob(os.path.join(state, "rein", "audit", "sandbox-*.log"))
    if not logs:
        return ""
    newest = max(logs, key=os.path.getmtime)
    with open(newest) as f:
        return f.read()


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)
    print(f"journey: open_egress (rein-enforced open mode) on {repo}", flush=True)

    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        pinned = {"REIN_SANDBOX_WORKDIR": workdir, "REIN_EGRESS_PRESET": "", "REIN_ALLOW_DOMAINS": ""}
        steps = [
            H.JourneyStep(
                argv=["run", "--", "bash", "-c", script(), workdir],
                label=f"rein run -- bash -c <open-egress probe> {workdir}",
                extra_env={"REIN_SESSION_FILE": _pinned_session(repo), **pinned},
                timeout=180,
            ),
        ]
        result = H.run_journey(steps, env=env)
        raw = result.transcript
        audit = _newest_audit_log()

        refused = "response 403"
        checks = {
            "open: public example.com answers 200": _val(raw, "PUBLIC") == "200",
            "open: loopback target forced through the proxy is refused by rein (403 after CONNECT)": refused in (_val(raw, "LOOPBACK") or ""),
            "open: private 10.0.0.1 forced through the proxy is refused by rein": refused in (_val(raw, "PRIVATE") or ""),
            "open: a non-443 port is refused": refused in (_val(raw, "OTHER_PORT") or ""),
            "open: plain http:// is refused (403 from the proxy)": (_val(raw, "PLAINTEXT") or "").endswith("403"),
            "banner: the EGRESS IS OPEN block is printed": "EGRESS IS OPEN" in raw and "open-egress off" in raw,
            "contract: NETWORK says egress is open": "Egress is OPEN" in raw,
            "self-test: egress answered by rein": "egress answered by rein" in raw,
            "audit: the allowed host is on record": "decision=allowed-egress-open" in audit and "example.com:443" in audit,
            "audit: the refusals are on record": "decision=refused-egress-" in audit,
        }
        broken = [name for name, ok in checks.items() if not ok]
        if broken:
            print("CLAIM BROKE:", flush=True)
            for b in broken:
                print(f"  - {b}", flush=True)
            print("--- transcript ---\n" + raw, flush=True)
            print("--- audit (tail) ---\n" + "\n".join(audit.splitlines()[-12:]), flush=True)
            return 1

        print("--- outcomes (asserted; not all in the golden) ---", flush=True)
        print("  open: example.com 200; 127.0.0.1 / 10.0.0.1 (forced via proxy) / :8443 / http:// all refused by rein", flush=True)
        print("  audit: allowed-egress-open example.com:443 + refused-egress-* recorded", flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0
        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "open_egress.fresh.txt")
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
