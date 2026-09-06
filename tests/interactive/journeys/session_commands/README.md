# session_commands — the human-side `rein session show` / `add-repo` (#69) (COVERED)

The HUMAN-side session management (issue #69, mocks §2):

- `rein session show` prints the standing scope ceiling with per-repo LIVE
  install-coverage (`[App installed]`) and any live-run deltas (`live runs: none` when
  none);
- `rein session add-repo <owner/name>` VALIDATES at write time (same-owner +
  install-coverage probe) then widens the ceiling; the next `show` lists the new repo.

One story: show → add-repo B → show. Also the FIRST live exercise of `rein session
show`, which previously had no test of any kind. RAW golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

**Beside it (plain assertion, not in the golden):** the CROSS-OWNER reject.

**Not in this journey — the IN-SANDBOX view (#180).** `rein session show` run
*inside* the sandbox is a different code path (no session file in there, and never
will be — scope is fixed at launch, outside): it used to print "no session file",
which read as breakage. It now prints the run's fixed scope from
`REIN_IN_SANDBOX_REPOS` plus a pointer to `rein declare <n>` / `rein declare --new`.
Covered by the Go unit test `cmd/rein/session_sandbox_test.go`, not this journey.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.session_commands.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.session_commands.journey   # regenerate the RAW golden
```
