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
- ``RenderedScreen``    — a pyte terminal emulator over a pty (issue #100): for
                          anything that REDRAWS (a TUI, a tmux popup) you assert
                          on the rendered SCREEN, not on ANSI-stripped bytes.
                          Line-oriented output keeps the raw-transcript path.
- ``branch_exists`` / ``delete_branch`` — HOST-side verification + cleanup via
                          the host's authed ``gh`` (the developer/operator, NOT
                          the sandbox).

Prereqs: a live throwaway repo + a working App (see README). ``python3`` +
``pexpect`` 4.9.0. The sandbox stack (srt/bwrap/socat/ripgrep) must be healthy.
``pyte`` (``apt install python3-pyte``) is needed ONLY for the rendered-screen
surfaces (the popup journey, the real-agent TUI test); it is imported lazily, so
everything else runs without it.
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
# State dir + gh-read cache seeding (issue #95 regression journey support)
# --------------------------------------------------------------------------


def state_dir(env: dict | None = None) -> Path:
    """The rein state dir, mirroring internal/config.StateDir: XDG_STATE_HOME/rein,
    else ~/.local/state/rein. A journey that pins XDG_STATE_HOME (via a step's
    extra_env) to a fresh temp dir uses this to compute where a seeded cache file
    must land so the run's own ReadCachePathForScope glob looks in the same place."""
    env = env or rein_env()
    base = env.get("XDG_STATE_HOME") or os.path.join(
        env.get("HOME", str(Path.home())), ".local", "state"
    )
    return Path(base) / "rein"


def legacy_gh_read_cache_path(env: dict | None = None) -> Path:
    """The PRE-#95 fixed, scope-BLIND gh-read token cache path
    (<state-dir>/cache/gh-read-token.json). Seeding a stale token HERE simulates
    the leftover a prior single-repo run wrote before the scope-tag fix; a pre-fix
    broker reads exactly this path, a post-fix broker reads a scope-tagged sibling
    and MISSES it (re-minting at the wider ceiling)."""
    return state_dir(env) / "cache" / "gh-read-token.json"


def seed_legacy_gh_read_token(repo: str, out_path: str | Path, env: dict | None = None) -> None:
    """Mint a REAL, currently-valid gh-read token scoped to `repo` ONLY and write
    it (as a tokencache.Entry) to `out_path` — the stale, narrower-scoped leftover
    the #95 regression journey plants before its sandboxed run. Delegates to the
    test-support `seedghread` binary (tests/interactive/seedghread), which mints
    exactly what cmd/rein/issue95_live_test.go mints; it is NOT a rein subcommand
    (no arbitrary-scope token surface in the shipped CLI). build_binaries must have
    run first (it builds bin/seedghread alongside bin/rein)."""
    env = env or rein_env()
    seeder = REPO_ROOT / "bin" / "seedghread"
    os.makedirs(os.path.dirname(str(out_path)), exist_ok=True)
    subprocess.run(
        [str(seeder), "--repo", repo, "--out", str(out_path)],
        cwd=REPO_ROOT, env=env, check=True,
    )


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


def list_prs_for_branch(repo: str, branch: str, env: dict | None = None) -> list[dict]:
    """HOST-side: PRs (any state) whose head is `branch`, via the operator's OWN
    authed gh — the GROUND TRUTH a gh-write journey checks a sandboxed `gh pr
    create` against (did the PR actually land at GitHub, distinct from the
    in-sandbox command merely exiting 0?). Legitimate host action: the
    developer/operator, NOT the sandboxed agent (which never holds a token).
    """
    env = env or rein_env()
    import json as _json

    out = subprocess.check_output(
        ["gh", "pr", "list", "--repo", repo, "--head", branch, "--state", "all",
         "--json", "number,url,state"],
        cwd=REPO_ROOT, env=env, text=True,
    ).strip()
    return _json.loads(out) if out else []


def list_matching_refs(repo: str, prefix: str, env: dict | None = None) -> list[str]:
    """HOST-side: branch names under `prefix` (e.g. `agent/<issue>/`), via the
    operator's OWN authed gh (`git/matching-refs/heads/<prefix>`).

    DISCOVERY, not assumption: a REAL agent picks its own branch name — the ref
    cross-check only requires the `agent/<issue>/` PREFIX, so a journey driving a
    live LLM cannot know the suffix in advance (a deterministic bash agent can,
    because the test chose it). Returns the short names (no `refs/heads/`), so the
    caller can verify + clean up whatever the agent actually pushed.
    """
    env = env or rein_env()
    import json as _json

    r = subprocess.run(
        ["gh", "api", f"repos/{repo}/git/matching-refs/heads/{prefix}",
         "--jq", "[.[].ref]"],
        cwd=REPO_ROOT, env=env, capture_output=True, text=True,
    )
    if r.returncode != 0:
        return []
    out = r.stdout.strip()
    refs = _json.loads(out) if out else []
    return [ref[len("refs/heads/"):] for ref in refs if ref.startswith("refs/heads/")]


