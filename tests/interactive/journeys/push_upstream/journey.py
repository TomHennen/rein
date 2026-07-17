"""push_upstream — a sandboxed `git push -u` reads CLEAN, and rein sets the
upstream on the operator's real checkout (#102 part 2 / #119).

The demo that surfaced #102 showed a real agent's `git push -u` printing
`could not write config file .git/config: Device or resource busy` and having to
explain it away: -u writes branch.<x>.remote/merge into .git/config, which #64
pins READ-ONLY in the sandbox. This journey is the reviewable proof that the push
now reads normal — the golden is the agent's own view of a clean push.

WHY THIS JOURNEY BINDS A HOST-SIDE HARDENED CHECKOUT (and doesn't clone in the
sandbox like write_ceremony): the bug only exists when .git/config is the #64
read-only bind. A clone made INSIDE the sandbox's writable mount has a WRITABLE
.git/config, so -u succeeds there regardless of the fix — a golden built that way
would be green whether or not the fix works (the exact trap #119 calls out). So
we clone HOST-side and let rein bind it hardened (spawn_rein_run exports it as
REIN_SANDBOX_WORKDIR; `cd "$0"` lands the in-sandbox script in it).

PROVE-IT-RAN: git only writes the upstream config on a SUCCESSFUL push, so a clean
transcript is only meaningful once the push LANDED. The invariants assert the tree
was a real .git dir, the push rc==0, and the branch reached the remote BEFORE the
golden's no-fault reading counts. The tracking-set half (host-side, post-run) is
asserted from rein's helper.log — it is a file log, not terminal output, so it is
NOT in the golden.

See CLAUDE.md for the golden-transcript rules. Self-contained: creates its own
throwaway issue + branch and cleans both up. Throwaway repo only (constraint #1).
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile

import pexpect

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"
ISSUE_ENV = "REIN_DEMO_ISSUE"

# The read-only-.git/config fault the fix must keep out of the agent's view.
_EBUSY_MARKERS = (
    "could not write config file .git/config",
    "Device or resource busy",
)


def _clone_hardened(repo: str, env: dict) -> str:
    """A HOST-side normal checkout (real .git dir -> rein binds it hardened).
    origin is forced to the plain https URL so the in-sandbox push routes through
    rein's injecting proxy."""
    d = tempfile.mkdtemp(prefix="rein-push-upstream-")
    subprocess.run(["gh", "repo", "clone", repo, d, "--", "-q"],
                   check=True, env=env, capture_output=True, text=True)
    subprocess.run(["git", "-C", d, "remote", "set-url", "origin", f"https://github.com/{repo}.git"],
                   check=True, env=env, capture_output=True, text=True)
    return d


def upstream_script(issue: int, good: str) -> str:
    """A `bash -c` body run as the srt child in the BOUND checkout ($0). Each step
    emits a tagged @PHASE.. sentinel so the run reads expect->act->expect; `run`
    (sandbox_preamble) echoes each command as `SBX| $ …` and tags its output. The
    push passes --progress so git's real chatter is captured (counts normalized at
    compare time)."""
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@GITKIND=$(test -d .git && echo dir || echo other)"
run git checkout -q -b {good}
echo "push-upstream probe" >> probe-upstream.txt
run git add -A
run git commit -q -m "push-upstream: agent commit"

emit "@DECLARE_START  rein declare {issue} (blocks for the human)"
run rein declare {issue}
emit "@DECLARE_RC=$?"

