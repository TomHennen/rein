# multi_repo — REAL cross-repo work in ONE run (COVERED)

A session statically scoped to TWO same-owner, App-installed throwaway repos, where
ONE `rein run` does real work in BOTH. The launch banner names the full ceiling
`repos=[A B]` (the #68 gate cleared BOTH — no separate launch-gate demo needed); the
agent CLONES both (reads flow, no declaration), then:

- `declare <issueA> --repo A` → approve → push LANDS on A;
- `declare <issueB> --repo B` → the SECOND declare renders the "agent wants to ALSO
  work on an issue" confirm (an additional-ISSUE confirm **within scope** — B is
  already in the ceiling, so `session.Contains` → NOT the AddRepo SCOPE-EXPANSION
  prompt an out-of-scope repo would trip, see `scope_expansion`) → approve → push
  LANDS on B.

Both branches are then verified host-side as actually landed on GitHub. This proves
the brokered run genuinely spans the ceiling and writes across BOTH repos in one run —
not merely that a 2-repo session launches.

This is the multi-repo HAPPY PATH, **not** the #95 guard (see
`sandbox_gh_read_staleness`): it passes clean-state with or without the #95 fix. Runs
**SANDBOXED** (the default; it ran `--direct` only while #95 blocked the sandboxed
second declare — that fix landed). RAW golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.multi_repo.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.multi_repo.journey   # regenerate the RAW golden
```
