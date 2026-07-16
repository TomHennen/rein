"""realagent_write — a REAL `claude` walks the whole write path (#101 gap 2).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code notes (why the code is shaped this way):
  * The agent DECLARES up front; it does NOT discover the gate from a locked push
    (design correction to #101). The folder-trust dialog is PLUMBING —
    H.dismiss_claude_trust_dialog answers it off the pane's RENDER and NOTHING asserts
    it, so a future claude that stops asking is not a red journey.
  * session.txt is RENDERED milestone frames (`capture-pane -p -J`), never compared. Do
    NOT switch it to a scrollback-keeping emulator: claude repaints its transcript region
    in place, so pyte.HistoryScreen yields torn, overlapping half-frames (tested on a real
    capture — it is garbage). FRAMES, not scrollback.
  * REIN_UPDATE_GOLDEN=1 regenerates BOTH golden.txt (compared) and session.txt (shown).
    The commit-side twin is journeys/git_author/journey.py; the pre-declare lock is
    covered deterministically by journeys/write_ceremony/journey.py and
    journeys/gh_write/journey.py.
"""

from __future__ import annotations

import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"     # COMPARED (rein's lines + Form A)
SESSION = Path(__file__).parent / "session.txt"   # SHOWN, never compared
ISSUE_ENV = "REIN_DEMO_ISSUE"


# The ONE-LINE task the real agent gets. Short on purpose (every run spends real API
# tokens). It names the issue but says nothing about declaring or about branch names:
# the agent learns THOSE from rein's injected contract, which is the point.
def task_for(issue: int) -> str:
    return (
        f"Add one short joke to jokes.md, then commit it, push it, and open a pull "
        f"request. This is issue #{issue}."
    )


# The tail of rein's own `rein: running: <cmdline>` echo — a line rein prints and we
# control, so it is a DETERMINISTIC boundary between rein's launch surface (kept
# verbatim in the golden) and everything claude paints (kept out of it entirely).
def launch_echo(issue: int) -> str:
    return f"--dangerously-skip-permissions {task_for(issue)}"


# Markers that claude's TUI is LIVE on the pane's render. Any one is enough: the input
# box's border and the shortcut/interrupt hints are painted for the whole session,
# INCLUDING while a tool call (the `rein declare`) blocks — which is exactly the moment
# the popup is up and we need to prove there is something real underneath it.
CLAUDE_TUI_MARKERS = ("? for shortcuts", "esc to interrupt", "╭", "│")


# --------------------------------------------------------------------------
# Host-side setup
# --------------------------------------------------------------------------


def clone_checkout(repo: str, env: dict) -> str:
    """A fresh normal checkout (a real `.git` DIR -> fully hardenable, so rein binds it
    writable rather than handing the agent an ephemeral scratch tree). `rein-` prefix so
    its /tmp path normalizes to <TMP> in the compare."""
    d = tempfile.mkdtemp(prefix="rein-realagent-")
    subprocess.run(
        ["gh", "repo", "clone", repo, d, "--", "-q"],
        check=True, env=env, capture_output=True, text=True,
    )
    return d


