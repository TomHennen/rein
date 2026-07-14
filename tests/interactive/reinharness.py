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
import json
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
#   SBX_TAG / POPUP_TAG  — EXACT host-vs-agent split. The in-sandbox script tags
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

# rein's OWN output lines, at column 0, as rein writes them to the run terminal.
# The `(?:=+ )?` arm is LOAD-BEARING: grant.ShowInstallNotice prints its NOTICE as
# `=== rein: NOTICE — App not installed ===`. Do not "simplify" it away.
#
# Used by the REAL-AGENT journey to pull rein's own lines back out of a pane stream
# that is otherwise a full-screen, non-deterministic LLM TUI (see split_at_agent_launch).
REIN_LINE_RE = re.compile(r"^(?:=+ )?rein: ")


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

    # THE POPUP IS NOT THE ONLY BOX ON THE SCREEN. Under it sits whatever the pane is
    # painting — and when that pane is a full-screen TUI, it is FULL of box art (claude
    # draws `╭─── Claude Code v… ───╮` around its welcome panel and its input). Taking
    # the FIRST box-top on the render therefore locked onto CLAUDE's box, sliced its
    # columns, found no `=== rein:` header, and returned [] — so Form A never read as
    # complete, the popup went unanswered until `rein approval grant` timed out at 60s,
    # and rein (correctly) degraded to the inline /dev/tty prompt. That looks exactly
    # like a rein bug and is NOT one; it is this extractor picking the wrong box. It
    # only ever showed up under a REAL agent (a bash pane paints no box art), and it
    # cleared up "by itself" once claude's welcome box scrolled off — which is what made
    # it read as flaky. So: scan EVERY box on the screen and take the one that actually
    # CONTAINS rein's Form A header. Geometry still, just not the first-match kind.
    for top in (i for i, ln in enumerate(display) if _BOX_TOP.search(ln)):
        left = min(display[top].find(c) for c in "┌╭┏" if c in display[top])
        right = max(display[top].rfind(c) for c in "┐╮┓" if c in display[top])
        # This box's OWN bottom: the corner glyphs must sit in ITS left/right columns.
        # (A bare _BOX_BOTTOM match would stop at the underlying TUI's border row and
        # truncate the popup's content mid-box.)
        bottom = next(
            (i for i in range(top + 1, len(display))
             if len(display[i]) > right
             and display[i][left] in "└╰┗" and display[i][right] in "┘╯┛"),
            len(display),
        )
        # Strictly INSIDE the border columns and rows == this box's content.
        lines = [ln[left + 1:right].rstrip() for ln in display[top + 1:bottom]]

        start = next((i for i, ln in enumerate(lines) if ln.startswith("=== rein:")), None)
        if start is None:
            continue  # a box the PANE painted, not rein's popup — keep looking.
        end = next((i for i in range(start, len(lines)) if lines[i].startswith(">")), None)
        if end is None:
            return lines[start:]  # still painting: no `>` prompt line yet
        lines = lines[start:end + 1]
        if answer is not None:
            lines = [f"> {answer}" if ln.rstrip() == ">" else ln for ln in lines]
        return lines
    return []


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


def popup_block(popup_lines: list[str]) -> list[str]:
    """The popup's Form A as POPUP|-tagged lines, with a blank line before it.

    rstrips the TAGGED line, not the content: Form A contains a genuinely blank line
    (rein prints one before "To approve"), which must still carry the tag to stay on
    the popup side of the tag split — but must not leave trailing whitespace in the
    checked-in golden, where an editor stripping it would read as drift.
    """
    return [""] + [(POPUP_TAG + ln).rstrip() for ln in popup_lines]


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
    block = popup_block(popup_lines)
    out: list[str] = []
    injected = False
    for ln in transcript.split("\n"):
        out.append(ln)
        if not injected and ln.startswith(anchor_prefix):
            out.extend(block)
            injected = True
    return "\n".join(out)


