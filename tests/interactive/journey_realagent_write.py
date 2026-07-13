"""journey_realagent_write — a REAL `claude` walks the WHOLE write path (#101 gap 2).

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

THE COVERAGE GAP (#101 "gap 2"): every other journey's "agent" is a deterministic
bash script we wrote, and `test_realagent_e2e` runs a REAL claude but only asks it
`2+2`. So the thing the product is actually ABOUT — a real LLM meeting rein's write
gate and working the whole ceremony — had never been tested. This journey drives a
REAL `claude` in the sandbox, gives it a one-line task, and asserts it comes out the
other end with a branch and a PR authored by the DELEGATED bot identity.

WHAT IT PROVES, end to end, with a live model:
  * a real agent reads the injected contract and DECLARES (see the correction below);
  * approval routes to the tmux POPUP (the default surface inside $TMUX, #37) and the
    human confirms there — the agent, sandboxed, has no tty and cannot self-approve;
  * the write-tier token is minted, the agent's push LANDS on `agent/<issue>/<its own
    suffix>`, and `gh pr create` opens a real PR (the #101 "gap 1" path, here driven
    by an LLM rather than our bash);
  * the PR's author is the App bot (`app/<slug>`, is_bot=true) — the delegated
    identity, NEVER the developer (the PR-side twin of journey_git_author's commit
    author `<name> (via rein)`).

TWO DESIGN CORRECTIONS TO ISSUE #101 (this is what a live spike actually showed):

  1. THE AGENT DECLARES FIRST — it does NOT "discover the declare from the locked-push
     error". #101 proposed letting claude hit the pre-declare LOCK and work it out.
     Reality: rein injects its agent contract via `--append-system-prompt` (cmd/rein/
     contract.go), claude READS it, and declares up front ("I'll start by declaring
     the issue, since writes are gated"). Contriving a locked-push discovery would
     mean fighting the product's own design. The pre-declare LOCK is already covered
     deterministically (journey_write_ceremony phase 1, journey_gh_write's deny-before),
     so this journey asserts the sequence that REALLY happens:
         contract -> `rein declare <n>` -> popup approval -> write+commit+push -> PR.

  2. THE FOLDER-TRUST DIALOG IS PART OF THE PATH, and it is THE demo of #100. The
     writable checkout is a fresh /tmp clone — a path claude has never seen — so
     claude opens "Quick safety check: Is this a project you created or one you
     trust?" and BLOCKS FOREVER if nobody answers (a spike run sat there 420s doing
     nothing). That dialog is unfindable in the raw ANSI byte soup and TRIVIAL to
     match on a RENDERED SCREEN: `H.wait_for_screen(child, "...trust...")` (pyte),
     then Enter selects the highlighted "1. Yes, I trust this folder". This is
     exactly the bug class #100 retired — the byte stream is a history of paint
     operations, not a picture.

THE GOLDEN — Tom's decision, and a deliberate deviation from the letter of the
raw-golden rule (documented here because it is the interesting call):

  A real LLM's TUI cannot be a verbatim golden — different prose, tool ordering,
  token counts, spinners, timing — and it REDRAWS, so its ANSI-stripped bytes are
  paint history, not lines. A raw capture would be thousands of lines of soup,
  rewritten wholesale on every adopt, and permanently red. So the golden keeps:

    * rein's OWN host-side output VERBATIM — the banner, the sandbox/egress/working-
      tree lines, the injected contract, the exit token accounting. That is the
      security-relevant surface, it is line-oriented, and it MUST stay stable: a new
      or changed rein line still trips drift, exactly as in every other journey.
    * the popup's Form A, folded in as `POPUP| ` lines (the same one-transcript /
      two-views model as journey_tmux_popup_approval) — what the human READ and
      answered.
    * a small `MILESTONE| ` block: what the agent OBSERVABLY DID, read from GROUND
      TRUTH (rein's helper.log + the GitHub API) rather than scraped out of claude's
      screen. Phrased to be stable: the agent's self-chosen branch SUFFIX is
      deliberately not pinned here (it is asserted as an invariant instead).
    * claude's TUI region collapsed to ONE placeholder line
      (reinharness.collapse_agent_tui) — the golden shows THAT the agent worked,
      without pinning WHAT it said.

  The collapse is bounded by two anchors that MUST both be found (the tail of rein's
  `rein: running:` echo; rein's `revoked N of N write token(s) on exit`), and a miss
  is a CEREMONY BREAK (exit 2), never a silently smaller golden. So determinism here
  does NOT come from loosening the compare: it comes from the fact that the only
  non-deterministic surface is collapsed, and everything security-relevant is still
  compared line for line.

DELIVERABLE: golden/realagent_write.txt.

    python3 tests/interactive/journey_realagent_write.py            # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_realagent_write.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_realagent_write.py

Exit 0 = the ceremony held AND the normalized transcript matches the golden. Exit 1 =
golden drift. Exit 2 = the ceremony itself broke (the agent never declared, the popup
never fired, no branch/PR landed, the PR author was not the delegated bot, or a
collapse anchor was missing). Exit 3 = SKIPPED (`claude`, `tmux` or `pyte` absent —
without any of them there is nothing to drive, and a skip must never look like a pass).

QUOTA: this launches ONE real `claude` and spends real API tokens. The task is one
line on purpose.

SELF-CONTAINED: creates its own throwaway issue; in a `finally` it DISCOVERS whatever
branch the agent pushed (it picks its own suffix), closes any PR on it, deletes the
branch, removes the checkout and closes the issue. Touches only the throwaway
(hard-constraint #1); the repo is resolved the rein-init way (resolve_throwaway_repo).
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import time

import pexpect

import reinharness as H

GOLDEN_NAME = "realagent_write.txt"
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
# control, so it is a DETERMINISTIC anchor for where claude's TUI region begins.
def tui_start_anchor(issue: int) -> str:
    return f"--dangerously-skip-permissions {task_for(issue)}"


# Ground truth, folded into the transcript as its own view (the mirror of POPUP| and
# SBX|). NOT terminal output, and the block says so on its first line.
MILESTONE_TAG = "MILESTONE| "

# claude's folder-trust dialog, on the RENDERED screen (#100). The checkout is a
# fresh /tmp clone, so this ALWAYS fires; unanswered, claude blocks forever.
TRUST_DIALOG_RE = r"(?i)(quick safety check|trust this folder|project you created or one you trust)"


# --------------------------------------------------------------------------
# Host-side setup
# --------------------------------------------------------------------------


def clone_checkout(repo: str, env: dict) -> str:
    """A fresh normal checkout (a real `.git` DIR -> fully hardenable, so rein binds
    it writable rather than handing the agent an ephemeral scratch tree). `rein-`
    prefix so its /tmp path normalizes to <TMP> in the compare. It is also a path
    claude has NEVER seen — which is what makes the folder-trust dialog fire."""
    d = tempfile.mkdtemp(prefix="rein-realagent-")
    subprocess.run(
        ["gh", "repo", "clone", repo, d, "--", "-q"],
        check=True, env=env, capture_output=True, text=True,
    )
    return d


def _pinned_session(repo: str) -> str:
    """A temp repo-only session so the journey never depends on the machine's
    ambient dev-session.yaml."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_realagent\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


