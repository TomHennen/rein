# onboarding — the first-run `rein init` guided flow (COVERED)

The interactive `rein init` a new user runs, then `rein doctor` to verify
(onboarding-ux-design.md §3). The slices it demonstrates:

- **§4/§8.1 machine-label prompt** — init asks "Name this machine", PRE-FILLED with
  the detected hostname and editable; the label is woven into the App name
  (`rein-<role>-<label>-<shortrand>`). The golden SHOWS the pre-filled prompt.
- **§5 install-on-repo** — after scaffolding the session, init prints the install
  deep-link (no `ssh -L` needed), degrading to the generic installations URL when it
  doesn't yet know the App slug. The golden SHOWS that link.
- **§2/§6 then `rein doctor`** — the post-onboarding verification; its FULL output is
  in the golden (#82: `run_journey` captures the complete session, so no section is
  hand-dropped), with machine-variable paths and the mint expiry normalized at compare
  time.

**The UNDRIVEABLE seam (marked, not faked):** the App-CREATION step (§3 step 4) is a
real browser/OAuth-callback flow (~25 min) and CANNOT run in a suite. This journey
stays on the ENV path (keeps `REIN_APP_*` present, so init never routes into
`RunManifestFlow`) and captures everything around that seam. The manifest-path
deep-link is exercised by the separate, genuinely browser-bound walkthrough
`scripts/cp5-manifest-manual-test.sh`.

Determinism: `REIN_MACHINE_HOSTNAME` pins the pre-filled hostname, the repo answer is
a fixed demo slug, and per-run volatiles are normalized at compare time. Every write
is confined to a throwaway HOME/XDG tempdir (hard-constraint #1). RAW golden,
normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.onboarding.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.onboarding.journey   # regenerate the RAW golden
```