def split_at_agent_launch(transcript: str, launch_needle: str) -> tuple[list[str], list[str], bool]:
    r"""Split a REAL-agent pane stream into (rein's launch surface, rein's later lines,
    found). Everything the agent painted is left OUT of both — by construction.

    THE PROBLEM. Every other journey's "agent" is a bash script we wrote: it is
    LINE-ORIENTED (a line, once printed, is final) and DETERMINISTIC, so its output
    belongs in the golden verbatim and the interleaving with rein's prompts IS the
    story. A REAL LLM is neither. It REDRAWS (so the pane's byte stream is a paint
    HISTORY, not a picture — #100) and its content genuinely varies run to run: prose,
    tool order, spinners, token counts, promo banners. There is no token-level
    normalization for that. So the real-agent journey does NOT put agent content in its
    compared golden at all; it compares rein's OWN output and shows the agent's session
    in a separate, uncompared artifact (see write_agent_session).

    THE SPLIT, which is the whole mechanism — one boundary, one regex:

      launch  — every line up to and INCLUDING the one containing `launch_needle`
                (the tail of rein's own `rein: running: <cmdline>` echo), plus the `---`
                separator rein prints right after it. This is rein's launch surface,
                VERBATIM: the sandbox preflight, the session/proxy/working-tree lines,
                the egress allowlist, the $HOME-is-hidden semantics, the writes-are-
                LOCKED notice, and the injected-agent-contract line. It is line-oriented
                and printed before the agent's TUI exists, so it is safe to keep whole —
                and it MUST be kept whole, because rein's banner body is INDENTED
                continuation text, not `rein: `-prefixed, and this journey is the ONLY
                compared golden that covers the claude-specific lines in it (the
                `--append-system-prompt` contract injection and the `\claude` escape
                hatch: no other journey runs claude as the sandboxed agent).

      tail    — from there on, ONLY rein's own lines (REIN_LINE_RE, a column-0
                `rein: …` / `=== rein: …`). That is rein's exit token accounting, and
                any line rein prints to its own terminal WHILE the agent runs — the
                `rein: SESSION EXPIRED` banner (cmd/rein/run_sandboxed.go
                printExpiryBanner) and the install NOTICE (internal/ui/grant/notice.go).
                A real claude never paints flush-left with rein's prefix (its output
                sits behind box art / glyphs / indentation — verified on captured pty
                streams), so this catches rein and only rein.

    WHY THIS IS SAFE, and simpler than the collapse it replaced: EVERY rein-emitted line
    lands in the compared golden — the banner whole, and every later rein line by regex.
    There is no uncompared region INSIDE the compared artifact for a new (possibly
    security-relevant) rein line to hide in, so no keep-vs-collapse filter is needed to
    rescue one. A brand-new rein line trips drift, exactly as in every other journey.

    `found` is False if the launch echo is not in the stream; the caller MUST treat that
    as a CEREMONY BREAK, never as a silently truncated golden.
    """
    lines = transcript.split("\n")
    i = next((n for n, ln in enumerate(lines) if launch_needle in ln), None)
    if i is None:
        return [], [ln for ln in lines if REIN_LINE_RE.match(ln)], False
    if i + 1 < len(lines) and lines[i + 1].strip() == "---":
        i += 1  # rein's own banner separator — keep it on rein's side
    return (lines[:i + 1],
            [ln for ln in lines[i + 1:] if REIN_LINE_RE.match(ln)],
            True)



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

    NOTHING is dropped here. Every line in a golden is compared. (A real agent's
    non-deterministic session is not IN a golden — it is a separate, uncompared
    artifact; see split_at_agent_launch and write_agent_session.)

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


# The SESSION ARTIFACT directory — NOT goldens, and deliberately not under golden/.
# A real LLM's session is committed and human-readable ("it helps me understand if the
# agent is confused") but it is NEVER COMPARED: its prose, turn count and tool ordering
# are not a regression signal, and a chronically-red journey trains everyone to ignore
# drift. Keeping it out of golden/ is what makes that structural rather than a promise:
# test_golden_shape only globs golden/*.txt, so nothing here is ever diffed, required to
# be normalize-idempotent, or flagged as an orphan golden. Behavior is asserted in the
# journey's own invariants (exit 2), not by reading this file.
AGENT_SESSION_DIR = Path(__file__).resolve().parent / "agent-sessions"


def write_agent_session(name: str, text: str) -> Path:
    """Write the SHOWN-not-compared record of a real agent's session (see above)."""
    AGENT_SESSION_DIR.mkdir(parents=True, exist_ok=True)
    p = AGENT_SESSION_DIR / name
    p.write_text(text)
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
# Driving a real popup needs a DEDICATED tmux server (`tmux -L <unique>`), so a
# journey NEVER touches the operator's own sessions and kills only its OWN socket
# on teardown — plus an ATTACHED client to answer the popup on. A popup is a
# CLIENT-OWNED overlay: it is NOT an addressable pane (it never appears in
# `list-panes`, and `send-keys` cannot reach it). It renders on, and grabs the
# keyboard of, an attached client, so the ONLY way to answer it is to write keys to
# that client's pty — which `drive_popup` does.
#
# WHY NOT `tmux capture-pane`? Because it CANNOT SEE THE POPUP — verified, not
# assumed: with a real attached client rendering Form A in a popup, `list-panes`
# reports only the base pane, and capturing every pane finds no Form A anywhere. A
# popup is a client-owned OVERLAY, not an addressable pane, so the pane-snapshot
# route (#100's original proposal) is a dead end. The client's own pty IS the
# surface — and running THAT through a terminal emulator (RenderedScreen, above)
# gives the popup exactly as the human sees it, box and all.
#
# ONE SHAPE, and it is the REAL one: `tmux_pane_session` / `TmuxPaneSession` (BELOW)
# — rein runs INSIDE a REAL tmux pane, launched the way a developer launches it
# (typed into the pane's shell), so $TMUX/$TMUX_PANE are INHERITED from tmux. rein's
# own output and the popup overlay SHARE ONE TERMINAL, exactly as they do on a
# developer's box.
#
# There USED to be a second shape (`tmux_popup_session`/`TmuxPopupSession`): rein on a
# SEPARATE pty with a SYNTHESIZED $TMUX aimed at a session whose pane was EMPTY. It is
# DELETED, deliberately and permanently. It proved the popup surface but could not see
# a popup-over-live-content bug (the popup had nothing to overlay), and it carried a
# real hazard (an empty sockpath makes $TMUX ",0,0" — still non-empty, so rein would
# fire a popup onto the OPERATOR's own default server). Do not reintroduce it: if a
# surface can't be driven for real, SKIP with exit 3.
#
# If tmux (or pyte) is not installed the popup surface cannot be driven at all; a
# journey must SKIP with exit 3 (see journey_tmux_popup_approval.py), never fake it
# and never exit 0.


