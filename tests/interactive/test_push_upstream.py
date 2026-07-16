"""test_push_upstream — `git push -u` no longer faults on the hardened .git/config
(#102 part 2 / #119), and rein sets the upstream tracking on the real checkout.

THE BUG (#119): a sandboxed agent's `git push -u` writes branch.<x>.remote/merge
into .git/config, which #64 pins READ-ONLY in the sandbox, so git prints
`could not write config file .git/config: Device or resource busy`. #103's
argv-rewrite fix lived in the rein-git shim, which is NOT on the sandbox PATH, so
it never ran in the mode that matters. This test is the regression #119 asked for.

THE TRAP #119 CALLS OUT (and the reason this test is shaped the way it is): a
clone made INSIDE the sandbox's writable mount has a WRITABLE .git/config, so
`-u` succeeds there regardless of any fix — green for a path nothing exercises.
So this binds a HOST-SIDE clone HARDENED by rein (read-only .git), which is the
only configuration in which the bug can appear. `spawn_rein_run(workdir=...)`
exports it as REIN_SANDBOX_WORKDIR, so rein binds THAT tree (bound, not
ephemeral) and `cd "$0"` lands the in-sandbox script in it.

PROVE-IT-RAN (guard against green-by-vacuity, per test_git_hardening's rule): git
only writes the upstream config on a SUCCESSFUL push, so the absence of the EBUSY
line is only meaningful if the push actually LANDED. The test asserts rc==0 and
the branch on the remote BEFORE it trusts the no-EBUSY assertion.

Gated on a real throwaway-repo issue (REIN_ITEST_ISSUE), like the other
declare-flow tests — the declare fetches the issue before Form A fires.
Throwaway repo only (hard-constraint #1).
"""

from __future__ import annotations

import subprocess
import tempfile
import unittest

import reinharness as H
from itest_base import DECLARE_ISSUE_ENV, ReinTestCase, declare_issue

_NEEDS_ISSUE = unittest.skipUnless(
    declare_issue(),
    f"gated: set {DECLARE_ISSUE_ENV} to a real issue number on the throwaway repo",
)

# The EBUSY signatures the fix must eliminate from the agent's view.
_EBUSY_MARKERS = (
    "could not write config file .git/config",
    "Device or resource busy",
)


def _clone_hardened(repo: str, env: dict) -> str:
    """A HOST-side normal checkout (real .git dir -> rein binds it hardened).

    origin is forced to the plain https URL so the in-sandbox push routes through
    rein's injecting proxy (the token-mint + ref cross-check point).
    """
    d = tempfile.mkdtemp(prefix="rein-upstream-clone-")
    subprocess.run(
        ["gh", "repo", "clone", repo, d, "--", "-q"],
        check=True, env=env, capture_output=True, text=True,
    )
    subprocess.run(
        ["git", "-C", d, "remote", "set-url", "origin", f"https://github.com/{repo}.git"],
        check=True, env=env, capture_output=True, text=True,
    )
    return d


def _upstream_push_script(issue: int, good: str) -> str:
    """In-sandbox bash: work in the BOUND checkout ($0), declare, then push WITH
    -u. No clone — the point is to push from the hardened bound tree, not a fresh
    writable one."""
    return "\n".join([
        "set +e",
        'cd "$0"',
        'echo "SBX_GITKIND=$(test -d .git && echo dir || echo other)"',
        f"git checkout -q -b {good}",
        'echo "harness upstream probe $(date -u +%FT%TZ)" >> probe-upstream.txt',
        "git add -A",
        'git commit -q -m "interactive harness: push -u upstream"',
        "echo SBX_DECLARE1_START",
        f"rein declare {issue}",
        'echo "SBX_DECLARE1_RC=$?"',
        f'echo "SBX_PUSH1_START branch={good}"',
        f"git push -u origin HEAD:refs/heads/{good}",
        'echo "SBX_PUSH1_RC=$?"',
        "echo SBX_SCRIPT_DONE",
    ])


@_NEEDS_ISSUE
class PushUpstreamHardened(ReinTestCase):
    """`git push -u` from a hardened bound checkout: lands, no EBUSY, tracking set."""

    def test_push_u_lands_without_ebusy_and_sets_tracking(self):
        issue = declare_issue()
        good = f"agent/{issue}/{H.unique_branch('up')}"
        self._branches.append(good)

        workdir = _clone_hardened(self.repo, self.env)
        script = _upstream_push_script(issue, good)
        run = H.spawn_rein_run(
            ["bash", "-c", script], workdir=workdir, env=self.env,
            extra_env=self.pinned_session_env(),
        )

        run.expect_prompt(timeout=120)
        run.answer(str(issue))
        run.expect_approved(timeout=60)
        run.child.expect(r"SBX_PUSH1_RC=\d+", timeout=120)
        run.wait(timeout=90)
        out = run.text()

        # PROVE IT RAN: the tree really was a hardened bound .git, and the push
        # actually landed (git only writes upstream config on a successful push,
        # so no-EBUSY is only meaningful once the push succeeds).
        self.assertIn("SBX_GITKIND=dir", out, f"workdir was not a real .git checkout:\n{out}")
        self.assertEqual(run.declare_rc(), 0, "declare should succeed after confirmation")
        self.assertEqual(run.sentinel_rc(1), 0, f"push -u must SUCCEED (rc=0):\n{out}")
        self.assertTrue(H.branch_exists(self.repo, good, self.env), "the branch must land on the remote")

        # THE FIX: the agent must NOT see the read-only .git/config fault.
        for marker in _EBUSY_MARKERS:
            self.assertNotIn(marker, out, f"push -u still surfaced the hardened-config fault ({marker!r}):\n{out}")

        # THE UX (#102 pt2 steer): rein set the upstream tracking on the real,
        # host-side checkout on the operator's behalf.
        remote = subprocess.run(
            ["git", "-C", workdir, "config", "--get", f"branch.{good}.remote"],
            env=self.env, capture_output=True, text=True,
        ).stdout.strip()
        merge = subprocess.run(
            ["git", "-C", workdir, "config", "--get", f"branch.{good}.merge"],
            env=self.env, capture_output=True, text=True,
        ).stdout.strip()
        self.assertEqual(remote, "origin", "rein must set branch.<x>.remote on the real checkout")
        self.assertEqual(merge, f"refs/heads/{good}", "rein must set branch.<x>.merge on the real checkout")


if __name__ == "__main__":
    unittest.main()