def pr_author(repo: str, number: int, env: dict | None = None) -> dict:
    """HOST-side: a PR's author as {"login": str, "is_bot": bool} via the operator's
    OWN authed gh — the GROUND TRUTH for the DELEGATED identity (#101): a PR a
    sandboxed agent opened must be authored by the App's bot
    (`app/<slug>`, is_bot=true), NEVER by the developer. The commit-side twin of
    this is journey_git_author's `<name> (via rein)` author stamp.
    """
    env = env or rein_env()
    import json as _json

    out = subprocess.check_output(
        ["gh", "pr", "view", str(number), "--repo", repo, "--json", "author",
         "--jq", "{login: .author.login, is_bot: .author.is_bot}"],
        cwd=REPO_ROOT, env=env, text=True,
    ).strip()
    return _json.loads(out) if out else {}


def close_pr(repo: str, number: int, env: dict | None = None) -> bool:
    """Best-effort HOST-side cleanup of a single PR via the host's gh (closes it;
    does NOT delete its branch — pair with delete_branch for that)."""
    env = env or rein_env()
    r = subprocess.run(
        ["gh", "pr", "close", str(number), "--repo", repo],
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
# RENDERED SCREEN (pyte) — assert on the PICTURE, not on the paint history (#100)
# --------------------------------------------------------------------------
#
# WHY. Everything else in this harness reads a pty as a LINE STREAM: strip the
# ANSI, split on newlines, substring-match. That is exactly right for output
# that is genuinely line-oriented (git, rein's banner, the SBX|-tagged agent
# script) — a line, once printed, is final.
#
# It is structurally wrong for anything that REDRAWS. A TUI (`claude`) and a
# tmux popup do not "print lines"; they PAINT CELLS: home the cursor, overwrite
# a region, repaint on every keystroke and resize. The byte stream is therefore
# a *history of paint operations*, and reconstructing "what was on screen" from
# it means hand-modelling a terminal — which is precisely what this harness used
# to do (a CUP-splitting, box-art-stripping, last-write-per-row extractor) and
# what #100 got rid of. The answer is prior art: pexpect drives the pty, a real
# in-memory terminal emulator (pyte) consumes the stream and maintains the
# screen buffer, and we assert on THAT. Redraws, cursor moves and read-chunk
# boundaries stop being our problem, because resolving them is the emulator's
# entire job.
#
# WHERE IT APPLIES — and where it must NOT. A rendered screen is a point-in-time
# FRAME (80x24-ish of cells), not linear scrollback: content that scrolled off is
# GONE. So:
#   * REDRAWING surfaces (the tmux popup, the real-agent TUI)  -> rendered screen.
#   * LINE-ORIENTED transcripts (every journey golden: write_ceremony, gh_write,
#     sandbox_filesystem, onboarding …)                        -> build_raw_transcript.
# Routing a line-oriented golden through a screen would silently truncate it to
# the last N lines. Don't.
#
# pyte is a TEST-ONLY dependency (LGPLv3; approved under hard-constraint #4 on
# that basis). It is NEVER linked into or shipped with the Go binary. It is
# imported LAZILY, inside this section only, so the line-oriented journeys and
# test_golden_shape.py keep working on a box with no pyte: a journey that needs a
# rendered screen SKIPs (exit 3) with an install hint instead of exploding.

PYTE_INSTALL_HINT = "install it with: sudo apt install python3-pyte"


class PyteMissing(RuntimeError):
    """pyte is not installed, so no rendered-screen surface can be driven.

    A JOURNEY that catches this must SKIP (exit 3), never exit 0 — a green run
    for a path nothing exercised is the #68 footgun (tests/interactive/CLAUDE.md).
    """


def _pyte():
    """Import pyte, or raise PyteMissing with an actionable hint. Lazy on purpose
    (see the section header): importing reinharness must not require pyte."""
    try:
        import pyte  # noqa: PLC0415  (deliberately lazy)
    except ImportError as e:  # pragma: no cover - depends on the box
        raise PyteMissing(
            f"pyte is not installed, so a rendered-screen surface (TUI / tmux "
            f"popup) cannot be driven — {PYTE_INSTALL_HINT}"
        ) from e
    return pyte


def pyte_available() -> bool:
    """Is the rendered-screen layer usable on this box? Journeys check this up
    front and SKIP (exit 3) if not."""
    try:
        _pyte()
    except PyteMissing:
        return False
    return True


class RenderedScreen:
    """A real terminal emulator (pyte) fed a pty's bytes: feed(text) -> display().

    ONE screen per pty, mutated by every byte — the same model the physical
    terminal uses. `display()` is the rendered grid (a list of `cols`-wide lines,
    space-padded); `text()` is that grid joined, with each line right-trimmed and
    the trailing all-blank lines dropped, which is the form assertions want.

    Size it to the pty that feeds it (`screen_for_child`): pyte wraps at `cols`
    exactly as the real terminal does, so a mismatched width renders line breaks
    the human never saw.
    """

    def __init__(self, cols: int = 200, rows: int = 50):
        pyte = _pyte()
        self.cols = cols
        self.rows = rows
        self._screen = pyte.Screen(cols, rows)
        self._stream = pyte.Stream(self._screen)

    def feed(self, text: str) -> None:
        """Apply pty output (str; pexpect is spawned with encoding='utf-8')."""
        if text:
            self._stream.feed(text)

    def display(self) -> list[str]:
        """The rendered grid: `rows` lines of `cols` chars, exactly as painted."""
        return list(self._screen.display)

    def text(self) -> str:
        """The rendered screen as text (lines right-trimmed, trailing blanks cut)."""
        lines = [ln.rstrip() for ln in self.display()]
        while lines and not lines[-1]:
            lines.pop()
        return "\n".join(lines)

    def contains(self, needle: str, *, ignore_case: bool = False) -> bool:
        hay = self.text()
        return (needle.lower() in hay.lower()) if ignore_case else (needle in hay)


def screen_for_child(child: pexpect.spawn) -> RenderedScreen:
    """A RenderedScreen sized to `child`'s pty (getwinsize() -> (rows, cols)), so
    the emulator wraps exactly where the child's terminal does."""
    rows, cols = child.getwinsize()
    return RenderedScreen(cols=cols, rows=rows)


def render_stream(text: str, *, cols: int = 200, rows: int = 50) -> RenderedScreen:
    """Render an ALREADY-CAPTURED pty stream (e.g. a pexpect `logfile_read`
    StringIO's value) into a screen — the after-the-fact form of feed()."""
    scr = RenderedScreen(cols=cols, rows=rows)
    scr.feed(text)
    return scr


def drain_children(children, *, poll: float = 0.05) -> None:
    """ONE `read_nonblocking` per child: take whatever is available RIGHT NOW and
    throw it away. Call it in a loop.

    WHY: a pty's kernel buffer is small (~64KB) and a REAL agent TUI (`claude`)
    repaints continuously, so if nobody reads its pty while we wait on, say, the
    tmux client the popup renders on, the agent BLOCKS on write — it can never
    reach the declare whose popup we are waiting for. Reading keeps it moving.

    WHAT THIS IS NOT: it is not a full drain and it gives no structural guarantee.
    It issues a single bounded read per child per call, so it is a RATE MITIGATION:
    it holds only because every caller returns to it promptly (`wait_for_screen`
    calls it once per poll iteration; `await_landing` calls it around each `gh api`
    subprocess). A caller that goes away for long enough can still let the buffer
    fill and stall the agent — transiently, not permanently, since the loop always
    comes back and reads. The STRUCTURAL fix would be a background reader thread
    per child; we deliberately do not have one, so keep the gaps between calls short.

    Discarding the bytes is safe: they are still captured by the child's
    `logfile_read` (every ReinRun installs one), so the raw transcript stays
    COMPLETE. This only stops the pipe from filling.
    """
    for ch in children or ():
        try:
            ch.read_nonblocking(size=65536, timeout=poll)
        except (pexpect.TIMEOUT, pexpect.EOF, OSError, ValueError):
            pass


def wait_for_screen(child: pexpect.spawn, pattern, *, timeout: float = 60.0,
                    screen: RenderedScreen | None = None,
                    poll: float = 0.5, drain=None) -> tuple[bool, RenderedScreen]:
    """Pump `child`'s bytes into a rendered screen until `pattern` appears ON IT.

    THE primitive that makes redraws / cursor moves / chunk boundaries a
    non-issue: unlike `pexpect.expect`, which matches a moving window of the raw
    byte stream (so a repaint that splits the text, or a chunk boundary, can miss
    it), this matches the RENDERED result — and it keeps matching once the text is
    on screen, no matter how it got there.

    `pattern` is a regex string searched (MULTILINE) against the screen text, or a
    callable `screen -> bool` for a richer screen-state condition (e.g. "the popup
    box is fully painted"). Returns (found, screen) — the screen either way, so a
    caller can report what WAS on it when it gave up. Bytes read here still land
    in the child's `logfile_read`, so the raw transcript is unaffected.

    `drain` — OTHER live pexpect children to keep reading while we wait (see
    `drain_children`). Pass the agent's pty here when waiting on the tmux client,
    or the whole run deadlocks on a full pty buffer.
    """
    scr = screen if screen is not None else screen_for_child(child)
    if callable(pattern):
        hit = pattern
    else:
        rx = re.compile(pattern, re.MULTILINE)

        def hit(s: RenderedScreen) -> bool:
            return bool(rx.search(s.text()))

    deadline = time.time() + timeout
    if hit(scr):
        return True, scr
    while time.time() < deadline:
        drain_children(drain)
        try:
            scr.feed(child.read_nonblocking(size=4096, timeout=poll))
        except pexpect.TIMEOUT:
            pass
        except pexpect.EOF:
            return hit(scr), scr
        if hit(scr):
            return True, scr
    return hit(scr), scr


# --------------------------------------------------------------------------
# ReinRun — a pexpect wrapper with a captured transcript
# --------------------------------------------------------------------------


@dataclass
class ReinRun:
    """A live `rein run` under a pty, with a full transcript for assertions."""

    child: pexpect.spawn
    transcript: io.StringIO
    workdir: str
    # Lazily built by .screen() — the pyte-rendered view of this pty (#100).
    # Only the TUI-facing helpers touch it, so a line-oriented test never needs
    # pyte installed.
    _screen: "RenderedScreen | None" = None

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
    #
    # These assert on a RENDERED SCREEN (pyte), not on strip_ansi(raw bytes)
    # — issue #100. A TUI repaints: it moves the cursor, overwrites cells and
    # re-emits the same region many times, so the raw stream is a *history of
    # paint operations*, not a picture. Substring-matching that history is
    # sensitive to redraw order and to where a read chunk happened to split.
    # Feeding it to a terminal emulator instead gives the picture the human is
    # looking at, which is what the assertions are actually about.

    def screen(self) -> RenderedScreen:
        """This run's persistent rendered screen, sized to the pty (lazily made).

        Persistent on purpose: a real terminal keeps ONE screen that every byte
        mutates, so a repaint updates cells rather than appending text. Every
        helper below pumps the child's bytes into this one screen.
        """
        if self._screen is None:
            self._screen = screen_for_child(self.child)
        return self._screen

    def read_until_ready(self, ready_markers, dialog_markers=None, timeout=60):
        """Pump the pty into the RENDERED SCREEN until a ready marker is ON IT.

        Returns (ready: bool, dialog: bool, exited: bool). `dialog` flags a
        startup dialog (trust/theme/login) so the caller never misreads one as
        a hang. Markers are matched case-insensitively against the rendered
        screen text, so a marker that a redraw split across two writes — or
        that got overwritten and repainted — still matches exactly once it is
        actually visible.
        """
        import pexpect as _px

        dialog_markers = dialog_markers or []
        scr = self.screen()
        ready = dialog = exited = False
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                scr.feed(self.child.read_nonblocking(size=4096, timeout=3))
            except _px.TIMEOUT:
                pass
            except _px.EOF:
                exited = True
                break
            low = scr.text().lower()
            if any(m.lower() in low for m in ready_markers):
                ready = True
                break
            if any(m.lower() in low for m in dialog_markers):
                dialog = True
        return ready, dialog, exited

    def send_and_collect(self, line: str, settle: float = 12.0, timeout: float = 10.0) -> str:
        """Send a line to the TUI, let it settle, drain the reply into the
        rendered screen, and return the SCREEN TEXT — what the human now sees.

        Not "the bytes that arrived": a TUI's answer may be painted, scrolled
        and repainted, so the delta of raw bytes is not the answer. The screen
        after the reply lands is.
        """
        import pexpect as _px

        scr = self.screen()
        self.child.send(line + "\r")
        time.sleep(settle)
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                scr.feed(self.child.read_nonblocking(size=16384, timeout=1))
            except (_px.TIMEOUT, _px.EOF):
                break
        return scr.text()

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

# The tag for lines captured from the tmux POPUP's own (client-owned) pty and
# FOLDED into the one transcript — the mirror of SBX_TAG for the sandbox child.
# rein's own pty shows `$ rein declare <n>` -> `confirmed` with NO inline Form A
# (approval routed AWAY to the popup); the POPUP| lines carry the Form A the human
# actually READ in the popup, interleaved right where the declare blocked. Same
# "one transcript, two views" model as SBX| vs untagged host lines (#82).
POPUP_TAG = "POPUP| "


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


def get_views(text: str) -> tuple[list[str], list[str], list[str]]:
    """Split ONE interleaved transcript into (host_lines, agent_lines, popup_lines).

    A line belongs to the AGENT iff it STARTS with SBX_TAG, and to the POPUP iff
    it STARTS with POPUP_TAG. `emit`/`runtagged` (and the popup fold) write the tag
    at column 0, so real tagged output always leads with it — whereas rein's own
    startup banner ECHOES the script body (`rein: running: bash -c …`), and those
    HOST lines contain the literal `SBX| ` *mid-line* (inside the emit/runtagged
    definitions). Matching on `startswith` (not a substring `find`) keeps that echo
    on the HOST side; a substring test would mis-file the fragment after the tag as
    agent output. One pass, so the views can never disagree about a line; the tag is
    stripped from tagged lines.

    Three views because the popup approval journey folds a SECOND, client-owned pty
    (the Form A the human read in the tmux popup) into the same transcript under
    POPUP_TAG — the mirror of SBX_TAG for the sandbox child. A caller wanting the
    Form A alone reads `popup`; the routing invariant "no inline Form A on rein's
    own terminal" is `PROMPT_HINT not in "\\n".join(host)`.

    Tag-split is exact for whole, unwrapped lines. It is NOT the whole artifact:
    callers still CURATE the views for the golden (dropping git object-count
    noise, masking pty line-wrap), so "tag-split + curation" — not the split
    alone — is what a human reviews.

    A tagged line whose CONTENT is blank appears in a transcript as the bare tag
    with its trailing space stripped (`SBX|`, `POPUP|`) — build_raw_transcript
    rstrips every line, and a golden must not carry trailing whitespace (an editor
    stripping it would read as drift). Such a line is still its view's, not the
    host's, so the split accepts the bare tag too.
    """
    host: list[str] = []
    agent: list[str] = []
    popup: list[str] = []
    for ln in _pty_lines(text):
        if ln.startswith(SBX_TAG) or ln.rstrip() == SBX_TAG.rstrip():
            agent.append(ln[len(SBX_TAG):].rstrip())
        elif ln.startswith(POPUP_TAG) or ln.rstrip() == POPUP_TAG.rstrip():
            popup.append(ln[len(POPUP_TAG):].rstrip())
        else:
            host.append(ln.rstrip())
    return host, agent, popup


# The popup box's corners, as tmux paints them (it picks a style depending on
# terminal/config: square, rounded, or heavy). Used ONLY to locate the box on the
# RENDERED screen — we no longer strip box art out of a byte stream, because we no
# longer read a byte stream (#100). A row is the top/bottom border iff it carries
# the matching left+right corner glyph.
_BOX_TOP = re.compile(r"[┌╭┏].*[┐╮┓]")
_BOX_BOTTOM = re.compile(r"[└╰┗].*[┘╯┛]")


def popup_forma_from_screen(screen: RenderedScreen,
                            answer: str | None = None) -> list[str]:
    """Extract the popup's Form A from a RENDERED client screen (#100).

    A tmux popup is a CLIENT-OWNED OVERLAY drawn ON TOP of the client's screen —
    it is NOT an addressable pane (it never appears in `list-panes`, and
    `capture-pane` over every pane finds no trace of it; verified empirically, so
    capture-pane is NOT an option here — see tests/interactive/CLAUDE.md). The only
    surface that has the popup on it is the ATTACHED CLIENT's pty, and once that
    pty's bytes are run through a terminal emulator, the popup is simply *there*,
    boxed, exactly as the human sees it:

        ┌────────────────────────────────────────────┐
        │=== rein: agent declares work on an issue ===│
        │   issue:    #100 "…"  [open]                │
        │ …                                           │
        │> │                                          │
        └────────────────────────────────────────────┘

    So extraction is geometry, not parsing: find the border rows, slice the
    columns strictly INSIDE them, and everything outside the box — the shell
    prompt painted above, the tmux status bar below — is excluded by construction.
    Leading spaces inside the box are KEPT: they are rein's own Form A indentation
    (`   issue:` / `             in …`), part of what the human read.

    The result is bounded to rein's own Form A: the `=== rein: …` header down to
    the `> ` prompt line (writePrompt, internal/ui/prompt) — stable anchors rein
    writes verbatim, so the slice is deterministic, not curation. Returns [] if the
    box (or the header) is not on screen yet, which is what `popup_forma_complete`
    uses to decide the frame is done.

    `answer` (the number the human typed) is recorded onto the trailing `>` prompt
    line, so the block reads `> <n>` — what the human saw AND did. The keystroke's
    echo is not captured (we snapshot BEFORE sending, since answering closes the
    popup and repaints over Form A), so recording what we sent is both faithful and
    deterministic.
    """
    display = screen.display()
    top = next((i for i, ln in enumerate(display) if _BOX_TOP.search(ln)), None)
    if top is None:
        return []
    left = min(display[top].find(c) for c in "┌╭┏" if c in display[top])
    right = max(display[top].rfind(c) for c in "┐╮┓" if c in display[top])
    bottom = next(
        (i for i in range(top + 1, len(display)) if _BOX_BOTTOM.search(display[i])),
        len(display),
    )
    # Strictly INSIDE the border columns and rows == the popup's content.
    lines = [ln[left + 1:right].rstrip() for ln in display[top + 1:bottom]]

    start = next((i for i, ln in enumerate(lines) if ln.startswith("=== rein:")), None)
    if start is None:
        return []
    end = next((i for i in range(start, len(lines)) if lines[i].startswith(">")), None)
    if end is None:
        return lines[start:]  # still painting: no `>` prompt line yet
    lines = lines[start:end + 1]
    if answer is not None:
        lines = [f"> {answer}" if ln.rstrip() == ">" else ln for ln in lines]
    return lines


def popup_forma_complete(screen: RenderedScreen) -> bool:
    """Is the popup's Form A FULLY painted on this screen — header through the
    `>` prompt rein blocks on?

    This is the SCREEN-STATE replacement for the old drain-to-quiescence timer.
    The old code matched "type the issue number" in the byte stream and then read
    until N ms passed with no new bytes, hoping the rest of the frame had landed —
    a timing bet. Here the completeness condition is stated directly: rein writes
    Form A once and then BLOCKS on input, so the final `>` prompt line being on
    screen IS the proof the frame is whole and will not change. No timer, no
    guess, and a snapshot taken at that moment is identical every run.
    """
    lines = popup_forma_from_screen(screen)
    return bool(lines) and lines[-1].startswith(">")


def fold_popup(transcript: str, popup_lines: list[str],
               anchor_prefix: str = SBX_TAG + "$ rein declare ") -> str:
    """Interleave the POPUP| Form A block into the ONE transcript, right after the
    `SBX| $ rein declare <n>` line — where the declare BLOCKED and the popup fired.

    That adjacency is the reviewable contrast (#82's "capture is structural"): on
    rein's own pty the declare goes straight to `confirmed` with NO inline Form A
    (approval routed to the popup); the POPUP| lines, sitting between the declare
    and its `confirmed`, are the Form A the human read in the popup. Mirrors
    write_ceremony's agent-vs-host two views in one artifact — here popup-vs-host.

    Deterministic: anchored to the FIRST declare-execution line (the sandbox `run`
    echoes it as `SBX| $ rein declare <n>`; rein's banner echo of the script body
    is `run rein declare <n>`, no `$`, so it does not match).
    """
    # rstrip the TAGGED line, not the content: Form A contains a genuinely blank
    # line (rein prints one before "To approve"), which must still carry the tag to
    # stay on the popup side of get_views — but must not leave trailing whitespace
    # in the checked-in golden, where an editor stripping it would read as drift.
    block = [""] + [(POPUP_TAG + ln).rstrip() for ln in popup_lines]
    out: list[str] = []
    injected = False
    for ln in transcript.split("\n"):
        out.append(ln)
        if not injected and ln.startswith(anchor_prefix):
            out.extend(block)
            injected = True
    return "\n".join(out)


# The line that STANDS IN for a real agent's TUI region in a golden (see
# collapse_agent_tui). Fixed text, so it is deterministic AND usable as the
# fold_popup anchor for a journey whose declare happens inside the agent's TUI.
AGENT_TUI_PLACEHOLDER = (
    "[claude's TUI — the real agent's prose, tool calls, spinners and token "
    "counts. NON-DETERMINISTIC, collapsed: what it SAID is not golden material. "
    "What it OBSERVABLY DID is asserted below and in the invariants.]"
)

# The rein line that ENDS the agent's region: rein's own exit accounting, printed
# on the host pty after the agent process is gone. Anchoring the collapse's end on
# it guarantees that security-relevant line survives the collapse verbatim.
#
# It ALSO matches the per-token `exit-revoke … failed` WARNING, because
# revokeRunWriteTokens (cmd/rein/run.go) prints that warning immediately before its
# own summary line — so on a partial-revoke run the warning is STRUCTURALLY the
# first exit-accounting line rein prints. Matching it here puts it OUTSIDE the
# collapsed region (and everything after it is preserved verbatim), rather than
# leaving a security-relevant line to be handled by the in-region keep filter alone.
AGENT_TUI_END_RE = (
    r"^rein: (?:revoked \d+ of \d+ write token"
    r"|warning: exit-revoke of a write token failed)"
)

# The lines INSIDE the agent's region that are rein's OWN host output and must
# therefore SURVIVE the collapse (see collapse_agent_tui). rein writes them to
# os.Stderr — i.e. the host pty — at column 0:
#   * `rein: …`      — e.g. the `rein: SESSION EXPIRED` banner headline
#                      (cmd/rein/run_sandboxed.go:printExpiryBanner)
#   * `=== rein: …`  — the banner form, e.g. the install NOTICE block
#                      (internal/ui/grant/notice.go:WriteInstallNotice, its
#                      non-interactive "plain stderr on the run terminal" surface)
# Anchored at column 0 on purpose: a real agent's TUI paints its content behind box
# art / glyphs / indentation, never flush-left with rein's own prefix (verified
# against captured real-claude pty streams — see the docstring below).
AGENT_TUI_KEEP_RE = r"^(?:=+ )?rein: "


def collapse_agent_tui(transcript: str, start_needle: str, *,
                       placeholder: str = AGENT_TUI_PLACEHOLDER,
                       end_pattern: str = AGENT_TUI_END_RE,
                       keep_pattern: str = AGENT_TUI_KEEP_RE) -> tuple[str, bool]:
    """Collapse a REAL agent's TUI region to a placeholder, KEEPING rein's own
    lines. Returns (transcript, anchors_found).

    WHY THIS IS NOT "DROPPING OUTPUT" (the doctrine question, #101). The golden
    model says: keep every line, normalize the volatile TOKENS at compare time. It
    holds because every other journey's agent is a deterministic bash script whose
    output is LINE-ORIENTED — a line, once printed, is final, and only tokens
    inside it vary. A REAL LLM's TUI is neither: it REDRAWS (so the ANSI-stripped
    byte stream is a paint history, not a picture — #100) and its content is
    genuinely non-deterministic (different prose, tool order, spinners, token
    counts, promo banners, timing). There is no token-level normalization of that;
    it is redraw+prose NOISE, of exactly the kind `build_raw_transcript` already
    drops when it discards sub-100% progress ticks. So this collapses it — once, at
    BUILD time, so the checked-in golden stays a HUMAN-REVIEWABLE artifact showing
    rein's own security surface (banner, injected contract, Form A, exit accounting)
    rather than 5000 lines of ANSI soup a reviewer would never read and that would
    be rewritten wholesale on every adopt.

    WHAT KEEPS IT HONEST — two anchors AND a keep filter:

    1. The region is bounded by two anchors that MUST both be found, and the caller
       treats a miss as a CEREMONY BREAK (exit 2), never as a silently smaller golden.
         start — the first line CONTAINING `start_needle` (use something rein itself
                 prints and the journey controls: the tail of rein's `rein: running:
                 <cmdline>` echo, i.e. the agent's own prompt text). The collapse
                 begins on the line AFTER it, so the whole banner + injected contract
                 echo is preserved verbatim — as is rein's `---` banner separator,
                 which it prints immediately after that echo and which is rein's own
                 line, not the agent's (a collapse that ate it would be dropping rein
                 output, the one thing the golden model never allows).
         end   — the first line after that matching `end_pattern` (rein's exit
                 accounting — the `revoked N of N` summary, or the per-token
                 `exit-revoke … failed` warning that structurally precedes it).
                 Everything rein prints from there on is preserved.

    2. This is a FILTER, not a delete. rein DOES have call sites that write to its
       OWN host pty (os.Stderr) strictly inside that window — `printExpiryBanner`'s
       `rein: SESSION EXPIRED` block (cmd/rein/run_sandboxed.go) and the
       non-interactive install-NOTICE surface (internal/ui/grant/notice.go) — so the
       region is NOT "pure agent TUI" (an earlier version of this comment claimed it
       was; it was false). Any line inside the region matching `keep_pattern`
       (`AGENT_TUI_KEEP_RE`: a column-0 `rein: …` or `=== rein: …`) is PRESERVED
       verbatim; only the runs of agent-TUI lines AROUND them collapse (one
       placeholder per contiguous run). So the doctrine holds inside this window
       exactly as everywhere else: a NEW rein line — especially a security-relevant
       one — lands in the golden and TRIPS DRIFT. It is not deleted at build time.

       Determinism is not weakened: rein prints nothing here on the happy path (the
       declare's Form A is routed to the tmux popup and the broker writes to
       helper.log), so a healthy run keeps exactly one placeholder and no kept lines.
       And a real claude TUI does not paint over the filter: it renders shell output
       and prose behind box art / glyphs / indentation, never flush-left with rein's
       own prefix. Verified against captured real-claude pty streams (an 893-line
       full session incl. a `rein declare` tool call, which renders as `⎿  $ rein
       declare <n>`): the only column-0 `rein: ` lines in them are rein's own
       pre-TUI banner lines, all OUTSIDE the region.
    """
    lines = transcript.split("\n")
    start = next((i for i, ln in enumerate(lines) if start_needle in ln), None)
    if start is None:
        return transcript, False
    if start + 1 < len(lines) and lines[start + 1].strip() == "---":
        start += 1  # rein's own banner separator — keep it on rein's side
    end_rx = re.compile(end_pattern)
    end = next((i for i in range(start + 1, len(lines)) if end_rx.search(lines[i])), None)
    if end is None:
        return transcript, False

    keep_rx = re.compile(keep_pattern)
    region: list[str] = []
    pending = False  # a contiguous run of agent-TUI lines awaiting its placeholder
    for ln in lines[start + 1:end]:
        if keep_rx.search(ln):
            if pending:
                region.append(placeholder)
                pending = False
            region.append(ln)  # rein's OWN line: survives, so it can trip drift
        else:
            pending = True
    if pending:
        region.append(placeholder)  # the trailing run of TUI lines
    elif placeholder not in region:
        # A (pathological) region with no collapsible line at all: still emit the
        # placeholder, which is ALSO this journey's fold_popup anchor for Form A.
        region.insert(0, placeholder)
    return "\n".join(lines[:start + 1] + region + lines[end:]), True


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
    # PR number inside a `gh pr create` URL/path, e.g. the gh-write journey's
    # `https://github.com/<owner>/<repo>/pull/<n>`. Generic (not a per-journey
    # whitelist): any `/pull/<digits>` collapses so two runs that mint DIFFERENT
    # PR numbers still match. BEFORE the hash rule for the same reason as
    # /issues/ above (bare digits, so the letter-requiring hash rule never eats
    # it). Distinct placeholder from <ISSUE> since a PR and an issue number are
    # different objects even though both also print as a bare `#<n>` elsewhere
    # (that bare form is already covered, and collapsed, by the `#\\d+` rule).
    (r"/pull/\d+", "/pull/<PR>"),
    (r"\bdeclare \d+", "declare <ISSUE>"),
    (r"issue number \(\d+\)", "issue number (<ISSUE>)"),
    (r"(?m)^> \d+$", "> <ISSUE>"),
    # the issue number the human typed into the tmux popup's Form A prompt,
    # folded into the transcript as `POPUP| > <n>` (see fold_popup).
    (r"(?m)^POPUP\| > \d+$", "POPUP| > <ISSUE>"),
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
#      client. This mirrors reality: `rein run -- <agent>` runs inside the
#      operator's tmux pane, and the broker it hosts launches the popup on that
#      same client.
#
# WHY NOT `tmux capture-pane`? Because it CANNOT SEE THE POPUP — verified, not
# assumed: with a real attached client rendering Form A in a popup, `list-panes`
# reports only the base pane, and capturing every pane finds no Form A anywhere. A
# popup is a client-owned OVERLAY, not an addressable pane, so the pane-snapshot
# route (#100's original proposal) is a dead end. The client's own pty IS the
# surface — and running THAT through a terminal emulator (RenderedScreen, above)
# gives the popup exactly as the human sees it, box and all.
#
# If tmux (or pyte) is not installed the popup surface cannot be driven at all; a
# journey must SKIP with exit 3 (see journey_tmux_popup_approval.py), never fake it
# and never exit 0.


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
    screen: RenderedScreen  # the attached client's pty, run through a terminal
    forma: list[str] = field(default_factory=list)

    def tmux_env(self) -> dict:
        """Env overlay that routes a SEPARATE rein process's approval to THIS
        session's popup: $TMUX (socket_path,pid,session — rein only checks it is
        non-empty) and $TMUX_PANE. Pass it as spawn_rein_run(extra_env=...)."""
        return {"TMUX": f"{self.sockpath},0,0", "TMUX_PANE": self.pane}

    def drive_popup(self, expect_pattern: str, answer: str, *, settle: float = 0.3,
                    timeout: int = 90, drain=None) -> list[str]:
        """Wait for the popup's Form A to be FULLY RENDERED on the attached client's
        SCREEN, read it off that screen, then type the answer. Returns the Form A
        lines (also stored on `self.forma`) for folding into the transcript.

        The popup grabs the client keyboard, so keys written to the client's pty
        reach `rein approval grant` inside the popup. `expect_pattern` must be text
        the popup PRINTS (e.g. "type the issue number"), not text from the command
        line that opened it.

        DETERMINISM, without a timer (#100). We pump the client's bytes into a real
        terminal emulator and wait for a SCREEN STATE: `expect_pattern` visible AND
        the Form A box painted through its trailing `>` prompt — the last thing rein
        writes before it BLOCKS on input, hence proof the frame is complete and
        stable. (The old code matched the pattern in the raw byte stream and then
        drained until N ms passed with no new bytes — a timing bet on the rest of
        the frame having arrived, which is exactly the fragility #100 is about.)

        We snapshot BEFORE sending the answer, because answering makes `rein
        approval grant` exit, which closes the popup and repaints over Form A.

        `drain` — other live ptys to keep reading while we block here. With a
        deterministic bash agent this is unnecessary (the agent is idle inside
        `rein declare`, printing nothing). With a REAL agent TUI it is REQUIRED:
        claude repaints while it waits for the declare to return, and an unread pty
        buffer fills and blocks it — see `drain_children`.
        """
        def ready(scr: RenderedScreen) -> bool:
            return scr.contains(expect_pattern) and popup_forma_complete(scr)

        found, _ = wait_for_screen(self.client, ready, timeout=timeout,
                                   screen=self.screen, drain=drain)
        if not found:
            raise RuntimeError(
                "the popup's Form A never fully rendered on the attached tmux "
                f"client within {timeout}s. Client screen was:\n{self.screen.text()}"
            )
        self.forma = popup_forma_from_screen(self.screen, answer=answer)
        time.sleep(settle)
        self.client.send(answer + "\r")
        return self.forma

    def render(self) -> str:
        """The attached client's RENDERED screen — what the human SAW (box art and
        all). Useful to show a reviewer; the popup's Form A alone (the part that
        goes in the golden) is `self.forma`."""
        return self.screen.text()

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
    exit. Raises RuntimeError if tmux is absent, or PyteMissing if the rendered-
    screen layer is unavailable (the caller should have checked `tmux_available()`
    + `pyte_available()` and SKIPped with exit 3)."""
    if not tmux_available():
        raise RuntimeError("tmux not on PATH — the popup approval surface cannot be driven")
    # The client's pty, run through a real terminal emulator: THE surface the popup
    # exists on (it is not a pane, so capture-pane cannot see it). Sized to the
    # client exactly, so pyte wraps where tmux wraps. Built FIRST so a missing pyte
    # raises before we start a tmux server we would then have to reap.
    screen = RenderedScreen(cols=width, rows=height)
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
        sess = TmuxPopupSession(socket=socket, client=client, log=log,
                                sockpath=sockpath, pane=pane, screen=screen)
        yield sess
    finally:
        if sess is not None:
            sess.close()  # kills the server AND closes the client
        elif server_up:
            _tmux(socket, "kill-server")  # setup raised before sess existed; don't leak it