def tmux_available() -> bool:
    """Is a `tmux` binary on PATH? The popup surface is undriveable without it."""
    return shutil.which("tmux") is not None


def _tmux(socket: str, *args: str, env: dict | None = None) -> subprocess.CompletedProcess:
    """Run `tmux -L <socket> <args>` on the DEDICATED server (never the default).

    `env` matters on the call that STARTS the server (`new-session`): the tmux
    server captures that environment ONCE, and every pane shell inherits it. A
    real-pane session therefore starts the server with `rein_env()`, so the rein
    the pane launches can mint (REIN_APP_*) and writes its helper.log where
    `helper_log_path(env)` will look for it (HOME / XDG_STATE_HOME). Every other
    call (send-keys, capture-pane, …) just talks to the already-running server, so
    it passes nothing.
    """
    return subprocess.run(
        ["tmux", "-L", socket, *args], capture_output=True, text=True, env=env
    )


# --------------------------------------------------------------------------
# TmuxPaneSession — rein INSIDE a REAL tmux pane (the developer's actual config)
# --------------------------------------------------------------------------
#
# THE CONFIGURATION. A developer runs `rein run -- <agent>` INSIDE a tmux pane, so
# the agent's output (a full-screen TUI, for a real agent) and rein's approval
# popup SHARE ONE TERMINAL. The now-DELETED TmuxPopupSession faked that: rein ran on
# a SEPARATE pty with a SYNTHESIZED $TMUX aimed at a session whose pane was EMPTY. It
# proved the popup surface — but with nothing running in the pane, there was
# nothing for the popup to OVERLAY, so a popup-over-live-TUI bug (a popup that
# corrupts the pane, a pane that never repaints, a popup that fails to take the
# keyboard from a live program) was structurally invisible to it.
#
# Here the command is TYPED INTO THE PANE's shell (`run_in_pane`), so
# $TMUX/$TMUX_PANE are INHERITED from tmux itself. Nothing is synthesized — and the
# synthesis hazard goes with it (an empty sockpath would have made $TMUX ",0,0",
# still non-empty, so rein would still fire a popup, but onto the OPERATOR's real
# default server).
#
# THREE SURFACES, each answering a question only it can answer — do not mix them up:
#
#   raw_stream()    the `pipe-pane` byte stream — everything the pane's program
#                   WROTE, append-only and COMPLETE. The LINE-ORIENTED source for
#                   the golden transcript. capture-pane cannot do this job: it
#                   shows only the VISIBLE screen, and while a TUI holds the
#                   ALTERNATE screen even `capture-pane -S -` yields NO scrollback
#                   at all. pipe-pane must start BEFORE anything runs in the pane,
#                   or the first bytes are raced away.
#   pane_text()     `capture-pane -p -J` — the RENDERED pane, right now (`-J` joins
#                   wrapped lines; `-e` only if you assert ATTRIBUTES, since it makes
#                   the output escape-noisy). What the pane LOOKS like — and the
#                   proof a popup is NOT in it.
#   client_screen() the attached client's pty, pyte-rendered — the ONLY surface a
#                   tmux popup exists on. popup.c holds a standalone `struct screen`
#                   registered via server_client_set_overlay(): no `window_pane`, so
#                   it never appears in `list-panes`, has no `#{popup_*}` format, and
#                   `capture-pane` cannot see it. Keys reach it only by writing to
#                   the attached client's pty (`send_client`), NEVER `send-keys`.
#
# That split is itself ASSERTABLE, and it is the whole point of running for real:
# while the popup is up, Form A is on client_screen() and ABSENT from pane_text(),
# which still shows the live command the popup is blocking on.
#
# TWO RULES, both learned the hard way:
#
#   1. DRAIN THE CLIENT, ALWAYS. A pty only yields bytes when you READ it (pexpect's
#      logfile_read fills on read, never on its own). If a long wait — a slow
#      sandbox clone — polls the pane's raw stream without reading the CLIENT, the
#      client's pty fills, tmux's attach blocks on write, the popup's render never
#      lands, nobody answers it, `rein approval grant` times out at 60s and rein
#      DEGRADES TO THE INLINE PROMPT. That looks exactly like a rein bug and is not
#      one. So draining is not a discipline: it lives INSIDE the one shared poll
#      primitive (`until`) that `until_raw` / `until_pane` / `until_client` /
#      `wait_stable` / `drive_popup` ALL go through, and the same read that drains
#      the client also feeds `self.screen`. The main thread stays the sole reader —
#      do not add a drain thread racing `send()`.
#   2. NEVER ASSERT ON A SINGLE `capture-pane` SHOT — it races the redraw. Retry the
#      predicate (`until_pane`, ~50ms poll; fzf's `Tmux#until` shape). For an
#      assertion with NO anchor string ("the pane repainted after the popup closed"),
#      wait for QUIESCENCE instead: `wait_stable(ms)` returns the render once it has
#      stopped changing.
#
# Geometry is PINNED (`-x`/`-y` PLUS `window-size manual` AND `status off`), or a
# client attaching at another size resizes the window and reflows every wrapped line
# in the golden. TERM is set deliberately (the popup's box-drawing depends on it).
# The pane's shell gets a FIXED `PS1` via `--rcfile` — bash IGNORES an exported PS1
# and would otherwise bake its own version (`bash-5.2$`) into the golden.


