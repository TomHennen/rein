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

import io
import os
import re
import subprocess
import tempfile
import time
import uuid
from dataclasses import dataclass
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
) -> ReinRun:
    """Spawn an arbitrary `rein <args>` under a pty with a captured transcript."""
    env = dict(env or rein_env())
    if extra_env:
        env.update(extra_env)
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
    return ReinRun(child=child, transcript=transcript, workdir="")
