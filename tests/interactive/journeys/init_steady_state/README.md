# init_steady_state — `rein init` re-run resolves the App from `state.json`, no `REIN_APP_*` (#128) (COVERED)

After a machine finishes the GitHub App manifest flow its App config lives in
`state.json` + the managed keystore, NOT in `REIN_APP_*` env vars. A re-run of
`rein init` in that state must resolve config from disk with no env vars — it didn't:
init's `BridgeUseState` branch called the env-ONLY loader and hard-failed with
`missing env var REIN_APP_CLIENT_ID (did you source ./dev-env?)` once the install-id
was cached. The fix routes that branch through `resolveStateApp` → `config.ResolveApp`
(the state path `rein doctor` uses). This journey is the live guard; no journey or
unit test exercised a steady-state re-run with env absent, which is why it shipped.

Three legs, each a real `rein init` under a pty in its OWN isolated HOME/XDG world
seeded with a manifest-flow `state.json`:

- **CACHED:** install-id already fetched (the true steady state). init resolves
  client_id/installation_id from `state.json` — no env vars — prints the app line and
  finishes. The leg that used to fail.
- **UNCACHED:** App registered but not yet installed (install-id 0). init recognizes
  the known intermediate state, prints the install-deep-link hint, and exits 0.
- **STALE PEM:** still the state path (identity vars absent), but a leftover
  `REIN_APP_PRIVATE_KEY_PATH` points at a now-deleted file (what a past
  `source ./dev-env` leaves behind). init validates the MANAGED keystore PEM the mint
  actually reads, NOT the stale env path, so it still exits 0.

This journey does the OPPOSITE of every other init journey — it CLEARS `REIN_APP_*`
(the state path is env-FREE by construction) and uses a SYNTHETIC `state.json` in an
isolated config dir, so it needs no real App/network and does NOT depend on `dev-env`
(#126). Every leg runs `--yes --no-alias --no-symlink --skip-mint-check`: `--yes`
keeps it non-interactive, `--skip-mint-check` keeps it offline (the mint is covered by
`init_then_run`; this journey is about which loader init uses). RAW golden,
normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.init_steady_state.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.init_steady_state.journey   # regenerate the RAW golden
```
