"""test_write_approval — the human-in-the-loop write-approval loop, END TO END.

This is the deliverable that automates the previously-manual write path
(docs/cp3-manual-test.sh, docs/cp4-manual-test.sh). Every test drives a write
INITIATED FROM INSIDE THE SANDBOX (a `git push` under `rein run`) and asserts
BOTH sides of the loop:

  - HOST side: the /dev/tty approval prompt actually appeared (pexpect matched
    it), and the correct approved/denied marker printed on the host terminal.
  - SANDBOX side: the in-sandbox git command's OWN outcome — captured via an
    explicit `SBX_PUSH<n>_RC=<code>` sentinel — is what we assert. On approval
    it BLOCKS then SUCCEEDS (RC 0); on denial it FAILS CLEANLY (git fatal 403,
    RC 128), never a hang.

Why the sentinel and not `rein run`'s exit code: the in-sandbox script runs
under `set +e` (so a denied push can't abort it into an ambiguous state), which
means `rein run` itself exits 0 even when the push was denied. The sentinel is
therefore the ground truth for "what the sandboxed agent experienced."

All behaviors here were proven live before these assertions were written
(prompt count, RC values, remote branch presence, the proxied deny message).
"""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import pexpect

import reinharness as H
from itest_base import BOUND_ISSUE, ReinTestCase


class WriteApprovalLoop(ReinTestCase):
    def test_correct_approval_completes_the_in_sandbox_push(self):
        """Approve with the issue number -> host [approved] -> in-sandbox push SUCCEEDS."""
        branch = self.new_branch("reintest-approve")
        wd = H.make_workdir()
        script = H.clone_and_push_script(self.repo, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        # HOST: the prompt must actually appear on the tty.
        run.expect_prompt(timeout=120)
        run.answer(str(BOUND_ISSUE))  # the pinned session's bound issue number
        run.expect_approved(timeout=60)  # host saw [approved]

        # SANDBOX: the push blocks until approval, then completes with RC 0.
        run.child.expect(r"SBX_PUSH1_RC=\d+", timeout=90)
        run.wait(timeout=60)

        self.assertEqual(run.sentinel_rc(1), 0, "in-sandbox push should succeed after approval")
        self.assertEqual(run.prompt_count(), 1, "exactly one approval prompt should fire")
        self.assertTrue(H.branch_exists(self.repo, branch, self.env), "branch must appear on throwaway")

    def test_wrong_answer_denies_and_push_fails_cleanly(self):
        """A wrong issue number -> host [denied] -> in-sandbox push FAILS cleanly, no branch."""
        branch = self.new_branch("reintest-deny")
        wd = H.make_workdir()
        script = H.clone_and_push_script(self.repo, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.expect_prompt(timeout=120)
        run.answer(str(BOUND_ISSUE + 1))  # NOT the bound issue number
        run.expect_denied(timeout=60)  # host saw [denied: ...]

        # SANDBOX: the push fails cleanly (git fatal 403 -> RC 128), never a hang.
        run.child.expect(r"SBX_PUSH1_RC=\d+", timeout=90)
        run.wait(timeout=60)

        rc = run.sentinel_rc(1)
        self.assertIsNotNone(rc)
        self.assertNotEqual(rc, 0, "in-sandbox push should fail after denial")
        self.assertEqual(run.prompt_count(), 1, "git must not re-prompt on a 403 denial")
        # Item 3: what the agent SEES in-sandbox when a write is denied — the
        # proxy surfaces a coherent message through git's `remote:` channel.
        self.assertIn("was not approved", run.text(), "agent should see a coherent deny reason")
        self.assertFalse(H.branch_exists(self.repo, branch, self.env), "denied write must NOT create branch")

    def test_run_scoped_approval_covers_a_second_write(self):
        """One approval covers the whole run: 2 in-sandbox pushes, exactly 1 prompt."""
        b1 = self.new_branch("reintest-scope1")
        b2 = self.new_branch("reintest-scope2")
        wd = H.make_workdir()
        script = H.clone_and_push_script(self.repo, [b1, b2])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.expect_prompt(timeout=120)
        run.answer(str(BOUND_ISSUE))

        # After approval, the SECOND push must NOT re-prompt. Expect either
        # another banner (index 0, BAD) or push2's RC sentinel (index 1, good).
        # A second banner here would mean the run-scoped record failed to
        # short-circuit the credential helper — a real regression, not noise.
        idx = run.child.expect([H.PROMPT_BANNER, r"SBX_PUSH2_RC=\d+"], timeout=120)
        self.assertEqual(idx, 1, "second write in the same run must not re-prompt")

        run.wait(timeout=60)
        self.assertEqual(run.prompt_count(), 1, "whole-transcript: exactly one prompt for the run")
        self.assertEqual(run.sentinel_rc(1), 0)
        self.assertEqual(run.sentinel_rc(2), 0)
        self.assertTrue(H.branch_exists(self.repo, b1, self.env))
        self.assertTrue(H.branch_exists(self.repo, b2, self.env))

    def test_no_issue_session_blocks_writes_without_prompting(self):
        """A session with NO `issue:` -> writes DENIED with no prompt; reads still flow."""
        branch = self.new_branch("reintest-noissue")
        # A valid session (id, role, repos) but deliberately NO `issue:` field.
        tmpdir = tempfile.mkdtemp(prefix="rein-itest-sess-")
        sess = Path(tmpdir) / "no-issue-session.yaml"
        sess.write_text(
            "id: sess_itest_noissue\n"
            "role: implement\n"
            "repos:\n"
            f"  - {self.repo}\n"
        )
        wd = H.make_workdir()
        script = H.clone_and_push_script(self.repo, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script],
            workdir=wd,
            env=self.env,
            extra_env={"REIN_SESSION_FILE": str(sess)},
        )

        # No prompt should EVER appear; the run just finishes.
        run.child.expect(pexpect.EOF, timeout=180)
        run.child.close()

        self.assertEqual(run.prompt_count(), 0, "no `issue:` means no approval channel — no prompt")
        self.assertIn("SBX_CLONE_OK", run.text(), "reads must still flow without an issue")
        self.assertIn(H.NO_ISSUE_BLOCKED, run.text(), "the no-issue block message should fire")
        rc = run.sentinel_rc(1)
        self.assertIsNotNone(rc)
        self.assertNotEqual(rc, 0, "the write must be denied")
        self.assertFalse(H.branch_exists(self.repo, branch, self.env), "no branch on a blocked write")


if __name__ == "__main__":
    unittest.main()
