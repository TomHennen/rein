"""test_confirm_shows_title — GATED / HUMAN-RUN, TDD-red. FOR ISSUE #35.

This is a PROPOSED test for DEFERRED work, not a current-phase failure. The
agent-declared, human-confirmed issue-scoping model is tracked in issue #35 and
is deliberately NOT built in CP1-CP4.5: today the bound issue is a static
`sess.Issue` and the /dev/tty write-approval prompt only string-compares the
typed number to it, printing that number back and doing no GitHub lookup
(internal/ui/prompt/prompt.go:145-146,157-168). That is a documented, fail-
closed simplification (PLAN-1.md:363-379), not a bug.

WHEN #35 lands, part of that model is: at confirm time rein FETCHES the bound
issue and DISPLAYS its title + home repo (design-adjacent — design.md:281
already lists "first word of title" as a valid confirmation token), so a human
can tell a wrong-but-plausible binding from the right one (the misattribution
probe's scenarios S1/S4/S5). This file specifies that human-facing behavior END
TO END, over a real /dev/tty prompt driven through `rein run`, because a Go unit
test with a stub prompter CANNOT reproduce what the human sees on the terminal
— that is the #36 lesson (a stubbed prompt passed its unit test but broke live).
It ships now as a ready spec so the behavior lands tested, not so it runs today.

STATUS: @unittest.expectedFailure. The feature is intentionally unbuilt, so the
prompt contains no title and this test MUST fail. When #35's model lands,
unittest reports an "unexpected success" and the suite goes red — the signal to
drop the decorator and promote this to a real regression test.

GATING: this is a live, human-run test. It needs a real srt sandbox, a real
/dev/tty, a throwaway repo, a configured rein App, AND a real issue on that
throwaway whose number + a distinctive title word the human supplies via env:

    REIN_ITEST_TITLE_ISSUE   the issue number to bind the session to
    REIN_ITEST_TITLE_WORD    a distinctive word that appears in that issue's title

Without those it SKIPS. Do not run it in CI. Manual recipe:
tests/interactive/recipes/confirm-shows-title.sh (printed by run.sh --list).
"""

from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path

import reinharness as H
from itest_base import ReinTestCase

_ISSUE_ENV = "REIN_ITEST_TITLE_ISSUE"
_WORD_ENV = "REIN_ITEST_TITLE_WORD"


@unittest.skipUnless(
    os.getenv(_ISSUE_ENV) and os.getenv(_WORD_ENV),
    f"gated: set {_ISSUE_ENV} and {_WORD_ENV} to a real throwaway issue + a word in its title",
)
class ConfirmShowsTitle(ReinTestCase):
    def _pinned_env(self, issue: int) -> dict:
        """A temp session pinned to the given real issue number."""
        d = tempfile.mkdtemp(prefix="rein-itest-title-")
        path = Path(d) / "session.yaml"
        path.write_text(
            "id: sess_itest_title\n"
            "role: implement\n"
            "repos:\n"
            f"  - {self.repo}\n"
            f"issue: {issue}\n"
        )
        return {"REIN_SESSION_FILE": str(path)}

    @unittest.expectedFailure  # feature not implemented: prompt shows no title today
    def test_prompt_displays_issue_title_and_home_repo(self):
        """The /dev/tty approval prompt must show the bound issue's TITLE and HOME repo."""
        issue = int(os.environ[_ISSUE_ENV])
        title_word = os.environ[_WORD_ENV]

        branch = self.new_branch("reintest-title")
        wd = H.make_workdir()
        script = H.clone_and_push_script(self.repo, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self._pinned_env(issue),
        )

        # The prompt must appear on the tty...
        run.expect_prompt(timeout=120)
        # ...and it must carry the issue TITLE fetched from GitHub, so the human
        # can tell this is the RIGHT issue and not a wrong-but-plausible number.
        prompt_text = H.strip_ansi(run.text())
        self.assertIn(
            title_word, prompt_text,
            "approval prompt must display the bound issue's title (a distinctive "
            "title word was expected but not shown) — misattribution S1/S5",
        )
        # And it must name the issue's HOME repo (not only the write target), so
        # a cross-repo binding (S4) is visible.
        self.assertIn(
            self.repo.split("/")[-1], prompt_text,
            "approval prompt must display the issue's home repo",
        )

        # Complete the ceremony so we don't leave the run hanging on the tty.
        run.answer(str(issue))
        run.expect_approved(timeout=60)
        run.wait(timeout=90)


if __name__ == "__main__":
    unittest.main()
