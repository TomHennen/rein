# Containment probe harness (issue #136B)

A dev/CI check that verifies rein's **current srt sandbox** actually
contains a possibly-prompt-injected agent. Adopts
[`controlplaneio/sandbox-probe`](https://github.com/controlplaneio/sandbox-probe)
(Apache-2.0, Go) as the enumerator and adds the one thing it can't supply: a
**config-derived oracle** that judges each observation against rein's intent.

Design of record: `docs/containment-probe-harness.md`. This directory implements
the "verification harness" layer only — the in-binary launch gate
(`internal/srt` `RunProbe`/`VerifyConfigApplied`) stays bespoke and is untouched.

## Posture (why sandbox-probe is a separate process)

This is **not** a licensing constraint. rein is licensed **Apache-2.0** (`./LICENSE`)
and sandbox-probe is **Apache-2.0** — the same license both ways, so importing it
would be fully license-compatible with **zero incompatibility**. Apache-2.0 poses
no barrier if pulling it in as a Go dependency later proves cleaner.

The reason it runs as an **external process** is **architectural**: sandbox-probe
is a runnable enumeration *harness*, not a library designed to be imported, and
keeping it out-of-binary keeps this whole containment check a standalone test
artifact rather than code linked into the shipped `rein`. That mirrors the
project's `pyte` posture (root `CLAUDE.md`, "Libraries"): a test-only dependency,
never linked into or shipped with the Go binary. It is **not** in `go.mod`; the
only Go code in this directory is the oracle + its CLI, which import
`internal/srt` and `internal/proxy` (same module) and nothing else.

## What the oracle checks

The oracle (`oracle.go`) consumes rein's **emitted** `settings.json` (unmarshaled
into `srt.Config`) so the expected/denied sets are the real per-run sets, never a
drifting copy. For each observation it returns:

| Verdict      | Meaning                                                        |
|--------------|---------------------------------------------------------------|
| `ok`         | matches intent (expected-open reachable / denied blocked)     |
| `leak`       | containment failure — **fails the run** (`HasLeak` → exit 3)  |
| `regression` | an expected-open channel is unexpectedly closed (needs agent) |
| `unknown`    | outside config-derived knowledge — fails until triaged        |

Channels classified today, fully from the emitted config:

- **Network egress** — reachable ⟺ host in `allowedDomains`; a denied host
  reachable in-sandbox is a leak (egress escape).
- **Token placement** — the oracle can judge `tokenInjected`, but `run.sh` has
  no observable evidence for it and does not produce it; token placement is
  covered by the proxy unit tests.
- **Filesystem read** — most-specific rule wins (as srt applies it): a path
  whose deepest covering rule is a `denyRead` (gh/ssh/`app.pem`/history + rein
  state/key/audit) must be unreadable, readable ⟹ leak; a path re-exposed by a
  deeper `allowRead` re-bind (the #59 home-deny model's toolchain/working-tree
  allow-backs) is expected-readable. A path outside both is `unknown` (triage,
  never silently ok).
- **Unix sockets** — creating an `AF_UNIX` socket is a leak; without creation
  nothing can be connected, so that one row carries the judgement. A socket
  path that is merely visible is `ok`, as is expected on any host (docker, the
  nix daemon, tmux, systemd), unless it shows through a read deny (leak).
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
present). `run.sh` also writes `writes` (persisted on the host), `sockets` (the
`AF_UNIX` creation row), `empty_dirs` (dirs sandbox-probe flagged that are empty
in-sandbox), and a `probe` path on each file entry (what was actually read).
sandbox-probe's own in-sandbox report is merged in with `-probe-report`.

## Running

Oracle CLI directly (works today; use the sample fixture or your own normalized
file plus a real emitted `settings.json`):

```sh
go build -o /tmp/classify ./tests/containment/cmd/classify
/tmp/classify -settings /path/to/emitted/settings.json \
              -observations tests/containment/testdata/observations.sample.json
# exit 3 if any leak
```

Full harness:

```sh
SANDBOX_PROBE=/path/to/sandbox-probe REIN_BIN=/path/to/rein \
  tests/containment/run.sh
```

`run.sh` **hard-fails** if sandbox-probe or the rein binary is absent — it never
fabricates results.

## How to run (wired end-to-end, #141)

```sh
go install github.com/controlplaneio/sandbox-probe@latest   # once per box
go build -o bin/ ./cmd/...
tests/containment/run.sh                        # compare to the committed golden
REIN_UPDATE_GOLDEN=1 tests/containment/run.sh   # adopt a new golden
```

