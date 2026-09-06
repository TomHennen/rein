"""public_read — an out-of-scope PUBLIC repo relays ANONYMOUSLY in-sandbox (#164).

The read-scope boundary changed: a read to a repo NOT in the session ceiling is
no longer refused — it is relayed to GitHub with NO token, so public repos serve
and private/nonexistent ones 404 (no anonymous access). This journey walks that
LIVE in the sandbox and its golden captures rein's surface + the SBX| probe.

What it proves, as INVARIANTS (a break is exit 1, independent of the golden):
  - an out-of-scope PUBLIC clone SUCCEEDS (CLONE_RC=0),
  - its REST read is 200 (served anonymously),
  - an out-of-scope PRIVATE/nonexistent read is 404 (no anonymous access — the
    scope ceiling still gates every credential),
  - the in-scope repo's authenticated read still works (200) — no regression.

Line-oriented (no tmux/pyte): one `rein run --sandbox -- bash -c` step through
run_journey, like sandbox_filesystem.
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

# A stable, well-known PUBLIC repo that is NOT the session's repo. Its tiny size
# keeps the clone chatter minimal (and normalized) in the golden.
PUBLIC_OOS = "octocat/Hello-World"
# An out-of-scope repo that anonymous access cannot see (nonexistent is
# indistinguishable from private to an anonymous client — both 404).
PRIVATE_OOS = "TomHennen/definitely-not-a-public-repo-x7"


def sandbox_script(in_scope: str) -> str:
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@OOS_PUBLIC_READ  (out-of-scope PUBLIC repo relays anonymously — no token)"
run git clone --quiet --depth 1 https://github.com/{PUBLIC_OOS} oos-pub
[ -d oos-pub/.git ] && emit "@CLONE_RC=0" || emit "@CLONE_RC=1"
emit "@PUB_API=$(curl -sS -o /dev/null -w %{{http_code}} --max-time 20 https://api.github.com/repos/{PUBLIC_OOS})"
emit "@OOS_PRIVATE_READ  (out-of-scope private/nonexistent — 404, no anonymous access)"
emit "@PRIV_API=$(curl -sS -o /dev/null -w %{{http_code}} --max-time 20 https://api.github.com/repos/{PRIVATE_OOS})"
emit "@IN_SCOPE_READ  (the session repo still reads authenticated — no regression)"
emit "@SCOPE_API=$(curl -sS -o /dev/null -w %{{http_code}} --max-time 20 https://api.github.com/repos/{in_scope})"
"""


def _pinned_session(repo: str) -> str:
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_public_read\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def clone_checkout(repo: str, env: dict) -> str:
    d = tempfile.mkdtemp(prefix="rein-pubread-")
    subprocess.run(["gh", "repo", "clone", repo, d, "--", "-q"],
                   check=True, env=env, capture_output=True, text=True)
    return d


def _val(text: str, name: str) -> str | None:
    # Anchor on the SBX| OUTPUT tag: rein re-echoes the script source (which
    # contains `@NAME=$(curl ...)`), so an unanchored match would read the echo,
    # not the value the probe emitted.
    m = re.search(rf"{re.escape(H.SBX_TAG)}@{name}=(\S+)", text)
    return m.group(1) if m else None


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)
    print(f"journey: out-of-scope public read (anonymous) on {repo}", flush=True)

    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        step = H.JourneyStep(
            argv=["run", "--", "bash", "-c", sandbox_script(repo), workdir],
            label=f"rein run -- bash -c <public-read probe> {workdir}",
            extra_env={"REIN_SESSION_FILE": _pinned_session(repo), "REIN_SANDBOX_WORKDIR": workdir},
            timeout=180,
        )
        result = H.run_journey([step], env=env)
        raw = result.transcript

        # ---- Invariants (independent of the golden). ----
        checks = {
            "out-of-scope public clone succeeds": _val(raw, "CLONE_RC") == "0",
            "out-of-scope public REST read is 200 (anonymous)": _val(raw, "PUB_API") == "200",
            "out-of-scope private/nonexistent read is 404": _val(raw, "PRIV_API") == "404",
            "in-scope repo still reads (authenticated) 200": _val(raw, "SCOPE_API") == "200",
        }
        broken = [name for name, ok in checks.items() if not ok]
        if broken:
            print("CLAIM BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print("--- transcript ---", flush=True)
            print(raw, flush=True)
            return 1

        print("--- outcomes (asserted; not all in the golden) ---", flush=True)
        print(f"  out-of-scope PUBLIC {PUBLIC_OOS}: clone OK, REST 200 — anonymous, no token", flush=True)
        print(f"  out-of-scope PRIVATE {PRIVATE_OOS}: REST 404 — no anonymous access", flush=True)
        print(f"  in-scope {repo}: REST 200 — still authenticated", flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0
        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "public_read.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if intended: REIN_UPDATE_GOLDEN=1 to adopt)", flush=True)
        return 1
    finally:
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        print("cleanup: checkout removed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
