"""test_init_interactive — the interactive `rein init`, live under a pty.

WHAT CHANGED (2026-07-11): this file used to skip FOUR "open decision §8.x"
specs. Two of them are no longer open — CP4.6 (PR #42) DECIDED and SHIPPED them
— so they are now REAL tests, verified against the actual binary rather than
against the docs:

  §8.2  sandbox gating   SHIPPED: SOFT-block. An unhealthy sandbox stack prints a
                         loud WARNING and init still finishes (exit 0); only
                         `--require-sandbox` turns it into a hard failure.
  §8.3  agent alias      SHIPPED: OPT-IN, default OFF. `--alias` installs,
                         `--no-alias` force-skips, and a genuine tty with neither
                         flag gets a `[y/N]` prompt defaulting to NO.

  §8.1  machine label    SHIPPED: init PROMPTS for the machine label, pre-filled
                         with the detected hostname and editable (design §4), and
                         the App name is now `rein-<role>-<label>-<shortrand>`
                         (appsetup/manifest.go). The label resolves up front on
                         every interactive path (cmd/rein/machinelabel.go), so it
                         is now a REAL test, not `expectedFailure`.

  §5    install-on-repo  SHIPPED: after the session scaffold init prints the
                         install deep-link (no ssh -L needed), degrading to the
                         generic installations URL when the slug is unknown and
                         never auto-opening on a headless session — so the
                         headless-link spec below is a REAL test too.

  §8.4  doctor --fix     PARTIALLY DECIDED: the NO-PRIVILEGE tier shipped
                         (`rein doctor --fix` reinstalls shims, refreshes the
                         PATH symlink, clears a stale cache; privileged/external
                         steps stay guide-only). The CONSENTED-PRIVILEGED tier is
                         still open, so test_doctor_fix_scope stays skipped: we
                         must not encode behavior Tom hasn't decided.

THE PTY IS THE POINT. These are not stubbed prompt tests — pexpect gives init a
real controlling terminal, so `stdinIsTerminal` is TRUE and the alias `[y/N]`
prompt genuinely fires and is genuinely answered. That is the #36 lesson: a
stubbed prompt passed its unit test and still broke live.

SAFETY (see reinharness.isolated_home_env): every init run is confined to a
throwaway HOME/XDG tempdir, so even the runs that DO install an alias write only
into that tempdir — never the developer's real rc. REIN_APP_* stays present, so
init keeps to the env-driven path and can never reach RunManifestFlow (the
~25-minute browser/callback flow that would create a REAL GitHub App). Runs pass
--no-symlink --skip-mint-check, and SHORT timeouts so red is fast.

NOTE on --no-alias: the alias tests deliberately DROP it. It is a safety flag for
every OTHER test, but here it would suppress the exact behavior under test; the
isolated HOME is what keeps those runs safe.
"""

from __future__ import annotations

import os
import unittest

import pexpect

import reinharness as H
from itest_base import ReinTestCase

# Flags that keep an init run inert: no ~/.local/bin symlink, no network mint.
# NOT included here: --no-alias (see the module docstring) — tests that don't
# exercise the alias add it themselves via SAFE_INIT_FLAGS.
INERT_FLAGS = ["--no-symlink", "--skip-mint-check"]
SAFE_INIT_FLAGS = ["--no-alias", *INERT_FLAGS]

# On a REAL tty init asks TWO questions, in this order:
#   1. "Which repo should the agent work on? (owner/name, Enter to skip)"
#   2. "Add alias claude='rein run -- claude' to <rc>? [y/N]"
# `--yes` suppresses both (that is what headless/CI uses). Any test that drives
# the pty WITHOUT --yes must answer #1 before it can ever see #2 — otherwise it
# hangs on the repo prompt and times out. NON_INTERACTIVE bundles the inert flags
# with --yes for the tests that don't care about the prompts.
NON_INTERACTIVE = [*SAFE_INIT_FLAGS, "--yes"]
REPO_PROMPT = "Which repo should the agent work on"

