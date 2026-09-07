"""approval_ttl_reattest — the approval-AGE bound re-attests writes IN PLACE,
independent of activity (#190).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md. This is the sibling of `idle_reattest`: that
journey proves the bound that fires on SILENCE; this one proves the bound
that fires on AGE ALONE — a busy run, with GitHub traffic every 5 seconds
throughout, still has its write approval withdrawn once the confirmation
itself is old enough. `REIN_IDLE_TIMEOUT` is left at its (30m) default so it
can never be what trips.

Shape: ONE sandboxed `rein run -- bash -c <script>` step, driven through
`H.run_journey`, with TWO ordered Form-A answers (the script declares twice).
Same write_ceremony phase-sentinel idiom as idle_reattest.
"""

from __future__ import annotations

import os
import re
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"

# The exact approval-age-lock banner FIRST line (cmd/rein/reattest.go
# printReattestBanner, the ExpireApprovalAge branch — distinct wording from
# idle_reattest's "idle for <d>" line). REIN_APPROVAL_TTL is pinned to "15s"
# below, so the printed duration is the CONFIGURED bound, not elapsed time,
# and needs no _NORMALIZE_RULES entry.
APPROVAL_TTL = "15s"
BANNER_LINE = f"rein: write approval is {APPROVAL_TTL} old — writes LOCKED."

# The exact pkt-line ERR a locked push surfaces (internal/proxy/gate.go
# undeclaredPushMsg), as git echoes it after "fatal: remote error: ". Same
# refusal as idle_reattest — the age bound and the idle bound withdraw the
# SAME approval the SAME way; only the banner and the audit tag differ.
LOCKED_PUSH_ERR = (
    "fatal: remote error: rein: writes are locked until you declare your "
    "issue. Run: rein declare <n> (then push to agent/<n>/<nonce>)"
)

# The proxy-socket line in rein's own launch banner names the run id
# (cmd/rein/run_sandboxed.go printSandboxBanner: ".../rein/run-<id>/proxy.sock").
# Extracting it lets the journey open THIS run's own audit log deterministically
# rather than guessing at the newest file under state-dir/audit/.
_RUN_ID_RE = re.compile(r"proxy socket \(out of sandbox\): .*/rein/run-([A-Za-z0-9_-]+)/proxy\.sock")


# --------------------------------------------------------------------------
# The in-sandbox agent script — every step emits a tagged sentinel
# --------------------------------------------------------------------------


