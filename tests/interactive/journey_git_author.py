"""journey_git_author — THE DELEGATED COMMIT AUTHOR: "<name> (via rein)" (CP4/B8).

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

rein does NOT let a sandboxed agent impersonate the developer. When the agent
commits, rein stamps a NON-IMPERSONATING identity into
GIT_AUTHOR_*/GIT_COMMITTER_* (internal/srt/env.go BuildEnv, from
internal/gitidentity): the name is the developer's name MARKED "(via rein)"
(internal/gitidentity.DefaultNameTemplate = "{name} (via rein)") and the email is
the App's bot NOREPLY address (`<bot-user-id>+<app-slug>[bot]@users.noreply.
github.com`), NEVER the developer's real email. So a landed commit is honestly
attributable to the human behind the agent AND linked to rein's App — without
claiming to BE the developer.

The existing sandbox journeys COMMIT but only print the subject (`git log
--oneline`); none asserts WHO authored. This journey closes that gap: the
in-sandbox agent runs `git log -1 --format='%an <%ae>'` (AUTHOR) and `'%cn <%ce>'`
(COMMITTER) so both delegated identities appear verbatim in the golden, and —
after the push lands — the HOST asserts the same author AND committer on the
pushed commit via the GitHub API, and that neither is the developer's own git
identity.

WHY SANDBOX MODE (not --direct): the "(via rein)" stamping is a SANDBOX property.
Direct mode (`rein run --direct`) deliberately layers the developer's real
`~/.gitconfig` (run.go include.path) and does NOT set GIT_AUTHOR_*, so a
direct-mode commit authors as the DEVELOPER. Only the sandboxed path resolves and
stamps the delegated identity, so this journey runs sandboxed.

CAPTURE IS STRUCTURAL (#82/#85): the run is one run_journey STEP; the runner
captures the COMPLETE pty session. The `@AUTHOR …` sentinel line is ordinary
tagged agent output, so it lands in the golden with the real identity; the
host-side API assertion is an outcome (exit 2 on failure), echoed but not baked
into the golden. Determinism lives in the COMPARATOR (normalize-on-compare); the
developer name + bot email are stable-by-construction on a machine, kept RAW.

DELIVERABLE: `golden/git_author.txt`.

    python3 tests/interactive/journey_git_author.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_git_author.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_git_author.py # also print the compare lens
    REIN_DEMO_ISSUE=<n>  python3 tests/interactive/journey_git_author.py    # reuse an issue

Exit 0 = the delegated author held AND the normalized transcript matches the
golden. Exit 1 = drift. Exit 2 = the identity itself was wrong (impersonation, or
a landed author that is not the delegated one).

SELF-CONTAINED: creates its own throwaway issue via gh and, in a `finally`,
deletes the branch and closes the issue. Touches only the throwaway
(hard-constraint #1).
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

import reinharness as H

GOLDEN_NAME = "git_author.txt"
ISSUE_ENV = "REIN_DEMO_ISSUE"


def author_script(repo: str, issue: int, good: str) -> str:
    """A `bash -c` body run as the srt child. Clone, declare (blocks for the
    human), commit a probe, print the AUTHOR rein stamped, then push. Each step
    emits a tagged `@PHASE..` sentinel; `git log -1 --format='%an <%ae>'` is
    surfaced as a tagged `@AUTHOR …` line so the delegated identity is visible in
    the golden AND parseable by the test.

    The CLONE omits `--progress` on purpose: a piped `--progress` clone's
    `remote: Total …` line races the local `Receiving/Resolving …` lines and
    reorders run-to-run (git nondeterminism normalize-on-compare can't reorder
    away); without it git just prints `Cloning into 'repo'…` and `@CLONE_OK`
    proves the read. The PUSH keeps `--progress` (stably ordered).
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"
rm -rf repo
run git clone https://github.com/{repo} repo
cd repo || {{ emit "@CLONE_FAIL"; exit 3; }}
emit "@CLONE_OK  (read flows with no declaration)"

emit "@PHASE1_START  rein declare {issue} (blocks for the human)"
run rein declare {issue}
emit "@PHASE1_RC=$?"

emit "@PHASE2_START  commit a probe (rein stamps GIT_AUTHOR_* AND GIT_COMMITTER_* = the delegated identity)"
echo "git-author probe $(date -u +%FT%TZ)" >> author-probe.txt
run git add -A
run git commit -q -m "git-author journey: delegated authorship"
emit "@PHASE2_RC=$?"
al=$(git log -1 --format='%an <%ae>')
emit "@AUTHOR $al"
cl=$(git log -1 --format='%cn <%ce>')
emit "@COMMITTER $cl"

