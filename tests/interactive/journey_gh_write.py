"""journey_gh_write — THE gh/API WRITE CEREMONY, in-sandbox (declare -> confirm -> gh write LANDS).

This is ONE journey. For what a journey IS, the golden-transcript rule, and how to
author the next one, read tests/interactive/CLAUDE.md — none of that lives here.

Where journey_write_ceremony shows the GIT-push boundary and journey_sandbox_filesystem
shows the FILESYSTEM boundary, THIS journey shows the gh / REST-API WRITE boundary — the
thing the sandbox-gh-scopes fix is ABOUT. It is the direct proof of that fix: the exact
`gh` write it exercises could NOT succeed before the fix and DOES after it.

The whole story is the GAP between two attempts at the SAME `gh` write, one terminal:

  * the AGENT's view (in-sandbox: no tty, no token) — a `gh api -X POST .../comments`
    issue-comment write BEFORE any declaration, DENIED by rein's declare gate (a local
    HTTP 403 carrying "rein: no issue declared for this run" — nothing minted, GitHub
    never contacted); then `rein declare <n>`; then the SAME gh write, which now LANDS
    (HTTP 201, the comment body echoed back); and
  * the HUMAN's view (host tty) — the Form A prompt carrying the issue title/state/HOME
    repo FETCHED from GitHub, then `[approved]`.

Before the fix, the sandbox proxy injected a CONTENTS-ONLY token into every github
request — so `git push` landed but every `gh`/REST/GraphQL issue-or-PR write 403'd with
'Resource not accessible by integration', falsifying the injected contract's promise
that approving covers ALL writes. The fix wires the sandbox proxy's write tier to the
implement-role token (contents+issues+pull_requests write), so the post-declare gh
write lands. THIS journey is the regression proof: a green run means the contract is
true in-sandbox.

DELIVERABLE: a RAW, human-reviewable transcript at golden/gh_write.txt — real repo, real
issue title, the real rein 403 text, the real 201 echo — so Tom SEES the deny-before /
lands-after contrast. Determinism does NOT live in the file: a fresh run is compared to
the golden by normalizing BOTH sides first (reinharness.compare_golden), so a different
issue number still matches while a genuinely new or changed line trips drift.

    python3 tests/interactive/journey_gh_write.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_gh_write.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_gh_write.py # also print the compare lens
    REIN_DEMO_ISSUE=<n>  python3 tests/interactive/journey_gh_write.py   # reuse an issue

Exit 0 = the ceremony held AND the normalized transcript matches the golden. Exit 1 =
drift (RAW fresh transcript dropped to a scratch path; NORMALIZED diff printed). Exit 2 =
the ceremony itself broke (deny-before failed to deny, or lands-after failed to land).

SELF-CONTAINED: creates its own throwaway issue, clones the throwaway for the writable
checkout, and in a `finally` DELETES the posted comment (host-side, verified first) and
closes the issue. Touches only the throwaway (hard-constraint #1). The repo is resolved
the rein-init way (reinharness.resolve_throwaway_repo).
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "gh_write.txt"
ISSUE_ENV = "REIN_DEMO_ISSUE"

# The comment body the sandboxed `gh` write posts. Fixed (not per-run) so it is
# stable across runs — the golden echoes it back verbatim and the host-side
# verification matches on it. Each run uses its OWN fresh issue, so there is no
# cross-run collision.
COMMENT_BODY = (
    "rein gh-write journey: this comment was posted by a SANDBOXED `gh` after "
    "declare+approve. If you can read it, in-sandbox issue/PR writes work. "
    "Safe to delete."
)


# --------------------------------------------------------------------------
# The in-sandbox agent script — a deterministic bash "agent", every step tagged
# --------------------------------------------------------------------------


def gh_write_script(repo: str, issue: int) -> str:
    """A `bash -c` body run as the srt child. It cannot be puppeted line-by-line
    (one sandboxed process), so each STEP emits a tagged `@PHASE..`/`@..._RC`
    sentinel and the test asserts on those IN SEQUENCE. Commands go through `run`
    (reinharness.sandbox_preamble): it echoes each as `SBX| $ <command>` then
    tags its output, so the transcript reads like a real terminal.

    The SAME `gh api -X POST .../comments` write runs twice — once BEFORE declare
    (must be denied by rein's declare gate) and once AFTER (must land). The
    after-write uses `--jq .body` so the golden shows a STABLE proof line (the
    comment body we control) instead of the volatile created-comment JSON
    (id/urls/timestamps). `cd "$0"` enters the writable checkout mount.
    """
    api_path = f"/repos/{repo}/issues/{issue}/comments"
    return f"""
{H.sandbox_preamble()}
cd "$0"

emit "@PHASE1_START  gh issue-comment write BEFORE declare (expect: rein declare-gate 403)"
run gh api -X POST {api_path} -f body={COMMENT_BODY!r}
emit "@BEFORE_RC=$?"

emit "@PHASE2_START  rein declare {issue} (blocks for the human on the host tty)"
run rein declare {issue}
emit "@DECLARE_RC=$?"

emit "@PHASE3_START  the SAME gh write AFTER declare (expect: HTTP 201, comment lands)"
run gh api -X POST {api_path} -f body={COMMENT_BODY!r} --jq .body
emit "@AFTER_RC=$?"
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# Host-side setup / teardown
# --------------------------------------------------------------------------


def clone_checkout(repo: str, env: dict) -> str:
    """A fresh normal checkout whose .git is a real dir -> fully hardenable. Named
    with a `rein-` prefix so its /tmp path normalizes to <TMP> in the compare.
    Cloned host-side so no clone chatter enters the sandbox transcript (the golden
    is just the banner + contract + the three gh/declare phases)."""
    d = tempfile.mkdtemp(prefix="rein-ghwrite-")
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
        f.write("id: sess_journey_gh_write\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def _rc(text: str, name: str) -> int | None:
    m = re.search(rf"@{name}_RC=(\d+)", text)
    return int(m.group(1)) if m else None


def drive_journey(env, repo, issue, workdir):
    """Drive the ONE sandboxed `rein run` through the shared runner (#82). The
    sandbox launch is a normal JourneyStep whose argv is the full `rein run --
    bash -c <script> <workdir>`; the Form-A declare prompt is answered inline via
    the step's `answers` (type the DISPLAYED issue number), exactly like any other
    step's prompts. run_journey captures the COMPLETE session (banner, injected
    contract, every tagged agent line) as `.transcript`, with no hand-slicing."""
    step = H.JourneyStep(
        argv=["run", "--", "bash", "-c", gh_write_script(repo, issue), workdir],
        # rein re-echoes the full script right below its banner, so keep the
        # boundary line concise instead of dumping the whole bash body twice.
        label=f"rein run -- bash -c <sandbox gh-write script> {workdir}",
        answers=[(H.PROMPT_HINT, str(issue))],  # the host-tty Form A declare prompt
        extra_env={
            "REIN_SESSION_FILE": _pinned_session(repo),
            "REIN_SANDBOX_WORKDIR": workdir,
        },
        timeout=180,
    )
    result = H.run_journey([step], env=env)
    return result, result.steps[0].text


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)

    supplied = os.getenv(ISSUE_ENV)
    ours = not supplied
    if supplied:
        issue = int(supplied)
    else:
        issue = H.create_issue(
            repo,
            "rein journey: gh-write walkthrough (safe to close)",
            "Opened by tests/interactive/journey_gh_write.py to prove that a SANDBOXED "
            "`gh` issue-comment write is denied before declare and LANDS after "
            "declare+approve. Throwaway repo only; closed again when the journey ends.",
            env,
        )

    print(f"journey: gh-write ceremony on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'})", flush=True)

    workdir = None
    posted_comment_ids: list[int] = []
    try:
        workdir = clone_checkout(repo, env)
        result, text = drive_journey(env, repo, issue, workdir)

        before_rc = _rc(text, "BEFORE")
        declare_rc = _rc(text, "DECLARE")
        after_rc = _rc(text, "AFTER")
        prompts = text.count(H.PROMPT_BANNER)

        # Host-side GROUND TRUTH: did the comment actually post at GitHub? (The
        # in-sandbox RC only says gh's request succeeded; this proves the write
        # is real.) Deny-before must have posted NOTHING; lands-after exactly one.
        comments = H.list_issue_comments(repo, issue, env)
        ours_comments = [c for c in comments if c["body"] == COMMENT_BODY]
        posted_comment_ids = [c["id"] for c in ours_comments]

        # ---- 1) The ceremony must hold, independent of the golden. ----
        invariants = [
            (before_rc not in (None, 0),
             "the gh write BEFORE declare must FAIL (rein's declare gate 403)"),
            ("rein: no issue declared for this run" in text,
             "the deny-before must carry rein's declare-gate message"),
            (declare_rc == 0, "rein declare must succeed after confirmation"),
            (prompts == 1, "exactly one Form A prompt for the run"),
            (after_rc == 0, "the SAME gh write AFTER declare must LAND (HTTP 201)"),
            (COMMENT_BODY in text,
             "the after-write must echo the posted comment body back (proof it landed)"),
            (len(ours_comments) == 1,
             f"exactly ONE comment must exist at GitHub (found {len(ours_comments)}): "
             "deny-before posted nothing, lands-after posted once"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if not result.reached_eof:
            broken.append("the sandbox step did not run to EOF (timed out / prompt missed)")
        if broken:
            print("CEREMONY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  before_rc={before_rc} declare_rc={declare_rc} after_rc={after_rc} "
                  f"prompts={prompts} comments_at_github={len(ours_comments)}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # ---- 2) Compare the WHOLE captured session NORMALIZED. ----
        raw = result.transcript
        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  gh write BEFORE declare: rc={before_rc} (denied by rein's declare gate)", flush=True)
        print(f"  rein declare: rc={declare_rc}  (Form A prompts fired: {prompts})", flush=True)
        print(f"  gh write AFTER declare: rc={after_rc} (LANDED)", flush=True)
        print(f"  GitHub ground truth: {len(ours_comments)} matching comment(s) on issue "
              f"#{issue} (ids={posted_comment_ids})", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN_NAME, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, raw)
        if ok:
            print("[golden OK] fresh run matches golden/gh_write.txt (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "gh_write.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print("[golden DRIFT] fresh run != golden/gh_write.txt (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        # Delete the comment the sandbox posted (host-side; verified above), then
        # clean the checkout and close the issue. Hard-constraint #1: throwaway only.
        for cid in posted_comment_ids:
            H.delete_issue_comment(repo, cid, env)
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        if ours:
            H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print(f"cleanup: {len(posted_comment_ids)} comment(s) deleted; checkout removed"
              + ("; issue closed" if ours else ""), flush=True)


if __name__ == "__main__":
    sys.exit(main())
