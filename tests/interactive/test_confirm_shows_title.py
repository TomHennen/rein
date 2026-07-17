"""test_confirm_shows_title — GATED (on a real issue). Regression spec for #35.

Issue #35 landed the agent-declared, human-confirmed issue-scoping model: at
declare time (`rein declare <n>`) rein FETCHES the declared issue and the
Form A prompt DISPLAYS its title + state + home repo, so a human can tell a
wrong-but-plausible number from the right one (decision E; misattribution
probe S1/S4/S5). This file specifies that human-facing behavior END TO END,
over a real /dev/tty prompt driven through `rein run`, because a Go unit test
with a stub prompter CANNOT reproduce what the human sees on the terminal —
the #36 lesson (a stubbed prompt passed its unit test but broke live).

Formerly TDD-red (@unittest.expectedFailure) while #35 was unbuilt; the
decorator is dropped now that the behavior exists — this is a REAL regression
test: if the prompt ever stops showing the fetched title/home-repo, this goes
red.

GATING: this is a LIVE test — but not a human-run one. It needs a real srt
sandbox, a real controlling terminal, a throwaway repo, a configured rein App,
AND a real issue on that throwaway. pexpect supplies the terminal and answers the
prompt (see the README's doctrine section), so an AGENT can run this itself; what
it cannot invent is the issue, whose number + a distinctive title word come from
env (an invented number would 404 at fetch time and fail closed, by design):

    REIN_ITEST_TITLE_ISSUE   the issue number the agent will declare
    REIN_ITEST_TITLE_WORD    a distinctive word that appears in that issue's title

Without those it SKIPS (it is live: it pushes to a real throwaway, so it is not
a CI test). Setup recipe: tests/interactive/recipes/confirm-shows-title.sh.
"""

from __future__ import annotations

import os
import unittest

from tests.interactive import reinharness as H
from tests.interactive.itest_base import ReinTestCase

_ISSUE_ENV = "REIN_ITEST_TITLE_ISSUE"
_WORD_ENV = "REIN_ITEST_TITLE_WORD"


@unittest.skipUnless(
    os.getenv(_ISSUE_ENV) and os.getenv(_WORD_ENV),
    f"gated: set {_ISSUE_ENV} and {_WORD_ENV} to a real throwaway issue + a word in its title",
)
class ConfirmShowsTitle(ReinTestCase):
    def test_prompt_displays_issue_title_and_home_repo(self):
        """The Form A declare prompt must show the FETCHED issue TITLE and HOME repo."""
        issue = int(os.environ[_ISSUE_ENV])
        title_word = os.environ[_WORD_ENV]

        branch = f"agent/{issue}/{H.unique_branch('title')}"
        self._branches.append(branch)
        wd = H.make_workdir()
        script = H.clone_declare_push_script(self.repo, issue, [branch])
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=wd, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        # The declare fires the prompt on the tty...
        run.expect_prompt(timeout=120)
        # ...and it must carry the issue TITLE fetched from GitHub, so the human
        # can tell this is the RIGHT issue and not a wrong-but-plausible number.
        prompt_text = H.strip_ansi(run.text())
        self.assertIn(
            title_word, prompt_text,
            "declare prompt must display the fetched issue title (a distinctive "
            "title word was expected but not shown) — misattribution S1/S5",
        )
        # And it must name the issue's HOME repo (not only the write target), so
        # a cross-repo binding (S4) is visible.
        self.assertIn(
            self.repo.split("/")[-1], prompt_text,
            "declare prompt must display the issue's home repo",
        )

        # Complete the ceremony: confirm, then the verified push must land.
        run.answer(str(issue))
        run.expect_approved(timeout=60)
        run.child.expect(r"SBX_PUSH1_RC=\d+", timeout=120)
        run.wait(timeout=90)
        self.assertEqual(run.sentinel_rc(1), 0, "verified push should succeed after confirmation")


if __name__ == "__main__":
    unittest.main()
