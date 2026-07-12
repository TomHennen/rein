"""test_git_hardening — the .git host-code-execution escape is CLOSED (#64).

Binding a developer's real checkout writable includes its `.git`, whose
`hooks/` and `config` are host-code-execution surfaces: a prompt-injected agent
that plants `.git/hooks/pre-commit` (or a `core.pager`/`core.fsmonitor` in
`.git/config`) gets code that runs AS THE DEVELOPER, ON THE HOST, at their next
git command in that tree — a time-displaced sandbox escape.

rein closes it by (a) PINNING `<tree>/.git` as its own bind mountpoint, so a
`mv .git .aside` (the rename-parent evasion of srt's path-based ro-bind) fails
EBUSY, and (b) ro-binding `.git/hooks` + `.git/config` so direct writes fail.

WHY THIS TEST EXISTS AND MUST STAY: the rename-parent mechanic (mv the parent
to free the ro-bound child path, then rebuild a malicious `.git`) is THIN in
public docs — no source states it directly. This live test IS rein's evidence
that pinning `.git` closes it. If it ever goes green-by-vacuity (e.g. the escape
script stops running), the security claim loses its proof. Assertions below
therefore check the escape ACTUALLY RAN before checking it FAILED.

These run a `bash -c` escape script INSIDE the sandbox (no tty needed); the pty
is just how the harness drives `rein run`. Throwaway repo only (constraint #1).
"""

from __future__ import annotations

import os
import subprocess
import tempfile
import unittest

import reinharness as H
from itest_base import ReinTestCase


def _clone_plain(repo: str, env: dict) -> str:
    """A fresh normal checkout (its .git is a real dir -> fully hardenable)."""
    d = tempfile.mkdtemp(prefix="rein-hard-plain-")
    subprocess.run(
        ["gh", "repo", "clone", repo, d, "--", "-q"],
        check=True, env=env, capture_output=True, text=True,
    )
    return d


# The in-sandbox escape attempt. $0 is the writable checkout mount (the harness
# appends the workdir as the script's positional arg). Every step prints a
# RAN/RC sentinel so the test can prove the attempt executed, not just that no
# hook fired.
# stderr is captured INLINE via $(... 2>&1) — the sandbox root (incl. /tmp) is
# read-only, so a `2>/tmp/file` redirect would itself fail and mask the result.
_ESCAPE = r"""
cd "$0" || { echo "ESC_CDFAIL=1"; exit 0; }
echo "ESC_PWD=$(pwd)"
echo "ESC_GITKIND=$(test -d .git && echo dir || echo other)"
echo "ESC_RAN=1"
# (a) THE rename-parent escape: free the ro-bound .git path by renaming it.
E=$(mv .git .git.aside 2>&1); echo "ESC_MV_RC=$?"; echo "ESC_MV_ERR=$E"
# (b) direct hook plant (the surface the ro-bind protects).
E=$( { echo '#!/bin/sh'; echo 'echo pwned'; } > .git/hooks/pre-commit 2>&1); echo "ESC_HOOK_RC=$?"; echo "ESC_HOOK_ERR=$E"
# (c) config-based exec (core.pager) via git itself.
E=$(git config --local core.pager 'sh -c pwned' 2>&1); echo "ESC_CFG_RC=$?"; echo "ESC_CFG_ERR=$E"
echo "ESC_DONE=1"
"""


