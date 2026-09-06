"""declare_new — `rein declare --new "<title>"`: file the run's FIRST issue (#180).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: unlike every other declare journey, there is no pre-existing issue to
declare — `rein declare --new` is the bootstrap for a repo/run with none yet. The
agent PROPOSES a title (and optional body); the human approves Form A by typing the
proposed title's FIRST WORD (issuemeta.FirstWord), not a number; on approval rein
files the issue itself, under a broker-minted issues:write token that never reaches
the sandbox, and it becomes the run's declared issue.

The DENY leg (a wrong first-word answer) is folded into the SAME sandboxed run as a
fourth phase rather than a second `rein run` launch: it is the cheapest way to
exercise the real refusal path (CLI -> proxy -> broker -> Form A -> deny) without a
second ~120-180s srt spin-up. "Host-side" here means the denial happens because the
word typed on the HOST tty was wrong — the same mechanism as every other Form A deny
in this suite, just with the new-issue ceremony's token.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"

# Fixed (not per-run) proposed title/body, mirroring gh_write's COMMENT_BODY/
# PR_TITLE: the golden echoes these back verbatim, so they must be STABLE
# across runs, not nonce-suffixed — a per-run nonce inside the title would
# land in the Form A prompt, the typed-answer echo, and rein's re-echo of the
# script, and nothing in `_NORMALIZE_RULES` normalizes a title word BY
# CONSTRUCTION (the generic hash rule only coincidentally matches short hex,
# and only when the nonce happens to contain a letter). Each run files and
# closes its OWN issue (a fresh number every time), so a fixed title never
# collides across runs.
FIRST_WORD = "JourneyDeclareNew"
# No apostrophes: this text is embedded into the in-sandbox bash script via
# Python repr(), which single-quotes it for the shell — an apostrophe would
# break that quoting (gh_write's PR_TITLE/PR_BODY convention).
TITLE = f"{FIRST_WORD}: files this runs declared issue via Form A (safe to close)"
BODY = ("Opened by journeys/declare_new/journey.py to prove that rein declare "
        "--new lets an agent PROPOSE an issue that the human approves via Form "
        "A, which rein then files under the App bot identity and makes this "
        "runs declared issue. Throwaway repo only; closed when the journey ends.")
DENY_FIRST_WORD = "JourneyDeclareNewDeny"
DENY_TITLE = f"{DENY_FIRST_WORD}: must be refused by a wrong first-word answer"
WRONG_ANSWER = "not-the-first-word"

# --------------------------------------------------------------------------
# The in-sandbox agent script — a deterministic bash "agent", every step tagged
# --------------------------------------------------------------------------


def declare_new_script(repo: str, title: str, body: str, deny_title: str, branch: str) -> str:
    """A `bash -c` body run as the srt child. It cannot be puppeted line-by-line
    (one sandboxed process), so each STEP emits a tagged `@PHASE..` sentinel and the
    test asserts on those IN SEQUENCE. `cd "$0"` enters the writable checkout mount.

    `rein declare --new` opens no tty of its own (the sandboxed agent has none at
    all) — it proxies to the broker, which prompts on the HOST tty exactly like
    every other declare. That prompt is answered by the JourneyStep's `answers`
    (matched against the WHOLE captured pty session, not this script's stdout), so
    from in here it is an ordinary blocking command.

    `declare_new()` is a local variant of `sandbox_preamble`'s `run`: it tags the
    command + its output into the transcript exactly like `run` does, but ALSO
    captures the combined output into $DECLARE_LAST_OUT so the script can parse the
    GitHub-assigned issue number out of it — there is no number to hard-code, since
    filing it is the whole point. That number is what names the push branch below,
    which is the in-script proof that the filed issue IS the run's declared issue.
    """
    tag = H.SBX_TAG
    return f"""
{H.sandbox_preamble()}
declare_new() {{
  printf '%s$ %s\\n' '{tag}' "$*"
  DECLARE_LAST_OUT=$("$@" 2>&1)
  DECLARE_LAST_RC=$?
  printf '%s\\n' "$DECLARE_LAST_OUT" | while IFS= read -r l; do printf '%s%s\\n' '{tag}' "$l"; done
  return $DECLARE_LAST_RC
}}
cd "$0"

emit "@PHASE1_START  rein declare --new (agent PROPOSES filing the run's issue; blocks for the human)"
declare_new rein declare --new {title!r} --body {body!r}
emit "@PHASE1_RC=$?"

NEW_ISSUE=$(printf '%s' "$DECLARE_LAST_OUT" | grep -oE '#[0-9]+' | head -1 | tr -d '#')
emit "@FILED_ISSUE=#$NEW_ISSUE"

emit "@PHASE2_START  push agent/$NEW_ISSUE/{branch} using the number rein just returned"
run git checkout -b "agent/$NEW_ISSUE/{branch}"
echo "declare_new journey: probe commit for the newly filed issue" >> declare-new-probe.txt
run git add -A
run git commit -q -m "declare_new journey: probe for the newly filed issue"
run git push origin HEAD:refs/heads/agent/$NEW_ISSUE/{branch}
emit "@PUSH_RC=$?"

emit "@PHASE3_START  gh issue view (read) — the filed issue as seen from inside the sandbox"
run gh issue view "$NEW_ISSUE" --repo {repo} --json number,title,author,url
emit "@VIEW_RC=$?"

emit "@PHASE4_START  rein declare --new AGAIN, answered with the WRONG word (expect: refused, nothing filed)"
declare_new rein declare --new {deny_title!r}
emit "@PHASE4_RC=$?"
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# Host-side setup / teardown
# --------------------------------------------------------------------------


def clone_checkout(repo: str, env: dict) -> str:
    """A fresh normal checkout whose .git is a real dir -> fully hardenable. Named
    with a `rein-` prefix so its /tmp path normalizes to <TMP> in the compare.
    Cloned host-side so no clone chatter enters the sandbox transcript."""
    d = tempfile.mkdtemp(prefix="rein-declnew-")
    subprocess.run(
        ["gh", "repo", "clone", repo, d, "--", "-q"],
        check=True, env=env, capture_output=True, text=True,
    )
    return d


def _pinned_session(repo: str) -> str:
    """A temp repo-only session so the journey never depends on the machine's
    ambient dev-session.yaml (and never trips the retired `issue:` warning)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_declare_new\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def _rc(text: str, name: str) -> int | None:
    m = re.search(rf"@{name}_RC=(\d+)", text)
    return int(m.group(1)) if m else None


def drive_journey(env, repo, branch, workdir):
    """Drive the ONE sandboxed `rein run` through the shared runner (#82). The
    sandbox launch is a normal JourneyStep whose argv is the full `rein run --
    bash -c <script> <workdir>`; the TWO Form-A new-issue prompts (approve, then
    deny) are answered inline via the step's `answers`, in order, exactly like any
    other step's prompts."""
    step = H.JourneyStep(
        argv=["run", "--", "bash", "-c",
              declare_new_script(repo, TITLE, BODY, DENY_TITLE, branch), workdir],
        label=f"rein run -- bash -c <sandbox declare-new script> {workdir}",
        answers=[
            (H.NEW_ISSUE_PROMPT_HINT, FIRST_WORD),    # PHASE1: approve with the real first word
            (H.NEW_ISSUE_PROMPT_HINT, WRONG_ANSWER),  # PHASE4: deny with the wrong word
        ],
        extra_env={
            "REIN_SESSION_FILE": _pinned_session(repo),
            "REIN_SANDBOX_WORKDIR": workdir,
        },
        timeout=240,
    )
    result = H.run_journey([step], env=env)
    return result, result.steps[0].text


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)

    branch_nonce = H.unique_branch("declnew")

    print(f"journey: declare --new on {repo} (proposed title: {TITLE!r})", flush=True)

    workdir = None
    filed_issue: int | None = None
    pushed_branch: str | None = None
    try:
        workdir = clone_checkout(repo, env)
        start = time.time()
        result, text = drive_journey(env, repo, branch_nonce, workdir)

        phase1_rc = _rc(text, "PHASE1")
        push_rc = _rc(text, "PUSH")
        view_rc = _rc(text, "VIEW")
        phase4_rc = _rc(text, "PHASE4")
        filed_match = re.search(r"@FILED_ISSUE=#(\d+)", text)
        filed_issue = int(filed_match.group(1)) if filed_match else None
        prompts = text.count(H.NEW_ISSUE_PROMPT_BANNER)

        if filed_issue is not None:
            pushed_branch = f"agent/{filed_issue}/{branch_nonce}"

        # Host-side GROUND TRUTH: did the issue actually get filed at GitHub, with
        # the exact proposed title, under the App's bot identity, with the
        # attribution trailer — and did the push the run made using that number
        # actually land? (The in-sandbox RCs only say the local commands exited 0;
        # this proves the filing and the push are both real.)
        got_title = H.issue_title(repo, filed_issue, env) if filed_issue else None
        author = H.issue_author(repo, filed_issue, env) if filed_issue else {}
        got_body = H.issue_body(repo, filed_issue, env) if filed_issue else ""
        # rein knows the repo owner but never learns the operator's GitHub
        # login, so the trailer deliberately @-mentions nobody (a wrong
        # @mention on a public issue would notify the wrong account).
        want_trailer = "_Filed by rein on behalf of the operator of this session._"
        branch_pushed = H.branch_exists(repo, pushed_branch, env) if pushed_branch else False

        # Host-side GROUND TRUTH for the audit trail (#180): the run's own
        # sandbox-<runID>.log must record the confirm and the deny by their exact
        # decision tags. Newest sandbox-*.log created at/after this run's start.
        audit_dir = H.state_dir(env) / "audit"
        audit_candidates = sorted(
            (p for p in audit_dir.glob("sandbox-*.log") if p.stat().st_mtime >= start - 1),
            key=lambda p: p.stat().st_mtime,
        )
        audit_text = audit_candidates[-1].read_text() if audit_candidates else ""

        # ---- 1) The ceremony must hold, independent of the golden. ----
        invariants = [
            (phase1_rc == 0, "PHASE1: rein declare --new must succeed once the human approves"),
            (filed_issue is not None, "PHASE1 must report the GitHub-assigned issue number (@FILED_ISSUE)"),
            (prompts == 2, f"exactly two new-issue Form A prompts (approve + deny), got {prompts}"),
            (got_title == TITLE, f"the filed issue's title must be the EXACT proposal: got {got_title!r}"),
            (bool(author.get("is_bot")), f"the filed issue's author must be the App's BOT identity: got {author!r}"),
            (got_body.endswith(want_trailer), f"the filed issue's body must end with the attribution trailer: got {got_body!r}"),
            (push_rc == 0, f"PHASE2: the push to {pushed_branch} must LAND (proves the filed issue IS the run's declared issue)"),
            (branch_pushed, f"the pushed branch {pushed_branch} must actually EXIST at GitHub"),
            (view_rc == 0, "PHASE3: the in-sandbox read (`gh issue view`) of the filed issue must succeed"),
            (phase4_rc not in (None, 0), "PHASE4: rein declare --new answered with the WRONG word must be REFUSED"),
            ("was NOT confirmed" in text and "Nothing was filed" in text,
             "PHASE4's refusal must say plainly that nothing was filed"),
            ("decision=confirmed-new-issue" in audit_text,
             "the run's audit log must record confirmed-new-issue"),
            ("decision=refused-declare-new-denied" in audit_text,
             "the run's audit log must record refused-declare-new-denied"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if not result.reached_eof:
            broken.append("the sandbox step did not run to EOF (timed out / prompt missed)")
        if broken:
            print("CEREMONY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  phase1_rc={phase1_rc} push_rc={push_rc} view_rc={view_rc} phase4_rc={phase4_rc} "
                  f"prompts={prompts} filed_issue={filed_issue} title_match={got_title == TITLE} "
                  f"author={author} branch_pushed={branch_pushed}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # ---- 2) Compare the WHOLE captured session NORMALIZED. ----
        raw = result.transcript
        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  rein declare --new (approve): rc={phase1_rc}  (Form A prompts fired: {prompts})", flush=True)
        print(f"  GitHub ground truth: issue #{filed_issue} filed, title matches, author={author}", flush=True)
        print(f"  body ends with attribution trailer: {got_body.endswith(want_trailer)}", flush=True)
        print(f"  push to {pushed_branch}: rc={push_rc} (branch exists at GitHub: {branch_pushed})", flush=True)
        print(f"  gh issue view (in-sandbox read): rc={view_rc}", flush=True)
        print(f"  rein declare --new (wrong word): rc={phase4_rc} (refused, nothing filed)", flush=True)
        print(f"  audit log: confirmed-new-issue + refused-declare-new-denied both present: "
              f"{'decision=confirmed-new-issue' in audit_text and 'decision=refused-declare-new-denied' in audit_text}",
              flush=True)

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
        scratch = os.path.join(tempfile.gettempdir(), "declare_new.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        if pushed_branch:
            H.delete_branch(repo, pushed_branch, env)
        if filed_issue is not None:
            H.close_issue(repo, filed_issue, env, comment="journey complete; closing.")
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        print("cleanup: branch deleted"
              + (f" ({pushed_branch})" if pushed_branch else " (none pushed)")
              + ("; issue closed" if filed_issue is not None else "; no issue filed")
              + "; checkout removed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
