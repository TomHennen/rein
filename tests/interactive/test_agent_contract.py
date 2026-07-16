"""test_agent_contract — the AGENT is told the rules of the sandbox it is in (#63).

The user journey: a developer runs `rein run -- claude`. Everything rein prints
about the sandbox — $HOME is ephemeral, there are no credentials, writes are
locked until `rein declare <n>` — goes to the DEVELOPER's terminal. The agent
never sees a word of it.

Before #63 the agent learned the rules only by FAILING: it hit a deny message on
its first push (#35's reactive tier), and it never learned the $HOME rules at all
— writes under $HOME succeed, read back fine all run, and evaporate at teardown,
so nothing ever tells the agent its work is doomed. rein now briefs the agent up
front, over the best channel that agent has:

  - claude  -> --append-system-prompt (a real context channel; verified in 2.1.207)
  - others  -> the contract is PRINTED into the sandbox's own output

Two journeys below, one per channel. The claude leg is the one that matters: it
proves the agent can actually ANSWER the rules back, which is the only evidence
that the contract reached the model rather than merely being emitted.

QUOTA: the claude leg launches ONE nested `claude` with a single headless prompt.
Keep it that way.
"""

from __future__ import annotations

import unittest

from tests.interactive import reinharness as H
from tests.interactive.itest_base import ReinTestCase


class AgentContract(ReinTestCase):
    def test_claude_knows_the_rules_from_the_injected_contract(self):
        """`rein run -- claude` briefs claude via --append-system-prompt, and the
        agent can state the three rules back: where work persists, how to unlock
        writes, and that it holds no credentials."""
        run = H.spawn_claude_interactive(
            [
                "-p",
                "Answer concisely from your system prompt: (1) where must you write "
                "files so they persist, and what happens to files you write under $HOME? "
                "(2) what exact command unlocks GitHub writes, and what branch name must "
                "you push to? (3) do you have any credentials?",
            ],
            env=self.env,
            timeout=180,
        )
        run.wait(timeout=180)
        out = run.text()

        # The operator must be able to SEE which channel was used — otherwise
        # "rein briefed the agent" is an assumption, not an observation.
        self.assertIn(
            "--append-system-prompt",
            out,
            "the banner did not report the system-prompt channel for claude; either detection "
            f"failed or the contract was not injected.\n--- transcript ---\n{out}",
        )

        # The agent ANSWERING correctly is the only proof the contract reached the
        # model's context (as opposed to being printed into the void).
        lowered = out.lower()
        self.assertIn(
            "rein declare",
            lowered,
            f"claude could not state the declare command — the contract did not reach its "
            f"context.\n--- transcript ---\n{out}",
        )
        self.assertIn(
            "agent/",
            lowered,
            f"claude could not state the agent/<n>/<nonce> branch convention.\n--- transcript ---\n{out}",
        )
        # It must know $HOME writes are LOST — the fact nothing else in the system
        # would ever tell it (they succeed silently and only vanish at teardown).
        self.assertTrue(
            any(w in lowered for w in ("discard", "ephemeral", "not persist", "lost", "evaporat")),
            f"claude did not state that $HOME writes are discarded — the silent-data-loss rule "
            f"is exactly the one it cannot learn any other way.\n--- transcript ---\n{out}",
        )

    def test_non_claude_agent_gets_the_contract_printed_into_its_output(self):
        """A non-claude agent has no system-prompt channel, so rein prints the
        contract into the sandbox's own output — and says so in the banner rather
        than implying the agent was briefed."""
        workdir = H.throwaway_repo(self.env)
        run = H.spawn_rein_run(
            ["/bin/sh", "-c", 'echo "[agent] contract-visible-in-my-output"'],
            workdir=workdir,
            env=self.env,
            timeout=120,
        )
        run.wait(timeout=120)
        out = run.text()

        # Honest banner: printed, NOT injected.
        self.assertIn(
            "PRINTED to the agent's output",
            out,
            f"the banner must say the contract was only printed for a non-system-prompt agent.\n"
            f"--- transcript ---\n{out}",
        )
        # And the contract body actually lands in the agent's own output stream.
        for rule in ("rein declare <n>", "agent/<n>/<nonce>", "$HOME is EPHEMERAL", "NO credentials"):
            self.assertIn(
                rule,
                out,
                f"the contract body is missing {rule!r} from the sandbox output.\n--- transcript ---\n{out}",
            )
        # The agent still ran (the contract must not disturb the launch).
        self.assertIn("[agent] contract-visible-in-my-output", out)


if __name__ == "__main__":
    unittest.main()
