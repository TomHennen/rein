# Containment probe harness (issue #136B)

A differential dev/CI check that verifies rein's **current srt sandbox** actually
contains a possibly-prompt-injected agent. Adopts
[`controlplaneio/sandbox-probe`](https://github.com/controlplaneio/sandbox-probe)
(Apache-2.0, Go) as the enumerator and adds the one thing it can't supply: a
**config-derived oracle** that judges each observation against rein's intent.

Design of record: `docs/containment-probe-harness.md`. This directory implements
the "verification harness" layer only — the in-binary launch gate
(`internal/srt` `RunProbe`/`VerifyConfigApplied`) stays bespoke and is untouched.

## Posture (why this is safe re: licensing)

sandbox-probe is invoked as an **external process**, exactly like `pyte` in
`tests/interactive/`. It is **never imported** into Go code here and is **not** in
`go.mod`, so its Apache-2.0 license never touches the shipped rein binary
(hard-constraint #4). The only Go code in this directory is the oracle + its CLI,
which import `internal/srt` and `internal/proxy` (same module) and nothing else.

## What the oracle checks

The oracle (`oracle.go`) consumes rein's **emitted** `settings.json` (unmarshaled
into `srt.Config`) so the expected/denied sets are the real per-run sets, never a
drifting copy. For each observation it returns:

| Verdict      | Meaning                                                        |
|--------------|---------------------------------------------------------------|
| `ok`         | matches intent (expected-open reachable / denied blocked)     |
| `leak`       | containment failure — **fails the run** (`HasLeak` → exit 3)  |
| `regression` | an expected-open channel is unexpectedly closed (needs agent) |
| `unknown`    | outside config-derived knowledge — surfaced for triage        |

Channels classified today, fully from the emitted config:

- **Network egress** — reachable ⟺ host in `allowedDomains`; a denied host
  reachable in-sandbox is a leak (egress escape).
- **Token placement** — a rein token must appear **iff** the host is in
  `mitmProxy.domains`. A token on a CDN/passthrough or extra-egress host is a
  leak (token onto a pre-signed URL). An inject host reachable with no token is a
  `regression` (would 401).
- **Filesystem read** — most-specific rule wins (as srt applies it): a path
  whose deepest covering rule is a `denyRead` (gh/ssh/`app.pem`/history + rein
  state/key/audit) must be unreadable, readable ⟹ leak; a path re-exposed by a
  deeper `allowRead` re-bind (the #59 home-deny model's toolchain/working-tree
  allow-backs) is expected-readable. A path outside both is `unknown` (triage,
  never silently ok).
- **Sensitive env** — a fixed denylist (`SensitiveEnv`: `ANTHROPIC_API_KEY`,
  `GH_TOKEN`, `AWS_*`, `SSH_AUTH_SOCK`, …) must be scrubbed; present ⟹ leak.
  (rein's env allowlist is build-time, not in `settings.json`, so this list is
  encoded in the oracle rather than derived.)

## Normalized observation schema

The oracle CLI consumes a flat JSON object of channel arrays; `kind` is stamped
from the section. See `testdata/observations.sample.json`:

```json
{
  "network": [{ "target": "api.github.com", "reachable": true, "tokenInjected": true }],
  "files":   [{ "target": "/home/dev/.ssh/id_ed25519", "reachable": false }],
  "env":     [{ "target": "ANTHROPIC_API_KEY", "reachable": false }]
}
```

`reachable` is the **in-sandbox** result (host connectable / file readable / env
present). The harness produces this file by mapping sandbox-probe's native report
(host run vs sandbox run) into it — **that mapping is the current stub** (see
below).

## Running

Oracle CLI directly (works today; use the sample fixture or your own normalized
file plus a real emitted `settings.json`):

```sh
go build -o /tmp/classify ./tests/containment/cmd/classify
/tmp/classify -settings /path/to/emitted/settings.json \
              -observations tests/containment/testdata/observations.sample.json
# exit 3 if any leak
```

Full differential harness:

```sh
SANDBOX_PROBE=/path/to/sandbox-probe REIN_BIN=/path/to/rein \
  tests/containment/run.sh
```

`run.sh` **hard-fails** if sandbox-probe or the rein binary is absent — it never
fabricates results.

## What is stubbed (TODO before this is a live CI gate)

Honest scope: the **oracle + CLI + schema are complete and unit-tested**
(`oracle_test.go`, config built via `srt.Build`). The end-to-end wiring is
skeleton with explicit `TODO(#136B)` markers in `run.sh`:

1. **Fetch/pin sandbox-probe** and **confirm its license is Apache-2.0**
   (hard-constraint #4 — taken here on the design note's word). Not vendored;
   `run.sh` expects it on `PATH` or `$SANDBOX_PROBE`. Confirm its real
   subcommands/flags (the `report --format json` call is a placeholder) and pin
   a version.
2. **sandbox-probe → normalized mapping.** The single piece we can't write until
   we see sandbox-probe's actual output shape. Deliberately not guessed into
   code (would be faking a schema).
3. **Real in-sandbox launch.** Must run the probe through `rein run --` so it
   inherits the exact scrubbed env/seccomp/binds — not a bespoke launcher.
4. **Emitted-settings capture.** The oracle needs the `settings.json` rein wrote
   for that run; rein must expose it (or the harness captures it from the run).
5. **Socket-placement / seccomp / caps / TTY channels.** Stubbed until we see
   which sandbox-probe surfaces; the mitm-socket-placement invariant (CP2) is
   rein-specific and will need a small custom probe.
6. **Golden wiring.** Emit the classified report as a checked-in golden wired
   into a `tests/interactive/` journey so drift = red = re-review (an srt bump
   reopening a channel flips a row red).

## Limits (state loudly, per the design note)

- **Enumeration ≠ soundness.** A positive finding is gold; "found nothing" only
  means none of the *known* channels leaked. This is a regression/drift detector,
  not a proof of confinement.
- **Overt only.** Covert/side channels (timing, `/proc` counters, cache) are out
  of scope.
- **Dual-use.** A leak enumerator is breakout recon pointed at our own sandbox.
  It must stay an enumerator/reporter — never grow into an exploit/escape kit
  (CLAUDE.md #5).
