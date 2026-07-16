# direct_mode — the #35 ceremony UNSANDBOXED (`--direct`) (COVERED)

The direct twin of `write_ceremony`: the SAME #35 ceremony without the sandbox.
Reads flow; a pre-declaration push is BLOCKED by the **credential-helper channel** —
a non-secret PLACEHOLDER credential + a stderr hint naming `rein declare` (issues
#45/#35), then git's OWN `Authentication failed` (NOT a proxy `remote error: rein:`
ERR — there is no proxy in direct mode). `rein declare <n>` prompts on the host
terminal; the verified push LANDS. No proxy means **no ref cross-check** — that stays
a sandbox-only feature.

RAW golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.
The contrast with the sandboxed ceremony is documented in the journey's docstring.
`REIN_DEMO_ISSUE=<n>` reuses an issue.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.direct_mode.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.direct_mode.journey   # regenerate the RAW golden
```
