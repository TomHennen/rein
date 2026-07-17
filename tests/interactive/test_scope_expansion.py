"""test_scope_expansion — the issue #69 scope-expansion journey, END TO END.

The journey (docs/session-scope-ux-mocks.md §1): "the agent needs a repo that
isn't in scope yet." The agent, running on repo A, declares an issue for repo
B via `rein declare <n> --repo B`. Because B is outside the session's standing
ceiling, this fires the SCOPE EXPANSION prompt on the host tty — a Form A
ceremony with a distinct "this ADDS a repo" header. On approval the repo joins
the run's ceiling, the agent's token is re-minted to cover it, and the agent
can clone + push to B. The `[y/N]` persist question follows the approval.

Every test drives writes INITIATED FROM INSIDE THE SANDBOX under `rein run`
and asserts BOTH sides:

  - HOST: the SCOPE EXPANSION prompt appeared (or, for the cross-owner deny,
    did NOT), and the correct marker printed.
  - SANDBOX: the in-sandbox commands' OWN outcomes via sentinels
    (SBX_DECLARE1_RC, SBX_CLONEB_RC, SBX_PUSHB_RC). Never a hang.

GATING: the expansion declare fetches the real issue on repo B before
prompting (decision E), so the approve/deny demos need:

    export REIN_TEST_REPO_B=<owner>/<second-throwaway>   (same owner as A)
    export REIN_ITEST_ISSUE_B=<a real open issue number on repo B>

./dev-env already exports REIN_TEST_REPO_B. The cross-owner deny needs
neither (it is refused locally, before any fetch), so it always runs.
"""

from __future__ import annotations

import os
import tempfile
import unittest

import pexpect

from tests.interactive import reinharness as H
from tests.interactive.itest_base import (
    DECLARE_ISSUE_B_ENV,
    ReinTestCase,
    declare_issue_b,
)

_NEEDS_ISSUE_B = unittest.skipUnless(
    declare_issue_b() and os.getenv("REIN_TEST_REPO_B"),
    f"gated: set REIN_TEST_REPO_B + {DECLARE_ISSUE_B_ENV} (a real open issue on repo B)",
)


class ScopeExpansionBase(ReinTestCase):
    """A session pinned to repo A ONLY — so repo B is genuinely out of scope
    and the declare must trigger the expansion path, not a plain confirm."""

    def setUp(self):
        super().setUp()
        self.repo_b = os.getenv("REIN_TEST_REPO_B", "")
        # The expansion prompt is an inline /dev/tty ceremony; force the tty
        # surface (not the tmux popup) so pexpect — which owns the pty and IS
        # the human — drives it, exactly as the write-approval suite does.
        self._approval_env = {"REIN_APPROVAL": "tty"}

    def a_only_session(self) -> tuple[str, dict]:
        """Write a temp session scoped to repo A only; return (path, env)."""
        d = tempfile.mkdtemp(prefix="rein-itest-sess-b-")
        path = os.path.join(d, "session.yaml")
        with open(path, "w") as f:
            f.write(
                "id: sess_itest_expansion\n"
                "role: implement\n"
                "repos:\n"
                f"  - {self.repo}\n"
            )
        env = {"REIN_SESSION_FILE": path}
        env.update(self._approval_env)
        return path, env


