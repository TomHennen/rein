# init_autodetect — `rein init` repo-prompt default from cwd git `origin` (#69/#78) (COVERED)

#78 made `rein init`'s repo-prompt DEFAULT the repo the human is STANDING IN:
`cmd/rein/gitremote.go:detectRepoFromGit` reads the cwd's git `origin` URL →
`owner/name`, and `resolveRepoForSession` offers it as the prompt default. Two legs,
both a real interactive `rein init` under a pty (NO `--yes`, so the prompt renders),
each confined to a throwaway HOME/XDG tempdir:

- **DETECTED:** init runs from a checkout of the throwaway; the repo prompt is
  PRE-FILLED with the detected `[owner/name]`, the human accepts with Enter, and the
  session is scaffolded for the detected repo.
- **CONTRAST:** init runs from a NON-git dir; there is no `origin`, so the prompt has
  NO default (the bare prompt) — proving the default is cwd-derived, not hardcoded.

A PLAIN ASSERTION rides along (NOT in the golden): `rein run` with no session prints a
hint naming the cwd repo (`gitremote.go:noSessionHint`). Reaching it needs `--direct`,
whose loud UNSANDBOXED-MODE banner would muddy an init-focused golden, so it stays an
assertion. The "checkout" is a bare `git init` + `remote add origin` at the throwaway
— enough for origin-URL detection, touching no real repo (hard-constraint #1). RAW
golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.init_autodetect.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.init_autodetect.journey   # regenerate the RAW golden
```