# --------------------------------------------------------------------------
# Driving the real agent
# --------------------------------------------------------------------------


def await_landing(repo: str, issue: int, env: dict, run: H.ReinRun,
                  timeout: float = 480.0) -> tuple[str | None, list[dict]]:
    """Wait for the agent's work to appear at GITHUB — the ground truth, not a
    string on its screen. Returns (branch, prs).

    Polling GitHub (rather than matching claude's prose "I've opened the PR!") is
    what makes the completion check independent of the model's wording. The branch is
    DISCOVERED under `agent/<issue>/` (matching-refs): a real agent picks its own
    suffix — the ref cross-check only pins the prefix.

    Keeps draining rein's pty throughout: claude repaints continuously, and an unread
    pty buffer would block it (see H.drain_children).
    """
    deadline = time.time() + timeout
    branch, prs = None, []
    while time.time() < deadline:
        H.drain_children([run.child], poll=1.0)
        if branch is None:
            refs = H.list_matching_refs(repo, f"agent/{issue}/", env)
            branch = refs[0] if refs else None
        if branch is not None:
            prs = H.list_prs_for_branch(repo, branch, env)
            if prs:
                return branch, prs
        # keep reading rein's pty between GitHub polls, never sleep on it
        H.drain_children([run.child], poll=1.0)
        H.drain_children([run.child], poll=1.0)
    return branch, prs


