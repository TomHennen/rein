# app_not_installed — MISCONFIG: App not installed on a session repo (#68) (COVERED)

Closes the #68 GAP: the D4 install-coverage check early-returned on the `REIN_APP_*`
env path, so an uncovered session repo used to launch happily and fail *inside* the
agent. A live journey here catches it; the unit tests didn't. Two legs, both a real
`rein run --direct` (the coverage gate runs before the mode split, so `--direct`
exercises it without the sandbox stack):

- **misconfig:** a session naming a fictional `<owner>/definitely-not-installed` — a
  FIXED name (stable-by-construction, so not normalized) that does not exist and 404s
  identically to "App not installed on repo", touching **no real repo**
  (hard-constraint #1). rein must refuse at LAUNCH, exit 1, and the refusal must name
  the repo, name the App (slug), and carry the App-specific `.../installations/new`
  deep-link. The inner command never runs.
- **control:** a normal single-repo session on the throwaway clears the gate and the
  inner command actually runs, exit 0.

RAW golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.app_not_installed.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.app_not_installed.journey   # regenerate the RAW golden
```
