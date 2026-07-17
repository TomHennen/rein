# sandbox_gh_read_staleness — the #95 regression guard: cross-run gh-read staleness (COVERED)

The load-bearing sandboxed guard for issue #95. A session statically scoped to
`[A, B]`, but BEFORE the run a REAL, currently-valid, **repo-A-ONLY-scoped** gh-read
token is SEEDED at the LEGACY untagged cache path in the run's state dir — the
leftover a prior single-repo-A run wrote.

- **PRE-FIX** the scope-blind broker serves that stale token for the SECOND declare
  and `declare <issueB> --repo B` 404s ("issue not found in B");
- **POST-FIX** the scope-tagged cache MISSES it, re-mints at `[A,B]`, fetches B's
  issue, and the push to B LANDS.

The guard assertions (declare B rc=0, the second Form A rendered, push-to-B landed)
are exactly the surfaces #95 breaks — proven load-bearing: RED on `780a7fb`, GREEN on
the fix. Unlike `multi_repo` (which passes clean-state either way), **the seed is what
makes THIS a regression guard.**

Seeds via the test-support `seedghread` mint (same as `cmd/rein/issue95_live_test.go`);
NOT a rein subcommand — no arbitrary-scope token surface in the shipped CLI. RAW
golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.sandbox_gh_read_staleness.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.sandbox_gh_read_staleness.journey   # regenerate the RAW golden
```
