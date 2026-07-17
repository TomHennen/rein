"""Shared unittest scaffolding for the rein interactive suite.

We use the STDLIB `unittest` (not pytest) on purpose: this VM has no pip and
pytest isn't installed, and installing it would need a privileged `apt`
(CLAUDE.md forbids silent privileged installs). unittest gives us everything the
suite needs:

  - `@unittest.expectedFailure`  == pytest's xfail — a test that MUST fail today
    (TDD-red). If it later PASSES, unittest reports an "unexpected success" and
    the suite goes red, which is the signal to promote it to a real test once
    the feature ships.
  - `@unittest.skip(reason)`     == pytest's skip — for OPEN design decisions we
    must not encode yet.

`ReinTestCase` builds the binary once per process, exposes the REIN_* env + the
throwaway repo, and registers disposable branches for best-effort HOST-side
cleanup in tearDown.
"""

from __future__ import annotations

import os
import tempfile
import unittest

from tests.interactive import reinharness as H

# Issue #35: the issue is agent-declared at runtime (`rein declare <n>`) and
# rein FETCHES it before prompting — so declare-flow tests need a REAL issue
# on the throwaway repo, supplied by the human via env (an arbitrary number
# would 404 at fetch time and the declare would fail closed, by design):
#
#   REIN_ITEST_ISSUE   the number of a real (open) issue on REIN_TEST_REPO_A
#
# Tests that need it skip when unset. Pre-declaration tests (writes locked,
# synthesized denies) need no issue and always run.
DECLARE_ISSUE_ENV = "REIN_ITEST_ISSUE"


def declare_issue() -> int | None:
    """The real throwaway-repo issue number for declare tests, or None."""
    v = os.getenv(DECLARE_ISSUE_ENV)
    return int(v) if v and v.isdigit() else None


# Issue #69 scope-expansion demo: the SECOND throwaway repo and a real open
# issue ON it (fetched before the expansion prompt fires, same decision E).
# Both come from the human via env, defaulting from ./dev-env's
# REIN_TEST_REPO_B and a conventional issue #1 on it.
DECLARE_ISSUE_B_ENV = "REIN_ITEST_ISSUE_B"


def declare_issue_b() -> int | None:
    """The real repo-B issue number for the scope-expansion demo, or None."""
    v = os.getenv(DECLARE_ISSUE_B_ENV)
    return int(v) if v and v.isdigit() else None


_ready: dict = {}


def _ensure_ready() -> dict:
    """Build rein + shims once; cache the env. Shared by all test cases."""
    if not _ready:
        env = {**H.rein_env(), **H.init_app_env()}
        H.build_binaries(env)
        _ready["env"] = env
        _ready["repo"] = H.throwaway_repo(env)
    return _ready


class ReinTestCase(unittest.TestCase):
    """Base case: env + throwaway repo + disposable-branch cleanup."""

    def setUp(self):
        ready = _ensure_ready()
        self.env = ready["env"]
        self.repo = ready["repo"]
        self._branches: list[str] = []

    def new_branch(self, prefix: str = "reintest") -> str:
        """A disposable branch name, auto-deleted from the HOST in tearDown."""
        b = H.unique_branch(prefix)
        self._branches.append(b)
        return b

    def pinned_session_env(self) -> dict:
        """Write a temp session (throwaway repo, NO issue field — issue #35
        retired it) and return the env that selects it via REIN_SESSION_FILE.

        This makes the write tests SELF-CONTAINED: they don't depend on the
        machine's ambient ~/.config/rein/dev-session.yaml. The issue is
        agent-declared at runtime (`rein declare <n>`), never configured here.
        """
        d = tempfile.mkdtemp(prefix="rein-itest-sess-")
        path = os.path.join(d, "session.yaml")
        with open(path, "w") as f:
            f.write(
                "id: sess_itest_pinned\n"
                "role: implement\n"
                "repos:\n"
                f"  - {self.repo}\n"
            )
        return {"REIN_SESSION_FILE": path}

    def tearDown(self):
        # Best-effort: leaving a few timestamped `reintest-*` branches on the
        # throwaway is acceptable if a delete fails (README notes it).
        for b in self._branches:
            H.delete_branch(self.repo, b, self.env)
