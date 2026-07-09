"""test_init_interactive — TDD scaffold for the INTERACTIVE `rein init`.

RED IS THE POINT. These tests encode the SETTLED parts of the interactive-init
design (docs/onboarding-ux-design.md) as executable specs for a build that does
NOT exist yet. STALE SPECS (reconcile in CP4.6): CP1 (#39) made `rein init`
scaffold a REPO-ONLY session from `--repo`/a repo prompt (no REIN_TEST_REPO_A,
no hardcoded `issue: 1`), and decision A (#35) settled that init must NOT prompt
for an issue at all — so `test_init_prompts_for_the_issue_number` below is
superseded and should be removed/inverted when the interactive flow is built.
So the settled specs
below FAIL — cleanly, as `unittest.expectedFailure` (== pytest xfail), NOT as
uncontrolled errors. When interactive init ships, these flip to "unexpected
success" and the suite goes red, signaling "promote me to a real test."

OPEN DECISIONS (design §8) are `unittest.skip`ped, not encoded — we must not
hard-code behavior Tom hasn't decided.

SAFETY (see reinharness.isolated_home_env): every init run here is confined to a
throwaway HOME/XDG tempdir and keeps REIN_APP_* present, so it can never mutate
the real environment nor trip the 25-minute manifest/browser flow. All runs pass
--no-alias --no-symlink --skip-mint-check and use SHORT timeouts so red is fast.
"""

from __future__ import annotations

import os
import unittest

import pexpect

import reinharness as H
from itest_base import ReinTestCase

# Flags that keep every init run inert: no rc edit, no ~/.local/bin symlink, no
# network mint. The isolated HOME (below) confines all writes to a tempdir.
SAFE_INIT_FLAGS = ["--no-alias", "--no-symlink", "--skip-mint-check"]


class InteractiveInitSettledSpecs(ReinTestCase):
    """Settled design behavior that the future interactive init MUST have.

    Each is expectedFailure until that init is built (TDD red).
    """

    def _spawn_init(self, home, extra_flags=None, timeout=20):
        flags = SAFE_INIT_FLAGS + list(extra_flags or [])
        return H.spawn_rein(
            ["init", *flags],
            env=self.env,
            extra_env=H.isolated_home_env(home),
            timeout=timeout,
        )

    @unittest.expectedFailure
    def test_init_prompts_for_the_issue_number(self):
        """Design §3 step 5: init ASKS 'Which issue backs write-approval?'.

        Today init never prompts (issue is hardcoded to 1), so the prompt never
        appears; a SHORT timeout turns that into a fast, clean assertion rather
        than a 20s hang or an uncaught pexpect.TIMEOUT.
        """
        home = H.isolated_home()
        run = self._spawn_init(home, timeout=15)
        try:
            run.child.expect(r"[Ww]hich issue", timeout=5)
            prompted = True
        except (pexpect.TIMEOUT, pexpect.EOF):
            prompted = False
        finally:
            try:
                run.child.close(force=True)
            except Exception:
                pass
        self.assertTrue(prompted, "interactive init should prompt for the backing issue (not built yet)")

    @unittest.expectedFailure
    def test_init_honors_the_answered_issue_number(self):
        """Design §3 step 5: the scaffolded session's `issue:` reflects the ANSWER.

        We drive init and 'answer' issue=42. Today init ignores stdin and
        hardcodes issue:1, so the resulting dev-session.yaml has `issue: 1` —
        the assertion for 42 fails cleanly (red). Deterministic: init always
        completes and writes a file, so this is an assertion miss, not a hang.
        """
        home = H.isolated_home()
        run = self._spawn_init(home, timeout=15)
        # Best-effort: feed an issue answer in case a future prompt reads it.
        try:
            run.child.sendline("42")
        except Exception:
            pass
        try:
            run.child.expect(pexpect.EOF, timeout=10)
        except pexpect.TIMEOUT:
            pass
        run.child.close(force=True)

        sess_path = os.path.join(home, ".config", "rein", "dev-session.yaml")
        self.assertTrue(os.path.exists(sess_path), "init should scaffold a session")
        with open(sess_path) as f:
            body = f.read()
        self.assertIn("issue: 42", body, "interactive init should honor the answered issue (today it hardcodes 1)")

    @unittest.expectedFailure
    def test_headless_init_prints_a_link_and_does_not_hang(self):
        """Design §5: browser steps degrade to a printed link when headless.

        Headless = SSH_CONNECTION set, no DISPLAY/WAYLAND_DISPLAY. We assert two
        things: (a) init does NOT hang headless — it reaches EOF within the
        timeout (this part already holds and guards a future regression); and
        (b) it prints a browser/install link. Today's env-path init prints only
        the 'Next:' steps, no link, so (b) is red.

        NOTE (per the review): the link-printing step lives on the manifest
        path, which init deliberately does NOT take here (REIN_APP_* present).
        We do NOT force the manifest flow (that's the 25-minute browser/callback
        flow). This test is therefore aspirational for the *interactive* build
        that will surface an install link in the env path too — hence xfail.
        """
        home = H.isolated_home()
        headless = dict(H.isolated_home_env(home))
        headless["SSH_CONNECTION"] = "203.0.113.1 222 203.0.113.9 22"
        headless.pop("DISPLAY", None)
        headless.pop("WAYLAND_DISPLAY", None)
        run = H.spawn_rein(
            ["init", *SAFE_INIT_FLAGS],
            env=self.env,
            extra_env=headless,
            timeout=20,
        )
        hung = False
        try:
            run.child.expect(pexpect.EOF, timeout=15)
        except pexpect.TIMEOUT:
            hung = True
        run.child.close(force=True)

        self.assertFalse(hung, "headless init must not hang (guards a regression)")
        text = run.text().lower()
        printed_link = ("github.com/apps" in text) or ("https://github.com/settings/apps" in text) or ("visit this url" in text)
        self.assertTrue(printed_link, "headless init should print a browser/install link (not built for the env path yet)")


class InteractiveInitOpenDecisions(ReinTestCase):
    """Open design decisions (§8): SKIPPED — we must not encode undecided behavior."""

    @unittest.skip("open decision §8.1: prompt vs. loud-labeled-default for the machine/App label")
    def test_machine_name_prompt_vs_default(self):
        raise NotImplementedError

    @unittest.skip("open decision §8.2: how hard to gate onboarding on a healthy sandbox")
    def test_gating_on_healthy_sandbox(self):
        raise NotImplementedError

    @unittest.skip("open decision §8.3: alias every named agent, or just the primary? alias non-claude at all in v1?")
    def test_multi_agent_alias(self):
        raise NotImplementedError

    @unittest.skip("open decision §8.4: doctor --fix scope for v1 — no-privilege tier only, or include consented-privileged?")
    def test_doctor_fix_scope(self):
        raise NotImplementedError


if __name__ == "__main__":
    unittest.main()
