"""realagent_e2e — a REAL agent (`claude`) running INSIDE the sandbox.

NOT swept by `run.sh` (deliberately named off the `test_*.py` discovery pattern):
it needs a real `claude`, `pyte`, live `api.anthropic.com` egress, and API quota,
so it is SELECTED by the runner, not self-skipped in the default sweep. A skip
inside the sweep reads as a silent pass — the #68 footgun. `run-journeys.sh
--sandbox` invokes it [B]: SKIP if `claude` is absent (not everyone has it), but
a HARD FAIL if `claude` is present and `pyte` is not (pyte is a cheap `apt install`
— its absence in an opted-in real-agent environment is a misconfig, not a skip).

For a long time this was SKIPPED, blocked on CP4.5 (the sandbox allowed only
GitHub egress, so `claude` could not reach api.anthropic.com). CP4.5 landed the
default extra-egress allowlist (api.anthropic.com) AND a writable per-run TMPDIR
(the EROFS fix), so `rein run -- claude` now starts a real interactive agent
in-sandbox. This test proves the FLOOR of that: the agent starts (no EROFS, no
startup hang) and answers a trivial deterministic prompt.

It runs under the SHIPPED DEFAULT (no REIN_*_MCP env set): claude's account MCP
connectors are enabled and connected non-blocking, so this also guards against a
regression where an unreachable MCP host blocks startup. (The blanket
ENABLE_CLAUDEAI_MCP_SERVERS=false disable was REMOVED; see internal/srt/env.go.)

Why interactive (pty) and not `claude -p`: headless `-p` masks startup errors —
an EROFS or a hung MCP connect can still exit 0 with empty output. Driving the
real TUI over a pty is the faithful reproduction of what a developer sees.

QUOTA: this launches ONE nested `claude`. Keep it to a single trivial prompt.

The fuller edit->commit->push agent task (agent autonomy + the write-approval
loop together) is intentionally NOT an automated test: a real agent's tool
choices are non-deterministic and would make it flaky. It lives as a documented
manual scenario in the dogfood plan instead. The write-approval loop itself is
covered deterministically by test_write_approval.py (a raw in-sandbox push).
"""

from __future__ import annotations

import unittest

import reinharness as H
from itest_base import ReinTestCase


class RealAgentEndToEnd(ReinTestCase):
    # A TUI REDRAWS, so both helpers below assert on a pyte-RENDERED SCREEN (#100):
    # `read_until_ready` / `send_and_collect` pump this pty into a RenderedScreen,
    # which raises PyteMissing without pyte. There is NO skip guard on purpose: this
    # file is not swept by run.sh, and the runner (run-journeys.sh --sandbox) only
    # selects it where `claude` is present — so a missing pyte here is a misconfigured
    # opted-in environment and must HARD-FAIL loudly, not skip. See the module docstring.
    def test_claude_starts_in_sandbox_and_answers(self):
        """`rein run -- claude` starts a real agent in-sandbox (no EROFS/hang) and
        answers 2+2 -> '4'."""
        # This test's cwd is the rein repo root, which in the dev flow is a git
        # LINKED WORKTREE (.claude/worktrees/…) — its `.git` is a FILE, which #64
        # treats as unhardenable, so by default `rein run` would give claude an
        # EPHEMERAL scratch tree (a fresh, untrusted dir → claude's folder-trust
        # dialog intercepts startup). That ephemeral fallback is exercised by its
        # own tests + demos; THIS test is about claude actually starting in the
        # tree the developer is in, so it opts into binding the real (linked)
        # worktree writable. A mainstream user in a NORMAL checkout (`.git` dir)
        # gets the hardened bind with no opt-in and no dialog.
        run = H.spawn_claude_interactive(
            env=self.env,
            extra_env={"REIN_SANDBOX_ALLOW_UNHARDENED_GIT": "1"},
            timeout=90,
        )
        try:
            ready, dialog, exited = run.read_until_ready(
                H.CLAUDE_READY_MARKERS,
                dialog_markers=H.CLAUDE_DIALOG_MARKERS,
                timeout=75,
            )
            # Distinguish the failure modes so a red test is actionable.
            self.assertFalse(
                exited,
                f"claude EXITED during startup (EROFS or crash?). Transcript:\n{run.text()[-1500:]}",
            )
            if not ready and dialog:
                self.fail(
                    "a STARTUP DIALOG (trust/theme/login) intercepted startup — "
                    f"not a hang, but the agent never became ready:\n{run.text()[-1500:]}"
                )
            self.assertTrue(
                ready,
                "claude did not reach an interactive prompt within 75s (startup hang?). "
                f"Transcript tail:\n{run.text()[-1500:]}",
            )

            reply = run.send_and_collect(
                "what is 2+2? reply with only the number", settle=14, timeout=12
            )
            self.assertIn(
                "4",
                reply,
                f"agent did not answer 2+2 -> 4. Reply tail:\n{reply[-800:]}",
            )
        finally:
            run.quit_tui()


if __name__ == "__main__":
    unittest.main()