def _pinned_session(repo: str) -> str:
    """A temp repo-only session so the journey never depends on the machine's ambient
    dev-session.yaml."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_realagent\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def launcher_script(env_overrides: dict, workdir: str, issue: int) -> str:
    """The one command a developer types in their pane, as a tiny shell file.

    `rein run` is launched from the PANE's OWN shell, so rein — and the claude it
    sandboxes — inherit $TMUX/$TMUX_PANE from tmux itself: the whole point of the
    real-pane configuration. NOTE there is NO trailing workdir positional: for
    INTERACTIVE claude a positional IS the initial prompt, so the working tree is named
    by REIN_SANDBOX_WORKDIR + the shell's cwd instead. Nothing is hidden by using a
    file — rein ECHOES the full command line under its own banner, into the very stream
    the golden is built from.
    """
    lines = [f"cd {shlex.quote(workdir)}"]
    lines += [f"export {k}={shlex.quote(v)}" for k, v in env_overrides.items()]
    lines.append(
        "exec "
        + " ".join(shlex.quote(a) for a in [
            str(H.REIN_BIN), "run", "--", "claude",
            "--dangerously-skip-permissions", task_for(issue),
        ])
    )
    return "\n".join(lines) + "\n"


# --------------------------------------------------------------------------
# Driving the real agent, INSIDE the pane
# --------------------------------------------------------------------------


def await_landing(pane: H.TmuxPaneSession, repo: str, issue: int, env: dict,
                  timeout: float = 600.0,
                  every: float = 6.0) -> tuple[str | None, list[dict]]:
    """Wait for the agent's work to appear at GITHUB — the ground truth, not a string
    on its screen. Returns (branch, prs).

    Polling GitHub (rather than matching claude's prose "I've opened the PR!") keeps the
    completion check independent of the model's wording. The branch is DISCOVERED under
    `agent/<issue>/` (matching-refs): a real agent picks its own suffix — the ref
    cross-check only pins the prefix.

    The wait runs through `pane.until`, so every poll iteration also DRAINS the attached
    client (rule 1): an unread client pty fills, tmux's attach blocks on write, and the
    pane — with claude in it — stalls. The GitHub calls are rate-limited to one every
    `every` seconds; the drain keeps running in between.
    """
    state: dict = {"last": 0.0, "branch": None, "prs": []}

    def landed() -> bool:
        now = time.time()
        if now - state["last"] < every:
            return False
        state["last"] = now
        if state["branch"] is None:
            refs = H.list_matching_refs(repo, f"agent/{issue}/", env)
            state["branch"] = refs[0] if refs else None
        if state["branch"] is not None:
            state["prs"] = H.list_prs_for_branch(repo, state["branch"], env)
        return bool(state["prs"])

    pane.until(landed, timeout=timeout, poll=0.5)
    return state["branch"], state["prs"]


def quit_agent(pane: H.TmuxPaneSession, timeout: float = 45.0) -> bool:
    """Quit the interactive agent with `/exit`, and wait for rein's exit accounting to
    reach the pane's stream. Returns whether it did.

    Deliberately NOT Ctrl-C: terminal SIGINT is UNTRAPPED BY DESIGN (cmd/rein/run.go —
    "SIGINT/SIGKILL skip this path", with the launch-time Sweep as the backstop), so
    rein would die without revoking and never print `rein: revoked N of N write
    token(s) on exit` — a security-relevant line AND this journey's TUI-collapse END
    anchor. `/exit` is the graceful quit a developer uses: claude exits, rein reaps it,
    runs its deferred exit-revoke, and PRINTS the accounting. A missing anchor trips
    exit 2 — loudly, never a silently truncated golden.
    """
    done = re.compile(r"revoked \d+ of \d+ write token")
    for _ in range(3):
        pane.send_pane("Escape")  # dismiss any transient TUI state first
        time.sleep(0.4)
        pane.send_pane_literal("/exit")
        pane.send_pane("Enter")
        if pane.until_raw(done, timeout=timeout):
            return True
    return False


def run_agent(env: dict, repo: str, issue: int, workdir: str) -> dict:
    """One live `rein run -- claude …`, TYPED INTO A REAL TMUX PANE, with the approval
    popup overlaying claude's TUI on the attached client. Returns everything the
    assertions and the transcript need."""
    session = _pinned_session(repo)
    log_path = H.helper_log_path(env)
    log_off = log_path.stat().st_size if log_path.exists() else 0

    launch_dir = tempfile.mkdtemp(prefix="rein-journey-launch-")
    # Named for the READER of the golden: its first line is the command typed into the
    # pane, so it should say what is being launched (`rein-run.sh`).
    launch_path = os.path.join(launch_dir, "rein-run.sh")
    with open(launch_path, "w") as f:
        f.write(launcher_script(
            {"REIN_SESSION_FILE": session, "REIN_SANDBOX_WORKDIR": workdir},
            workdir, issue,
        ))

    def note(msg: str) -> None:
        """Progress, with a clock. This journey drives a live LLM for ~10 minutes; the
        ONE thing that matters is that the harness reaches drive_popup BEFORE the agent
        declares (else the popup goes unanswered for its 60s and rein degrades to the
        inline prompt — which looks like a rein bug and is not one). Timestamping the
        stages is what makes that race visible instead of mysterious."""
        print(f"[drive {time.strftime('%H:%M:%S')}] {msg}", flush=True)

    forma: list[str] = []
    frames: list[tuple[str, str]] = []
    branch, prs = None, []
    pane_after_popup = ""
    broke = None
    quit_ok = False

    # The tmux SERVER is started with rein_env(), so the pane's shell — and the rein it
    # launches — inherit REIN_APP_* (to mint) and HOME/XDG_STATE_HOME (so the helper.log
    # this journey reads back is the one the run writes).
    with H.tmux_pane_session(env=env) as pane:
        try:
            # A developer types the command in their pane. $TMUX comes FROM TMUX.
            note("typed `rein run -- claude …` into the pane")
            pane.run_in_pane(f"bash {launch_path}")

            # PLUMBING, not a step (see the docstring): claude's folder-trust dialog
            # would block the session forever. Dismissed if it fires, ignored if not —
            # nothing about it is asserted, and nothing about it lands in the golden.
            # The window is generous because the dialog can only paint AFTER rein's
            # sandbox preflight + srt launch have brought claude up.
            fired = H.dismiss_claude_trust_dialog(pane, timeout=240)
            note(f"folder-trust dialog: {'dismissed' if fired else 'never fired'} "
                 f"(plumbing either way)")

            # FRAME 1 — claude's TUI, live in the pane. (until_pane re-captures on a
            # ~50ms poll and drains the client on every iteration.)
            if not pane.until_pane(
                lambda scr: any(m in scr for m in CLAUDE_TUI_MARKERS), timeout=120
            ):
                raise RuntimeError("claude's TUI never appeared in the pane")
            note("claude's TUI is live in the pane")
            frames.append(
                ("claude's TUI is LIVE in the pane — rein launched it sandboxed, and "
                 "the agent is working the task",
                 pane.pane_text()),
            )

            # The agent reads the injected contract and runs `rein declare <n>`. That
            # BLOCKS; $TMUX is set (BY TMUX), so approval routes to a POPUP, which
            # renders OVER the live TUI on the attached client. drive_popup polls the
            # CLIENT until Form A is FULLY PAINTED, snapshots what `capture-pane` shows
            # WHILE it is up (the overlay proof), reads Form A off the RENDER, and types
            # the number into the CLIENT — where the popup's keyboard is (`send-keys`
            # can never reach a client-owned overlay).
            note("ARMED at drive_popup — waiting for the agent to `rein declare` "
                 "(the popup must not fire before this line)")
            forma = pane.drive_popup(H.PROMPT_HINT, str(issue), timeout=600)
            note("popup answered on the attached client")

            # FRAME 2 — the same moment, from the PANE: claude's TUI, and NO Form A.
            frames.append(
                ("the approval popup is UP — but this is `capture-pane` of the PANE: "
                 "claude's TUI is live underneath and Form A is NOT here (a popup is a "
                 "CLIENT-owned overlay, invisible to the tmux server's own render)",
                 pane.pane_while_popup),
            )

            # FRAME 3 — the popup closed. "Did the TUI repaint?" has NO anchor string,
            # so wait for QUIESCENCE and look at the settled frame.
            pane_after_popup = pane.wait_stable(300, timeout=30)
            frames.append(
                ("just after approval — the popup closed and claude's TUI REPAINTED "
                 "(quiesced render; no Form A residue)",
                 pane_after_popup),
            )

            # The work itself: commit, push agent/<issue>/<its own suffix>, open a PR.
            # Completion is read from GITHUB, not from claude's prose.
            branch, prs = await_landing(pane, repo, issue, env, timeout=600)

            # FRAME 4 — the FINAL settled screen: the agent's own account of the work,
            # right after it landed. THE record a human reads to see what it did.
            frames.append(
                ("the FINAL settled screen — the agent's own account of the work, "
                 "after the branch and the PR landed at GitHub",
                 pane.wait_stable(500, timeout=45)),
            )
        except RuntimeError as e:  # the popup never rendered, the TUI never came up
            broke = str(e)
            print(f"[drive] the live agent run broke: {e}", flush=True)
        finally:
            forma = forma or pane.forma
            quit_ok = quit_agent(pane)
            raw = pane.raw_stream()
        pane_while_popup = pane.pane_while_popup

    text = H.strip_ansi(raw)
    return {
        "text": raw,
        "forma": forma,
        "frames": frames,
        "branch": branch,
        "prs": prs,
        "pane_while_popup": pane_while_popup,
        "pane_after_popup": pane_after_popup,
        "quit_ok": quit_ok,
        "broke": broke,
        # Inline approval prompts on rein's OWN terminal — which IS the pane claude's
        # TUI owns. BOTH kinds: Form A (the declare) and the SCOPE EXPANSION prompt are
        # separate surfaces with separate banners; counting only one would leave the
        # other free to render inline and be caught by neither the invariant nor the
        # golden, whose TUI region is collapsed.
        "prompts": text.count(H.PROMPT_BANNER),
        "expansions": text.count(H.EXPANSION_BANNER),
        "log": H.read_log_since(log_path, log_off),
    }


# --------------------------------------------------------------------------
# The two artifacts (see the docstring: one COMPARED, one SHOWN)
# --------------------------------------------------------------------------


def compared_golden(pane_raw: str, forma: list[str], issue: int) -> tuple[str, bool]:
    """ARTIFACT 2 — the COMPARED golden: rein's OWN output + the popup's Form A.

    Deterministic content ONLY, so a completely different claude session still compares
    clean. One boundary + one regex (H.split_at_agent_launch): rein's launch surface
    verbatim through its `rein: running:` echo, then column-0 `rein: …` lines. The Form
    A the human answered sits between them, which is where it happened — the declare
    blocked inside claude's TUI and the popup fired over it. Returns (text, found): a
    missing launch echo is a CEREMONY BREAK, never a silently truncated golden.
    """
    launch, rein_tail, found = H.split_at_agent_launch(
        H.build_raw_transcript(pane_raw), launch_echo(issue),
    )
    lines = launch + H.popup_block(forma) + [""] + rein_tail
    return "\n".join(lines).strip("\n") + "\n", found


def ground_truth_lines(repo: str, issue: int, pr: int, author: dict) -> list[str]:
    """What the agent OBSERVABLY DID, from rein's helper.log + the GitHub API — never
    scraped off the agent's screen, because a real agent's screen is not evidence.

    Printed as run outcomes and used as context at the top of the session artifact. The
    agent's SELF-CHOSEN branch suffix is deliberately not pinned anywhere: that a branch
    exists under the prefix is an invariant; which suffix it picked is not.
    """
    return [
        "helper.log: approval was routed to the tmux POPUP (launched), and the issue "
        "was CONFIRMED there by the human — the sandboxed agent has no tty.",
        f"helper.log: rein then APPROVED the write to {repo} and minted a WRITE-TIER "
        "token — which happens only after that confirmation.",
        f"GitHub: the agent pushed a branch under agent/{issue}/ — it chose the suffix "
        "itself; the ref cross-check only pins the prefix.",
        f"GitHub: it opened the PR https://github.com/{repo}/pull/{pr} for that branch.",
        f"GitHub: the PR's author is {author.get('login')} (is_bot="
        f"{str(bool(author.get('is_bot'))).lower()}) — the DELEGATED App identity, "
        "never the developer.",
    ]


def agent_session(frames: list[tuple[str, str]], ground_truth: list[str]) -> str:
    """ARTIFACT 3 — the agent's session: rendered milestone frames, COMMITTED but NEVER
    COMPARED (it lands as session.txt, not golden.txt, so nothing diffs it).

    This is the record a human reads to see what the agent did — and whether it was
    confused. Each frame is a `capture-pane -p -J` render: the tmux server's OWN picture
    of the pane, so it shows claude UNOBSCURED even while the popup overlay is up. The
    ground truth heads the file as CONTEXT for reading the frames (it is an assertion,
    not terminal output, which is exactly why it is not in the golden).
    """
    out = [
        "This is the REAL agent's session — SHOWN, NOT COMPARED.",
        "",
        "It is regenerated on every REIN_UPDATE_GOLDEN=1 adopt and is never diffed: a",
        "live LLM's prose, turn count and tool ordering are not a regression signal, and",
        "a permanently-red journey trains everyone to ignore drift. What the run must do",
        "is asserted as INVARIANTS in journeys/realagent_write/journey.py (a break is exit 2), and",
        "rein's own output is compared in journeys/realagent_write/golden.txt. This file is here",
        "so a human can READ what the agent actually did.",
        "",
        "Rendered frames (`tmux capture-pane -p -J`) at each milestone.",
        "",
        "--- ground truth (helper.log + the GitHub API; asserted as invariants) ---",
    ]
    out += [f"  {ln}" for ln in ground_truth]
    for i, (label, render) in enumerate(frames, start=1):
        rows = [ln.rstrip() for ln in (render or "").split("\n")]
        while rows and not rows[-1]:
            rows.pop()
        out += ["", f"---- frame {i}/{len(frames)}: {label} ----"] + rows
    return "\n".join(out).strip("\n") + "\n"


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def main() -> int:
    env = H.rein_env()

    # Three hard prerequisites — without ANY of them there is nothing to exercise, so
    # SKIP with exit 3. Exit 0 on a path that never ran is the #68 footgun this suite
    # exists to prevent (tests/interactive/CLAUDE.md).
    if shutil.which("claude") is None:
        print("SKIP: `claude` is not on PATH — this journey IS a real-agent run, so "
              "there is nothing to exercise without it. (Exit 3 = SKIPPED: no "
              "coverage, and it must not look like a pass.)", flush=True)
        return 3
    if not H.tmux_available():
        print("SKIP: tmux is not on PATH — rein and the agent must run INSIDE a real "
              "tmux pane, so approval routes to the popup that overlays the agent's "
              "TUI. (Exit 3 = SKIPPED.)", flush=True)
        return 3
    if not H.pyte_available():
        print(f"SKIP: pyte is not installed — the tmux popup exists ONLY on the "
              f"attached client's pty and can be read back only through a terminal "
              f"emulator. {H.PYTE_INSTALL_HINT}. (Exit 3 = SKIPPED.)", flush=True)
        return 3

    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    supplied = os.getenv(ISSUE_ENV)
    ours = not supplied
    if supplied:
        issue = int(supplied)
    else:
        issue = H.create_issue(
            repo,
            "rein journey: real-agent write walkthrough (safe to close)",
            "Opened by journeys/realagent_write/journey.py so a REAL `claude` "
            "can walk the whole write path (declare -> popup approval -> push -> PR) "
            "inside the sandbox, running in a REAL tmux pane. Throwaway repo only; "
            "closed again when the journey ends.",
            env,
        )

    print(f"journey: REAL-agent write on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'})", flush=True)

    workdir = None
    branch = None
    pr_numbers: list[int] = []
    try:
        workdir = clone_checkout(repo, env)
        r = run_agent(env, repo, issue, workdir)
        branch, prs, forma, log = r["branch"], r["prs"], r["forma"], r["log"]
        pr_numbers = [p["number"] for p in prs]
        author = H.pr_author(repo, pr_numbers[0], env) if pr_numbers else {}

        raw, launch_found = compared_golden(r["text"], forma, issue)

        # ---- 1) The ceremony must hold, independent of the golden. ----
        forma_text = "\n".join(forma)
        pane_while_popup, pane_after_popup = r["pane_while_popup"], r["pane_after_popup"]
        # THE REAL-PANE ASSERTIONS — impossible in the old synthesized-$TMUX shape,
        # where the pane was EMPTY and the popup had nothing to overlay. Here the popup
        # lands on top of a LIVE full-screen TUI:
        #   Form A was on the CLIENT's render (that is where `forma` was read from) and
        #   ABSENT from the PANE's render, which at that moment still showed claude's
        #   TUI, live, blocked on its `rein declare` tool call — the two halves of "it
        #   OVERLAYS, it does not PRINT" — and once the popup closed the TUI REPAINTED.
        forma_absent_from_pane = (H.PROMPT_HINT not in pane_while_popup
                                  and H.PROMPT_BANNER not in pane_while_popup)
        tui_live_under_popup = any(m in pane_while_popup for m in CLAUDE_TUI_MARKERS)
        tui_repainted = (any(m in pane_after_popup for m in CLAUDE_TUI_MARKERS)
                         and H.PROMPT_HINT not in pane_after_popup)
        invariants = [
            # NB: NO "the folder-trust dialog must fire" invariant. It is claude's
            # behavior, not rein's; it is HANDLED (plumbing) but asserting it would make
            # a future auto-trusting claude a red journey.
            (r["broke"] is None, f"the live run must complete: {r['broke']}"),
            ("launching tmux popup" in log,
             "rein's log must record launching the tmux popup (the agent's TUI owns the "
             "terminal, so approval routes off it)"),
            ("CONFIRMED via tmux popup" in log,
             "rein's log must record the issue CONFIRMED via the tmux popup"),
            ((H.PROMPT_BANNER in forma_text) and (H.PROMPT_HINT in forma_text),
             "the popup must have shown Form A on the CLIENT (the real agent DID run "
             "`rein declare`)"),
            (f"#{issue}" in forma_text,
             f"the Form A the human answered must be for issue #{issue}"),
            (tui_live_under_popup,
             "the popup must OVERLAY A LIVE claude TUI — the pane's own render still "
             "shows the TUI while Form A is up"),
            (forma_absent_from_pane,
             "capture-pane must NOT see Form A while the popup is up — the popup "
             "OVERLAYS the TUI, it does not PRINT into the pane"),
            (tui_repainted,
             "claude's TUI must REPAINT once the popup closes (settled render, no Form "
             "A residue)"),
            (r["prompts"] == 0 and r["expansions"] == 0,
             "NO inline approval prompt of EITHER kind in rein's own pane — neither "
             "Form A nor a SCOPE EXPANSION (approval routed to the popup)"),
            ("ConfirmWrite: APPROVED" in log and "write mint succeeded: tier=write" in log,
             "rein must have APPROVED the write and minted a WRITE-TIER token (it is "
             "minted only after the human's confirmation)"),
            (branch is not None,
             f"the agent must push a branch under agent/{issue}/ (it picks the suffix)"),
            (len(prs) == 1,
             f"exactly ONE PR must exist at GitHub for {branch} (found {len(prs)})"),
            (bool(author.get("is_bot")) and str(author.get("login", "")).startswith("app/"),
             f"the PR author must be the DELEGATED App bot, not the developer "
             f"(got {author!r})"),
            (launch_found,
             "rein's `running:` launch echo must be in the pane stream — it is the "
             "boundary between rein's own output and the agent's TUI, and without it "
             "the golden would be silently truncated"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("CEREMONY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  prompts={r['prompts']} expansions={r['expansions']} branch={branch} "
                  f"prs={pr_numbers} author={author} launch_found={launch_found} "
                  f"quit_ok={r['quit_ok']}", flush=True)
            print(f"  tui_live_under_popup={tui_live_under_popup} "
                  f"forma_absent_from_pane={forma_absent_from_pane} "
                  f"tui_repainted={tui_repainted}", flush=True)
            print(f"  log: {[l for l in log.splitlines() if 'popup' in l or 'mint' in l]}",
                  flush=True)
            print("--- the pane's render WHILE the popup was up (capture-pane) ---",
                  flush=True)
            print(pane_while_popup, flush=True)
            # The compared golden holds NO agent content, so on a break it cannot show
            # what the agent was doing when things went wrong. Dump the session frames
            # here instead — that is the whole point of keeping them.
            print("--- the agent's session (rendered frames — what it was actually "
                  "doing) ---", flush=True)
            print(agent_session(r["frames"],
                                ["(the run broke before ground truth could be read)"]),
                  flush=True)
            print("--- rein's own output (the compared golden's content) ---", flush=True)
            print(raw, flush=True)
            return 2

        # ---- 2) The two artifacts. The golden (COMPARED) is already built above and
        #         holds rein's own output + Form A — no agent content. The agent's
        #         session (SHOWN) is a separate file that is never diffed. ----
        truth = ground_truth_lines(repo, issue, pr_numbers[0], author)
        session = agent_session(r["frames"], truth)

        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  inline approval prompts in rein's own pane: {r['prompts']} Form A, "
              f"{r['expansions']} scope-expansion (both routed to the popup)", flush=True)
        for line in log.splitlines():
            if "tmux popup" in line or "write mint succeeded" in line:
                print(f"  log: {line.split('] ', 1)[-1]}", flush=True)
        print("  popup OVERLAID THE LIVE claude TUI: Form A was on the attached client's "
              "render and ABSENT from capture-pane of the pane, which still showed the "
              "TUI blocked on its `rein declare` tool call", flush=True)
        print("  claude's TUI REPAINTED after the popup closed (settled render, no Form "
              "A residue) and the run carried on to branch + PR", flush=True)
        for ln in truth:
            print(f"  {ln}", flush=True)
        print(f"  branch the AGENT chose: {branch}", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            s = H.write_agent_session(SESSION, session)
            print(f"[golden UPDATED] {p} (raw; COMPARED)", flush=True)
            print(f"[session UPDATED] {s} ({len(r['frames'])} rendered frames; SHOWN, "
                  f"never compared — read it to see what the agent did)", flush=True)
            return 0

        # The committed session artifact is what a human READS to see what the agent did;
        # it is only rewritten on an adopt, so point at it rather than diffing it.
        print(f"  the agent's session ({len(r['frames'])} rendered frames, SHOWN not "
              f"compared): {SESSION}", flush=True)

        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized) — a "
                  f"DIFFERENT claude session still compares clean: no agent content is "
                  f"in the compared golden", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "realagent_write.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:",
              flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)",
              flush=True)
        return 1

    finally:
        # Hard-constraint #1: leave the throwaway clean. Belt-and-suspenders — the agent
        # chose its own branch name, so DISCOVER whatever is under `agent/<issue>/` (and
        # re-list its PRs) even if an exception fired before the host verification ran.
        branches = set()
        if branch:
            branches.add(branch)
        try:
            branches |= set(H.list_matching_refs(repo, f"agent/{issue}/", env))
        except Exception:
            pass
        prs_to_close = set(pr_numbers)
        for br in branches:
            try:
                prs_to_close |= {p["number"] for p in H.list_prs_for_branch(repo, br, env)
                                 if p["state"] == "OPEN"}
            except Exception:
                pass
        for pn in prs_to_close:
            H.close_pr(repo, pn, env)
        for br in branches:
            H.delete_branch(repo, br, env)
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        if ours:
            H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print(f"cleanup: {len(prs_to_close)} PR(s) closed; branches deleted "
              f"({sorted(branches)}); checkout removed"
              + ("; issue closed" if ours else ""), flush=True)


if __name__ == "__main__":
    sys.exit(main())
