# push_upstream — `git push -u` reads clean in the sandbox (COVERED)

A sandboxed agent's `git push -u` used to print `could not write config file
.git/config: Device or resource busy` (#64 pins `.git/config` read-only; #102 pt2
/ #119). rein now strips `-u` before real git runs and sets the upstream on the
operator's real checkout itself, post-run. This journey is the reviewable proof —
the golden is the agent's own view of a clean push.

- **Binds a HOST-side hardened checkout, on purpose.** An in-sandbox clone has a
  writable `.git/config`, so `-u` succeeds there regardless of the fix — a golden
  built that way would pass whether or not the fix works (the #119 trap). So it
  clones host-side and lets rein bind it hardened.
- **Golden contract.** Exit 0 = invariants held AND normalized fresh run matches
  `golden.txt`; 1 = drift; 2 = the run broke. If the fix regresses, the EBUSY line
  reappears and trips drift.
- **Off-golden invariants:** `@GITKIND=dir`, `push rc==0`, one prompt, branch
  landed, no EBUSY, and rein set `branch.<x>.remote/merge` on the real checkout
  (from the host `.git/config` and `helper.log`).
- Self-contained: creates its own throwaway issue + branch, cleans both up.

Beside it (plain test, no golden): `test_push_upstream.py` asserts the same invariant.

```sh
python3 -m tests.interactive.journeys.push_upstream.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.push_upstream.journey   # regenerate the golden
```