class AsciicastRecorder:
    """An asciicast v2 writer — the RECORDING surface for the README demo (#99).

    OPT-IN AND DEFAULT-OFF. A TmuxPaneSession only ever has one if a caller asks for
    it (`start_recording`); every journey leaves it None and is byte-identical.

    WHAT IT RECORDS, and why it is the ONLY thing worth recording: the bytes read from
    the ATTACHED CLIENT's pty. That surface carries the pane's content AND the popup
    composited on top of it — the tmux popup is a client-owned overlay, so `pipe-pane`
    (pane-only) and `capture-pane` structurally cannot see it. A recording made from
    either would show rein's write-approval Form A nowhere at all, which is the one
    frame the demo exists for.

    The harness already reads that pty on EVERY poll iteration (`pump_client` — rule
    1's drain). This just TIMESTAMPS those reads. No extra reader, no second thread:
    the drain and the recording are the same read.

    Format (asciicast v2): a JSON header line, then one JSON array per event —
        [<seconds since start, float>, "o", "<the bytes>"]
    which `agg` renders straight to a GIF.
    """

    def __init__(self, path: str, *, width: int, height: int, term: str = "tmux-256color"):
        self.path = path
        self.t0 = time.time()
        self._f = open(path, "w", encoding="utf-8")
        header = {
            "version": 2,
            "width": width,
            "height": height,
            "timestamp": int(self.t0),
            "env": {"TERM": term, "SHELL": os.environ.get("SHELL", "/bin/bash")},
        }
        self._f.write(json.dumps(header) + "\n")
        self._f.flush()

    def write(self, data: str) -> None:
        """Append one output event. Flushed immediately: a real-agent take runs for
        minutes, and an interrupted one must still leave a playable cast."""
        if not data or self._f.closed:
            return
        self._f.write(json.dumps([round(time.time() - self.t0, 6), "o", data]) + "\n")
        self._f.flush()

    def close(self) -> None:
        if not self._f.closed:
            self._f.close()


