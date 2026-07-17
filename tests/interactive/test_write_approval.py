"""test_write_approval — the issue #35 declare-first write loop, END TO END.

The model under test (docs/35-design-proposal.md): the agent has NO GitHub
write path until it DECLARES which issue its work is for (`rein declare <n>`)
and a human CONFIRMS it on the terminal (Form A: the fetched title + state +
home repo are displayed; the human types the displayed number). Push refs are
then still VERIFIED: they must match agent/<issue>/<nonce> and resolve to a
confirmed issue.

Every test drives writes INITIATED FROM INSIDE THE SANDBOX under `rein run`
and asserts BOTH sides:

  - HOST: the Form A prompt appeared (or, for pre-declaration writes, did
    NOT), and the correct approved/denied marker printed.
  - SANDBOX: the in-sandbox command's OWN outcome via explicit sentinels —
    `SBX_DECLARE1_RC=<code>` for the declare, `SBX_PUSH<n>_RC=<code>` for each
    push. Never a hang.

GATING: the declare fetches the REAL issue from GitHub before prompting
(decision E — no prompt without a fetched title), so declare-flow tests need a
real issue on the throwaway repo:

    export REIN_ITEST_ISSUE=<number of a real issue on REIN_TEST_REPO_A>

Without it those tests SKIP. The pre-declaration test (writes locked, the
synthesized `fatal: remote error` deny) needs no issue and always runs.
"""

from __future__ import annotations

import unittest

import pexpect

from tests.interactive import reinharness as H
from tests.interactive.itest_base import DECLARE_ISSUE_ENV, ReinTestCase, declare_issue

_NEEDS_ISSUE = unittest.skipUnless(
    declare_issue(),
    f"gated: set {DECLARE_ISSUE_ENV} to a real issue number on the throwaway repo",
)


