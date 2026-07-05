"""test_realagent_e2e — the ULTIMATE approval loop, with a REAL agent. SKIPPED.

This is item 4 of the brief: the aspirational end state of the write-approval
loop is a REAL agent (`claude -p "<task>"`) running INSIDE the sandbox, doing a
task that ends in a `git push` -> the same HOST-tty approval prompt -> pexpect
(the human) approves -> the agent's push completes.

It is SKIPPED (not xfail) because it is BLOCKED on CP4.5: the sandbox currently
allows only GitHub egress, so `claude` cannot reach api.anthropic.com to run.
Once CP4.5 lands (per-session egress incl. the agent endpoint), unskip this and
fill in the task. We deliberately do NOT try to run `claude` today.

INTENDED FLOW (unblock after CP4.5):
  1. `rein run -- claude -p "clone <throwaway>, add a probe file, commit, and
     push to branch <unique>"` under a pty (this harness's spawn_rein_run).
  2. The agent works autonomously inside the sandbox; its final `git push`
     trips the write-approval prompt on the HOST tty.
  3. pexpect matches the prompt banner and sends the issue number ("1").
  4. Assert BOTH sides, exactly as test_write_approval does for a raw push:
       - HOST: the prompt appeared and [approved] printed.
       - REMOTE: the unique branch now exists on the throwaway.
     Plus: the agent process exits 0 (it experienced "wait, then it worked").
  5. Clean up the disposable branch from the HOST.

Everything the harness needs for this already exists (spawn_rein_run,
expect_prompt/answer, branch_exists/delete_branch); only the CP4.5 egress gate
stands between this scaffold and a live run.
"""

from __future__ import annotations

import unittest

from itest_base import ReinTestCase


class RealAgentEndToEnd(ReinTestCase):
    @unittest.skip("BLOCKED on CP4.5 (sandbox egress currently GitHub-only; claude can't reach api.anthropic.com). Unblock after CP4.5.")
    def test_claude_push_triggers_and_completes_the_approval_loop(self):
        # Intended body (see module docstring). Left unimplemented on purpose so
        # nobody accidentally runs `claude` before CP4.5 opens the egress path.
        raise NotImplementedError("unblock after CP4.5 — see module docstring")


if __name__ == "__main__":
    unittest.main()
