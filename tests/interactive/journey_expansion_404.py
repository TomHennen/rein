"""journey_expansion_404 — THE 404-AT-EXPANSION INSTALL NOTICE (issue #69, mocks §1.4/§5.2).

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

WHERE journey_scope_expansion shows the HAPPY expansion (declare a repo outside scope
-> APPROVE -> push to it), THIS journey shows its sibling failure mode: the agent
declares an expansion to a repo the GitHub App is NOT installed on. There is nothing
to approve — rein cannot mint any credential for a repo outside its installation — so
NO approval prompt fires. Instead the human gets an interactive NOTICE carrying the
install deep-link and NO approval authority, and the declare REFUSES (the agent is
told to install-then-retry). Keeping this out of scope_expansion's golden preserves
that journey's "one coherent approve->push story"; the two failure/​success legs read
better as separate goldens.

THE GAP THIS PROVES, in one terminal:

  * the AGENT's view (in-sandbox, SBX|-tagged): `rein declare <n> --repo <uninstalled>`
    BLOCKS, then comes back REFUSED with the agent-facing 404 message ("cannot request
    … the GitHub App … is not installed on it. The human must install it first: <link>.
    Then run this command again."), exit != 0; and
  * the HUMAN's view (host tty): the `=== rein: NOTICE — App not installed (nothing to
    approve) ===` block — it names the repo, says "there is no approval to give",
    carries the install deep-link, and asks the human to ENTER-when-installed or 's' to
    skip. We answer 's' (the deterministic path: nothing is installed mid-journey).

SAME-OWNER IS CHECKED FIRST (mocks §1.2): the cross-owner reject is structural and
fires BEFORE the install-coverage probe. So to reach the 404/​NOTICE path the target
must share the session owner. This journey uses `<owner>/definitely-not-installed` —
the SAME owner as the throwaway, a FIXED fictional leaf (stable-by-construction, so
NOT normalized; #78). GitHub's `GET /repos/{owner}/{repo}/installation` 404s
identically for "repo does not exist" and "App not installed", so this touches NO real
repo — it names one that does not exist. Hard-constraint #1 holds.

CAPTURE IS STRUCTURAL: uses reinharness.run_journey — the sandbox launch is one
JourneyStep whose only prompt is the NOTICE acknowledgement (answered 's'); the runner
captures the COMPLETE session (banner, injected contract, the tagged agent line, and
the host-tty NOTICE that interleaves with it) as `.transcript`, with no hand-slicing.

DETERMINISM: the declared issue number normalizes to #<ISSUE>; the session id is a
fixed literal; the working tree lives under /tmp/rein-… (normalized). The uncovered
repo name and the App slug/​deep-link are stable-by-construction on a machine, kept RAW
and matched verbatim. REIN_APPROVAL=tty forces the inline /dev/tty NOTICE (pexpect IS
the human), never the tmux popup.

    python3 tests/interactive/journey_expansion_404.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_expansion_404.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_expansion_404.py # also print the compare lens

Exit 0 = the NOTICE fired + the declare refused AND the normalized transcript matches
the golden. Exit 1 = drift. Exit 2 = the notice/refusal path itself broke.

SELF-CONTAINED: a throwaway working tree (removed in a `finally`) + a throwaway session
file; the uncovered repo is fictional; no real repo is touched. Repo A is resolved the
rein-init way (reinharness.resolve_throwaway_repo).
"""

from __future__ import annotations

import os
import re
import shutil
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "expansion_404.txt"

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
            p = H.update_golden(GOLDEN_NAME, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, raw)
        if ok:
            print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "expansion_404.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
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
