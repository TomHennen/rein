# push_upstream — `git push -u` reads clean in the sandbox (COVERED)

A sandboxed agent's `git push -u` used to print `could not write config file
.git/config: Device or resource busy` — `-u` writes `branch.<x>.remote/merge` into
`.git/config`, which #64 pins **read-only** in the sandbox (#102 pt2 / #119). rein
now strips `-u` before real git runs (so the push reads normal) and sets the
upstream on the operator's real checkout itself, post-run. This journey is the
reviewable proof.

- **Binds a HOST-side hardened checkout, on purpose.** The bug only exists when
  `.git/config` is the #64 read-only bind. A clone made *inside* the sandbox's
  writable mount has a writable `.git/config`, so `-u` succeeds there regardless of
  the fix — a golden built that way would be green whether or not the fix works
  (the trap #119 calls out). So this clones **host-side** and lets rein bind it
  hardened; `cd "$0"` lands the in-sandbox script in it.
- **Golden contract.** Exit **0** = invariants held AND the normalized fresh run
  matches `golden.txt`; **1** = drift; **2** = the run broke (a wrong rc / prompt /
  branch / a resurfaced `.git/config` fault). The golden is the agent's own view of
  a **clean** push; if the fix regresses, the EBUSY line reappears and trips drift.
- **Prove-it-ran.** git only writes upstream config on a *successful* push, so the
  clean transcript only counts once the push landed — the invariants assert
  `@GITKIND=dir`, `push rc==0`, and the branch on the remote first.
- **Tracking-set is asserted off-golden.** rein sets `branch.<x>.remote/merge` on
  the real checkout host-side after the run and logs it to `helper.log` (a file, not
  terminal output) — asserted as invariants, not in the golden.
- **Self-contained.** Creates its own throwaway issue + branch, deletes both in a
  `finally`. `REIN_DEMO_ISSUE=<n>` reuses an issue.

**Beside it (plain test, no golden):** `test_push_upstream.py` — the same invariant
(hardened bound checkout, `git push -u` lands, no EBUSY, tracking set), as a
pass/fail assertion.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.push_upstream.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.push_upstream.journey   # regenerate the RAW golden
```
