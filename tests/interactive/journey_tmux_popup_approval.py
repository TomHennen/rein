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
  * with $TMUX set (BY TMUX — see below) and REIN_APPROVAL unset, the declare
    routes to the POPUP, not the inline tty — the Form A prompt is ABSENT from
    rein's own pane and rein's forensic log records
    `grant: launching tmux popup (... approval grant --run-id ...)` then
    `grant: issue #<n> CONFIRMED via tmux popup` (a POSITIVE proof of the surface);
  * the human approves IN the popup (the harness types the issue number on the
    attached tmux client the popup grabs the keyboard of);
  * the popup OVERLAYS A LIVE PANE and the pane survives it (see below);
  * the write proceeds: the pre-declare push is LOCKED, the verified push to
    agent/<n>/<nonce> LANDS, and a non-convention ref is REJECTED.

HOW THE POPUP IS DRIVEN — IN A REAL PANE (reinharness.tmux_pane_session): rein runs
INSIDE a real tmux pane, launched the way a developer launches it (TYPED INTO THE
PANE's shell), on a DEDICATED tmux server (`tmux -L <unique>`, never the operator's
own). So $TMUX/$TMUX_PANE are INHERITED from tmux — nothing is synthesized, and
rein's own output and the popup overlay SHARE ONE TERMINAL, exactly as they do on a
developer's box. (The earlier shape ran rein on a SEPARATE pty with a FAKED $TMUX
pointing at an EMPTY pane. It proved the popup surface, but it structurally could
NOT see a popup-over-live-content bug — the popup had nothing to overlay.)