# A PATH with no `srt` on it. sandboxDoctorChecks resolves srt via PATH, so this
# is the lever that makes the sandbox stack look UNHEALTHY to init without
# breaking anything on the box. /usr/bin + /bin still provide git, bwrap, etc.
NO_SRT_PATH = "/usr/bin:/bin"

UNHEALTHY_BANNER = "WARNING: sandbox stack is UNHEALTHY"


def _run_init(env, extra_env, flags, timeout=30):
    """Spawn `rein init <flags>` under a pty; run to EOF; return (transcript, rc)."""
    run = H.spawn_rein(["init", *flags], env=env, extra_env=extra_env, timeout=timeout)
    run.child.expect(pexpect.EOF, timeout=timeout)
    run.child.close()
    return run.text(), run.child.exitstatus


class InitSandboxGating(ReinTestCase):
    """§8.2 — SHIPPED (CP4.6/#42): soft-block on an unhealthy sandbox stack.

    The decision was "finish now, warn loudly; sandboxed `rein run` fails closed
    anyway". The security posture is unchanged — this is onboarding-time
    SURFACING, not enforcement (onboarding-ux-design.md §7).
    """

    def _unhealthy_env(self, home):
        env = dict(H.isolated_home_env(home))
        env["PATH"] = NO_SRT_PATH  # hide srt => the stack looks unhealthy
        return env

    def test_unhealthy_sandbox_warns_loudly_but_init_still_finishes(self):
        """SOFT-block: loud WARNING naming the failing check, but exit 0 — the
        rest of init's setup (state dir, shim, config) is NOT thrown away."""
        home = H.isolated_home()
        text, rc = _run_init(self.env, self._unhealthy_env(home), NON_INTERACTIVE)

        self.assertIn(UNHEALTHY_BANNER, text, "an unhealthy stack must warn LOUDLY")
        self.assertIn("srt", text, "the warning must name the failing check")
        self.assertIn("rein init: done.", text, "soft-block: init still completes its other setup")
        self.assertEqual(rc, 0, "soft-block: an unhealthy sandbox must NOT fail init by default")

    def test_require_sandbox_turns_the_soft_block_into_a_hard_gate(self):
        """--require-sandbox is the opt-in hard gate: same warning, non-zero exit."""
        home = H.isolated_home()
        flags = [*NON_INTERACTIVE, "--require-sandbox"]
        text, rc = _run_init(self.env, self._unhealthy_env(home), flags)

        self.assertIn(UNHEALTHY_BANNER, text, "the warning is identical; only the exit code differs")
        self.assertNotEqual(rc, 0, "--require-sandbox must hard-fail on an unhealthy stack")

    def test_healthy_sandbox_does_not_warn(self):
        """The control: with the real PATH (srt present) there is no warning at all.

        Without this, the two tests above would pass just as happily against an
        init that warned unconditionally.
        """
        home = H.isolated_home()
        text, rc = _run_init(self.env, H.isolated_home_env(home), NON_INTERACTIVE)

        self.assertNotIn(UNHEALTHY_BANNER, text, "a healthy stack must produce no scary warning")
        self.assertEqual(rc, 0)


