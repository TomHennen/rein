# init_autodetect — `rein init` repo-prompt default from cwd git `origin` (#69/#78) (COVERED)

#78 made `rein init`'s repo-prompt DEFAULT the repo the human is STANDING IN:
`cmd/rein/gitremote.go:detectRepoFromGit` reads the cwd's git `origin` URL →
`owner/name`, and `resolveRepoForSession` offers it as the prompt default. The same
detection turns `rein run`'s cold "no session" dead-end into a hint that NAMES the cwd
repo (`gitremote.go:noSessionHint`). FOUR legs, all captured in the golden, each a real
interactive `rein` under a pty (init NO `--yes`, so the prompt renders), confined to a
throwaway HOME/XDG tempdir:

- **init DETECTED:** from a checkout of the throwaway; the repo prompt is PRE-FILLED
  with the detected `[owner/name]`, the human accepts with Enter, and the session is
  scaffolded for the detected repo.
- **init CONTRAST:** from a NON-git dir; there is no `origin`, so the prompt has NO
  default (the bare prompt) — proving the default is cwd-derived, not hardcoded.
- **run DETECTED:** `rein run --direct` with no session FROM the checkout — its cold
  "no session" failure carries a hint naming the cwd repo (`rein init --repo <repo>`).
- **run CONTRAST:** `rein run --direct` with no session from the NON-git dir — the hint
  degrades to the generic `run rein init`, again proving it is cwd-derived.

The `run --direct` legs DO print the loud UNSANDBOXED-MODE banner and it is captured
whole — the same as every other line the flow produced (nothing is sliced). The
"checkout" is a bare `git init` + `remote add origin` at the throwaway — enough for
origin-URL detection, touching no real repo (hard-constraint #1). RAW golden,
normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.init_autodetect.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.init_autodetect.journey   # regenerate the RAW golden
```