class GitHardening(ReinTestCase):
    def test_git_dir_rename_fails_ebusy_and_hook_config_are_readonly(self):
        """A hardened checkout: `mv .git` fails (EBUSY — .git is a pinned
        mountpoint), and .git/hooks + .git/config are read-only. The escape is
        proven to have RUN, then proven to have FAILED at every step."""
        workdir = _clone_plain(self.env["REIN_TEST_REPO_A"], self.env)
        run = H.spawn_rein_run(["bash", "-c", _ESCAPE], workdir=workdir, env=self.env, timeout=120)
        run.wait(timeout=120)
        out = run.text()

        # PROVE IT RAN (guard against green-by-vacuity).
        self.assertIn("ESC_RAN=1", out, f"escape script never ran:\n{out}")
        self.assertIn("ESC_GITKIND=dir", out, f"workdir was not a real .git-dir checkout:\n{out}")
        self.assertIn("ESC_DONE=1", out, f"escape script did not complete:\n{out}")

        # (a) THE load-bearing assertion: the rename fails, and specifically
        # because .git is busy (a mountpoint), not for some unrelated reason.
        self.assertNotIn("ESC_MV_RC=0", out, f"mv .git SUCCEEDED — the rename-parent escape is OPEN:\n{out}")
        self.assertTrue(
            any(s in out.lower() for s in ("busy", "resource busy", "device or resource")),
            f"mv .git failed but NOT with EBUSY — the failure must be the mountpoint pin, "
            f"not an accident:\n{out}",
        )
        # (b) + (c): the hook and config writes must fail (read-only binds).
        self.assertNotIn("ESC_HOOK_RC=0", out, f".git/hooks/pre-commit was WRITABLE:\n{out}")
        self.assertNotIn("ESC_CFG_RC=0", out, f".git/config was WRITABLE (git config --local succeeded):\n{out}")

    def test_config_worktree_is_also_read_only(self):
        """When extensions.worktreeConfig is set (in the read-only common config),
        git ALSO reads .git/config.worktree — a per-worktree exec surface that
        fires at the REPO ROOT. rein deny-writes it unconditionally, so an
        in-sandbox `git config --worktree core.pager <payload>` must FAIL. The
        agent cannot enable the extension itself (it is in the ro common config),
        so this defends developers who already use `git config --worktree`."""
        workdir = _clone_plain(self.env["REIN_TEST_REPO_A"], self.env)
        # Enable the extension on the HOST (the agent can't — it's in the common
        # config, which is read-only in-sandbox). This is the pre-existing repo
        # state the fix protects.
        subprocess.run(["git", "-C", workdir, "config", "extensions.worktreeConfig", "true"],
                       check=True, env=self.env, capture_output=True, text=True)
        script = (
            'cd "$0" || exit 1\n'
            'E=$(git config --worktree core.pager "sh -c PWNED" 2>&1); echo "CW_RC=$?"; echo "CW_ERR=$E"\n'
            'echo "CW_PAGER=$(git config --get core.pager 2>&1)"\n'
        )
        run = H.spawn_rein_run(["bash", "-c", script], workdir=workdir, env=self.env, timeout=120)
        run.wait(timeout=120)
        out = run.text()
        self.assertNotIn("CW_RC=0", out, f"git config --worktree SUCCEEDED — config.worktree was writable:\n{out}")
        self.assertNotIn("CW_PAGER=sh -c PWNED", out, f"a code-exec core.pager landed via config.worktree:\n{out}")
        # HOST-side: the payload must not have landed in the real checkout.
        host_pager = subprocess.run(["git", "-C", workdir, "config", "--get", "core.pager"],
                                    env=self.env, capture_output=True, text=True).stdout.strip()
        self.assertEqual(host_pager, "", f"core.pager was planted on the host via config.worktree: {host_pager!r}")

    def test_ordinary_edits_still_work_in_a_hardened_tree(self):
        """The pin must not break the developer's actual workflow: editing tracked
        files and committing still work in a hardened checkout (only .git/hooks +
        .git/config are read-only; the tree and the rest of .git stay writable)."""
        workdir = _clone_plain(self.env["REIN_TEST_REPO_A"], self.env)
        script = (
            'cd "$0" || exit 1\n'
            'echo "WORK_EDIT=$(echo hi > agent-edit.txt && echo ok || echo fail)"\n'
            'A=$(git add -A 2>&1); echo "WORK_ADD_RC=$?"; echo "WORK_ADD_ERR=$A"\n'
            'C=$(git commit -q -m "agent edit" 2>&1); echo "WORK_COMMIT_RC=$?"; echo "WORK_COMMIT_ERR=$C"\n'
        )
        run = H.spawn_rein_run(["bash", "-c", script], workdir=workdir, env=self.env, timeout=120)
        run.wait(timeout=120)
        out = run.text()
        self.assertIn("WORK_EDIT=ok", out, f"editing a tracked file failed in a hardened tree:\n{out}")
        self.assertIn("WORK_ADD_RC=0", out, f"git add failed in a hardened tree (.git objects not writable?):\n{out}")
        self.assertIn("WORK_COMMIT_RC=0", out, f"git commit failed in a hardened tree:\n{out}")


if __name__ == "__main__":
    unittest.main()