def quit_agent(run: H.ReinRun, timeout: int = 90) -> bool:
    """Stop the interactive agent and READ THROUGH TO EOF so rein prints its exit
    lines. Returns whether EOF was reached.

    Deliberately NOT ReinRun.quit_tui, and deliberately NOT Ctrl-C — both LOSE rein's
    exit accounting, which is a security-relevant line AND this journey's collapse END
    anchor:
      * quit_tui force-closes the pty, cutting the tail off;
      * Ctrl-C is UNTRAPPED BY DESIGN (cmd/rein/run.go: "Terminal SIGINT (Ctrl-C) is
        NOT trapped … SIGINT/SIGKILL skip this path", with the launch-time Sweep as
        the backstop), so rein dies without revoking and never prints the line. A
        live run confirmed exactly that: two Ctrl-Cs quit claude and the transcript
        ended with NO `rein: revoked …` line at all.
    `/exit` is the graceful TUI quit a developer uses: claude exits, rein reaps it,
    runs its deferred exit-revoke, and PRINTS the accounting. Ctrl-C is kept only as a
    last-resort unblock (if it is ever needed, the missing anchor trips exit 2 —
    loudly, never a silently truncated golden).
    """
    deadline = time.time() + timeout
    for attempt in range(3):
        if time.time() > deadline:
            break
        try:
            run.child.send("\x1b")  # dismiss any transient TUI state first
            time.sleep(0.3)
            run.child.send("/exit\r")
            run.child.expect(pexpect.EOF, timeout=25)
            run.child.close(force=True)
            return True
        except pexpect.TIMEOUT:
            continue
        except (pexpect.EOF, OSError):
            run.child.close(force=True)
            return True
    try:
        run.child.sendcontrol("c")
        time.sleep(0.5)
        run.child.sendcontrol("c")
        run.child.expect(pexpect.EOF, timeout=20)
    except (pexpect.TIMEOUT, pexpect.EOF, OSError):
        pass
    try:
        run.child.close(force=True)
    except Exception:
        pass
    return False