def approval_ttl_script(repo: str, issue: int, good1: str, good2: str) -> str:
    """A `bash -c` body run as the srt child: declare -> push (lands) -> a
    BUSY window that keeps GitHub traffic flowing every ~5s for ~25s (proving
    activity does NOT defer the approval-age bound, unlike idle) -> push
    WITHOUT re-declaring (must be refused, the confirmation aged out) ->
    declare again (a SECOND Form A) -> push again (lands).
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"
rm -rf repo
run git clone --progress https://github.com/{repo} repo
cd repo || {{ emit "@CLONE_FAIL"; exit 3; }}
emit "@CLONE_OK"

emit "@PHASE1_START  rein declare {issue} (first Form A; blocks for the human)"
run rein declare {issue}
emit "@PHASE1_RC=$?"

emit "@PHASE2_START  push agent/{issue}/<nonce1> (expect: lands)"
echo "approval-ttl-reattest: write #1, before the busy window" >> probe-1.txt
run git add -A
run git commit -q -m "approval-ttl-reattest: write #1, before the busy window"
run git push --progress origin HEAD:refs/heads/{good1}
emit "@PHASE2_RC=$?"

emit "@PHASE3_BUSY_START  busy window (~25s, GitHub traffic every ~5s) -- the approval AGE bound must still expire despite continuous activity"
run git ls-remote origin
run sleep 5
run git ls-remote origin
run sleep 5
run git ls-remote origin
run sleep 5
run git ls-remote origin
run sleep 5
run git ls-remote origin
emit "@PHASE3_BUSY_END"

emit "@PHASE4_START  push agent/{issue}/<nonce2> WITHOUT re-declaring (expect: locked -- the confirmation aged out)"
echo "approval-ttl-reattest: write #2, after the busy window (should be refused)" >> probe-2.txt
run git add -A
run git commit -q -m "approval-ttl-reattest: write #2, after the busy window"
run git push --progress origin HEAD:refs/heads/{good2}
emit "@PHASE4_RC=$?"

emit "@PHASE5_START  rein declare {issue} again (second Form A; re-confirm required)"
run rein declare {issue}
emit "@PHASE5_RC=$?"

emit "@PHASE6_START  push agent/{issue}/<nonce2> again (expect: lands)"
run git push --progress origin HEAD:refs/heads/{good2}
emit "@PHASE6_RC=$?"
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# Host-side helpers
# --------------------------------------------------------------------------


def _rc(text: str, name: str) -> int | None:
    m = re.search(rf"@{name}_RC=(\d+)", text)
    return int(m.group(1)) if m else None


def _pinned_session(repo: str) -> str:
    """A temp repo-only session, so the journey never depends on the machine's
    ambient dev-session.yaml and never writes an `issue:` (#35 retired it)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_approval_ttl_reattest\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def _run_audit_log(env: dict, text: str) -> str:
    """This run's own audit log (stateDir/audit/sandbox-<runID>.log), located by
    the run id rein's own launch banner names — not by newest-mtime, since
    read-only reviewers may run `go test` (and its own short-lived sandboxes)
    concurrently in this worktree."""
    m = _RUN_ID_RE.search(text)
    if not m:
        return ""
    p = H.state_dir(env) / "audit" / f"sandbox-{m.group(1)}.log"
    return p.read_text(errors="replace") if p.exists() else ""


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def drive_journey(env, repo, issue, workdir, good1, good2):
    """Drive the ONE sandboxed `rein run` through the shared runner (#82).
    Both Form-A prompts (the initial declare and the post-lock re-declare)
    fire inside this SAME step; `answers` matches them in order.
    REIN_IDLE_TIMEOUT is left UNSET (default 30m) so only the approval-age
    bound can trip inside this run's ~1 minute lifetime."""
    step = H.JourneyStep(
        argv=["run", "--", "bash", "-c", approval_ttl_script(repo, issue, good1, good2), workdir],
        # rein re-echoes the full script right below its banner, so keep the
        # boundary line concise instead of dumping the whole bash body twice.
        label=f"rein run -- bash -c <approval-ttl re-attestation script> {workdir}",
        answers=[(H.PROMPT_HINT, str(issue)), (H.PROMPT_HINT, str(issue))],
        extra_env={
            "REIN_SESSION_FILE": _pinned_session(repo),
            "REIN_SANDBOX_WORKDIR": workdir,
            "REIN_APPROVAL_TTL": APPROVAL_TTL,
        },
        # Covers the slow srt launch + clone + the ~25s busy window + a second
        # declare/push round trip, all inside one step's expects.
        timeout=300,
    )
    result = H.run_journey([step], env=env)
    return result, result.steps[0].text


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)

    title = "rein journey: approval-age re-attestation walkthrough (safe to close)"
    issue = H.create_issue(
        repo, title,
        "Opened by journeys/approval_ttl_reattest/journey.py to demonstrate #190: "
        "the approval-AGE bound withdraws the write approval IN PLACE even on a "
        "busy run, since activity does not extend it. Throwaway repo only; closed "
        "again when the journey ends.",
        env,
    )
    print(f"journey: approval-age re-attestation on {repo}, issue #{issue} (created)", flush=True)

    nonce1 = H.unique_branch("ttl1")
    nonce2 = H.unique_branch("ttl2")
    good1 = f"agent/{issue}/{nonce1}"
    good2 = f"agent/{issue}/{nonce2}"
    branches = [good1, good2]

    workdir = H.make_workdir()
    try:
        result, text = drive_journey(env, repo, issue, workdir, good1, good2)

        # ---- 1) The ceremony itself must hold — independent of the golden. ----
        rc1 = _rc(text, "PHASE1")
        rc2 = _rc(text, "PHASE2")
        rc4 = _rc(text, "PHASE4")
        rc5 = _rc(text, "PHASE5")
        rc6 = _rc(text, "PHASE6")

        def idx(needle: str) -> int:
            return text.find(needle)

        # rein echoes the WHOLE script source verbatim under "rein: running:"
        # before executing it, so every @PHASE.. sentinel LITERAL appears
        # TWICE — once in that echo, once for real. rfind gets the real one
        # (see idle_reattest/README.md for the full story of this gotcha).
        # The _RC=$? markers are immune: the echo has a literal "$?", not
        # digits, so _rc()'s regex only matches the evaluated, real line.
        busy_start, busy_end = text.rfind("@PHASE3_BUSY_START"), text.rfind("@PHASE3_BUSY_END")
        phase4_start, phase5_start = text.rfind("@PHASE4_START"), text.rfind("@PHASE5_START")
        banner_at = idx(BANNER_LINE)
        # "rein: revoked <N> of <N> write token(s) on idle re-attestation" —
        # cmd/rein/run.go's drainRunWriteTokens always logs this exact phase
        # text, EVEN for an approval-age trip (the drain helper is shared and
        # was never made reason-aware). Documented as a wording quirk in this
        # journey's README rather than "fixed" here (no Go changes).
        m_revoke = re.search(r"rein: revoked \d+ of \d+ write token\(s\) on idle re-attestation", text)
        revoke_at = m_revoke.start() if m_revoke else -1
        locked_err_at = idx(LOCKED_PUSH_ERR)

        landed = {br: H.branch_exists(repo, br, env) for br in branches}
        audit_text = _run_audit_log(env, text)
        confirmed_positions = [m.start() for m in re.finditer(r"decision=confirmed-issue", audit_text)]
        # expired-approval-withdrawn (internal/runbroker/expiry.go
        # expiryDecision, reason=ExpireApprovalAge): the age trip actually
        # found and withdrew a write approval.
        expired_at = audit_text.find("decision=expired-approval-withdrawn")

        invariants = [
            (rc1 == 0, "phase 1 (first declare) must succeed after confirmation"),
            (rc2 == 0, "phase 2 (push before the busy window) must succeed"),
            (busy_start != -1 and busy_end != -1 and busy_start < busy_end,
             "the busy-window sentinels must both appear, in order"),
            (banner_at != -1, f"the approval-age-lock banner ({BANNER_LINE!r}) must be printed"),
            (busy_start < banner_at < busy_end,
             "the approval-age-lock banner must print DURING the busy window (phases 3->4), "
             "proving it fired on AGE ALONE despite continuous GitHub traffic"),
            (revoke_at != -1, "the write-token revoke line must be printed"),
            (busy_start < revoke_at < busy_end,
             "the write-token revoke must happen DURING the busy window (proof it "
             "fired at the approval's EXPIRY, not just at the run's real exit)"),
            (rc4 not in (None, 0), "phase 4 (push without re-declaring) must be REFUSED — the confirmation aged out"),
            (locked_err_at != -1 and phase4_start < locked_err_at < phase5_start,
             f"phase 4's refusal must carry the locked-writes ERR ({LOCKED_PUSH_ERR!r})"),
            (rc5 == 0, "phase 5 (re-declare) must succeed after the second confirmation"),
            (rc6 == 0, "phase 6 (push after re-declaring) must succeed"),
            (result.transcript.count(H.PROMPT_BANNER) == 2, "exactly TWO Form A prompts must have fired"),
            (landed.get(good1) is True, f"the pre-lock branch must LAND ({good1})"),
            (landed.get(good2) is True, f"the post-re-declare branch must LAND ({good2})"),
            (bool(audit_text), "this run's own audit log (state-dir/audit/sandbox-<runID>.log) must be found and non-empty"),
            (expired_at != -1, "the audit log must record the expiry decision tag (decision=expired-approval-withdrawn)"),
            ("decision=expired-idle" not in audit_text,
             "the IDLE bound must NEVER fire here — the run was busy throughout, "
             "so only the approval-AGE bound may trip"),
            (len(confirmed_positions) >= 2, "the audit log must record TWO confirmed-issue decisions (initial + re-declare)"),
            (bool(confirmed_positions) and confirmed_positions[0] < expired_at < confirmed_positions[-1],
             "the audit log's expiry tag must land BETWEEN the first and the (later) second confirmed-issue"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if not result.reached_eof:
            broken.append("the sandbox step did not run to EOF (timed out / prompt missed)")
        if broken:
            print("APPROVAL-TTL RE-ATTESTATION BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  rcs: phase1={rc1} phase2={rc2} phase4={rc4} phase5={rc5} phase6={rc6}", flush=True)
            print(f"  landed={landed}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            if audit_text:
                print("--- this run's audit log ---", flush=True)
                print(audit_text, flush=True)
            return 2

        # ---- 2) Compare the WHOLE captured session NORMALIZED. ----
        raw = result.transcript
        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        for ph, meaning in (
            (1, "human confirmed (1st declare)"), (2, "verified push"),
            (4, "writes locked (approval-age re-attestation, despite activity)"),
            (5, "human re-confirmed (2nd declare)"), (6, "verified push after re-declare"),
        ):
            print(f"  phase {ph}  rc={_rc(text, f'PHASE{ph}')}  ({meaning})", flush=True)
        print(f"  Form A prompts fired: {result.transcript.count(H.PROMPT_BANNER)}", flush=True)
        for br, ok in landed.items():
            print(f"  branch {br}: {'LANDED' if ok else 'ABSENT'}", flush=True)
        print(f"  audit log: expired-approval at offset {expired_at}, "
              f"confirmed-issue at {confirmed_positions}", flush=True)

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
        scratch = os.path.join(tempfile.gettempdir(), "approval_ttl_reattest.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        for br in branches:
            H.delete_branch(repo, br, env)
        H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print("cleanup: branches deleted; issue closed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