`run.sh` (0) builds a throwaway fixture `$HOME`, (1) captures the emitted
settings.json via `REIN_SRT_SETTINGS_COPY` and derives probe TARGETS from it
(`classify -targets`: deny paths, allow-backs, srt's own default write paths —
the substrate-chosen set that leaked in #153), (2) runs sandbox-probe plus the
target probes through the REAL `rein run --sandbox` launch, (3) checks write
PERSISTENCE host-side, (4) classifies everything against that run's own
settings.json, and (5) compares the normalized rows to `golden-report.txt`
(drift or an untriaged row = exit 4; any leak = exit 1).

It fails closed on its own inputs: stale outputs are removed first, and the run
dies if sandbox-probe fails or writes no report in-sandbox, if the report lacks
a finding type the oracle maps, or if it does not list `/etc/passwd` as readable
(the liveness canary: a scan that saw nothing did not run).

## Fixture home

The suite does not probe the developer's home. `run.sh` points `$HOME` at a
fresh directory that `classify -fixture` fills with fake files: one for every
denyRead and allowRead entry rein configures under home, srt's default write
dirs, and sentinels only the wholesale home deny hides (`.aws/credentials`,
`.docker/config.json`, `.bash_history`, ...). The list is
`cmd/classify/fixture.go`; `classify -targets` fails if the policy has a home
entry the fixture does not plant, so a new deny or allow-back cannot go unprobed.

rein still needs its real App config and state, so `XDG_CONFIG_HOME` and
`XDG_STATE_HOME` are pinned to the real locations. rein denies those dirs
in-sandbox as usual (the `<XDG_CONFIG>` / `<XDG_STATE>` rows), and writes its
normal audit log there. Nothing else in the real home is written; the claude
overlay is a throwaway inside the fixture home (a writable bind nested in the
home deny). The rest of the real home is NOT hidden during this run, since it
is no longer `$HOME`: only the suite's own probes run in it.

Those `<XDG_*>` rows and `/run/user/<UID>` are the only deny rows outside the
fixture. Where such a path is absent or empty on a host, its "correctly
unreadable" row proves nothing there; `run.sh` says so in a `NOTE:` line before
`PASS`. The same rules are content-tested through their fixture copies
(`<HOME>/.config/gh`, ...).

Every `REIN_*` variable is unset before launch (they change the emitted
policy), and every name in `SensitiveEnv` is set to a canary value on the host,
so "correctly scrubbed" is proven even where the developer has no such variable.

What this gives up: a developer's own layout (tools under `~/.nvm`, a symlinked
`~/.nix-profile`, relocated `GNUPGHOME`) is not exercised here, and neither are
the install-chain allow-backs rein adds when srt itself lives under `$HOME`
(`~/.npm-global`). The `--sandbox` journeys run against the real home, and
`TestSandboxPathEnv` covers the symlinked-PATH case.

### Checking the suite itself

`CONTAINMENT_WEAKEN` re-applies knobs after the scrub, to confirm a weakened
sandbox fails (`$HOME` is the fixture):

```sh
CONTAINMENT_WEAKEN='REIN_SANDBOX_SHOW_HOME=1' tests/containment/run.sh             # leak
CONTAINMENT_WEAKEN='REIN_SANDBOX_ALLOW_READ=$HOME/.npm' tests/containment/run.sh   # drift
```

## Golden and tolerated rows

With the fixture, the report is the same on every machine and is compared
exactly to `golden-report.txt`. Two kinds of row live outside the fixture, vary
by host, and are left out of the golden:

- **`tolerated.txt`**: reviewed `unknown` rows (`/etc`, `/proc`), one per line
  as `kind | target | why it is acceptable`. Each may or may not appear. Targets
  match literally (`/proc/1/cgroup` excuses pid 1 only). Any other `unknown` row
  fails the run until someone triages it here. Hand-edited;
  `REIN_UPDATE_GOLDEN` never touches it, and the golden can only hold `ok` rows.
- **Visible unix sockets**: which sockets a host has is arbitrary. A visible
  path is `ok` and not in the golden; the pinned `AF_UNIX` creation row is what
  proves none of them can be connected. Visible under a read deny is a leak.

Rows are normalized: `<HOME>` (the fixture), `<WORKTREE>`, `<FIXTURE>`,
`<XDG_CONFIG>`, `<XDG_STATE>`, `<UID>` (this user's uid only; another user's
`/run/user/N` stays literal), `<PID>`, `<RUNID>`. `_out/report.txt` is the raw
report, `_out/report.normalized.txt` the compared rows, and
`_out/observations.json` records which file each deny was read through.

Probe semantics, decided the hard way:

- **Reads are CONTENT-level, not name-level.** A denied dir is probed through a
  host-sampled file inside it, and dir readability means "a real file readable
  within depth 3" — the deny tmpfs legitimately lists allow-back scaffolding and
  tombstones (#150), so a non-empty listing is not a leak. The row is reported
  under the deny ENTRY, not the sampled file, so it reads the same everywhere.
  sandbox-probe itself calls a dir readable when it can list it, so a denied
  dir it flags that is EMPTY in-sandbox (the tmpfs mount point) is not counted.
- **GH_TOKEN in-sandbox is rein's deliberate stub**, not a secret; only a
  non-stub value counts as present.
- **The ephemeral clone dir is skipped** in the write sweep (discarded by rein
  after the run by design, and per-run).
- **Egress is mostly out of this harness's reach.** rein is the egress proxy
  (#185), so the emitted srt allowlist is empty. Two fixed hosts are probed: a
  denied canary (`example.com`) and a control that must be reachable
  (`api.github.com`), so a broken proxy or CA cannot pass as "denied". rein's
  own egress policy is covered by the proxy tests and the egress journeys.
- **The in-sandbox probe is Python + curl**, so both must be on `PATH` and
  visible in the sandbox. Porting it into `classify` would drop that; not done.

Remaining (tracked in #141): the mitm-socket-placement custom probe, and wiring
this into a journey / CI once srt runs anywhere in CI (#93).

## Limits (state loudly, per the design note)

- **Enumeration ≠ soundness.** A positive finding is gold; "found nothing" only
  means none of the *known* channels leaked. This is a regression/drift detector,
  not a proof of confinement.
- **Overt only.** Covert/side channels (timing, `/proc` counters, cache) are out
  of scope.
- **Dual-use.** A leak enumerator is breakout recon pointed at our own sandbox.
  It must stay an enumerator/reporter — never grow into an exploit/escape kit
  (CLAUDE.md #5).