def run_agent(env: dict, repo: str, issue: int, workdir: str) -> dict:
    """One live `rein run -- claude …` on a pty, with the popup on a dedicated tmux
    server. Returns everything the assertions and the transcript need."""
    session = _pinned_session(repo)
    log_path = H.helper_log_path(env)
    log_off = log_path.stat().st_size if log_path.exists() else 0

    forma: list[str] = []
    trust_seen = False
    branch, prs = None, []

    with H.tmux_popup_session() as tmux_sess:
        # rein on its OWN plain pty, with $TMUX/$TMUX_PANE pointing at the dedicated
        # session so the popup renders on the attached client. NO trailing workdir
        # positional: for INTERACTIVE claude a positional IS the initial prompt (see
        # spawn_claude_interactive) — the working tree is named via
        # REIN_SANDBOX_WORKDIR + cwd instead.
        run = H.spawn_rein(
            ["run", "--", "claude", "--dangerously-skip-permissions", task_for(issue)],
            env=env, cwd=workdir, timeout=600,
            extra_env={
                "REIN_SESSION_FILE": session,
                "REIN_SANDBOX_WORKDIR": workdir,
                **tmux_sess.tmux_env(),
            },
        )
        try:
            # 1) THE FOLDER-TRUST DIALOG (#100's demo). A fresh /tmp checkout is a
            #    path claude has never seen, so it asks — and blocks forever if we
            #    ignore it. Invisible in the raw byte soup; simply THERE on a
            #    rendered screen. Enter takes the highlighted "1. Yes, I trust…".
            #
            #    HANDLED, NOT ASSERTED. Whether claude asks at all is THIRD-PARTY
            #    behavior: a future version that auto-trusts the checkout is not a
            #    rein regression, and making it a hard invariant would turn that
            #    into a red journey. So we answer it if it fires and carry on if it
            #    does not — the rein-side invariants below are what must hold. Short
            #    timeout on purpose: the dialog paints within seconds of startup if
            #    at all, so a long one only buys a slow failure. (If claude never
            #    asks, the declare simply happens sooner and step 2 catches it.)
            trust_seen, _ = H.wait_for_screen(
                run.child, TRUST_DIALOG_RE, timeout=45, screen=run.screen(),
            )
            if trust_seen:
                run.child.send("\r")

            # 2) The agent reads the injected contract and declares. The declare
            #    BLOCKS; approval routes to the POPUP ($TMUX is set). Drain rein's
            #    pty while we wait on the client, or claude's repaints fill the pty
            #    buffer and it never gets to finish the declare at all.
            forma = tmux_sess.drive_popup(
                H.PROMPT_HINT, str(issue), timeout=420, drain=[run.child],
            )

            # 3) The work itself: commit, push agent/<issue>/<its own suffix>, open a
            #    PR. Completion is read from GITHUB, not from claude's prose.
            branch, prs = await_landing(repo, issue, env, run, timeout=480)
            # The PR appears at GitHub the instant `gh pr create` returns — while
            # claude's bash tool call is often still finishing. Let it land before we
            # interrupt, so Ctrl-C ends the SESSION rather than just that tool call.
            for _ in range(8):
                H.drain_children([run.child], poll=1.0)
        except (pexpect.EOF, pexpect.TIMEOUT, RuntimeError) as e:
            print(f"[drive] the live agent run broke: {e}", flush=True)
        finally:
            forma = forma or tmux_sess.forma
            quit_agent(run)

    return {
        "text": run.text(),
        "forma": forma,
        "trust_seen": trust_seen,
        "branch": branch,
        "prs": prs,
        # Inline approval prompts on rein's OWN terminal — BOTH kinds. Form A (the
        # declare) and the SCOPE EXPANSION prompt are separate surfaces with separate
        # banners; counting only one leaves the other able to render inline (on the
        # terminal the agent's TUI owns) and be caught by neither the invariant nor
        # the golden, since the golden's TUI region is collapsed.
        "prompts": run.prompt_count(),
        "expansions": run.expansion_prompt_count(),
        "log": H.read_log_since(log_path, log_off),
    }


# --------------------------------------------------------------------------
# The transcript
# --------------------------------------------------------------------------


