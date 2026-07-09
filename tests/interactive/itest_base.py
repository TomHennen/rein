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

import reinharness as H

# The issue number the write tests pin their session to. It is ARBITRARY on
# purpose: the /dev/tty prompt only string-compares the typed line to
# `sess.Issue` (internal/ui/prompt — no GitHub lookup), and the minted token is
# scoped by REPO, not issue. So a distinctive non-#1 value both works AND proves
# the tests aren't secretly relying on the machine's default `issue: 1` session.
BOUND_ISSUE = 4242

_ready: dict = {}


def _ensure_ready() -> dict:
    """Build rein + shims once; cache the env. Shared by all test cases."""
    if not _ready:
        env = H.rein_env()
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

    def pinned_session_env(self, issue: int = BOUND_ISSUE) -> dict:
        """Write a temp session (throwaway repo + `issue`) and return the env that
        selects it via REIN_SESSION_FILE.

        This makes the write tests SELF-CONTAINED: they no longer depend on the
        machine's ambient ~/.config/rein/dev-session.yaml happening to bind a
        particular issue (which would otherwise ERROR on a machine without that
        file, or deny on one with a different issue number).
        """
        d = tempfile.mkdtemp(prefix="rein-itest-sess-")
        path = os.path.join(d, "session.yaml")
        with open(path, "w") as f:
            f.write(
                "id: sess_itest_pinned\n"
                "role: implement\n"
                "repos:\n"
                f"  - {self.repo}\n"
                f"issue: {issue}\n"
            )
        return {"REIN_SESSION_FILE": path}

    def tearDown(self):
        # Best-effort: leaving a few timestamped `reintest-*` branches on the
        # throwaway is acceptable if a delete fails (README notes it).
        for b in self._branches:
            H.delete_branch(self.repo, b, self.env)