class InitAgentAlias(ReinTestCase):
    """§8.3 — SHIPPED (CP4.6/#42): the `claude` alias is OPT-IN, default OFF.

    Only the primary agent (`claude`) is aliased in v1. These runs deliberately
    omit --no-alias so the real alias machinery executes; the isolated HOME is
    what makes that safe (the rc file written is the tempdir's, never the dev's).
    """

    def _rc_path(self, home):
        return os.path.join(home, ".bashrc")

    def _alias_installed(self, home) -> bool:
        try:
            with open(self._rc_path(home)) as f:
                return "rein run -- claude" in f.read()
        except FileNotFoundError:
            return False

    def test_default_is_off_no_alias_installed(self):
        """Neither flag + no tty-consent => the alias is NOT installed, and init
        SAYS so (opt-in default; --yes suppresses the prompt entirely)."""
        home = H.isolated_home()
        flags = [*INERT_FLAGS, "--yes", "--shell", "bash"]
        text, rc = _run_init(self.env, H.isolated_home_env(home), flags)

        self.assertEqual(rc, 0)
        self.assertIn("alias:      not installed", text, "init must state the opt-in default")
        self.assertFalse(self._alias_installed(home), "default OFF: nothing may be written to the rc")

    def test_alias_flag_installs_the_block_into_the_rc(self):
        """--alias is the explicit opt-in: the alias block lands in the (isolated) rc."""
        home = H.isolated_home()
        flags = [*INERT_FLAGS, "--yes", "--alias", "--shell", "bash"]
        text, rc = _run_init(self.env, H.isolated_home_env(home), flags)

        self.assertEqual(rc, 0)
        self.assertIn(self._rc_path(home), text, "init must name the exact rc it edits")
        self.assertTrue(self._alias_installed(home), "--alias must install `alias claude='rein run -- claude'`")

    def _drive_tty_init(self, home, alias_answer: str):
        """Walk the REAL interactive init sequence on a pty: accept the machine
        label (bare Enter = keep the pre-filled hostname), answer the repo
        prompt (bare Enter = skip scaffolding), then answer the alias [y/N].

        The ORDER matters and is NOT optional. On a real tty init now asks, in
        sequence: (1) "Name this machine [<hostname>]", (2) the repo prompt,
        (3) the alias [y/N]. Each answer is sent only AFTER its prompt is seen,
        so init's per-prompt bufio reader can never swallow the next line — and
        a test that skips an earlier prompt just hangs on it (the machine-label
        prompt fires FIRST, so it must be answered before the repo prompt is
        even printed).
        """
        run = H.spawn_rein(
            ["init", *INERT_FLAGS, "--shell", "bash"],
            env=self.env,
            extra_env=H.isolated_home_env(home),
            timeout=30,
        )
        run.child.expect(r"(?i)name this machine", timeout=30)
        run.answer("")  # Enter => keep the pre-filled hostname label
        run.child.expect(REPO_PROMPT, timeout=30)
        run.answer("")  # Enter => skip session scaffolding (a graceful no-op)
        run.child.expect(r"\[y/N\]", timeout=30)  # the alias prompt genuinely fired
        run.answer(alias_answer)
        run.child.expect(pexpect.EOF, timeout=30)
        run.child.close()
        return run

    def test_tty_prompt_fires_and_yes_installs(self):
        """On a REAL tty with neither flag, init ASKS. Answer y => installed.

        This is the case a stubbed unit test cannot reach: it exists only
        because pexpect hands init a genuine controlling terminal.
        """
        home = H.isolated_home()
        run = self._drive_tty_init(home, "y")

        self.assertIn("Add alias claude=", run.text(), "the prompt must name what it will add")
        self.assertTrue(self._alias_installed(home), "answering y must install the alias")

    def test_tty_prompt_defaults_to_no_on_bare_enter(self):
        """[y/N] means N: a bare enter declines. The opt-in default holds even
        when the human just mashes return."""
        home = H.isolated_home()
        self._drive_tty_init(home, "")  # bare enter at the alias prompt

        self.assertFalse(self._alias_installed(home), "a bare enter must NOT install the alias")

    def test_no_alias_wins_over_alias(self):
        """--no-alias beats --alias (documented precedence: the safe flag wins)."""
        home = H.isolated_home()
        flags = [*INERT_FLAGS, "--yes", "--alias", "--no-alias", "--shell", "bash"]
        _text, rc = _run_init(self.env, H.isolated_home_env(home), flags)

        self.assertEqual(rc, 0)
        self.assertFalse(self._alias_installed(home), "--no-alias must win when both flags are given")