def milestone_block(repo: str, issue: int, pr: int, author: dict) -> list[str]:
    """The MILESTONE| view: what the agent OBSERVABLY DID, from ground truth.

    Every line is phrased to normalize CORRECTLY, which is why the PR is written as
    its URL: a bare `#<n>` would be eaten by the generic `#\\d+` -> <ISSUE> rule (it
    runs before the `/pull/\\d+` -> <PR> rule), so the one line whose whole point is
    the PR number would read `PR #<ISSUE>`. Harmless in the compare (it hits both
    sides identically) but wrong to a reader; the URL form takes the `/pull/` rule
    and normalizes to `.../pull/<PR>`. The agent's SELF-CHOSEN branch suffix is
    deliberately NOT pinned — it is an invariant (a branch exists under the prefix),
    not golden material, because a real agent names it differently every run.
    """
    return [
        "ground truth — NOT terminal output: read from rein's helper.log and the "
        "GitHub API after the run, because a real agent's screen is not evidence.",
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


def fold_milestones(transcript: str, lines: list[str]) -> str:
    """Fold the MILESTONE| block in after the popup's Form A (which itself was folded
    at the TUI placeholder) — so the artifact reads in the order it happened: rein's
    launch surface, the agent's collapsed TUI, the Form A the human answered inside
    it, then what that produced."""
    block = [""] + [(MILESTONE_TAG + ln).rstrip() for ln in lines]
    out = transcript.split("\n")
    last_popup = max(
        (i for i, ln in enumerate(out) if ln.startswith(H.POPUP_TAG.rstrip())),
        default=None,
    )
    if last_popup is None:
        return transcript
    return "\n".join(out[: last_popup + 1] + block + out[last_popup + 1:])


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
        print("SKIP: tmux is not on PATH — the agent's TUI owns the terminal, so "
              "approval must route to a tmux popup, which cannot be driven without "
              "tmux. (Exit 3 = SKIPPED.)", flush=True)
        return 3
    if not H.pyte_available():
        print(f"SKIP: pyte is not installed — both redrawing surfaces this journey "
              f"depends on (claude's folder-trust dialog, the tmux popup's Form A) can "
              f"only be read off a RENDERED screen. {H.PYTE_INSTALL_HINT}. "
              f"(Exit 3 = SKIPPED.)", flush=True)
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
            "Opened by tests/interactive/journey_realagent_write.py so a REAL `claude` "
            "can walk the whole write path (declare -> popup approval -> push -> PR) "
            "inside the sandbox. Throwaway repo only; closed again when the journey ends.",
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

        raw, collapsed = H.collapse_agent_tui(
            H.build_raw_transcript(r["text"]), tui_start_anchor(issue),
        )

        # ---- 1) The ceremony must hold, independent of the golden. ----
        forma_text = "\n".join(forma)
        invariants = [
            # NB: NO "the folder-trust dialog must fire" invariant. It is claude's
            # behavior, not rein's; it is HANDLED (run_agent answers it) but asserting
            # it would make a future auto-trusting claude a red journey.
            ("launching tmux popup" in log,
             "rein's log must record launching the tmux popup (the agent's TUI owns "
             "the terminal, so approval routes off it)"),
            ("CONFIRMED via tmux popup" in log,
             "rein's log must record the issue CONFIRMED via the tmux popup"),
            ((H.PROMPT_BANNER in forma_text) and (H.PROMPT_HINT in forma_text),
             "the popup must have shown Form A (the real agent DID run `rein declare`)"),
            (f"#{issue}" in forma_text,
             f"the Form A the human answered must be for issue #{issue}"),
            (r["prompts"] == 0 and r["expansions"] == 0,
             "NO inline approval prompt of EITHER kind on rein's own terminal — "
             "neither Form A nor a SCOPE EXPANSION (approval routed to the popup)"),
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
            (collapsed,
             "both TUI-collapse anchors must be found (rein's `running:` echo and its "
             "exit token accounting) — a missing anchor would silently truncate the golden"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("CEREMONY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  trust_seen={r['trust_seen']} prompts={r['prompts']} "
                  f"expansions={r['expansions']} branch={branch} "
                  f"prs={pr_numbers} author={author} collapsed={collapsed}", flush=True)
            print(f"  log: {[l for l in log.splitlines() if 'popup' in l or 'mint' in l]}",
                  flush=True)
            print("--- transcript (raw) ---", flush=True)
            print(raw, flush=True)
            return 2

        # ---- 2) Build the artifact: collapsed TUI + the popup's Form A + the
        #         ground-truth milestones, then compare NORMALIZED. ----
        raw = H.fold_popup(raw, forma, anchor_prefix=H.AGENT_TUI_PLACEHOLDER)
        raw = fold_milestones(raw, milestone_block(repo, issue, pr_numbers[0], author))
        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  folder-trust dialog: fired={r['trust_seen']} (handled on the rendered "
              f"screen when it does; not asserted — it is claude's behavior, not rein's)",
              flush=True)
        print(f"  inline approval prompts on rein's terminal: {r['prompts']} Form A, "
              f"{r['expansions']} scope-expansion (both routed to the popup)", flush=True)
        for line in log.splitlines():
            if "tmux popup" in line or "write mint succeeded" in line:
                print(f"  log: {line.split('] ', 1)[-1]}", flush=True)
        print(f"  branch the AGENT chose: {branch}", flush=True)
        print(f"  PR(s) at GitHub: {pr_numbers}", flush=True)
        print(f"  PR author: {author} (delegated identity, not the developer)", flush=True)

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
        scratch = os.path.join(tempfile.gettempdir(), "realagent_write.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:",
              flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)",
              flush=True)
        return 1

    finally:
        # Hard-constraint #1: leave the throwaway clean. Belt-and-suspenders — the
        # agent chose its own branch name, so DISCOVER whatever is under
        # `agent/<issue>/` (and re-list its PRs) even if an exception fired before the
        # host verification ran.
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
