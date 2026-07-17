# init_then_run — `rein init` (real mint, no env) then a real `rein run` clone (#128) (COVERED)

The LIVE companion to `init_steady_state`. That journey proves `rein init` RESOLVES
App config from `state.json` with no `REIN_APP_*` — but it stops there
(`--skip-mint-check`), so it never shows the resolved App actually WORKING (PR #128
review: "we never see rein run work after init"). This journey closes that: it seeds
the box's REAL `rein init` App (`state.json` + `primary.pem`) into an isolated home,
clears `REIN_APP_*`, then drives three real steps, all in the golden:

- **init** (NO `--skip-mint-check`): a REAL mint check —
  `mint check: … ok (token expires <TS>)` — proving the state-resolved App actually
  mints, with no env vars.
- **`rein run --direct`**: DIRECT mode mints a git token and a real `git clone` of the
  throwaway lands THROUGH the broker credential helper.
- **`rein run`** (SANDBOXED, the default mode): the same broker read, but the clone
  runs INSIDE the srt sandbox. Both modes must work after init.

Together they are the end-to-end proof of what the #126 harness change unblocks: the
real `state.json` App mints and drives a live git operation in BOTH modes,
env-var-free. The clone proves a broker read only because the throwaway is PRIVATE (an
unauthenticated clone would 404) — a landed `.git` means a token was minted +
injected. RAW golden, normalize-on-compare. Hard-constraint #1: every write is confined
to a throwaway HOME/XDG tempdir; the only network touch is a read of the throwaway.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke,
**3** = SKIPPED (no configured `rein init` App to mint with — never a false green,
#68).

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.init_then_run.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.init_then_run.journey   # regenerate the RAW golden
```
