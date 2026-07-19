# write_ceremony_nono

The #35 write ceremony — declare -> confirm -> verified push — run on the **nono**
sandbox backend (`REIN_SANDBOX=nono`), the nono pivot. It is the nono twin of the
`write_ceremony` journey: same four-phase in-sandbox script, driven live against a
throwaway GitHub repo.

## What it proves

Under nono, exactly as under srt:

1. **Reads flow** with no declaration (the clone succeeds).
2. **Phase 1** — a push BEFORE `rein declare` is **locked** (rc != 0), no prompt.
3. **Phase 2** — `rein declare <n>` (run via the staged in-sandbox `rein` binary,
   reached over the nono-tunnelled declare host) **blocks** for the human; pexpect
   answers the host-side Form A prompt.
4. **Phase 3** — a push to `agent/<n>/<nonce>` **lands** (verified via the GitHub API).
5. **Phase 4** — a non-convention ref is **rejected**.

Plus two nono-specific host-side facts visible in the golden:

- the **containment gate** runs before the agent (`verifying nono containment …` ->
  `containment gate passed`, with the accepted UDP-open warning);
- the nono launch banner (loopback-proxy egress, `--allow` writable paths).

## Shared body

The journey logic lives in `journeys/write_ceremony/journey.py`; this module calls
its `main(sandbox="nono", golden=...)`. Only the golden differs between backends
(the host-side banner + gate diverge; the `SBX|` in-sandbox lines are identical).

## Run

    python3 -m tests.interactive.journeys.write_ceremony_nono.journey          # compare to golden
    REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.write_ceremony_nono.journey  # adopt golden

Prereq: nono installed + digest-verified (`rein init`), a `rein init` App, and a
throwaway repo (resolved the rein-init way). See tests/interactive/CLAUDE.md.
