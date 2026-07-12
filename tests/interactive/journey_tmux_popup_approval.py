"""journey_tmux_popup_approval — THE DEFAULT approval surface inside $TMUX (#37).

This is ONE journey. For what a journey IS, the golden-transcript rule, and how
to author the next one, read tests/interactive/CLAUDE.md — none of that lives here.

THE COVERAGE GAP (#37): rein's write-approval prompt does NOT default to the
inline /dev/tty prompt when $TMUX is set. It defaults to a **tmux popup**
(internal/ui/grant: PopupPreferenceFromEnv is true inside $TMUX, so attemptPopup
runs `tmux popup -E "rein approval grant --run-id <id>"`, whose pty is its own
/dev/tty where Form A renders). Every OTHER journey/test runs OUTSIDE tmux (or
forces REIN_APPROVAL=tty), so the DEFAULT surface a developer-in-tmux actually
hits was untested end to end. This journey drives the REAL popup.

WHAT IT PROVES, on the same #35 loop as the write ceremony:
  * with $TMUX set and REIN_APPROVAL unset, the declare routes to the POPUP, not
    the inline tty — the Form A prompt is ABSENT from rein's own terminal
    (rein.prompt_count()==0) and rein's forensic log records
    `grant: launching tmux popup (... approval grant --run-id ...)` then
    `grant: issue #<n> CONFIRMED via tmux popup` (a POSITIVE proof of the surface);
  * the human approves IN the popup (the harness types the issue number on the
    attached tmux client the popup grabs the keyboard of);
  * the write proceeds: the pre-declare push is LOCKED, the verified push to
    agent/<n>/<nonce> LANDS, and a non-convention ref is REJECTED.

HOW THE POPUP IS DRIVEN (reinharness.tmux_popup_session): a DEDICATED tmux server
(`tmux -L <unique>`, never the operator's own), an ATTACHED pexpect client the
popup renders on (a popup is a client-owned overlay — not an addressable pane, so
`send-keys` can't reach it; only keys written to the attached client's pty do),
and rein on a SEPARATE plain pty whose $TMUX/$TMUX_PANE point at that session so
rein's OWN output stays clean and deterministic. See the module for the full
rationale.

DELIVERABLE: a RAW, human-reviewable transcript at golden/tmux_popup_approval.txt
— rein's OWN terminal capture. The Form A block is deliberately NOT in it (it
rendered in the popup, on the client); its ABSENCE where the write ceremony's
golden shows it inline IS the reviewable signal that approval went to the popup.
The popup-routing facts and the popup's Form A content are asserted / printed as
outcomes (not golden material — the popup render is finicky box-art).

    python3 tests/interactive/journey_tmux_popup_approval.py            # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_tmux_popup_approval.py  # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_tmux_popup_approval.py

Exit 0 = ceremony held AND the normalized transcript matches the golden (or tmux
is absent, in which case it SKIPs cleanly — the popup surface is undriveable
without tmux). Exit 1 = golden drift. Exit 2 = the ceremony itself broke.

SELF-CONTAINED: creates its own throwaway issue via gh, and in a `finally` deletes
both branches and closes the issue. Touches only the throwaway (hard-constraint
#1). The repo is resolved the rein-init way (reinharness.resolve_throwaway_repo).
"""

from __future__ import annotations

import os
import sys
import tempfile

import pexpect

import reinharness as H

GOLDEN_NAME = "tmux_popup_approval.txt"
ISSUE_ENV = "REIN_DEMO_ISSUE"


# --------------------------------------------------------------------------
# The in-sandbox agent script — identical shape to the write ceremony, so the
# ONLY golden difference is WHERE the approval prompt went (popup, not inline).
# --------------------------------------------------------------------------