emit "@PHASE3_START  push agent/{issue}/<nonce> (expect: lands)"
run git push --progress origin HEAD:refs/heads/{good}
emit "@PHASE3_RC=$?"
emit "@SCRIPT_DONE"
"""


def _pinned_session(repo: str) -> str:
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_gitauthor\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def _rc(step_text: str, phase: int) -> int | None:
    m = re.search(re.escape(H.SBX_TAG) + rf"@PHASE{phase}_RC=(\d+)", step_text)
    return int(m.group(1)) if m else None


def _sandbox_identity(step_text: str, tag: str) -> tuple[str | None, str | None]:
    """Pull (name, email) from an in-sandbox `@<tag> <name> <<email>>` line
    (tag is AUTHOR or COMMITTER)."""
    m = re.search(re.escape(H.SBX_TAG) + rf"@{tag} (.+?)\s*<([^>]+)>", step_text)
    return (m.group(1).strip(), m.group(2).strip()) if m else (None, None)


def _host_commit_identity(repo: str, branch: str, env: dict):
    """HOST-side ground truth: the pushed commit's AUTHOR and COMMITTER
    name+email via the operator's own gh (never the sandbox). Resolves the ref to
    a SHA, then reads the git commit object. Returns
    (author_name, author_email, committer_name, committer_email); all None on any
    API failure (so the caller reports a clean 'host fetch failed' rather than a
    spurious identity mismatch)."""
    try:
        sha = subprocess.check_output(
            ["gh", "api", f"repos/{repo}/git/refs/heads/{branch}", "--jq", ".object.sha"],
            text=True, env=env,
        ).strip()
        out = subprocess.check_output(
            ["gh", "api", f"repos/{repo}/git/commits/{sha}",
             "--jq", '[.author.name, .author.email, .committer.name, .committer.email]|join("\\t")'],
            text=True, env=env,
        ).strip()
        an, ae, cn, ce = out.split("\t", 3)
        return an, ae, cn, ce
    except Exception:
        return None, None, None, None


def _dev_identity() -> tuple[str, str]:
    """The DEVELOPER's own git identity (host `git config`) — what the delegated
    author must NOT be."""
    def cfg(key):
        try:
            return subprocess.check_output(["git", "config", "--get", key], text=True).strip()
        except Exception:
            return ""
    return cfg("user.name"), cfg("user.email")


def _bot_cache_email(env: dict) -> str | None:
    """The resolved App-bot noreply email rein caches (bot-identity.json). Present
    on a box whose git identity has been resolved once; used to assert the landed
    email is exactly the resolved bot address."""
    cfg = env.get("XDG_CONFIG_HOME") or os.path.join(env.get("HOME", str(Path.home())), ".config")
    p = Path(cfg) / "rein" / "bot-identity.json"
    if p.exists():
        try:
            return json.loads(p.read_text()).get("email")
        except Exception:
            return None
    return None


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)

    supplied = os.getenv(ISSUE_ENV)
    ours = not supplied
    if supplied:
        issue = int(supplied)
    else:
        issue = H.create_issue(
            repo,
            "rein journey: git-author walkthrough (safe to close)",
            "Opened by tests/interactive/journey_git_author.py to demonstrate the "
            "CP4 delegated commit author '<name> (via rein)' + App-bot noreply email. "
            "Throwaway repo only; closed when the journey ends.",
            env,
        )

    good = f"agent/{issue}/{H.unique_branch('author')}"
    wd = H.make_workdir()
    session = _pinned_session(repo)
    script = author_script(repo, issue, good)
    dev_name, dev_email = _dev_identity()
    bot_email = _bot_cache_email(env)

    print(f"journey: git author on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'}); developer git id = "
          f"{dev_name!r} <{dev_email}>", flush=True)

    try:
        result = H.run_journey(
            [
                H.JourneyStep(
                    argv=["run", "--", "bash", "-c", script, wd],
                    answers=[(H.PROMPT_HINT, str(issue))],
                    label="rein run -- bash -c <git-author-ceremony> " + wd,
                    timeout=240,
                ),
            ],
            env=env,
            extra_env={
                "REIN_SESSION_FILE": session,
                "REIN_APPROVAL": "tty",
                # Bind the workdir as the sandbox's writable working tree. Unlike
                # spawn_rein_run, run_journey's spawn_rein does NOT auto-export
                # this, so a sandbox step must set it (the sandbox_filesystem
                # exemplar does the same) or rein binds an empty ephemeral tree.
                "REIN_SANDBOX_WORKDIR": wd,
            },
        )
        text = result.transcript
        step_text = result.steps[0].text if result.steps else ""
        rc1, rc2, rc3 = (_rc(step_text, 1), _rc(step_text, 2), _rc(step_text, 3))
        prompts = step_text.count(H.PROMPT_BANNER)
        sbx_name, sbx_email = _sandbox_identity(step_text, "AUTHOR")
        sbx_cname, sbx_cemail = _sandbox_identity(step_text, "COMMITTER")
        landed = H.branch_exists(repo, good, env)
        host_name, host_email, host_cname, host_cemail = (
            _host_commit_identity(repo, good, env) if landed else (None, None, None, None))
        host_fetched = host_name is not None

        bot_re = re.compile(r"^\d+\+.+\[bot\]@users\.noreply\.github\.com$")
        expected = f"{dev_name} (via rein)"  # the delegated name, inline (CLAUDE.md)
        # 1) The delegated identity must hold — independent of the golden (exit 2).
        #    rein stamps BOTH author and committer (env.go), so assert both.
        invariants = [
            (result.reached_eof, "the run must complete (no missed prompt / timeout)"),
            (rc1 == 0, "phase 1 (declare) must succeed after confirmation"),
            (rc2 == 0, "phase 2 (commit) must succeed"),
            (rc3 == 0, "phase 3 (verified push) must succeed"),
            (prompts == 1, "exactly one Form A prompt for the run"),
            (landed is True, "the branch must LAND on GitHub"),
            # in-sandbox AUTHOR (what git recorded locally, from GIT_AUTHOR_*)
            (sbx_name is not None and sbx_name.endswith("(via rein)"),
             "in-sandbox author name is delegated ('… (via rein)')"),
            (bool(dev_name) and sbx_name == expected,
             f"in-sandbox author name is the developer's name marked (via rein): '{expected}'"),
            (sbx_email is not None and bool(bot_re.match(sbx_email)),
             "in-sandbox author email is an App-bot noreply (<id>+<slug>[bot]@users.noreply.github.com)"),
            # in-sandbox COMMITTER (from GIT_COMMITTER_*) — same delegated identity
            (bool(dev_name) and sbx_cname == expected,
             f"in-sandbox committer name is also the delegated '{expected}'"),
            (sbx_cemail == sbx_email,
             "in-sandbox committer email equals the author email (both the App-bot noreply)"),
            # NOT the developer (author AND committer)
            (sbx_name != dev_name and sbx_cname != dev_name,
             "author/committer name is NOT the bare developer name (non-impersonating)"),
            (bool(dev_email) and sbx_email != dev_email and sbx_cemail != dev_email,
             "author/committer email is NOT the developer's real email"),
            # host-side ground truth on the PUSHED commit: fetch succeeded, THEN matches
            (host_fetched, "host-side fetch of the pushed commit succeeded (GitHub API)"),
            (host_fetched and host_name == sbx_name and host_email == sbx_email,
             "the PUSHED commit's AUTHOR (GitHub API) matches the delegated identity"),
            (host_fetched and host_cname == sbx_cname and host_cemail == sbx_cemail,
             "the PUSHED commit's COMMITTER (GitHub API) matches the delegated identity"),
        ]
        if bot_email:
            invariants.append(
                (sbx_email == bot_email,
                 f"author email is exactly the resolved App-bot address ({bot_email})"))
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("GIT-AUTHOR BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  rc1={rc1} rc2={rc2} rc3={rc3} prompts={prompts} landed={landed}", flush=True)
            print(f"  sandbox author:    {sbx_name!r} <{sbx_email}>", flush=True)
            print(f"  sandbox committer: {sbx_cname!r} <{sbx_cemail}>", flush=True)
            print(f"  host author:       {host_name!r} <{host_email}>", flush=True)
            print(f"  host committer:    {host_cname!r} <{host_cemail}>", flush=True)
            print(f"  developer id:      {dev_name!r} <{dev_email}>", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # 2) The golden IS the complete captured session (the @AUTHOR line shows
        #    the delegated identity RAW). Outcomes asserted above, echoed here.
        print()
        print(text, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  delegated author    (in-sandbox): {sbx_name!r} <{sbx_email}>", flush=True)
        print(f"  delegated committer (in-sandbox): {sbx_cname!r} <{sbx_cemail}>", flush=True)
        print(f"  same on the PUSHED commit (GitHub API): author {host_name!r} <{host_email}>; "
              f"committer {host_cname!r} <{host_cemail}>", flush=True)
        print(f"  developer git identity (NOT this): {dev_name!r} <{dev_email}>", flush=True)
        print(f"  branch {good}: {'LANDED' if landed else 'ABSENT'}", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(text), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN_NAME, text)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, text)
        if ok:
            print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "git_author.fresh.txt")
        with open(scratch, "w") as f:
            f.write(text)
        print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        H.delete_branch(repo, good, env)
        if ours:
            H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print("cleanup: branch deleted" + ("; issue closed" if ours else ""), flush=True)


if __name__ == "__main__":
    sys.exit(main())
