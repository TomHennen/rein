"""reinharness — pexpect-based harness for rein's interactive/tty flows.

WHY THIS EXISTS
---------------
rein's write-approval prompt opens ``/dev/tty`` DIRECTLY (internal/ui/prompt)
and reads a single line; it approves iff the trimmed line equals the session's
issue number. It does NOT read stdin. So the only way to drive it from a test
is to give the ``rein run`` process a *controlling terminal* — which is exactly
what a pty (via pexpect) provides. pexpect stands in for the human approver.

This is legitimate and does not weaken the security model: the SANDBOXED agent
(srt ``--new-session``) has no tty at all, so it still cannot self-answer. What
pexpect drives is the HOST-side prompt on the terminal where ``rein run`` was
launched — the same terminal a real developer would type into.

WHAT THE HELPERS GIVE YOU
-------------------------
- ``rein_env()``        — the REIN_* environment (pre-sourced, or parsed from
                          ./dev-env). REIN_APP_* MUST stay present so ``rein
                          init`` never routes into the 25-minute manifest flow.
- ``build_binaries()``  — ``go build -o bin/ ./...`` + ``rein install-shim``.
- ``unique_branch()``   — a timestamped disposable branch name (throwaway repo).
- ``ReinRun``           — a pexpect wrapper around ``rein run`` with a captured
                          transcript, prompt matchers, and both-sides sentinels.
- ``branch_exists`` / ``delete_branch`` — HOST-side verification + cleanup via
                          the host's authed ``gh`` (the developer/operator, NOT
                          the sandbox).

Prereqs: a live throwaway repo + a working App (see README). ``python3`` +
``pexpect`` 4.9.0. The sandbox stack (srt/bwrap/socat/ripgrep) must be healthy.
"""

from __future__ import annotations

import contextlib
import io
import os
import re
import shutil
import subprocess
import tempfile
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path

import pexpect

# --------------------------------------------------------------------------
# Locations
# --------------------------------------------------------------------------

# reinharness.py lives at <repo>/tests/interactive/reinharness.py
REPO_ROOT = Path(__file__).resolve().parents[2]
DEV_ENV = REPO_ROOT / "dev-env"
REIN_BIN = REPO_ROOT / "bin" / "rein"


# --------------------------------------------------------------------------
# Observable strings from the Go code (matched, but always cross-checked
# against what actually appears — see the tests; the advisor's rule is
# "assert messages you observe, not ones you copied from source").
# --------------------------------------------------------------------------

# The distinctive first line of the Form A declaration prompt
# (internal/ui/prompt writePrompt, issue #35). Counting occurrences of this in
# a transcript == number of prompts.
PROMPT_BANNER = "agent declares work on an issue"
# The line that tells the human what to type (unchanged mechanism: type the
# DISPLAYED issue number).
PROMPT_HINT = "type the issue number"
# TTYPrompter.Confirm outcome markers (host tty).
APPROVED_MARK = "[approved]"
DENIED_MARK = "[denied"  # "[denied: input did not match the issue number]"
# The pre-declaration deny git prints after `fatal: remote error: ` (the
# proxy's synthesized pkt-line ERR advertisement, issue #35 §5.3).
PRE_DECLARE_LOCKED = "writes are locked until you declare your issue"
# The post-approval convention deny git prints as `! [remote rejected] ...`.
REF_CONVENTION_DENY = "refs must match agent/"
# The distinctive first line of the SCOPE EXPANSION prompt (issue #69,
# internal/ui/prompt writePrompt, AddRepo branch). Its presence means the
# declare was recognized as a REPO expansion, not a plain issue declaration.
EXPANSION_BANNER = "SCOPE EXPANSION requested"
# The line naming the repo the ceiling is about to grow by.
EXPANSION_ADD_REPO = "agent asks to ADD repo"
# TTYPrompter's expansion approval marker (distinct from the plain
# "[approved]" so a test can tell an expansion approval from an issue one).
EXPANSION_APPROVED_MARK = "[approved for this run]"
# The second (in-prompt persist) question the expansion prompt asks after a
# successful number match (Tom's decision, mocks §1.2).
PERSIST_QUESTION = "save"  # "Also save <repo> to the session for future runs? [y/N]"


def throwaway_repo_b(env: dict | None = None) -> str:
    """The owner/name of the SECOND throwaway repo (issue #69 scope
    expansion). Same owner as REIN_TEST_REPO_A — the App installation is
    single-owner, so an expansion target must share the owner.

    Hard-constraint #1: this is a throwaway too. It is created once via
    `gh repo create <owner>/agentcreds-validation-b --private` if absent.
    """
    env = env or rein_env()
    repo = env.get("REIN_TEST_REPO_B")
    if not repo:
        raise RuntimeError("REIN_TEST_REPO_B unset; source ./dev-env")
    return repo


# --------------------------------------------------------------------------
# Environment
# --------------------------------------------------------------------------


def rein_env() -> dict:
    """Return os.environ augmented with the REIN_* vars.

    If REIN_APP_ID is already exported (the caller sourced ./dev-env, which is
    what run.sh does), we use the live environment as-is. Otherwise we source
    ./dev-env in a subshell and merge the REIN_* vars it exports. Keeping the
    REIN_APP_* vars PRESENT is load-bearing for the init tests: with them set,
    `rein init` stays on the env-driven path and never reaches RunManifestFlow
    (a 25-minute browser/callback flow that would try to create a real App).
    """
    env = dict(os.environ)
    if env.get("REIN_APP_ID"):
        return env
    if not DEV_ENV.exists():
        raise RuntimeError(
            f"REIN_APP_ID not set and {DEV_ENV} not found; "
            "source ./dev-env before running (run.sh does this for you)."
        )
    # Capture the REIN_* vars ./dev-env exports, with $HOME etc. expanded.
    out = subprocess.check_output(
        ["bash", "-c", f"set -a; source {DEV_ENV!s}; env"],
        text=True,
    )
    for line in out.splitlines():
        if "=" not in line:
            continue
        k, v = line.split("=", 1)
        if k.startswith("REIN_"):
            env[k] = v
    if not env.get("REIN_APP_ID"):
        raise RuntimeError(f"sourcing {DEV_ENV} did not yield REIN_APP_ID")
    return env


def throwaway_repo(env: dict | None = None) -> str:
    """The owner/name of the throwaway repo (hard-constraint #1: touch ONLY this)."""
    env = env or rein_env()
    repo = env.get("REIN_TEST_REPO_A")
    if not repo:
        raise RuntimeError("REIN_TEST_REPO_A unset; source ./dev-env")
    return repo


# --------------------------------------------------------------------------
# Build
# --------------------------------------------------------------------------

_built = False


def build_binaries(env: dict | None = None) -> Path:
    """Build rein + shims once per process and install the shims. Returns bin/rein."""
    global _built
    env = env or rein_env()
    if not _built:
        subprocess.run(
            ["go", "build", "-o", "bin/", "./..."],
            cwd=REPO_ROOT,
            env=env,
            check=True,
        )
        subprocess.run(
            [str(REIN_BIN), "install-shim"],
            cwd=REPO_ROOT,
            env=env,
            check=True,
            stdout=subprocess.DEVNULL,
        )
        _built = True
    return REIN_BIN