def ceremony_script(repo: str, issue: int, good: str, bad: str) -> str:
    """A `bash -c` body run as the srt child. Each STEP emits a tagged `@PHASE..`
    sentinel; commands go through `run` (reinharness.sandbox_preamble), which
    echoes each as `SBX| $ <command>` and tags its output. The clone/pushes pass
    `--progress` on purpose (see journey_write_ceremony for the full why: git's
    stderr is piped so `--progress` forces the transfer chatter the golden keeps
    with counts normalized)."""
    return f"""
{H.sandbox_preamble()}
cd "$0"
rm -rf repo
run git clone --progress https://github.com/{repo} repo
cd repo || {{ emit "@CLONE_FAIL"; exit 3; }}
emit "@CLONE_OK  (reads flow with no declaration at all)"

emit "@PHASE1_START  push BEFORE declare (expect: locked, no prompt)"
echo "phase 1" >> probe-1.txt
run git add -A
run git commit -q -m "popup ceremony: pre-declaration write attempt"
run git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE1_RC=$?"

emit "@PHASE2_START  rein declare {issue} (blocks; approval fires in a tmux POPUP)"
run rein declare {issue}
emit "@PHASE2_RC=$?"

emit "@PHASE3_START  push agent/{issue}/<nonce> (expect: lands)"
run git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE3_RC=$?"

emit "@PHASE4_START  push a non-convention ref (expect: rejected)"
run git push --progress origin HEAD:refs/heads/{bad}
emit "@PHASE4_RC=$?"
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def _rc(child_match) -> int:
    return int(child_match.group(1))


def run_ceremony(env, repo, issue):
    """Drive the live run through the REAL tmux popup; return a result dict."""
    good = f"agent/{issue}/{H.unique_branch('popup')}"
    bad = H.unique_branch("popup-nonconvention")
    branches = [good, bad]

    wd = H.make_workdir()
    script = ceremony_script(repo, issue, good, bad)
    session = _pinned_session(repo)
    log_path = H.helper_log_path(env)
    log_off = log_path.stat().st_size if log_path.exists() else 0

    rcs: dict[int, int] = {}
    popup_render = ""
    with H.tmux_popup_session() as tmux_sess:
        # $TMUX/$TMUX_PANE route THIS rein's approval to the session's popup; the
        # session file pins scope so the journey never depends on the ambient one.
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=env,
            extra_env={"REIN_SESSION_FILE": session, **tmux_sess.tmux_env()},
        )
        try:
            # expect -> act -> expect, one step at a time. A pexpect EOF/TIMEOUT
            # here means a live step didn't happen (most often a transient clone
            # rate-limit); we catch it and return PARTIAL rcs so main() reports a
            # clean "ceremony broke" (exit 2) with the transcript.
            run.child.expect(r"@CLONE_OK", timeout=180)

            run.child.expect(r"@PHASE1_START", timeout=60)
            run.child.expect(r"@PHASE1_RC=(\d+)", timeout=120)
            rcs[1] = _rc(run.child.match)

            run.child.expect(r"@PHASE2_START", timeout=30)
            # The declare BLOCKS. Because $TMUX is set, approval routes to a tmux
            # POPUP (NOT the inline host tty). Answer it on the attached client the
            # popup renders on and grabs the keyboard of — type the DISPLAYED
            # number, exactly as a human would in the popup.
            tmux_sess.drive_popup(H.PROMPT_HINT, str(issue), timeout=120)
            run.child.expect(r"@PHASE2_RC=(\d+)", timeout=90)
            rcs[2] = _rc(run.child.match)

            run.child.expect(r"@PHASE3_START", timeout=30)
            run.child.expect(r"@PHASE3_RC=(\d+)", timeout=120)
            rcs[3] = _rc(run.child.match)

            run.child.expect(r"@PHASE4_START", timeout=30)
            run.child.expect(r"@PHASE4_RC=(\d+)", timeout=120)
            rcs[4] = _rc(run.child.match)

            run.child.expect(r"@SCRIPT_DONE", timeout=60)
            run.wait(timeout=120)
        except (pexpect.EOF, pexpect.TIMEOUT):
            pass
        finally:
            popup_render = tmux_sess.render()
            try:
                run.child.close(force=True)
            except Exception:
                pass

    new_log = H.read_log_since(log_path, log_off)
    prompts = run.prompt_count()  # inline Form A prompts on rein's OWN terminal
    landed = {br: H.branch_exists(repo, br, env) for br in branches}
    return {
        "text": run.text(),
        "rcs": rcs,
        "prompts": prompts,
        "landed": landed,
        "branches": branches,
        "good": good,
        "bad": bad,
        "log": new_log,
        "popup_render": popup_render,
    }


def main() -> int:
    env = H.rein_env()

    # tmux is a hard prerequisite for the popup surface — without it there is
    # nothing to drive. SKIP cleanly (exit 0) rather than fake success.
    if not H.tmux_available():
        print("SKIP: tmux is not on PATH — the popup approval surface cannot be "
              "driven, so this journey has nothing to exercise. Install tmux to "
              "run it. (Exit 0: a missing optional prerequisite is not a failure.)",
              flush=True)
        return 0

    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    supplied = os.getenv(ISSUE_ENV)
    ours = not supplied
    if supplied:
        issue = int(supplied)
        title = H.issue_title(repo, issue, env)
    else:
        title = "rein journey: tmux-popup approval walkthrough (safe to close)"
        issue = H.create_issue(
            repo, title,
            "Opened by tests/interactive/journey_tmux_popup_approval.py to "
            "demonstrate the #35 declare -> confirm -> verified-push ceremony with "
            "approval routed to a tmux POPUP (the default surface inside $TMUX, "
            "issue #37). Throwaway repo only; closed again when the journey ends.",
            env,
        )

    print(f"journey: tmux-popup approval on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'})", flush=True)

    branches: list[str] = []
    try:
        r = run_ceremony(env, repo, issue)
        rcs, prompts, landed = r["rcs"], r["prompts"], r["landed"]
        branches = r["branches"]
        good, bad = r["good"], r["bad"]
        log, popup_render = r["log"], r["popup_render"]

        # 1) The ceremony + the POPUP ROUTING must hold — independent of the golden.
        host_has_forma = (H.PROMPT_BANNER in r["text"]) or (H.PROMPT_HINT in r["text"])
        popup_launched = "launching tmux popup" in log
        popup_confirmed = "CONFIRMED via tmux popup" in log
        popup_showed_forma = H.PROMPT_HINT in popup_render
        invariants = [
            (rcs.get(1, 0) != 0, "phase 1 (pre-declaration push) must FAIL — writes locked"),
            (rcs.get(2) == 0, "phase 2 (declare) must succeed after popup confirmation"),
            (rcs.get(3) == 0, "phase 3 (verified push) must succeed"),
            (rcs.get(4, 0) != 0, "phase 4 (non-convention ref) must be REJECTED"),
            (prompts == 0, "NO inline Form A prompt on rein's own terminal (it routed to the popup)"),
            (not host_has_forma, "Form A must NOT appear on rein's own terminal (it rendered in the popup)"),
            (popup_launched, "rein's log must record launching the tmux popup (surface = popup)"),
            (popup_confirmed, "rein's log must record CONFIRMED via tmux popup"),
            (popup_showed_forma, "the popup client must have shown Form A ('type the issue number')"),
            (landed.get(good) is True, "the convention-following branch must LAND"),
            (landed.get(bad) is False, "the non-convention branch must NOT land"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("CEREMONY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  rcs={rcs} prompts={prompts} landed={landed}", flush=True)
            print(f"  popup_launched={popup_launched} popup_confirmed={popup_confirmed} "
                  f"popup_showed_forma={popup_showed_forma} host_has_forma={host_has_forma}",
                  flush=True)
            return 2

        # 2) Build the RAW transcript (real values) and compare NORMALIZED.
        raw = H.build_raw_transcript(r["text"])
        print()
        print(raw, flush=True)  # what actually happened on rein's terminal
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        for ph, meaning in ((1, "writes locked"), (2, "human confirmed in popup"),
                            (3, "verified push"), (4, "ref cross-check")):
            print(f"  phase {ph}  rc={rcs[ph]}  ({meaning})", flush=True)
        print(f"  inline Form A prompts on rein's terminal: {prompts} (routed to popup)", flush=True)
        for line in log.splitlines():
            if "tmux popup" in line:
                print(f"  log: {line.split('] ', 1)[-1]}", flush=True)
        for br, ok in landed.items():
            print(f"  branch {br}: {'LANDED' if ok else 'ABSENT'}", flush=True)
        # What the HUMAN saw in the popup (client render; finicky box-art, so NOT
        # golden material — printed here so a reviewer can see the Form A the
        # popup presented).
        print("  --- what the human saw in the popup (client render; not golden) ---", flush=True)
        forma = _extract_forma(popup_render)
        for line in forma:
            print(f"  | {line}", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN_NAME, raw)  # store RAW
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, raw)  # normalizes BOTH sides
        if ok:
            print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "tmux_popup_approval.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        for br in branches:
            H.delete_branch(repo, br, env)
        if ours:
            H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print("cleanup: branches deleted" + ("; issue closed" if ours else ""), flush=True)


def _extract_forma(render: str) -> list[str]:
    """Best-effort readable slice of the popup's Form A from the (box-art, cursor-
    collapsed) client render — for the human, never for the golden. Falls back to
    a compact single line if the render can't be split cleanly."""
    marker = "agent declares work on an issue"
    idx = render.find(marker)
    if idx == -1:
        return ["(popup render unavailable)"]
    tail = render[idx:]
    end = tail.find("press enter")
    snippet = tail[: end + len("press enter")] if end != -1 else tail[:400]
    # Collapse whitespace runs the cursor positioning introduced; keep it compact.
    compact = " ".join(snippet.split())
    return [compact[i:i + 100] for i in range(0, len(compact), 100)]


def _pinned_session(repo: str) -> str:
    """A temp repo-only session, so the journey never depends on the machine's
    ambient dev-session.yaml and never writes an `issue:` (#35 retired it)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_popup\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


if __name__ == "__main__":
    sys.exit(main())