emit "@PUSH_START  git push -u agent/{issue}/<nonce> (expect: lands, no config-file fault)"
run git push -u --progress origin HEAD:refs/heads/{good}
emit "@PUSH_RC=$?"
emit "@SCRIPT_DONE"
"""


def _rc(child_match) -> int:
    return int(child_match.group(1))


def run_journey(env, repo, issue):
    """Drive the live run; return (transcript, workdir, decl_rc, push_rc, prompts, landed, good)."""
    good = f"agent/{issue}/{H.unique_branch('up')}"
    workdir = _clone_hardened(repo, env)
    session = _pinned_session(repo)
    run = H.spawn_rein_run(
        ["bash", "-c", upstream_script(issue, good)], workdir=workdir, env=env,
        extra_env={"REIN_SESSION_FILE": session},
    )

    decl_rc = push_rc = None
    try:
        run.child.expect(r"@GITKIND=\w+", timeout=180)
        run.child.expect(r"@DECLARE_START", timeout=60)
        # the declare BLOCKS -> the Form A prompt fires on the host tty
        run.expect_prompt(timeout=120)
        run.answer(str(issue))
        run.expect_approved(timeout=60)
        run.child.expect(r"@DECLARE_RC=(\d+)", timeout=60)
        decl_rc = _rc(run.child.match)

        run.child.expect(r"@PUSH_START", timeout=30)
        run.child.expect(r"@PUSH_RC=(\d+)", timeout=120)
        push_rc = _rc(run.child.match)

        run.child.expect(r"@SCRIPT_DONE", timeout=60)
        run.wait(timeout=120)
    except (pexpect.EOF, pexpect.TIMEOUT):
        # Partial rcs -> main()'s invariant check fails -> exit 2, with the transcript.
        pass
    finally:
        try:
            run.child.close(force=True)
        except Exception:
            pass

    prompts = run.prompt_count()
    landed = H.branch_exists(repo, good, env)
    return run.text(), workdir, decl_rc, push_rc, prompts, landed, good


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
            repo, "rein journey: push -u upstream (safe to close)",
            "Opened by journeys/push_upstream/journey.py to demonstrate that a "
            "sandboxed `git push -u` reads clean and rein sets the upstream on the "
            "real checkout (#102/#119). Throwaway repo only; closed when the journey ends.",
            env,
        )

    print(f"journey: push -u upstream on {repo}, issue #{issue} "
          f"({'created' if ours else 'supplied'})", flush=True)

    good = None
    try:
        text, workdir, decl_rc, push_rc, prompts, landed, good = run_journey(env, repo, issue)

        # Host-side ground truth: rein set the tracking on the REAL checkout, and
        # rein's forensic log recorded it (helper.log is a file, not in the golden).
        cfg = lambda k: subprocess.run(
            ["git", "-C", workdir, "config", "--get", k], env=env,
            capture_output=True, text=True).stdout.strip()
        tracked_remote = cfg(f"branch.{good}.remote")
        tracked_merge = cfg(f"branch.{good}.merge")
        helper_log = H.helper_log_path(env).read_text() if H.helper_log_path(env).exists() else ""
        no_ebusy = not any(m in text for m in _EBUSY_MARKERS)

        # 1) Invariants — independent of the golden.
        invariants = [
            ("@GITKIND=dir" in text, "workdir must be a real .git-dir checkout (bound, hardened)"),
            (decl_rc == 0, "declare must succeed after confirmation"),
            (push_rc == 0, "git push -u must SUCCEED (rc=0)"),
            (prompts == 1, "exactly one Form A prompt for the run"),
            (landed is True, "the agent branch must LAND on the remote"),
            (no_ebusy, "the agent must NOT see the read-only .git/config fault"),
            (tracked_remote == "origin", "rein must set branch.<x>.remote=origin on the real checkout"),
            (tracked_merge == f"refs/heads/{good}", "rein must set branch.<x>.merge on the real checkout"),
            (f"git upstream: set branch.{good}" in helper_log,
             "helper.log must record rein setting the upstream"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("JOURNEY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  decl_rc={decl_rc} push_rc={push_rc} prompts={prompts} landed={landed} "
                  f"remote={tracked_remote!r} merge={tracked_merge!r} no_ebusy={no_ebusy}", flush=True)
            return 2

        # 2) Build the RAW transcript and compare NORMALIZED.
        raw = H.build_raw_transcript(text)
        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  declare rc={decl_rc}; push -u rc={push_rc}; Form A prompts={prompts}", flush=True)
        print(f"  branch {good}: {'LANDED' if landed else 'ABSENT'}", flush=True)
        print(f"  no .git/config fault in the agent's view: {no_ebusy}", flush=True)
        print(f"  real checkout tracking: {tracked_remote}/{tracked_merge}", flush=True)
        print("  helper.log recorded the upstream-set", flush=True)

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
        scratch = os.path.join(tempfile.gettempdir(), "push_upstream.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        if good:
            H.delete_branch(repo, good, env)
        if ours:
            H.close_issue(repo, issue, env, comment="journey complete; closing.")
        print("cleanup: branch deleted" + ("; issue closed" if ours else ""), flush=True)


def _pinned_session(repo: str) -> str:
    """A temp repo-only session, so the journey never depends on the machine's
    ambient dev-session.yaml."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_push_upstream\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


if __name__ == "__main__":
    sys.exit(main())