@_NEEDS_ISSUE_B
class ScopeExpansionApprove(ScopeExpansionBase):
    """The design-anchored happy path: declare B -> expansion prompt ->
    approve -> persist choice -> clone + push to B land on GitHub."""

    def test_expansion_approve_persist_then_push_to_b_succeeds(self):
        issue_b = declare_issue_b()
        branch_b = f"agent/{issue_b}/{H.unique_branch('exp')}"
        session_path, extra_env = self.a_only_session()
        wd = H.make_workdir()
        script = H.scope_expansion_script(self.repo, self.repo_b, issue_b, branch_b)
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env, extra_env=extra_env,
        )

        # HOST: the SCOPE EXPANSION prompt (not a plain issue declaration).
        run.expect_expansion_prompt(timeout=120)
        assert H.EXPANSION_BANNER in run.text(), "the header must mark this a SCOPE EXPANSION"
        # Approve with the issue number (Form A — the sole approval token).
        run.answer(str(issue_b))
        run.expect_expansion_approved(timeout=60)
        # Then the in-prompt persist question; answer 'y' to save repo B.
        run.expect_persist_question(timeout=30)
        run.answer("y")

        run.child.expect(pexpect.EOF, timeout=240)
        run.child.close()

        text = run.text()
        # SANDBOX: the expansion declare, the clone of B, and the push all
        # succeeded — the widening reached every surface (scope check + mint).
        self.assertEqual(run.declare_rc(1), 0, f"expansion declare must succeed\n{text}")
        self.assertEqual(run.named_rc("CLONEB"), 0, "clone of repo B must succeed after approval")
        self.assertEqual(run.named_rc("PUSHB"), 0, "push to repo B must succeed after approval")
        self.assertEqual(run.expansion_prompt_count(), 1, "exactly one expansion prompt for the run")
        # HOST: verify the branch actually landed on repo B via the operator's gh.
        self._branches_b = getattr(self, "_branches_b", [])
        self.assertTrue(
            H.branch_exists(self.repo_b, branch_b, self.env),
            "the agent's branch must exist on repo B (verified on GitHub)",
        )
        # Persistence: repo B was appended to the session file (the `y` answer).
        with open(session_path) as f:
            self.assertIn(self.repo_b, f.read(), "persist=y must save repo B to the session file")

        # Cleanup B's branch on the host (best-effort).
        H.delete_branch(self.repo_b, branch_b, self.env)


@_NEEDS_ISSUE_B
class ScopeExpansionDeny(ScopeExpansionBase):
    """Deny path: a wrong answer denies the expansion; the run continues at
    its ORIGINAL scope (repo A) and nothing about B is torn down or granted."""

    def test_wrong_answer_denies_expansion_and_b_stays_out_of_scope(self):
        issue_b = declare_issue_b()
        branch_b = f"agent/{issue_b}/{H.unique_branch('expdeny')}"
        _, extra_env = self.a_only_session()
        wd = H.make_workdir()
        script = H.scope_expansion_script(self.repo, self.repo_b, issue_b, branch_b)
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env, extra_env=extra_env,
        )

        run.expect_expansion_prompt(timeout=120)
        run.answer("0")  # wrong number -> denied
        run.expect_denied(timeout=60)

        run.child.expect(pexpect.EOF, timeout=240)
        run.child.close()

        self.assertNotEqual(run.declare_rc(1), 0, "denied expansion declare must fail")
        # The push to B must NOT succeed — B never entered scope.
        self.assertNotEqual(run.named_rc("PUSHB"), 0, "push to repo B must fail after a denied expansion")
        self.assertFalse(
            H.branch_exists(self.repo_b, branch_b, self.env),
            "nothing may reach repo B after a denied expansion",
        )


class ScopeExpansionCrossOwner(ScopeExpansionBase):
    """The same-owner rule is structural (the App installation is
    single-owner): a DIFFERENT-owner --repo is refused locally — no prompt,
    no network. Needs no issue (nothing is ever fetched), so it always runs."""

    def test_cross_owner_declare_refused_without_prompt(self):
        cross_repo = "some-other-owner/nope"
        _, extra_env = self.a_only_session()
        wd = H.make_workdir()
        script = H.cross_owner_declare_script(1, cross_repo)
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env, extra_env=extra_env,
        )

        run.child.expect(pexpect.EOF, timeout=120)
        run.child.close()

        text = run.text()
        self.assertNotEqual(run.declare_rc(1), 0, "cross-owner declare must be refused")
        self.assertEqual(run.expansion_prompt_count(), 0, "no prompt may fire for a cross-owner repo")
        self.assertEqual(run.prompt_count(), 0, "no issue prompt either")
        self.assertIn("single-owner", text, "the deny must explain the single-owner rule")


if __name__ == "__main__":
    unittest.main()