THREE SURFACES, and the journey uses each for what only it can answer:
  * `raw_stream()`    — pipe-pane's byte stream: everything the pane's program wrote,
    append-only and complete. THIS is the transcript (a `capture-pane` shot shows
    only the visible screen — and under a TUI's alternate screen it has no
    scrollback at all, which is why the golden can never come from there).
  * `pane_text()`     — `capture-pane -p -J`: the RENDERED pane. A popup is NOT in it.
  * `client_screen()` — the attached client's pty, pyte-rendered: the ONLY surface a
    tmux popup exists on (it is a client-owned overlay; `send-keys` cannot reach it
    and `capture-pane` cannot see it — only keys written to the client's pty can).

WHAT THE REAL PANE NEWLY MAKES ASSERTABLE (the whole point of the flip):
  * the popup OVERLAYS A LIVE PANE — while Form A is up, it is on the client's
    render and ABSENT from `capture-pane` of the pane, which at that moment still
    shows the live `SBX| $ rein declare <n>` the popup is blocking on;
  * the pane is INTACT and REPAINTS once the popup closes — the run carries on to
    the verified push and @SCRIPT_DONE, and no Form A residue is left on the client.

DELIVERABLE: a RAW, human-reviewable transcript at golden/tmux_popup_approval.txt
— ONE transcript, TWO views (the write-ceremony model). The PANE shows the declare
going straight to `confirmed` with NO inline Form A (approval routed AWAY); folded
in right at the `$ rein declare <n>` line are the POPUP|-tagged Form A lines — the
exact prompt the human READ and answered in the tmux popup, on the client's own
surface. That adjacency (popup content beside rein's own Form-A-less declare) IS the
reviewable proof the surface was the popup, not the inline tty — the same "capture
is structural" doctrine as SBX| vs host lines (#82).

The popup REDRAWS (box-art borders, cursor-positioned paints), so its Form A is read
off a RENDERED SCREEN — the attached client's pty run through a real terminal
emulator (pyte; reinharness.RenderedScreen + popup_forma_from_screen), issue #100.
With a LIVE pane underneath, the client's RAW bytes interleave the pane's own writes
(a row the popup paints blank lets stale pane text bleed inside the box), so the
render — where the overlay is genuinely on top — is the only truthful surface. It is
DETERMINISTIC without any timer: rein writes Form A once and BLOCKS on input, so the
trailing `>` prompt line appearing on screen IS the proof the frame is complete
(popup_forma_complete) — only issue #/title/repo vary, handled by
normalize-on-compare. The positive routing facts (helper.log launched/CONFIRMED, no
Form A in the pane's stream) are ALSO asserted as outcomes.

    python3 tests/interactive/journey_tmux_popup_approval.py            # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_tmux_popup_approval.py  # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_tmux_popup_approval.py
    REIN_SHOW_PANE=1 python3 tests/interactive/journey_tmux_popup_approval.py      # the overlay contrast

Exit 0 = ceremony held AND the normalized transcript matches the golden. Exit 1 =
golden drift. Exit 2 = the ceremony itself broke. Exit 3 = SKIPPED (tmux or pyte
absent — the popup surface is undriveable, and a skip must never look like a pass).

SELF-CONTAINED: creates its own throwaway issue via gh, and in a `finally` deletes
both branches and closes the issue. Touches only the throwaway (hard-constraint
#1). The repo is resolved the rein-init way (reinharness.resolve_throwaway_repo).
"""

from __future__ import annotations

import os
import re
import shlex
import sys
import tempfile

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


def launcher_script(env_overrides: dict, script: str, wd: str) -> str:
    """The one command a developer types in their pane, as a tiny shell file.

    `rein run` is launched from the PANE's OWN shell, so the rein process inherits
    $TMUX/$TMUX_PANE from tmux itself — the whole point of the real-pane flip. A
    file (rather than a giant typed command line) keeps `send-keys` from having to
    type the multi-line ceremony script into the pane; nothing is hidden by it,
    because rein still ECHOES the full script under its own banner, into the very
    stream the golden is built from.
    """
    lines = [f"cd {shlex.quote(str(H.REPO_ROOT))}"]
    lines += [f"export {k}={shlex.quote(v)}" for k, v in env_overrides.items()]
    lines.append(
        "exec "
        + " ".join(shlex.quote(a) for a in
                   [str(H.REIN_BIN), "run", "--", "bash", "-c", script, wd])
    )
    return "\n".join(lines) + "\n"


def run_ceremony(env, repo, issue):
    """Drive the live run through the REAL tmux popup, with rein running INSIDE a
    real tmux pane; return a result dict."""
    good = f"agent/{issue}/{H.unique_branch('popup')}"
    bad = H.unique_branch("popup-nonconvention")
    branches = [good, bad]

    wd = H.make_workdir()
    script = ceremony_script(repo, issue, good, bad)
    session = _pinned_session(repo)
    log_path = H.helper_log_path(env)
    log_off = log_path.stat().st_size if log_path.exists() else 0

    launch_dir = tempfile.mkdtemp(prefix="rein-journey-launch-")
    # Named for the READER of the golden: its first line is the command typed into
    # the pane, so it should say what is being launched (`rein-run.sh`), not
    # `launch.sh`. The full `rein run -- …` invocation appears right below it, in
    # rein's own banner echo.
    launch_path = os.path.join(launch_dir, "rein-run.sh")
    with open(launch_path, "w") as f:
        f.write(launcher_script(
            {"REIN_SESSION_FILE": session, "REIN_SANDBOX_WORKDIR": wd}, script, wd))

    broke = None
    forma: list[str] = []
    pane_while_popup = ""
    pane_after = ""
    client_after = ""
    # The tmux SERVER is started with rein_env(), so the pane's shell — and the rein
    # it launches — inherit REIN_APP_* (to mint) and HOME/XDG_STATE_HOME (so the
    # helper.log this journey reads back is the one that run writes).
    with H.tmux_pane_session(env=env) as pane:
        try:
            # A developer types the command in their pane. $TMUX comes FROM TMUX.
            pane.run_in_pane(f"bash {launch_path}")

            # expect -> act -> expect. The waits watch the pipe-pane RAW stream (the
            # complete, append-only surface — a marker that scrolled off the screen
            # still counts), and every poll iteration DRAINS the attached client,
            # which is what keeps the popup renderable at all (reinharness rule 1).
            # The `SBX| ` prefix is deliberate: rein ECHOES the script body under its
            # banner, so an untagged `@CLONE_OK` would match that echo instead of the
            # agent's own line.
            # Each wait is CHECKED: if the sandbox never reached a marker (a transient
            # clone failure, a run that never started), say THAT — a silent
            # fallthrough would resurface 180s later as "the popup never rendered",
            # blaming the wrong thing.
            for marker, why in (
                (r"SBX\| @CLONE_OK", "the sandboxed clone never completed"),
                (r"SBX\| @PHASE1_RC=\d", "the pre-declaration push never returned"),
                (r"SBX\| @PHASE2_START", "the run never reached the declare"),
            ):
                if not pane.until_raw(re.compile(marker), timeout=300):
                    raise RuntimeError(f"{why} (no {marker!r} in the pane's stream)")

            # The declare BLOCKS. $TMUX is set (by tmux), so approval routes to a tmux
            # POPUP, which renders OVER the live pane. drive_popup polls the attached
            # client (the popup's only surface) until Form A is FULLY PAINTED,
            # snapshots what `capture-pane` shows WHILE the popup is up — the overlay
            # proof — and then types the DISPLAYED number into the client, exactly as
            # a human sitting at that terminal would.
            forma = pane.drive_popup(H.PROMPT_HINT, str(issue), timeout=180)
            pane_while_popup = pane.pane_while_popup

            for marker, why in (
                (r"SBX\| @PHASE2_RC=\d", "the declare never returned after approval"),
                (r"SBX\| @PHASE3_RC=\d", "the verified push never returned"),
                (r"SBX\| @PHASE4_RC=\d", "the non-convention push never returned"),
                (r"SBX\| @SCRIPT_DONE", "the in-sandbox script never finished"),
                (r"revoked \d+ of \d+ write token", "rein never printed its exit accounting"),
            ):
                if not pane.until_raw(re.compile(marker), timeout=180):
                    raise RuntimeError(f"{why} (no {marker!r} in the pane's stream)")
        except RuntimeError as e:  # a marker never arrived, or the popup never rendered
            broke = str(e)
        finally:
            forma = forma or pane.forma
            # The popup has closed. wait_stable answers a question with NO anchor
            # string — "did the pane REPAINT?" — by waiting for the render to stop
            # changing and then looking at the settled frame.
            pane_after = pane.wait_stable(300)
            client_after = pane.client_screen()
            raw = pane.raw_stream()

    text = H.strip_ansi(raw)
    rcs = {int(n): int(rc) for n, rc in re.findall(r"SBX\| @PHASE(\d)_RC=(\d+)", text)}
    new_log = H.read_log_since(log_path, log_off)
    prompts = text.count(H.PROMPT_BANNER)  # inline Form A prompts in rein's OWN pane
    landed = {br: H.branch_exists(repo, br, env) for br in branches}
    return {
        "text": raw,
        "rcs": rcs,
        "prompts": prompts,
        "landed": landed,
        "branches": branches,
        "good": good,
        "bad": bad,
        "log": new_log,
        "forma": forma,
        "pane_while_popup": pane_while_popup,
        "pane_after": pane_after,
        "client_after": client_after,
        "broke": broke,
    }


def main() -> int:
    env = H.rein_env()

    # tmux is a hard prerequisite for the popup surface: without it there is no
    # popup to drive. pyte is a hard prerequisite for SEEING it: the popup lives
    # ONLY on the attached client's pty, which is a terminal byte stream — only an
    # emulator says what is on that screen. Missing either => SKIP with exit 3, so
    # the runner reports "this journey did NOT run" — NEVER exit 0, which would
    # report green for a path nothing exercised (the #68 footgun; see
    # tests/interactive/CLAUDE.md).
    if not H.tmux_available():
        print("SKIP: tmux is not on PATH — the popup approval surface cannot be "
              "driven, so this journey has nothing to exercise. Install tmux to "
              "run it. (Exit 3 = SKIPPED: no coverage, and it must not look like "
              "a pass.)", flush=True)
        return 3
    if not H.pyte_available():
        print(f"SKIP: pyte is not installed — the popup renders on the attached "
              f"tmux client's pty and can only be read back through a terminal "
              f"emulator, so this journey has nothing to exercise. "
              f"{H.PYTE_INSTALL_HINT}. (Exit 3 = SKIPPED: no coverage, and it must "
              f"not look like a pass.)", flush=True)
        return 3

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
            "issue #37), with rein running inside a REAL tmux pane. Throwaway repo "
            "only; closed again when the journey ends.",
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
        log, forma = r["log"], r["forma"]
        pane_while_popup, pane_after = r["pane_while_popup"], r["pane_after"]

        # 1) The ceremony + the POPUP ROUTING must hold — independent of the golden.
        #    rein's "own terminal" IS the pane now, so the routing proof is read off
        #    the pane's own byte stream: the Form A the human answered was never
        #    printed there.
        pane_stream = H.strip_ansi(r["text"])
        host_has_forma = (H.PROMPT_BANNER in pane_stream) or (H.PROMPT_HINT in pane_stream)
        popup_launched = "launching tmux popup" in log
        popup_confirmed = "CONFIRMED via tmux popup" in log
        forma_text = "\n".join(forma)
        popup_showed_forma = (H.PROMPT_BANNER in forma_text) and (H.PROMPT_HINT in forma_text)
        # THE REAL-PANE ASSERTIONS — only possible now that rein runs in a real pane
        # with LIVE content underneath the popup:
        #   the pane was LIVE and BLOCKED on the declare at the moment the popup was
        #   up (so the popup had something real to overlay), and Form A was NOT in the
        #   pane's own render (a popup is a client-owned overlay, invisible to
        #   capture-pane) — the two halves of "it OVERLAYS, it does not PRINT";
        pane_live_under_popup = f"$ rein declare {issue}" in pane_while_popup
        forma_absent_from_pane = (H.PROMPT_HINT not in pane_while_popup
                                  and H.PROMPT_BANNER not in pane_while_popup)
        #   and after the popup closed, the pane REPAINTED and carried on — its
        #   settled render shows the run's later phases, and no Form A residue is left
        #   on the client's screen. (`SBX| @SCRIPT_DONE`, tagged: rein ECHOES the
        #   script body, so an untagged `@SCRIPT_DONE` would also match the echo of
        #   `emit "@SCRIPT_DONE"` and pass even if nothing ever ran.)
        pane_repainted = "SBX| @SCRIPT_DONE" in pane_after
        client_clean_after = H.PROMPT_HINT not in r["client_after"]
        invariants = [
            (r["broke"] is None, f"the live run must complete: {r['broke']}"),
            (rcs.get(1, 0) != 0, "phase 1 (pre-declaration push) must FAIL — writes locked"),
            (rcs.get(2) == 0, "phase 2 (declare) must succeed after popup confirmation"),
            (rcs.get(3) == 0, "phase 3 (verified push) must succeed"),
            (rcs.get(4, 0) != 0, "phase 4 (non-convention ref) must be REJECTED"),
            (prompts == 0, "NO inline Form A prompt in rein's own pane (it routed to the popup)"),
            (not host_has_forma, "Form A must NOT appear in rein's own pane (it rendered in the popup)"),
            (popup_launched, "rein's log must record launching the tmux popup (surface = popup)"),
            (popup_confirmed, "rein's log must record CONFIRMED via tmux popup"),
            (popup_showed_forma, "the popup client must have shown Form A (banner + 'type the issue number')"),
            (len(forma) >= 6, "the popup's Form A must have captured cleanly for the transcript fold"),
            (pane_live_under_popup, "the popup must OVERLAY A LIVE PANE (blocked on `rein declare` underneath)"),
            (forma_absent_from_pane, "capture-pane must NOT see Form A while the popup is up (it is a client overlay)"),
            (pane_repainted, "the pane must repaint and carry on after the popup closes"),
            (client_clean_after, "no Form A residue on the client's screen once the popup closed"),
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
            print(f"  pane_live_under_popup={pane_live_under_popup} "
                  f"forma_absent_from_pane={forma_absent_from_pane} "
                  f"pane_repainted={pane_repainted} client_clean_after={client_clean_after}",
                  flush=True)
            print("--- the pane's render WHILE the popup was up (capture-pane) ---", flush=True)
            print(pane_while_popup, flush=True)
            # A broken ceremony is only debuggable with the pane's own stream in front
            # of you (was it a transient clone failure? did rein never start?). Print
            # it — the same raw surface the golden is built from.
            print("--- the pane's raw stream (pipe-pane) ---", flush=True)
            print(H.build_raw_transcript(r["text"]), flush=True)
            return 2

        # 2) Build the RAW transcript (real values) from the PANE's own byte stream,
        #    FOLD the popup's Form A into it (POPUP| lines, adjacent to rein's own
        #    `$ rein declare` -> confirmed with NO inline Form A — the reviewable
        #    routing contrast), compare NORMALIZED.
        raw = H.fold_popup(H.build_raw_transcript(r["text"]), forma)
        print()
        print(raw, flush=True)  # the pane's own stream + the folded POPUP| Form A
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        for ph, meaning in ((1, "writes locked"), (2, "human confirmed in popup"),
                            (3, "verified push"), (4, "ref cross-check")):
            print(f"  phase {ph}  rc={rcs[ph]}  ({meaning})", flush=True)
        print(f"  inline Form A prompts in rein's own pane: {prompts} (routed to popup)", flush=True)
        for line in log.splitlines():
            if "tmux popup" in line:
                print(f"  log: {line.split('] ', 1)[-1]}", flush=True)
        print("  popup OVERLAID A LIVE PANE: while Form A was up, capture-pane showed the "
              f"pane still blocked on `$ rein declare {issue}` and NO Form A "
              "(a popup is a client-owned overlay, invisible to capture-pane)", flush=True)
        print("  pane REPAINTED after the popup closed: the run carried on to "
              "@SCRIPT_DONE and no Form A residue is left on the client's screen", flush=True)
        for br, ok in landed.items():
            print(f"  branch {br}: {'LANDED' if ok else 'ABSENT'}", flush=True)

        if os.getenv("REIN_SHOW_PANE"):
            print("\n--- capture-pane WHILE the popup was up (Form A is NOT here) ---", flush=True)
            print(pane_while_popup, flush=True)
            print("\n--- the client's rendered screen: where the popup DID render ---", flush=True)
            print("\n".join(forma), flush=True)
            print("\n--- the pane, settled, AFTER the popup closed (it repainted) ---", flush=True)
            print(pane_after, flush=True)

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