class InteractiveInitSettledSpecs(ReinTestCase):
    """Settled design behavior the interactive init MUST have — now BUILT.

    Both specs below were `expectedFailure` (TDD-RED) until the onboarding
    slices landed; they are now REAL tests, verified against the binary. Each
    runs init on a live pty and asserts on the transcript.
    """

    def test_machine_label_prompt_prefilled_with_hostname(self):
        """§8.1 — DECIDED and BUILT.

        Decision (design §4/§8.1): init prompts for the machine label,
        PRE-FILLED with the detected hostname and editable — because the App
        name is globally unique on GitHub and hostname inference alone is
        unreliable (this VM's hostname is literally `ubuntu`). The App name is
        now `rein-<role>-<label>-<shortrand>` (internal/appsetup/manifest.go),
        and cmd/rein/machinelabel.go resolves the label — prompting on a real
        tty, falling back to the hostname headless/--yes.

        DRIVEN ON A REAL TTY, WITHOUT --yes, ON PURPOSE. The behavior is a
        PROMPT, and `--yes` is exactly the "never prompt" path — so a `--yes`
        run could never observe it. We walk the live prompt sequence and accept
        the label prompt appearing at ANY point (it fires FIRST in practice).

        The prompt fires on EVERY interactive path, not only App creation: init
        resolves the label up front (before the bridge dispatch) and displays it
        regardless of whether an App is created here, so this env-path run
        (REIN_APP_* present, no 25-minute browser flow) genuinely sees it.
        """
        home = H.isolated_home()
        run = H.spawn_rein(
            ["init", *SAFE_INIT_FLAGS, "--shell", "bash"],  # no --yes: prompts may fire
            env=self.env,
            extra_env=H.isolated_home_env(home),
            timeout=30,
        )
        label_pat = r"(?i)(name this machine|machine label)"
        asked = False
        # The label prompt could land before or after the repo prompt, so match on
        # a pattern SET and answer whatever arrives until init exits. Never expect
        # a single prompt blindly — that turns "absent" into a TIMEOUT error
        # instead of a clean expected failure.
        for _ in range(4):
            i = run.child.expect([label_pat, REPO_PROMPT, pexpect.EOF, pexpect.TIMEOUT], timeout=20)
            if i == 0:
                asked = True
                break
            if i == 1:
                run.answer("")  # Enter => skip session scaffolding
                continue
            break
        run.child.close(force=True)

        self.assertTrue(asked, "init should prompt for an editable, hostname-prefilled machine label (not built)")

    def test_headless_init_prints_a_link_and_does_not_hang(self):
        """Design §5: browser steps degrade to a printed link when headless — BUILT.

        Headless = SSH_CONNECTION set, no DISPLAY/WAYLAND_DISPLAY. Two claims:
        (a) init does NOT hang headless — reaches EOF (guards a regression);
        (b) it prints a browser/install link. The install-on-repo step (§5) now
        prints the deep-link on EVERY path (env path included), degrading to the
        generic installations URL when it doesn't know the App slug and never
        auto-opening a browser on a headless session — so (b) holds here.
        """
        home = H.isolated_home()
        headless = dict(H.isolated_home_env(home))
        headless["SSH_CONNECTION"] = "203.0.113.1 222 203.0.113.9 22"
        headless.pop("DISPLAY", None)
        headless.pop("WAYLAND_DISPLAY", None)
        run = H.spawn_rein(["init", *NON_INTERACTIVE], env=self.env,
                           extra_env=headless, timeout=20)
        hung = False
        try:
            run.child.expect(pexpect.EOF, timeout=15)
        except pexpect.TIMEOUT:
            hung = True
        run.child.close(force=True)

        self.assertFalse(hung, "headless init must not hang (guards a regression)")
        text = run.text().lower()
        printed_link = (
            "github.com/apps" in text
            or "https://github.com/settings/apps" in text
            or "visit this url" in text
        )
        self.assertTrue(printed_link, "headless init should print a browser/install link (not built for the env path yet)")


class InteractiveInitOpenDecisions(ReinTestCase):
    """Open design decisions (§8): SKIPPED — we must not encode undecided behavior.

    Only ONE is left. §8.1 moved to expectedFailure (decided, unbuilt); §8.2/§8.3
    became the real tests above (decided AND shipped in CP4.6/#42).
    """

    @unittest.skip("open decision §8.4: doctor --fix scope for v1 — no-privilege tier only, or include consented-privileged?")
    def test_doctor_fix_scope(self):
        raise NotImplementedError


if __name__ == "__main__":
    unittest.main()
