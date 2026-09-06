"""expose_port — `expose_ports` forwards the HOST's loopback to a server the agent
starts INSIDE the sandbox (#179). srt unshares the network namespace and its
seccomp filter blocks in-sandbox AF_UNIX sockets, so rein bridges an
operator-declared port with an agent-initiated reverse tunnel through the broker
socket. This walks it LIVE: an in-sandbox http.server on 127.0.0.1:18473 answers a
curl from the host at http://127.0.0.1:18473/.

Invariants (a break is exit 1, independent of the golden):
  - the host's curl of the DECLARED port gets 200 with the sandbox-written body,
  - the host's curl of an UNDECLARED port (18474) gets nothing (000): only the
    declared port is bridged,
  - the launch banner names the forwarded URL and the injected contract carries
    the PORTS section, so both the human and the agent were told,
  - the agent sees the port in REIN_IN_SANDBOX_EXPOSE_PORTS.

Shape: one `rein run -- bash -c` step. The host side runs in a thread while the
step is live; it writes its verdict into the shared working tree, and the
in-sandbox script emits that verdict as a tagged line so the golden carries what
the host observed. Line-oriented (no tmux/pyte).
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"
PORT = 18473            # fixed so the golden is stable; fails closed if busy
UNDECLARED = 18474      # never in expose_ports
VERDICT_FILE = ".rein-journey-host-verdict"
BODY = "hello from the sandbox"


def script() -> str:
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@EXPOSE  (an in-sandbox http.server on 127.0.0.1:{PORT}; the HOST curls it through the tunnel)"
emit "@ENV REIN_IN_SANDBOX_EXPOSE_PORTS=$REIN_IN_SANDBOX_EXPOSE_PORTS"
www="$TMPDIR/www"; mkdir -p "$www"; printf '%s' '{BODY}' > "$www/index.html"
python3 -m http.server {PORT} --bind 127.0.0.1 --directory "$www" >/dev/null 2>&1 &
emit "@SERVER_STARTED  (waiting for the host's verdict in the working tree)"
for i in $(seq 1 90); do [ -f {VERDICT_FILE} ] && break; sleep 1; done
emit "@HOST_VERDICT=$(cat {VERDICT_FILE} 2>/dev/null || echo none)"
rm -f {VERDICT_FILE}
"""


def _get(port: int) -> tuple[str, str]:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=5) as r:
            return str(r.status), r.read().decode(errors="replace")
    except urllib.error.HTTPError as e:
        return str(e.code), ""
    except Exception:
        return "000", ""


def host_side(workdir: str, out: dict) -> None:
    """Poll the declared port until it answers (the sandbox takes a while to
    launch), probe the undeclared one, then hand the verdict to the sandbox."""
    code, body = "000", ""
    for _ in range(120):
        code, body = _get(PORT)
        if code == "200":
            break
        time.sleep(1)
    ucode, _ = _get(UNDECLARED)
    out.update(code=code, body=body, ucode=ucode)
    verdict = f"declared={code}:{body} undeclared={ucode}"
    with open(os.path.join(workdir, VERDICT_FILE), "w") as f:
        f.write(verdict)


def _pinned_session(repo: str) -> str:
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write(f"id: sess_journey_expose_port\nrole: implement\nrepos:\n  - {repo}\nexpose_ports:\n  - {PORT}\n")
    return path


def clone_checkout(repo: str, env: dict) -> str:
    d = tempfile.mkdtemp(prefix="rein-expose-")
    subprocess.run(["gh", "repo", "clone", repo, d, "--", "-q"],
                   check=True, env=env, capture_output=True, text=True)
    return d


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)
    print(f"journey: expose_ports reverse tunnel (host -> sandbox 127.0.0.1:{PORT}) on {repo}", flush=True)

    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        seen: dict = {}
        t = threading.Thread(target=host_side, args=(workdir, seen), daemon=True)
        t.start()
        steps = [
            H.JourneyStep(
                argv=["run", "--", "bash", "-c", script(), workdir],
                label=f"rein run -- bash -c <expose-port probe> {workdir}",
                extra_env={"REIN_SESSION_FILE": _pinned_session(repo), "REIN_SANDBOX_WORKDIR": workdir},
                timeout=240,
            ),
        ]
        result = H.run_journey(steps, env=env)
        t.join(timeout=5)
        raw = result.transcript

        m = re.search(rf"{re.escape(H.SBX_TAG)}@HOST_VERDICT=(.*)", raw)
        verdict = m.group(1).strip() if m else None
        checks = {
            f"host curl of the declared port {PORT} got 200 + the sandbox body": seen.get("code") == "200" and seen.get("body") == BODY,
            f"host curl of the undeclared port {UNDECLARED} got nothing (000)": seen.get("ucode") == "000",
            "the sandbox echoed the host's verdict into the transcript": verdict == f"declared=200:{BODY} undeclared=000",
            "the launch banner names the forwarded URL": f"http://localhost:{PORT}" in raw and "exposed ports" in raw,
            "the injected contract carries the PORTS section": "PORTS" in raw and f"{PORT} -> http://localhost:{PORT}" in raw,
            "the agent sees the port in REIN_IN_SANDBOX_EXPOSE_PORTS": f"@ENV REIN_IN_SANDBOX_EXPOSE_PORTS={PORT}" in raw,
        }
        broken = [name for name, ok in checks.items() if not ok]
        if broken:
            print("CLAIM BROKE:", flush=True)
            for b in broken:
                print(f"  - {b}", flush=True)
            print(f"host saw: {seen}", flush=True)
            print("--- transcript ---\n" + raw, flush=True)
            return 1

        print("--- outcomes (asserted; not all in the golden) ---", flush=True)
        print(f"  host GET http://127.0.0.1:{PORT}/ -> 200 {BODY!r} (served from inside the sandbox)", flush=True)
        print(f"  host GET http://127.0.0.1:{UNDECLARED}/ -> 000 (undeclared port: not bridged)", flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0
        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "expose_port.fresh.txt")
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