@dataclass
class TmuxPaneSession:
    """A dedicated-socket tmux session running a REAL pane (a bare shell), with a
    `pipe-pane` capture and a pexpect-attached client. Build it via
    `tmux_pane_session()` (a context manager that ALWAYS kills the server). See the
    section comment above for the three surfaces and the two rules."""

    socket: str
    session: str
    pane: str
    sockpath: str
    width: int
    height: int
    pipe_path: str
    client: pexpect.spawn
    log: io.StringIO
    screen: RenderedScreen  # the client's pty, run through a terminal (the popup's home)
    forma: list[str] = field(default_factory=list)
    pane_while_popup: str = ""
    # OPT-IN, DEFAULT-OFF (#99). None => `pump_client` behaves EXACTLY as before, so
    # every journey is byte-identical; only the demo recorder ever sets it.
    recorder: AsciicastRecorder | None = None
    _final_raw: str | None = None

    # -- the three surfaces --------------------------------------------------

    def raw_stream(self) -> str:
        """Everything the pane's program has written (`pipe-pane`), append-only —
        the source `build_raw_transcript` turns into the golden.

        `close()` snapshots the file first, so a caller can still read the WHOLE
        stream after the context manager has torn the server (and its scratch dir)
        down.
        """
        if self._final_raw is not None:
            return self._final_raw
        try:
            with open(self.pipe_path, "r", errors="replace") as f:
                return f.read()
        except FileNotFoundError:
            return ""

    def pane_text(self) -> str:
        """The RENDERED pane right now (`capture-pane -p -J`; `-J` joins wrapped
        lines). A popup is NOT in here — that is the point, and it is assertable."""
        return _tmux(self.socket, "capture-pane", "-p", "-J", "-t", self.session).stdout

    def client_screen(self) -> str:
        """The attached client's screen as TEXT — the pane's content WITH any popup
        overlaid on top. Pumps first, so it is current. For a predicate that needs the
        screen OBJECT (`popup_forma_complete`), use `until_client` with a callable."""
        self.pump_client()
        return self.screen.text()

    # -- input ---------------------------------------------------------------

    def send_pane(self, *keys: str) -> None:
        """`tmux send-keys` into the PANE (its shell / the program it runs). Reaches
        the pane's program; can NEVER reach a popup."""
        _tmux(self.socket, "send-keys", "-t", self.session, *keys)

    def send_pane_literal(self, text: str) -> None:
        """Type LITERAL text into the pane (`send-keys -l`), no Enter — so a
        command's own characters are never interpreted as tmux key names.

        The trailing `--` is LOAD-BEARING: without it tmux parses text that STARTS with
        a dash as its own options, so typing `rein run -- claude …` a chunk at a time
        silently loses the `--` separator and the command runs mangled (observed while
        recording the demo). `--` ends tmux's option parsing; text is text."""
        _tmux(self.socket, "send-keys", "-t", self.session, "-l", "--", text)

    def run_in_pane(self, command: str) -> None:
        """Type `command` into the pane's shell and press Enter — exactly what a
        developer does. Whatever it starts INHERITS $TMUX/$TMUX_PANE from tmux."""
        self.send_pane_literal(command)
        self.send_pane("Enter")

    def send_client(self, text: str) -> None:
        """Write keys to the ATTACHED CLIENT's pty. The ONLY way to answer a popup
        (it grabs the client's keyboard; `send-keys` cannot reach it)."""
        self.client.send(text)

    # -- draining + waiting --------------------------------------------------

    def pump_client(self, seconds: float = 0.15) -> None:
        """Read whatever the client's pty has RIGHT NOW and feed it into
        `self.screen`. ONE act, TWO (or three) jobs: it is rule 1's drain (an unread
        pty fills and stalls tmux's attach, and with it the popup), the render update,
        AND — only if a caller opted in via `start_recording` — the asciicast tap.
        Every poll step calls it; nothing else reads the client."""
        deadline = time.time() + seconds
        while True:
            try:
                data = self.client.read_nonblocking(size=65536, timeout=0.05)
            except (pexpect.TIMEOUT, pexpect.EOF, OSError, ValueError):
                break
            self.screen.feed(data)
            if self.recorder is not None:  # default None: no-op, byte-identical (#99)
                self.recorder.write(data)
            if time.time() >= deadline:
                break

    # -- recording (opt-in; #99's README demo) --------------------------------

    def start_recording(self, path: str, *, term: str = "tmux-256color") -> AsciicastRecorder:
        """Begin writing every subsequent CLIENT read to `path` as asciicast v2.

        The client's pty is the recording surface BECAUSE it is the only one that
        carries the pane AND the popup overlaid on it (see AsciicastRecorder). t=0 is
        NOW, so the cast opens on whatever the caller does next — not on the harness's
        own attach/priming keystrokes.
        """
        self.recorder = AsciicastRecorder(path, width=self.width, height=self.height,
                                          term=term)
        return self.recorder

    def stop_recording(self) -> None:
        if self.recorder is not None:
            self.recorder.close()
            self.recorder = None

    def hold(self, seconds: float) -> None:
        """Idle for `seconds` while STILL DRAINING the client (rule 1) — a bare
        `time.sleep` here would stall tmux's attach and freeze the pane. Used by the
        demo to leave the approval Form A on screen long enough for a viewer to READ
        it (a plain sleep would also record nothing)."""
        self.until(lambda: False, timeout=seconds, poll=0.05)

    def until(self, pred, *, timeout: float = 60.0, poll: float = 0.05) -> bool:
        """THE poll primitive: re-evaluate `pred()` every `poll` seconds until it is
        true or `timeout` expires (fzf's `Tmux#until` shape — never assert on a single
        shot, a render races the predicate). EVERY wait goes through here, and every
        iteration PUMPS THE CLIENT first, so rule 1's drain cannot be forgotten by a
        caller that is waiting on something else entirely."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            self.pump_client()
            if pred():
                return True
            time.sleep(poll)
        self.pump_client()
        return bool(pred())

    def until_raw(self, needle, *, timeout: float = 60.0, poll: float = 0.1) -> bool:
        """Wait for `needle` (a str, or a compiled regex) in the `pipe-pane` byte
        stream — the append-only surface, so a marker that has already SCROLLED OFF
        the screen still counts. Use this for flow progress; `until_pane` for what is
        ON screen."""
        pat = needle if hasattr(needle, "search") else re.compile(re.escape(needle))
        return self.until(lambda: bool(pat.search(strip_ansi(self.raw_stream()))),
                          timeout=timeout, poll=poll)

    def until_pane(self, pred_or_needle, *, timeout: float = 60.0,
                   poll: float = 0.05) -> bool:
        """Wait until the RENDERED pane satisfies a predicate (or contains a string).
        Re-captures every poll — a single `capture-pane` shot races the redraw."""
        pred = (pred_or_needle if callable(pred_or_needle)
                else (lambda scr: pred_or_needle in scr))
        return self.until(lambda: bool(pred(self.pane_text())), timeout=timeout, poll=poll)

    def until_client(self, pred_or_needle, *, timeout: float = 60.0,
                     poll: float = 0.1) -> bool:
        """Wait until the CLIENT's rendered screen satisfies a predicate (or contains
        a string) — the surface a popup lives on. A callable receives the
        RenderedScreen itself, so a richer screen state (`popup_forma_complete`) is
        expressible, not just a substring."""
        if callable(pred_or_needle):
            pred = (lambda: pred_or_needle(self.screen))
        else:
            pred = (lambda: self.screen.contains(pred_or_needle))
        return self.until(pred, timeout=timeout, poll=poll)

    def wait_stable(self, ms: int = 300, *, timeout: float = 20.0,
                    poll: float = 0.05) -> str:
        """QUIESCENCE: return the pane's render once it has STOPPED CHANGING for `ms`
        milliseconds. The tool for an assertion with NO anchor string — "the pane
        REPAINTED after the popup closed" has nothing to grep for, so you wait for the
        redraw to settle and then look at the settled frame."""
        need = ms / 1000.0
        last = None
        stable_since = time.time()
        deadline = time.time() + timeout
        while time.time() < deadline:
            self.pump_client()
            cur = self.pane_text()
            if cur != last:
                last, stable_since = cur, time.time()
            elif time.time() - stable_since >= need:
                return cur
            time.sleep(poll)
        return last if last is not None else self.pane_text()

    # -- the popup -----------------------------------------------------------

    def drive_popup(self, expect_pattern: str, answer: str, *, timeout: float = 120.0,
                    settle: float = 0.3) -> list[str]:
        """Wait for the popup's Form A to be FULLY PAINTED on the attached client's
        RENDERED SCREEN (over the LIVE pane), snapshot it, then type the answer INTO
        THE CLIENT — where the popup's keyboard is. Returns the Form A lines (also on
        `self.forma`) for folding into the transcript.

        Order is load-bearing, for determinism AND for the popup-over-live-pane proof:

          * poll the CLIENT's screen (the popup is nowhere else) for a SCREEN STATE —
            `expect_pattern` visible AND the box painted through its trailing `>`
            prompt (`popup_forma_complete`), the last thing rein writes before it
            BLOCKS on input, hence proof the frame is whole and will not change. No
            timer, no drain-to-quiescence bet (#100). Polling through `until` (not
            `client.expect`) also keeps ONE reader on the pty.
          * snapshot `pane_while_popup` (`capture-pane`) BEFORE answering: with the
            popup up, Form A is on the client and ABSENT from the pane, which still
            shows the live command it is blocking on. That contrast is the direct
            evidence the popup OVERLAYS rather than PRINTS — observable only because
            something real is running in the pane.
          * read Form A off the RENDERED screen (`popup_forma_from_screen`), NOT the
            client's raw bytes: with a live pane those bytes interleave the pane's own
            writes, and a row the popup paints blank lets stale pane text bleed INSIDE
            the box. On the render the overlay is genuinely on top, so the geometry
            slice is truthful.
          * and only THEN send the answer — answering makes `rein approval grant`
            exit, which closes the popup and repaints over Form A.
        """
        nudged = [0.0]

        def ready(scr: RenderedScreen) -> bool:
            if scr.contains(expect_pattern) and popup_forma_complete(scr):
                return True
            # ASK tmux TO REDRAW THE CLIENT while we wait. The popup is an OVERLAY the
            # server composites onto the client; the pane under it here is a live,
            # repainting TUI. A refresh forces the overlay to be drawn again, which is
            # what a human pressing any key would cause — so a Form A that is up on the
            # server cannot sit unread on a stale client render. That matters because an
            # unread popup dies at `rein approval grant`'s 60s timeout and rein then
            # (correctly) degrades to the inline /dev/tty prompt, which reads exactly like
            # a rein bug and is NOT one. Slow cadence, so we never fight the paint; a
            # no-op for a quiet pane (the deterministic popup journey).
            now = time.time()
            if now - nudged[0] > 1.0:
                nudged[0] = now
                with contextlib.suppress(Exception):
                    _tmux(self.socket, "refresh-client")
            return False

        if not self.until_client(ready, timeout=timeout):
            raise RuntimeError(
                f"the popup's Form A never fully rendered on the attached tmux client "
                f"within {timeout}s. If rein fell back to the INLINE prompt, the usual "
                f"cause is an UNDRAINED client pty (rule 1), not a rein bug. Client "
                f"screen was:\n{self.screen.text()}"
            )
        self.pane_while_popup = self.pane_text()
        self.forma = popup_forma_from_screen(self.screen, answer=answer)
        time.sleep(settle)
        self.send_client(answer + "\r")
        return self.forma

    def close(self) -> None:
        self.stop_recording()  # no-op unless a caller opted in
        # Snapshot the pipe-pane stream BEFORE the server (and the scratch dir it
        # writes into) go away, so the transcript survives teardown.
        if self._final_raw is None:
            self._final_raw = self.raw_stream()
        _tmux(self.socket, "kill-server")
        try:
            self.client.close(force=True)
        except Exception:
            pass
        # kill-server does not always unlink the socket FILE, and a journey must not
        # litter /tmp/tmux-<uid>/ with a dead socket per run. OURS only, by name.
        if self.sockpath:
            with contextlib.suppress(OSError):
                os.unlink(self.sockpath)


@contextlib.contextmanager
def tmux_pane_session(*, env: dict | None = None, width: int = 200, height: int = 50,
                      term: str = "tmux-256color", attach_timeout: float = 10.0):
    """Stand up a dedicated-socket tmux session with a REAL pane (a bare shell), a
    `pipe-pane` capture already running, and a pexpect-attached client; yield a
    TmuxPaneSession; ALWAYS kill the server on exit.

    - `env` seeds the tmux SERVER, and every pane shell inherits it — so pass
      `rein_env()`: the rein the pane launches needs REIN_APP_* to mint, and
      HOME/XDG_STATE_HOME so the helper.log it writes is the one a journey then
      reads back via `helper_log_path(env)`.
    - The pane's shell is `bash --noprofile --rcfile <tmp>` with a FIXED `PS1='$ '`.
      Not cosmetic: bash IGNORES an exported PS1, and would otherwise bake its own
      version into the golden (`bash-5.2$`), drifting across machines. The fixed
      prompt also makes the pane read like a real terminal.
    - `pipe-pane` starts AFTER `new-session -d` but BEFORE anything is typed, so the
      first bytes of the first command are never raced away.

    Raises RuntimeError if tmux is absent, PyteMissing if the rendered-screen layer
    is unavailable (the caller should have checked `tmux_available()` +
    `pyte_available()` and SKIPped with exit 3).
    """
    if not tmux_available():
        raise RuntimeError("tmux not on PATH — a real tmux pane cannot be driven")
    # Built FIRST so a missing pyte raises (PyteMissing) before we start a tmux
    # server we would then have to reap. Sized to the pane/client exactly, so the
    # emulator wraps where tmux wraps.
    screen = RenderedScreen(cols=width, rows=height)
    socket = f"reinpane-{os.getpid()}-{uuid.uuid4().hex[:6]}"
    _tmux(socket, "kill-server")  # clean slate on OUR socket only
    time.sleep(0.2)
    scratch = tempfile.mkdtemp(prefix="rein-pane-")
    rcfile = os.path.join(scratch, "bashrc")
    with open(rcfile, "w") as f:
        f.write("PS1='$ '\n")
    pipe_path = os.path.join(scratch, "pane.raw")

    # Everything from new-session onward is inside the try, so ANY failure during
    # setup still kills the dedicated server in the finally — the unique socket name
    # means a later call's kill-server would NOT reap an orphan, so don't leak one.
    sess = None
    server_up = False
    try:
        r = _tmux(
            socket, "new-session", "-d", "-s", "w",
            "-x", str(width), "-y", str(height),
            "-e", f"TERM={term}",
            f"bash --noprofile --rcfile {rcfile}",
            env=env,  # the SERVER's env; every pane shell inherits it
        )
        if r.returncode != 0:
            raise RuntimeError(f"tmux new-session failed: {r.stderr.strip() or r.stdout.strip()}")
        server_up = True
        # PIN the geometry: without `window-size manual`, a client attaching at a
        # different size RESIZES the window and reflows every wrapped line in the
        # golden. `status off` keeps the status bar out of the client's render.
        _tmux(socket, "set", "-g", "window-size", "manual")
        _tmux(socket, "set", "-g", "status", "off")
        _tmux(socket, "set", "-g", "default-terminal", term)
        pane = _tmux(socket, "list-panes", "-t", "w", "-F", "#{pane_id}").stdout.strip()
        sockpath = _tmux(socket, "display-message", "-p", "#{socket_path}").stdout.strip()
        if not pane:
            raise RuntimeError("tmux did not report a pane id; refusing to proceed")
        # The pane's RAW byte stream, captured from before the first keystroke.
        _tmux(socket, "pipe-pane", "-o", "-t", "w", f"cat >> {pipe_path}")
        log = io.StringIO()
        client = pexpect.spawn(
            "tmux", ["-L", socket, "attach", "-t", "w"],
            encoding="utf-8", codec_errors="replace",
            timeout=30, dimensions=(height, width),
            env={**(env or os.environ), "TERM": term},
        )
        client.logfile_read = log
        sess = TmuxPaneSession(
            socket=socket, session="w", pane=pane, sockpath=sockpath,
            width=width, height=height, pipe_path=pipe_path,
            client=client, log=log, screen=screen,
        )
        # The client must be ATTACHED before a popup can render. POLL for the pane's
        # shell prompt on the client's screen rather than sleeping blind — and the
        # poll drains the client, which is what attaching needs anyway.
        if not sess.until_client("$", timeout=attach_timeout):
            raise RuntimeError("the tmux client never rendered the pane's shell prompt")
        # The shell printed its FIRST prompt before pipe-pane could attach (a race we
        # cannot win: the pane's program starts with the session). Press Enter once so
        # a prompt is emitted INTO the capture — then the transcript opens on
        # `$ <the command the developer typed>`, like a real terminal, instead of a
        # naked command line.
        sess.send_pane("Enter")
        if not sess.until_raw("$ ", timeout=10.0):
            raise RuntimeError("the pane's shell prompt never reached the pipe-pane capture")
        yield sess
    finally:
        if sess is not None:
            sess.close()  # kills the server AND closes the client
        elif server_up:
            _tmux(socket, "kill-server")  # setup raised before sess existed; don't leak it
        shutil.rmtree(scratch, ignore_errors=True)


# claude's folder-trust dialog, as it renders ("Quick safety check: Is this a project
# you created or one you trust?" / "❯ 1. Yes, I trust this folder").
CLAUDE_TRUST_DIALOG_RE = re.compile(
    r"(?i)(quick safety check|trust this folder|project you created or one you trust)"
)


def dismiss_claude_trust_dialog(pane: TmuxPaneSession, *, timeout: float = 45.0) -> bool:
    """PLUMBING, not ceremony: answer claude's folder-trust dialog if it appears in
    the pane, and carry on if it does not. Returns whether it fired.

    WHY IT MUST BE HANDLED: rein gives the agent an EPHEMERAL $HOME, so claude has no
    persisted config and treats the fresh /tmp checkout as an unknown folder. The
    dialog then BLOCKS the session forever if nobody answers (a spike sat on it for
    420s). There is no way to skip it for an INTERACTIVE session — only `-p` /
    non-TTY invocations bypass it, and this journey needs a real TUI.

    WHY IT IS NOT A JOURNEY STEP: it is claude's UX, not rein's story. Asserting on it
    would make the journey hostage to a third-party dialog — a future claude (or a
    persisted claude config dir) that stops asking would turn a healthy run red. So it
    is dismissed here, silently, with a SHORT timeout (it paints within seconds of
    startup if at all), and it never appears in the golden's narrative.

    Detected on the pane's RENDER (`until_pane`, which also drains the client), never
    in the raw byte soup: a dialog is a redrawing surface. Enter takes the highlighted
    default, `1. Yes, I trust this folder`.

    The wait ends as soon as EITHER the dialog paints (dismiss it) OR claude's main TUI
    is up without it (nothing to dismiss — carry straight on). So a claude that stops
    asking costs the journey nothing, and the `timeout` is only ever paid if claude
    never comes up at all — the window is generous because the dialog can only paint
    after rein's sandbox preflight + srt launch have brought claude up.
    """
    seen = {"dialog": False}

    def ready(scr: str) -> bool:
        if CLAUDE_TRUST_DIALOG_RE.search(scr):
            seen["dialog"] = True
            return True
        # The main TUI is live and it never asked. Keyed on claude's INPUT BOX (its
        # border art + the `❯` caret), NOT on a footer hint: the hints are WORDING and
        # they move. This helper used to test only `? for shortcuts`, which a session
        # started with --dangerously-skip-permissions never prints — and the current
        # claude prints no `esc to interrupt` on this surface either. So when the dialog
        # did not fire, the helper burned its FULL timeout; the agent then declared while
        # nobody was armed at drive_popup, the popup went unanswered for its 60s, and rein
        # (correctly) degraded to the inline /dev/tty prompt — which reads exactly like a
        # rein bug and is not one. The dialog is claude's FIRST screen (it replaces the
        # TUI), and it is checked above on the same render, so "TUI up" cannot mask it.
        return any(m in scr for m in ("? for shortcuts", "esc to interrupt", "❯", "╭"))

    pane.until_pane(ready, timeout=timeout)
    if not seen["dialog"]:
        # Guard the one race the marker test cannot see: a dialog caught HALF-PAINTED
        # (its box art up, its text not yet) would read as "TUI live" and we would never
        # answer it — and an unanswered dialog blocks the session forever. Re-test
        # briefly; if the text lands, dismiss it. Cheap, and it cannot hang.
        pane.until_pane(lambda scr: bool(CLAUDE_TRUST_DIALOG_RE.search(scr)), timeout=3)
        if not CLAUDE_TRUST_DIALOG_RE.search(pane.pane_text()):
            return False
        seen["dialog"] = True
    pane.send_pane("Enter")
    return True