class PreDeclarationLock(ReinTestCase):
    """Writes are locked before any declaration — no prompt, no upload."""

    def test_push_without_declare_fails_with_remote_error(self):
        """Pre-declaration push -> synthesized ERR advertisement -> git prints
        `fatal: remote error: rein: writes are locked ...` and exits cleanly;
        reads still flow; NO prompt ever fires; nothing lands on the remote."""
        branch = self.new_branch("reintest-predeclare")
        wd = H.make_workdir()
        script = H.clone_and_push_script(self.repo, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.child.expect(pexpect.EOF, timeout=180)
        run.child.close()

        self.assertIn("SBX_CLONE_OK", run.text(), "reads must still flow pre-declaration")
        self.assertEqual(run.prompt_count(), 0, "no prompt may fire on a write attempt (prompts live at declare time)")
        self.assertIn(H.PRE_DECLARE_LOCKED, run.text(), "the agent must see the instructive locked message")
        self.assertIn("rein declare", run.text(), "the deny must name the exact next step")
        rc = run.sentinel_rc(1)
        self.assertIsNotNone(rc, "push must terminate (never hang)")
        self.assertNotEqual(rc, 0, "pre-declaration push must fail")
        self.assertFalse(H.branch_exists(self.repo, branch, self.env), "nothing may reach the remote")


@_NEEDS_ISSUE
class DeclareConfirmPush(ReinTestCase):
    """The full declare -> Form A confirm -> verified push loop."""

    def _agent_branch(self, issue: int) -> str:
        return f"agent/{issue}/{H.unique_branch('t')}"

    def test_declare_approve_then_push_succeeds(self):
        """Declare -> prompt -> type the number -> [approved] -> push to
        agent/<n>/<nonce> succeeds; exactly one prompt for the run."""
        issue = declare_issue()
        branch = self._agent_branch(issue)
        self._branches.append(branch)
        wd = H.make_workdir()
        script = H.clone_declare_push_script(self.repo, issue, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.expect_prompt(timeout=120)
        run.answer(str(issue))
        run.expect_approved(timeout=60)

        run.child.expect(r"SBX_PUSH1_RC=\d+", timeout=120)
        run.wait(timeout=60)

        self.assertEqual(run.declare_rc(), 0, "declare should exit 0 after confirmation")
        self.assertEqual(run.sentinel_rc(1), 0, "verified push should succeed after confirmation")
        self.assertEqual(run.prompt_count(), 1, "exactly one Form A prompt for the run")
        self.assertTrue(H.branch_exists(self.repo, branch, self.env), "branch must appear on throwaway")

    def test_wrong_answer_denies_declare_and_writes_stay_locked(self):
        """A wrong number -> [denied] -> declare fails -> the push is still
        pre-declaration and fails cleanly; nothing on the remote."""
        issue = declare_issue()
        branch = self._agent_branch(issue)
        self._branches.append(branch)
        wd = H.make_workdir()
        script = H.clone_declare_push_script(self.repo, issue, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.expect_prompt(timeout=120)
        run.answer(str(issue + 1))  # NOT the displayed number
        run.expect_denied(timeout=60)

        run.child.expect(pexpect.EOF, timeout=180)
        run.child.close()

        self.assertNotEqual(run.declare_rc(), 0, "denied declare must exit nonzero")
        rc = run.sentinel_rc(1)
        self.assertIsNotNone(rc)
        self.assertNotEqual(rc, 0, "push after a denied declare must fail (still locked)")
        self.assertIn(H.PRE_DECLARE_LOCKED, run.text(), "the locked message re-teaches the next step")
        self.assertFalse(H.branch_exists(self.repo, branch, self.env))

    def test_one_declare_covers_a_second_push(self):
        """One confirmation covers ALL writes for the run: two pushes (both to
        agent/<n>/... refs), exactly one prompt."""
        issue = declare_issue()
        b1 = self._agent_branch(issue)
        b2 = self._agent_branch(issue)
        self._branches += [b1, b2]
        wd = H.make_workdir()
        script = H.clone_declare_push_script(self.repo, issue, [b1, b2])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.expect_prompt(timeout=120)
        run.answer(str(issue))

        # After confirmation the SECOND push must NOT re-prompt: expect either
        # another banner (index 0, BAD) or push2's sentinel (index 1, good).
        idx = run.child.expect([H.PROMPT_BANNER, r"SBX_PUSH2_RC=\d+"], timeout=180)
        self.assertEqual(idx, 1, "second write in the same run must not re-prompt")

        run.wait(timeout=60)
        self.assertEqual(run.prompt_count(), 1, "whole-transcript: exactly one prompt for the run")
        self.assertEqual(run.sentinel_rc(1), 0)
        self.assertEqual(run.sentinel_rc(2), 0)
        self.assertTrue(H.branch_exists(self.repo, b1, self.env))
        self.assertTrue(H.branch_exists(self.repo, b2, self.env))

    def test_nonmatching_ref_rejected_after_approval(self):
        """decision C: a confirmed run still cannot push a non-convention ref —
        the proxy's report-status deny surfaces as `! [remote rejected]`."""
        issue = declare_issue()
        plain = H.unique_branch("reintest-plainref")  # violates agent/<n>/<nonce>
        self._branches.append(plain)
        wd = H.make_workdir()
        script = H.clone_declare_push_script(self.repo, issue, [plain])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.expect_prompt(timeout=120)
        run.answer(str(issue))
        run.expect_approved(timeout=60)

        run.child.expect(r"SBX_PUSH1_RC=\d+", timeout=120)
        run.wait(timeout=60)

        rc = run.sentinel_rc(1)
        self.assertIsNotNone(rc)
        self.assertNotEqual(rc, 0, "non-convention ref must be rejected even after approval")
        self.assertIn(H.REF_CONVENTION_DENY, run.text(), "the agent must see the convention in the rejection")
        self.assertIn("[remote rejected]", run.text(), "the deny must be a report-status git can render")
        self.assertFalse(H.branch_exists(self.repo, plain, self.env))


if __name__ == "__main__":
    unittest.main()