# --------------------------------------------------------------------------
# Disposable branches
# --------------------------------------------------------------------------


def unique_branch(prefix: str = "reintest") -> str:
    """A disposable, clearly-timestamped branch name for the throwaway repo."""
    return f"{prefix}-{time.strftime('%Y%m%d-%H%M%S')}-{uuid.uuid4().hex[:6]}"


def branch_exists(repo: str, branch: str, env: dict | None = None) -> bool:
    """HOST-side: does <branch> exist on the throwaway? Uses the host's authed gh.

    This is the developer/operator verifying via their OWN GitHub token — NOT
    the sandboxed agent (which never holds a token). Legitimate host action.
    """
    env = env or rein_env()
    r = subprocess.run(
        ["gh", "api", f"repos/{repo}/git/refs/heads/{branch}"],
        cwd=REPO_ROOT,
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return r.returncode == 0


def delete_branch(repo: str, branch: str, env: dict | None = None) -> bool:
    """Best-effort HOST-side cleanup of a disposable branch via the host's gh."""
    env = env or rein_env()
    r = subprocess.run(
        ["gh", "api", "-X", "DELETE", f"repos/{repo}/git/refs/heads/{branch}"],
        cwd=REPO_ROOT,
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return r.returncode == 0


def list_issue_comments(repo: str, issue: int, env: dict | None = None) -> list[dict]:
    """HOST-side: the issue's comments as [{"id":int,"body":str}, ...] via the
    operator's OWN authed gh — the GROUND TRUTH a gh-write journey checks a
    sandboxed `gh` write against (did the comment actually post at GitHub?).
    Legitimate host action: the developer/operator, NOT the sandboxed agent.
    """
    env = env or rein_env()
    import json as _json

    out = subprocess.check_output(
        ["gh", "api", f"repos/{repo}/issues/{issue}/comments",
         "--jq", "[.[] | {id, body}]"],
        cwd=REPO_ROOT, env=env, text=True,
    ).strip()
    return _json.loads(out) if out else []


def delete_issue_comment(repo: str, comment_id: int, env: dict | None = None) -> bool:
    """Best-effort HOST-side cleanup of a single issue comment via the host's gh."""
    env = env or rein_env()
    r = subprocess.run(
        ["gh", "api", "-X", "DELETE", f"repos/{repo}/issues/comments/{comment_id}"],
        cwd=REPO_ROOT, env=env,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    return r.returncode == 0


# --------------------------------------------------------------------------
# In-sandbox script generation (the SANDBOX-side of the loop)
# --------------------------------------------------------------------------
#
# The script runs INSIDE srt. It clones the throwaway, commits, and pushes to a
# disposable branch. We deliberately use `set +e` around the push and echo an
# explicit `SBX_PUSHn_RC=<code>` sentinel so the test can assert the in-sandbox
# command's OWN outcome (blocks-then-succeeds on approval; fails cleanly on
# denial) — never a hang. `cd "$0"` enters the writable working-tree mount that
# `rein run` passes as the final arg.


def _push_block(n: int, repo: str, branch: str) -> str:
    return f"""
echo "SBX_PUSH{n}_START branch={branch}"
d{n}="probe-{branch}.txt"
echo "interactive-harness probe {branch} $(date -u +%FT%TZ)" >> "$d{n}"
git add -A
git commit -q -m "interactive harness: {branch}"
git push origin HEAD:refs/heads/{branch}
echo "SBX_PUSH{n}_RC=$?"
"""


def clone_and_push_script(repo: str, branches: list[str]) -> str:
    """A bash -c body: clone the throwaway once, then push each branch in turn.

    Multiple branches in ONE script == multiple writes in ONE `rein run`. With
    NO preceding declare this is the PRE-DECLARATION fixture: every push must
    fail with the proxy's synthesized `fatal: remote error: rein: ...` (#35).
    """
    body = [
        "set +e",  # capture push RC even on denial; never abort into a hang
        'cd "$0"',
        "rm -rf repo",
        f"git clone --depth 1 https://github.com/{repo} repo",
        'cd repo || { echo "SBX_CLONE_FAIL"; exit 3; }',
        "echo SBX_CLONE_OK",
    ]
    for i, br in enumerate(branches, start=1):
        body.append(_push_block(i, repo, br))
    body.append("echo SBX_SCRIPT_DONE")
    return "\n".join(body)


def _declare_block(n: int, issue: int) -> str:
    # `rein` resolves in-sandbox via the staged per-run binary on PATH
    # (issue #35 §3). The call BLOCKS while the human (pexpect) decides.
    return f"""
echo "SBX_DECLARE{n}_START issue={issue}"
rein declare {issue}
echo "SBX_DECLARE{n}_RC=$?"
"""


def clone_declare_push_script(repo: str, issue: int, branches: list[str]) -> str:
    """The #35 declare-first fixture: clone, `rein declare <issue>` (fires the
    Form A prompt on the HOST tty), then push each branch in turn.

    Branch names should follow agent/<issue>/<nonce> for pushes expected to
    pass the ref cross-check; non-matching names exercise the deny path.
    Sentinels: SBX_CLONE_OK, SBX_DECLARE1_RC=<code>, SBX_PUSH<n>_RC=<code>.
    """
    body = [
        "set +e",
        'cd "$0"',
        "rm -rf repo",
        f"git clone --depth 1 https://github.com/{repo} repo",
        'cd repo || { echo "SBX_CLONE_FAIL"; exit 3; }',
        "echo SBX_CLONE_OK",
        _declare_block(1, issue),
    ]
    for i, br in enumerate(branches, start=1):
        body.append(_push_block(i, repo, br))
    body.append("echo SBX_SCRIPT_DONE")
    return "\n".join(body)


def scope_expansion_script(
    repo_a: str, repo_b: str, issue_b: int, branch_b: str
) -> str:
    """The issue-#69 scope-expansion fixture, END TO END inside the sandbox.

    Order matters and is the natural one (no local checkout is needed to
    declare): clone A into the working-tree mount, then `rein declare
    <issue_b> --repo <repo_b>` — which fires the SCOPE EXPANSION prompt on
    the HOST tty and BLOCKS. Only AFTER approval does the token cover repo B,
    so the clone + push of B come next.

    Repo B has NO sandbox bind mount (binds are fixed at launch, #64), so B
    is cloned into the writable scratch dir the sandbox DOES provide — the
    child's TMPDIR (rein's per-run agentTmp, bound writable) — NOT nested in
    A's working tree. This mirrors the approve-message's steering.

    Sentinels: SBX_CLONE_OK, SBX_DECLARE1_RC (the expansion declare),
    SBX_CLONEB_RC, SBX_PUSHB_RC (the push to repo B).
    """
    scratch = "${TMPDIR:-/tmp}/rein-expansion-b"
    return "\n".join(
        [
            "set +e",
            'cd "$0"',
            "rm -rf repo",
            f"git clone --depth 1 https://github.com/{repo_a} repo",
            'cd repo || { echo "SBX_CLONE_FAIL"; exit 3; }',
            "echo SBX_CLONE_OK",
            # The expansion declare — fires the host-tty SCOPE EXPANSION prompt.
            f'echo "SBX_DECLARE1_START issue={issue_b} repo={repo_b}"',
            f"rein declare {issue_b} --repo {repo_b}",
            "echo SBX_DECLARE1_RC=$?",
            # Repo B lives in the writable scratch dir, never inside A's tree.
            f'rm -rf "{scratch}"',
            f'git clone --depth 1 https://github.com/{repo_b} "{scratch}"',
            f'echo "SBX_CLONEB_RC=$?"',
            f'cd "{scratch}" || {{ echo "SBX_CLONEB_FAIL"; exit 4; }}',
            # A FLAT probe filename: branch_b holds slashes (agent/<n>/<nonce>)
            # that would be read as directory components.
            'echo "probe $(date -u +%FT%TZ)" >> expansion-probe.txt',
            "git add -A",
            f'git commit -q -m "interactive harness expansion: {branch_b}"',
            f"git push origin HEAD:refs/heads/{branch_b}",
            "echo SBX_PUSHB_RC=$?",
            "echo SBX_SCRIPT_DONE",
        ]
    )


def cross_owner_declare_script(issue: int, cross_repo: str) -> str:
    """Drive a declare against a DIFFERENT-owner repo. The same-owner rule is
    structural (the App installation is single-owner), so this must be denied
    WITHOUT any prompt and WITHOUT a network call — the deny is synthesized
    locally. Sentinel: SBX_DECLARE1_RC (non-zero)."""
    return "\n".join(
        [
            "set +e",
            'cd "$0"',
            f'echo "SBX_DECLARE1_START issue={issue} repo={cross_repo}"',
            f"rein declare {issue} --repo {cross_repo}",
            "echo SBX_DECLARE1_RC=$?",
            "echo SBX_SCRIPT_DONE",
        ]
    )


# --------------------------------------------------------------------------
# ReinRun — a pexpect wrapper with a captured transcript
# --------------------------------------------------------------------------


@dataclass
class ReinRun:
    """A live `rein run` under a pty, with a full transcript for assertions."""

    child: pexpect.spawn
    transcript: io.StringIO
    workdir: str

    def text(self) -> str:
        return self.transcript.getvalue()

    def prompt_count(self) -> int:
        """How many approval prompts fired (distinctive banner occurrences)."""
        return self.text().count(PROMPT_BANNER)

    def expect(self, patterns, timeout=60):
        return self.child.expect(patterns, timeout=timeout)

    def expect_prompt(self, timeout=90):
        """Wait for the /dev/tty approval prompt to appear on the host tty."""
        return self.child.expect(PROMPT_HINT, timeout=timeout)

    def expect_approved(self, timeout=60):
        """Wait for the host tty to print the [approved] marker.

        The literal marker contains regex metacharacters ('[', ']'); pexpect
        treats patterns as regexes, so it MUST be escaped or it silently
        compiles to a character class. re.escape keeps the match literal.
        """
        return self.child.expect(re.escape(APPROVED_MARK), timeout=timeout)

    def expect_denied(self, timeout=60):
        """Wait for the host tty to print the [denied ...] marker (escaped; see above)."""
        return self.child.expect(re.escape(DENIED_MARK), timeout=timeout)

    def answer(self, line: str):
        """Type an answer to the approval prompt (as the human would)."""
        self.child.sendline(line)

    def wait(self, timeout=120) -> int:
        """Wait for `rein run` to exit; return its exit status."""
        self.child.expect(pexpect.EOF, timeout=timeout)
        self.child.close()
        return self.child.exitstatus if self.child.exitstatus is not None else 1

    def sentinel_rc(self, n: int) -> int | None:
        """Parse the SBX_PUSH<n>_RC=<code> sentinel from the transcript."""
        m = re.search(rf"SBX_PUSH{n}_RC=(\d+)", self.text())
        return int(m.group(1)) if m else None

    def declare_rc(self, n: int = 1) -> int | None:
        """Parse the SBX_DECLARE<n>_RC=<code> sentinel from the transcript."""
        m = re.search(rf"SBX_DECLARE{n}_RC=(\d+)", self.text())
        return int(m.group(1)) if m else None

    def named_rc(self, name: str) -> int | None:
        """Parse an arbitrary SBX_<name>_RC=<code> sentinel (issue #69 uses
        SBX_PUSHB / SBX_CLONEB for the expansion's repo-B leg)."""
        m = re.search(rf"SBX_{name}_RC=(\d+)", self.text())
        return int(m.group(1)) if m else None

    def expansion_prompt_count(self) -> int:
        """How many SCOPE EXPANSION prompts fired (issue #69 banner)."""
        return self.text().count(EXPANSION_BANNER)

    def expect_expansion_prompt(self, timeout=90):
        """Wait for the SCOPE EXPANSION prompt to appear on the host tty."""
        return self.child.expect(re.escape(EXPANSION_ADD_REPO), timeout=timeout)

    def expect_expansion_approved(self, timeout=60):
        """Wait for the expansion-approved marker (escaped; regex metachars)."""
        return self.child.expect(re.escape(EXPANSION_APPROVED_MARK), timeout=timeout)

    def expect_persist_question(self, timeout=30):
        """Wait for the in-prompt 'Also save ... to the session?' question."""
        return self.child.expect("Also save", timeout=timeout)

    # -- interactive (TUI) helpers, for the real-agent e2e ------------------

    def read_until_ready(self, ready_markers, dialog_markers=None, timeout=60):
        """Drain the pty until a ready marker appears (ANSI-stripped match).

        A TUI redraws constantly, so `expect` on a single banner is brittle;
        instead we accumulate bytes and substring-match the stripped buffer.
        Returns (ready: bool, dialog: bool, exited: bool). `dialog` flags a
        startup dialog (trust/theme/login) so the caller never misreads one as
        a hang.
        """
        import pexpect as _px

        dialog_markers = dialog_markers or []
        buf = ""
        ready = dialog = exited = False
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                buf += self.child.read_nonblocking(size=4096, timeout=3)
            except _px.TIMEOUT:
                pass
            except _px.EOF:
                exited = True
                break
            low = strip_ansi(buf).lower()
            if any(m.lower() in low for m in ready_markers):
                ready = True
                break
            if any(m.lower() in low for m in dialog_markers):
                dialog = True
        return ready, dialog, exited

    def send_and_collect(self, line: str, settle: float = 12.0, timeout: float = 10.0) -> str:
        """Send a line to the TUI, wait `settle`s, drain the reply; return it
        ANSI-stripped."""
        import pexpect as _px

        self.child.send(line + "\r")
        time.sleep(settle)
        out = ""
        try:
            out = self.child.read_nonblocking(size=16384, timeout=timeout)
        except (_px.TIMEOUT, _px.EOF):
            pass
        return strip_ansi(out)

    def quit_tui(self):
        """Best-effort teardown of an interactive claude session."""
        try:
            self.child.sendcontrol("c")
            time.sleep(0.5)
            self.child.sendcontrol("c")
            time.sleep(0.5)
            self.child.sendline("/exit")
            time.sleep(0.5)
            self.child.sendcontrol("d")
        except Exception:
            pass
        try:
            self.child.close(force=True)
        except Exception:
            pass


def spawn_rein_run(
    inner_argv: list[str],
    *,
    workdir: str,
    env: dict | None = None,
    extra_env: dict | None = None,
    timeout: int = 120,
    direct: bool = False,
) -> ReinRun:
    """Spawn `rein run [--direct] -- <inner_argv> <workdir>` under a pty.

    - `workdir` is exported as REIN_SANDBOX_WORKDIR AND passed as the final arg
      to the inner command (so an in-sandbox `cd "$0"` lands in the writable
      mount, matching the manual-test convention).
    - `direct=True` spawns the unsandboxed leg (`rein run --direct`) — the #35
      unified model means the declare + confirm flow is drivable on the same
      pty in both modes.
    - The transcript logfile captures EVERYTHING both sides print on the pty,
      so tests can count prompts and read the in-sandbox sentinels.
    - Wide cols (200) stop pexpect's default 80-col wrap from splitting the
      banner mid-word and breaking substring matches.
    """
    env = dict(env or rein_env())
    env["REIN_SANDBOX_WORKDIR"] = workdir
    if extra_env:
        env.update(extra_env)

    mode = ["--direct"] if direct else []
    args = ["run", *mode, "--", *inner_argv, workdir]
    transcript = io.StringIO()
    child = pexpect.spawn(
        str(REIN_BIN),
        args,
        cwd=str(REPO_ROOT),
        env=env,
        encoding="utf-8",
        codec_errors="replace",
        timeout=timeout,
        dimensions=(40, 200),
    )
    child.logfile_read = transcript
    return ReinRun(child=child, transcript=transcript, workdir=workdir)


def make_workdir() -> str:
    """A scratch working tree OUTSIDE the repo (srt's allowWrite mount)."""
    return tempfile.mkdtemp(prefix="rein-itest-")


# --------------------------------------------------------------------------
# Interactive real-agent (claude) under `rein run` — for the e2e test
# --------------------------------------------------------------------------
#
# Unlike spawn_rein_run (which drives a `bash -c` write script and appends the
# workdir as the script's $0), the real-agent e2e drives INTERACTIVE `claude`.
# We must NOT append a trailing positional: for interactive claude a positional
# is treated as the initial prompt. So this helper runs plain `rein run -- claude
# [extra]` with NO trailing arg. cwd defaults to the repo (a path already trusted
# by claude, so no folder-trust dialog fires); the sandbox makes it the writable
# mount, but the 2+2 probe writes nothing.

# TUI-ready markers: any of these appearing means claude's interactive prompt is
# up and accepting input. Matched ANSI-stripped (see ReinRun.read_until_ready).
# Strong, prompt-specific ready signals only. The generic "esc to" is
# deliberately excluded — it can appear in a startup dialog ("esc to go back")
# and would false-ready before the input box is live.
CLAUDE_READY_MARKERS = ["? for shortcuts", 'try "', "how can i help"]
# Startup-dialog markers (trust/theme/onboarding/login) — distinct from an MCP
# hang; surfaced so a dialog is never misread as a hang.
CLAUDE_DIALOG_MARKERS = [
    "do you trust", "trust the files", "onboarding", "select theme",
    "dark mode", "light mode", "log in", "sign in", "authenticate",
]

_ANSI = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]")
_OSC = re.compile(r"\x1b\][0-9;].*?(\x07|\x1b\\)")


def strip_ansi(s: str) -> str:
    """Strip CSI + OSC escapes so TUI redraws don't defeat substring matching."""
    return _OSC.sub("", _ANSI.sub("", s))


def spawn_claude_interactive(
    extra_args: list[str] | None = None,
    *,
    cwd: str | None = None,
    env: dict | None = None,
    extra_env: dict | None = None,
    timeout: int = 60,
) -> ReinRun:
    """Spawn interactive `rein run -- claude [extra_args]` under a pty.

    No trailing workdir positional (that would become claude's initial prompt).
    cwd defaults to the repo root (a claude-trusted path, so no trust dialog).
    """
    env = dict(env or rein_env())
    if extra_env:
        env.update(extra_env)
    args = ["run", "--", "claude", *(extra_args or [])]
    transcript = io.StringIO()
    child = pexpect.spawn(
        str(REIN_BIN),
        args,
        cwd=str(cwd or REPO_ROOT),
        env=env,
        encoding="utf-8",
        codec_errors="replace",
        timeout=timeout,
        dimensions=(40, 120),
    )
    child.logfile_read = transcript
    return ReinRun(child=child, transcript=transcript, workdir=str(cwd or REPO_ROOT))


# --------------------------------------------------------------------------
# `rein init` under a pty — for the interactive-init TDD tests
# --------------------------------------------------------------------------
#
# DANGER the init tests must contain: real `rein init` writes a shell-rc alias,
# creates ~/.local/bin/rein, scaffolds the real dev-session.yaml, and — if the
# REIN_APP_* env vars were ABSENT — would route into RunManifestFlow, a
# ~25-minute browser/callback flow that tries to create a REAL GitHub App. Two
# guards keep the tests inert:
#   1. isolated_home_env() redirects HOME + XDG_CONFIG_HOME + XDG_STATE_HOME to a
#      throwaway tempdir, so every write lands there, never the dev's real dirs.
#   2. rein_env() keeps REIN_APP_* PRESENT, so init stays on the env path and
#      NEVER reaches the manifest flow. The tests also always pass
#      --no-alias --no-symlink --skip-mint-check.


def isolated_home() -> str:
    """A throwaway HOME for an init run. Nothing here touches the dev's real home."""
    return tempfile.mkdtemp(prefix="rein-itest-home-")


def isolated_home_env(home: str) -> dict:
    """HOME + XDG overrides that confine `rein init`'s writes to `home`."""
    return {
        "HOME": home,
        "XDG_CONFIG_HOME": os.path.join(home, ".config"),
        "XDG_STATE_HOME": os.path.join(home, ".local", "state"),
    }


def spawn_rein(
    args: list[str],
    *,
    env: dict | None = None,
    extra_env: dict | None = None,
    timeout: int = 30,
    cwd: str | None = None,
) -> ReinRun:
    """Spawn an arbitrary `rein <args>` under a pty with a captured transcript.

    `cwd` defaults to the repo root. A journey step that drives a sandbox launch
    (`rein run -- … <workdir>`) can point it (or REIN_SANDBOX_WORKDIR via
    extra_env) at the writable checkout so rein binds the intended tree — see
    run_journey, which threads a step's cwd/extra_env through here.
    """
    env = dict(env or rein_env())
    if extra_env:
        env.update(extra_env)
    transcript = io.StringIO()
    child = pexpect.spawn(
        str(REIN_BIN),
        args,
        cwd=str(cwd or REPO_ROOT),
        env=env,
        encoding="utf-8",
        codec_errors="replace",
        timeout=timeout,
        dimensions=(40, 200),
    )
    child.logfile_read = transcript
    return ReinRun(child=child, transcript=transcript, workdir=str(cwd or ""))


# --------------------------------------------------------------------------
# Shared journey RUNNER — complete capture is structural, not a discipline
# --------------------------------------------------------------------------
#
# Issue #82 / Tom's "shared lib" ruling: a journey golden dropped `rein doctor`
# because the journey hand-assembled what went into the golden. The fix is to
# take capture OUT of the author's hands. With run_journey the author declares
# only STEPS — the argv of each command and the ordered answers to its prompts.
# The runner captures the COMPLETE pty session of everything it drove and
# returns it as one raw transcript. There is no supported path to hand-pick or
# slice which sections land in the golden: a section is present because its
# command ran in the captured session. Volatiles are handled downstream by
# normalize-on-compare, NEVER by dropping output.


@dataclass
class JourneyStep:
    """One `rein <argv>` command a journey drives — a host command OR a sandbox
    launch (`argv=["run", "--", …inner…, <workdir>]`).

    The author declares the argv and, for an interactive command, the ORDERED
    (expect_pattern, answer) pairs — and nothing about capture. `label`
    overrides the `$ rein <argv>` echo shown before the command's output. For a
    sandbox step whose inner argv is a big `bash -c <script>`, set a concise
    `label` (rein re-echoes the full script itself right below), so the golden's
    boundary line stays readable.

    Per-step overrides (each WINS over the journey-level value of the same name,
    so a single slow sandbox step can raise just its own timeout without slowing
    the fast host steps around it). `cwd`+`extra_env` use the SAME names/semantics
    as parallel branch #78 so the two converge:
      cwd        — working directory rein is spawned in. A sandbox step points it
                   (or REIN_SANDBOX_WORKDIR via extra_env) at the writable
                   checkout so rein binds the intended tree.
      extra_env  — env overlaid for THIS step only (e.g. REIN_SESSION_FILE, or
                   REIN_SANDBOX_WORKDIR to name the sandbox working tree).
      timeout    — seconds for this step's spawn + every expect (a sandbox launch
                   needs ~120-180s vs the fast host-command default).
    """

    argv: list[str]
    answers: list[tuple[str, str]] = field(default_factory=list)
    label: str | None = None
    cwd: str | None = None
    extra_env: dict | None = None
    timeout: int | None = None


@dataclass
class StepResult:
    argv: list[str]
    text: str  # this step's raw pty capture (for per-step assertions only)
    exitstatus: int | None
    reached_eof: bool


@dataclass
class JourneyResult:
    transcript: str  # COMPLETE raw transcript of the WHOLE session (all steps)
    steps: list[StepResult] = field(default_factory=list)

    @property
    def reached_eof(self) -> bool:
        """True iff every step ran to EOF (no prompt was missed / timed out)."""
        return all(s.reached_eof for s in self.steps)


def run_journey(steps, *, env=None, extra_env=None, timeout: int = 60) -> JourneyResult:
    """Drive a sequence of host `rein <argv>` commands under a pty, answering
    each step's prompts in order, and capture the COMPLETE session.

    Returns a JourneyResult whose `.transcript` is the whole captured session
    (build_raw_transcript over every step's output, each preceded by a `$ rein
    <argv>` echo so it reads like a real terminal). Pass THAT to compare_golden
    — the author never calls `.text()` and slices, so no section can be omitted.

    The steps share the journey-level `env`+`extra_env`+`timeout`, so an earlier
    `rein init` sets up the HOME/XDG world a later `rein doctor` inspects. A step
    may OVERRIDE `cwd`/`extra_env`/`timeout` for itself (per-step wins) — that is
    what makes a single-run SANDBOX launch a first-class step: declare
    `argv=["run", "--", …inner…, <workdir>]`, point it at the writable tree
    (`extra_env={"REIN_SANDBOX_WORKDIR": workdir}` or `cwd=workdir`), and give it
    the slow-launch `timeout` (~180s). Its `answers` drive the mid-run Form-A
    declare prompt exactly like any other step's prompts, and the in-sandbox
    script's `sandbox_preamble()`/`run` SBX| output is captured as session
    content. There is no host-vs-sandbox carve-out any more: run_journey is THE
    interface for every journey, and the complete session (banner, contract, the
    tagged agent output — no slicing) becomes the golden.

    `.reached_eof` is False if any step missed a prompt / timed out, so the
    caller can report a clean "flow broke" instead of diffing a partial golden.
    """
    parts: list[str] = []
    results: list[StepResult] = []
    for st in steps:
        step_timeout = st.timeout if st.timeout is not None else timeout
        # journey-level extra_env first, step extra_env overlaid (step wins).
        step_extra_env = {**(extra_env or {}), **(st.extra_env or {})} or None
        run = spawn_rein(
            st.argv, env=env, extra_env=step_extra_env,
            timeout=step_timeout, cwd=st.cwd,
        )
        reached = False
        try:
            for pat, ans in st.answers:
                run.child.expect(pat, timeout=step_timeout)
                run.answer(ans)
            run.child.expect(pexpect.EOF, timeout=step_timeout)
            reached = True
        except (pexpect.EOF, pexpect.TIMEOUT):
            reached = False
        finally:
            try:
                run.child.close()
            except Exception:
                pass
        label = st.label or ("rein " + " ".join(st.argv))
        parts.append(f"$ {label}\n" + run.text())
        results.append(
            StepResult(argv=st.argv, text=run.text(),
                       exitstatus=run.child.exitstatus, reached_eof=reached)
        )
    transcript = build_raw_transcript("\n".join(parts))
    return JourneyResult(transcript=transcript, steps=results)


# --------------------------------------------------------------------------
# JOURNEY helpers (shared exemplar API — see tests/interactive/CLAUDE.md)
# --------------------------------------------------------------------------
#
# A "journey" drives one major user path end to end and produces a checked-in,
# human-reviewable GOLDEN transcript. These helpers are what make the NEXT
# journey mostly declarative; journey_write_ceremony.py is the exemplar that
# proved the shape. The four moving parts:
#
#   SBX_TAG / get_views  — EXACT host-vs-agent split. The in-sandbox script tags
#                          every line it emits with SBX_TAG, so a line is agent
#                          iff it carries the tag. No content heuristics.
#   normalize_transcript — swap the volatile bits (issue #, nonces, timestamps,
#                          object counts, hashes, tmp paths) for stable
#                          placeholders so two runs yield an identical golden.
#   read_golden/compare_golden/update_golden — the golden file itself.
#   create_issue/close_issue/issue_title      — a throwaway issue to declare.

GOLDEN_DIR = Path(__file__).resolve().parent / "golden"

# The agent tags EVERY line it emits with this. `tr '\r' '\n'` upstream turns
# git's carriage-return progress redraws into real lines so each one still
# carries the tag (a lone \r would otherwise overwrite the tag on the terminal).
SBX_TAG = "SBX| "


def sandbox_preamble() -> str:
    """Bash helper functions every in-sandbox journey script prepends (shared so
    #78's scope-expansion journey inherits the exact same transcript shape).

    Two helpers, both tagging their output with SBX_TAG:
      emit "<text>"   -- a tagged line (step labels, @PHASE.. sentinels).
      run  <cmd...>   -- ECHO the command as `SBX| $ <cmd>` FIRST, then run it,
                         tagging each line of its combined output. So the
                         transcript reads like a real terminal: command, then its
                         output, then the next command. (Deliberately NOT `set
                         -x`, which would dump the while-loop/PIPESTATUS internals
                         of the tagging machinery as noise.) The command's own exit
                         code is preserved via PIPESTATUS, so `run git push; emit
                         "@RC=$?"` still records the real push result.
    """
    tag = SBX_TAG
    return (
        "set +e\n"
        f"emit() {{ printf '%s%s\\n' '{tag}' \"$*\"; }}\n"
        "run() {\n"
        f"  printf '%s$ %s\\n' '{tag}' \"$*\"\n"
        f"  \"$@\" 2>&1 | tr '\\r' '\\n' | while IFS= read -r l; do printf '%s%s\\n' '{tag}' \"$l\"; done\n"
        "  return ${PIPESTATUS[0]}\n"
        "}"
    )


def _pty_lines(text: str) -> list[str]:
    """ANSI-stripped physical lines, with CR treated as a line break.

    A bare \\r is a terminal cursor-return; splitting on it (rather than letting
    it overwrite) keeps every tagged progress redraw as its own analyzable line.
    """
    return strip_ansi(text).replace("\r\n", "\n").replace("\r", "\n").split("\n")


def get_views(text: str) -> tuple[list[str], list[str]]:
    """Split ONE interleaved pty transcript into (host_lines, agent_lines).

    A line belongs to the AGENT iff it STARTS with SBX_TAG. `emit`/`runtagged`
    write the tag at column 0, so real agent output always leads with it —
    whereas rein's own startup banner ECHOES the script body (`rein: running:
    bash -c …`), and those HOST lines contain the literal `SBX| ` *mid-line*
    (inside the emit/runtagged definitions). Matching on `startswith` (not a
    substring `find`) keeps that echo on the HOST side; a substring test would
    mis-file the fragment after the tag as agent output. One pass, so the two
    views can never disagree about a line; the tag is stripped from agent lines.

    Tag-split is exact for whole, unwrapped lines. It is NOT the whole artifact:
    callers still CURATE the views for the golden (dropping git object-count
    noise, masking pty line-wrap), so "tag-split + curation" — not the split
    alone — is what a human reviews.
    """
    host: list[str] = []
    agent: list[str] = []
    for ln in _pty_lines(text):
        if ln.startswith(SBX_TAG):
            agent.append(ln[len(SBX_TAG):].rstrip())
        else:
            host.append(ln.rstrip())
    return host, agent


def normalize_transcript(text: str, subs: list[tuple[str, str]] | None = None) -> str:
    """Apply the generic volatile-token rules (the COMPARISON transform).

    PR #78 model: the golden FILE is stored RAW (real issue number, repo, title,
    nonce, object counts). Determinism lives HERE — this transform is applied to
    BOTH the committed golden and a fresh run before they are diffed, so a run
    with a different issue / nonce / count still matches. The rules are therefore
    GENERIC regexes (not runtime-exact swaps): the committed golden's baked-in
    values differ from a fresh run's, so both must map to the same placeholder
    with no knowledge of either's specific value.

    `subs` (optional) are exact-string swaps applied first — unused by the write
    ceremony, kept for a future journey that needs a value regex can't capture.
    """
    for old, new in subs or []:
        if old:
            text = text.replace(old, new)
    for pat, repl in _NORMALIZE_RULES:
        text = re.sub(pat, repl, text)
    return text


# The GENERIC volatile-token rules — the comparison transform (PR #78). Order
# matters: issue-number rules run BEFORE the hash rule (a 7+-digit issue would
# otherwise be eaten as a <HASH>), and RATE before SIZE. Everything a rule does
# NOT recognize is left verbatim, so a brand-new rein line still trips drift.
# Extend this list (not a per-journey whitelist) for a genuinely new volatile.
_NORMALIZE_RULES = [
    # per-operator GitHub App identity that `rein init` echoes on the env path
    # (client_id + installation_id are whoever ran `source ./dev-env`). Generic,
    # so the onboarding golden is portable across operators. BEFORE the hash
    # rule (client_id can look hex-ish) and the issue rules (installation_id is
    # bare digits that no issue rule would otherwise touch).
    (r"client_id=[A-Za-z0-9_]+", "client_id=<CLIENT_ID>"),
    (r"installation_id=\d+", "installation_id=<INSTALL_ID>"),
    # scaffolded dev-session id carries a per-run random hex suffix
    # (sess_dev_init_<hex8> from `rein init`). Fixed-id sessions used by other
    # journeys (sess_journey_ceremony, sess_itest_pinned) do NOT match.
    (r"sess_dev_init_[0-9a-f]+", "sess_dev_init_<ID>"),
    # issue number, in every context it appears (BEFORE the hash rule). NB the
    # `agent/\d+/` rule also rewrites the literal example `agent/73/kx3q` in
    # rein's error text — harmless, since it hits both compare sides identically.
    (r"#\d+", "#<ISSUE>"),
    (r"agent/\d+/", "agent/<ISSUE>/"),
    # issue number inside a REST API path, e.g. the gh-write journey's
    # `gh api /repos/<owner>/<repo>/issues/<n>/comments`. Generic (not a
    # per-journey whitelist): any `/issues/<digits>` collapses so two runs on
    # DIFFERENT issue numbers still match. Bare digits, so the letter-requiring
    # hash rule below never eats it.
    (r"/issues/\d+", "/issues/<ISSUE>"),
    (r"\bdeclare \d+", "declare <ISSUE>"),
    (r"issue number \(\d+\)", "issue number (<ISSUE>)"),
    (r"(?m)^> \d+$", "> <ISSUE>"),
    # our disposable branch suffix: <8-digit date>-<6-digit time>-<hex6>
    (r"\d{8}-\d{6}-[0-9a-f]{6}", "<NONCE>"),
    # ISO-ish timestamps the probe writes (2026-07-11T18:17:26Z)
    (r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", "<TS>"),
    # per-run proxy socket + run id
    (r"/run/user/\d+/rein/run-[A-Za-z0-9_-]+/proxy\.sock", "<PROXY_SOCK>"),
    (r"run-[A-Za-z0-9_-]{16,}", "run-<RUNID>"),
    # scratch dirs (the trailing char class excludes '/', so a suffix like
    # /session.yaml is preserved: <TMP>/session.yaml)
    (r"/tmp/rein-[A-Za-z0-9_.-]+", "<TMP>"),
    # machine-variable absolute paths -> placeholders (issue #82: `rein doctor`'s
    # transcript names real box paths — the running binary, the operator's home,
    # the App key, srt. Normalize them so the golden is stable run-to-run and not
    # tied to one checkout/home). REIN_BIN before HOME so a home-based rein binary
    # collapses to <REIN_BIN> whole rather than <HOME>/.../bin/rein. These run
    # AFTER the /tmp rule so tmp paths are already <TMP>.
    (r"[^\s()]+/bin/rein(?:-git|-gh)?\b", "<REIN_BIN>"),
    (r"/home/[^/\s]+|/Users/[^/\s]+", "<HOME>"),
    # git transfer chatter: keep the line, normalize every volatile number so the
    # golden is identical whatever the repo's object count / network speed is.
    # The hash rule REQUIRES a hex letter (?=...[a-f]) so a large all-digit object
    # count (e.g. `Total 1234567`) is NOT mistaken for a hash — it must reach the
    # count rules below and normalize to <N>. A real git SHA effectively always
    # has a letter, so this keeps SHA normalization intact.
    (r"\b(?=[0-9a-f]*[a-f])[0-9a-f]{7,40}\b", "<HASH>"),
    (r"\((?:delta|deltas) \d+\)", "(delta <N>)"),
    (r"\(from \d+\)", "(from <N>)"),
    (r"\b(reused|pack-reused|Total|Enumerating objects:|Counting objects:|"
     r"Compressing objects:|Receiving objects:|Resolving deltas:|Writing objects:|"
     r"Unpacking objects:|up to) \d+", r"\1 <N>"),
    (r"\(\d+/\d+\)", "(<N>/<N>)"),
    (r"\d+%", "<N>%"),
    (r"\d+ bytes", "<N> bytes"),
    # transfer RATE (with /s) BEFORE plain SIZE, so "MiB/s" is consumed first and
    # the bare "MiB" size that remains is not mis-normalized.
    (r"\d+(\.\d+)? [KMG]iB/s", "<RATE>"),
    (r"\d+(\.\d+)? [KMG]iB\b", "<SIZE>"),
]

# The ONE narrow progress-meter rule (Tom's ruling): drop the intermediate `%`
# ticks a git operation redraws, but KEEP every terminal `done.` line and every
# error/reject line. Everything not matched here stays in the golden verbatim.
_PROGRESS_RE = re.compile(
    r"(remote:\s*)?(Counting|Compressing|Receiving|Resolving|Writing|Enumerating|Unpacking)"
    r"\s+(objects|deltas):\s+\d+%"
)


def is_progress_tick(line: str) -> bool:
    """A mid-progress redraw (drop it); a terminal `done.` line is NOT (keep it)."""
    return bool(_PROGRESS_RE.search(line)) and "done." not in line


def build_raw_transcript(text: str) -> str:
    """The RAW terminal transcript that goes in the golden file (PR #78).

    REAL values throughout — real issue number, repo, title, branch nonce, object
    counts — so a human reviewing the checked-in golden sees exactly what the run
    produced. The ONLY things stripped are mechanical, not semantic: ANSI escapes,
    the sub-100% progress redraw ticks (transient `\\r` overwrites a terminal
    never shows as separate lines; the terminal `done.` summary — with its real
    counts — stays), and runs of blank lines collapsed to one. NO placeholders.

    Determinism does NOT live here; it lives in normalize_for_compare, applied to
    both sides at compare time.
    """
    out: list[str] = []
    prev_blank = False
    for ln in _pty_lines(text):
        ln = ln.rstrip()
        if is_progress_tick(ln):
            continue
        if ln == "":
            if prev_blank:
                continue
            prev_blank = True
        else:
            prev_blank = False
        out.append(ln)
    return "\n".join(out).strip("\n") + "\n"


def normalize_for_compare(text: str) -> str:
    """The comparison LENS: raw transcript -> volatiles replaced by placeholders.

    Applied to BOTH the committed (raw) golden and a fresh (raw) run before they
    are diffed, so different issue numbers / nonces / object counts still match
    but a genuinely new or changed line trips drift. Idempotent: re-normalizing an
    already-normalized transcript is a no-op (test_golden_shape asserts this).

    Use REIN_SHOW_NORMALIZED=1 on a journey to eyeball this form directly.
    """
    return normalize_transcript(build_raw_transcript(text))


def read_golden(name: str) -> str | None:
    p = GOLDEN_DIR / name
    return p.read_text() if p.exists() else None


def update_golden(name: str, raw_text: str) -> Path:
    """Write the RAW transcript to the golden file (real values, no placeholders)."""
    GOLDEN_DIR.mkdir(parents=True, exist_ok=True)
    p = GOLDEN_DIR / name
    p.write_text(raw_text)
    return p


def compare_golden(name: str, fresh_raw: str) -> tuple[bool, str]:
    """Compare a fresh RAW run to the committed RAW golden, NORMALIZING BOTH first.

    Returns (matches, normalized_unified_diff). The diff is over the NORMALIZED
    forms, so a human sees the meaningful change, not per-run noise. A missing
    golden reads as a diff against empty.
    """
    import difflib

    golden_raw = read_golden(name) or ""
    expected = normalize_for_compare(golden_raw)
    actual = normalize_for_compare(fresh_raw)
    if expected == actual:
        return True, ""
    diff = difflib.unified_diff(
        expected.splitlines(keepends=True),
        actual.splitlines(keepends=True),
        fromfile=f"golden/{name} (normalized)",
        tofile="fresh run (normalized)",
    )
    return False, "".join(diff)


# -- throwaway issue lifecycle (gh; the operator's own token) ----------------


def create_issue(repo: str, title: str, body: str, env: dict | None = None) -> int:
    """Open a real issue on the THROWAWAY and return its number.

    The declare FETCHES the issue before prompting (#35 decision E), so a
    journey needs a real, open issue — an invented number 404s and fails closed.
    """
    env = env or rein_env()
    out = subprocess.check_output(
        ["gh", "issue", "create", "--repo", repo, "--title", title, "--body", body],
        text=True, env=env,
    ).strip()
    return int(out.rstrip("/").split("/")[-1])


def issue_title(repo: str, issue: int, env: dict | None = None) -> str:
    env = env or rein_env()
    import json as _json

    out = subprocess.check_output(
        ["gh", "issue", "view", str(issue), "--repo", repo, "--json", "title"],
        text=True, env=env,
    )
    return _json.loads(out)["title"]


def close_issue(repo: str, issue: int, env: dict | None = None, comment: str = "") -> None:
    env = env or rein_env()
    args = ["gh", "issue", "close", str(issue), "--repo", repo]
    if comment:
        args += ["--comment", comment]
    subprocess.run(args, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def resolve_throwaway_repo(env: dict | None = None) -> str:
    """The throwaway repo a journey pushes to, resolved the rein-init way FIRST.

    Order (issue #40 — journeys must not DEPEND on REIN_TEST_REPO_A special-
    casing):
      1. REIN_JOURNEY_REPO           — an explicit journey override.
      2. the configured dev-session  — repos[0] of ~/.config/rein/dev-session.yaml
                                       (what `rein init` scaffolds).
      3. REIN_TEST_REPO_A            — LEGACY this-box shortcut, clearly last.

    A journey still only ever touches this one throwaway (hard-constraint #1).
    """
    env = env or rein_env()
    if env.get("REIN_JOURNEY_REPO"):
        return env["REIN_JOURNEY_REPO"]
    cfg = env.get("XDG_CONFIG_HOME") or os.path.join(env.get("HOME", str(Path.home())), ".config")
    session_file = Path(cfg) / "rein" / "dev-session.yaml"
    if session_file.exists():
        for line in session_file.read_text().splitlines():
            m = re.match(r"\s*-\s*(\S+/\S+)\s*$", line)
            if m:
                return m.group(1)
    if env.get("REIN_TEST_REPO_A"):
        return env["REIN_TEST_REPO_A"]
    raise RuntimeError(
        "no throwaway repo: set REIN_JOURNEY_REPO, or run `rein init` to scaffold "
        "a dev-session, or (legacy) source ./dev-env for REIN_TEST_REPO_A"
    )


# --------------------------------------------------------------------------
# helper.log — rein's forensic log (state-dir/helper.log)
# --------------------------------------------------------------------------


def helper_log_path(env: dict | None = None) -> Path:
    """Path of rein's forensic log (openLog -> <state-dir>/helper.log).

    State dir mirrors internal/config.StateDir: XDG_STATE_HOME/rein, else
    ~/.local/state/rein. A journey reads the DELTA it produced (seek to the
    pre-run size) to assert, e.g., that the broker routed approval to the tmux
    popup — a POSITIVE proof of the surface chosen, not an inference from the
    absence of an inline prompt.
    """
    env = env or rein_env()
    base = env.get("XDG_STATE_HOME") or os.path.join(
        env.get("HOME", str(Path.home())), ".local", "state"
    )
    return Path(base) / "rein" / "helper.log"


def read_log_since(path: Path, offset: int) -> str:
    """Return helper.log bytes written AFTER `offset` (this run's lines only)."""
    if not path.exists():
        return ""
    with path.open("r", errors="replace") as f:
        f.seek(offset)
        return f.read()


# --------------------------------------------------------------------------
# tmux-popup approval surface — the DEFAULT surface inside $TMUX (issue #37)
# --------------------------------------------------------------------------
#
# rein's default write-approval surface when $TMUX is set is NOT the inline
# /dev/tty prompt — it is a tmux popup (internal/ui/grant: PopupPreferenceFromEnv
# returns true inside $TMUX, and attemptPopup runs, via DefaultTmuxRunner,
#     tmux popup -E "rein approval grant --run-id <id>"
# whose pty is its OWN /dev/tty, where `rein approval grant` renders Form A).
# Every OTHER journey runs OUTSIDE tmux (so $TMUX is unset and the inline prompt
# fires), so this default path was untested end to end — the coverage gap #37.
#
# Driving a real popup under pexpect needs three things, and this helper wires
# all of them:
#
#   1. A DEDICATED tmux server (`tmux -L <unique>`), so the journey NEVER touches
#      the operator's own sessions. It kills only its OWN socket on teardown.
#   2. An ATTACHED client — a pexpect pty running `tmux attach`. A popup is a
#      CLIENT-OWNED overlay (it is NOT an addressable pane: it never appears in
#      `list-panes`, and `send-keys` cannot reach it). It renders on, and grabs
#      the keyboard of, an attached client. So the ONLY way to answer it is to
#      write keys to that client's pty — which this helper's `drive_popup` does.
#   3. The rein process on a SEPARATE plain pty whose $TMUX/$TMUX_PANE point at
#      this session (see `tmux_env`). That keeps rein's OWN output clean and
#      deterministic (the golden), while the popup renders on the attached
#      client (finicky full-screen box-art, deliberately NOT in the golden).
#      This mirrors reality: `rein run -- <agent>` runs inside the operator's
#      tmux pane, and the broker it hosts launches the popup on that same client.
#
# If tmux is not installed the popup surface cannot be driven at all; a journey
# should SKIP gracefully (see journey_tmux_popup_approval.py), never fake it.


def tmux_available() -> bool:
    """Is a `tmux` binary on PATH? The popup surface is undriveable without it."""
    return shutil.which("tmux") is not None


def _tmux(socket: str, *args: str) -> subprocess.CompletedProcess:
    """Run `tmux -L <socket> <args>` on the DEDICATED server (never the default)."""
    return subprocess.run(["tmux", "-L", socket, *args], capture_output=True, text=True)


@dataclass
class TmuxPopupSession:
    """A live tmux session on a dedicated socket, with a pexpect-attached client
    a popup can render on and be answered through. Build it via
    `tmux_popup_session()` (a context manager that tears the server down)."""

    socket: str
    client: pexpect.spawn
    log: io.StringIO
    sockpath: str
    pane: str

    def tmux_env(self) -> dict:
        """Env overlay that routes a SEPARATE rein process's approval to THIS
        session's popup: $TMUX (socket_path,pid,session — rein only checks it is
        non-empty) and $TMUX_PANE. Pass it as spawn_rein_run(extra_env=...)."""
        return {"TMUX": f"{self.sockpath},0,0", "TMUX_PANE": self.pane}

    def drive_popup(self, expect_pattern: str, answer: str, *, settle: float = 0.5,
                    timeout: int = 90) -> None:
        """Wait for the popup's Form A to RENDER on the attached client, then type
        the answer. The popup grabs the client keyboard, so keys written to the
        client pty reach `rein approval grant` inside the popup. `expect_pattern`
        must be text the popup PRINTS (e.g. "type the issue number"), not text
        from the command line that opened it."""
        self.client.expect(expect_pattern, timeout=timeout)
        time.sleep(settle)
        self.client.send(answer + "\r")

    def render(self) -> str:
        """ANSI-stripped bytes the client received — what the human SAW in the
        popup. Useful to show a reviewer; too box-art/cursor-driven to golden."""
        return strip_ansi(self.log.getvalue())

    def close(self) -> None:
        _tmux(self.socket, "kill-server")
        try:
            self.client.close(force=True)
        except Exception:
            pass


@contextlib.contextmanager
def tmux_popup_session(*, width: int = 200, height: int = 50, attach_settle: float = 1.0):
    """Stand up a dedicated-socket tmux session with a pexpect-attached client for
    a popup to render on, yield a TmuxPopupSession, and ALWAYS kill the server on
    exit. Raises RuntimeError if tmux is absent (the caller should have checked
    `tmux_available()` and skipped)."""
    if not tmux_available():
        raise RuntimeError("tmux not on PATH — the popup approval surface cannot be driven")
    socket = f"reinjourney-{os.getpid()}-{uuid.uuid4().hex[:6]}"
    _tmux(socket, "kill-server")  # clean slate on OUR socket only
    time.sleep(0.2)
    # Everything from new-session onward is inside the try, so ANY failure during
    # setup (a raise, a failed attach) still kills the dedicated server in the
    # finally — the unique socket name means the next call's kill-server would
    # NOT reap an orphan, so we must not leak one here.
    sess = None
    server_up = False
    try:
        r = _tmux(socket, "new-session", "-d", "-s", "w", "-x", str(width), "-y", str(height))
        if r.returncode != 0:
            raise RuntimeError(f"tmux new-session failed: {r.stderr.strip() or r.stdout.strip()}")
        server_up = True
        pane = _tmux(socket, "list-panes", "-t", "w", "-F", "#{pane_id}").stdout.strip()
        sockpath = _tmux(socket, "display-message", "-p", "#{socket_path}").stdout.strip()
        # tmux_env() feeds `sockpath` into the rein process's $TMUX, and rein's
        # own `tmux popup` (DefaultTmuxRunner) has NO -L — it lands on THIS server
        # only because it inherits that $TMUX. An empty sockpath would make $TMUX
        # ",0,0" (still non-empty, so rein still fires the popup) whose fallback
        # could be the operator's real default server. Fail closed instead.
        if not pane or not sockpath:
            raise RuntimeError(
                f"tmux did not report a pane id / socket path (pane={pane!r} "
                f"sockpath={sockpath!r}); refusing to proceed — an empty $TMUX "
                "socket could fall back to the operator's own tmux server"
            )
        log = io.StringIO()
        client = pexpect.spawn(
            "tmux", ["-L", socket, "attach", "-t", "w"],
            encoding="utf-8", codec_errors="replace",
            timeout=30, dimensions=(height, width),
        )
        client.logfile_read = log
        time.sleep(attach_settle)  # the client must be attached before a popup can render
        sess = TmuxPopupSession(socket=socket, client=client, log=log, sockpath=sockpath, pane=pane)
        yield sess
    finally:
        if sess is not None:
            sess.close()  # kills the server AND closes the client
        elif server_up:
            _tmux(socket, "kill-server")  # setup raised before sess existed; don't leak it
