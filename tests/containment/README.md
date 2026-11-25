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
- **Host env** — rein's env policy is an allowlist, so the check is by value:
  the host sets a random canary under each `EnvCanaryNames` entry (the usual
  secrets plus one arbitrary name), and the canary appearing in ANY in-sandbox
  variable is a leak.

## How to run

```sh
go install github.com/controlplaneio/sandbox-probe@v1.1.0   # once per box
go build -o bin/ ./cmd/...
tests/containment/run.sh                        # compare to the committed golden
REIN_UPDATE_GOLDEN=1 tests/containment/run.sh   # adopt a new golden
```

`SANDBOX_PROBE`, `REIN_BIN` and `OUT_DIR` override the defaults. sandbox-probe
is pinned: `run.sh` refuses any other version, because the real home is
readable to that binary while the suite runs (see "Fixture home").

`run.sh` (0) builds a throwaway fixture `$HOME`, (1) captures the emitted
settings.json via `REIN_SRT_SETTINGS_COPY` and derives probe TARGETS from it
(`classify -targets`: deny paths, allow-backs, srt's own default write paths —
the substrate-chosen set that leaked in #153), (2) runs sandbox-probe plus the
target probes through the REAL `rein run --sandbox` launch, (3) classifies
everything, including host-side write PERSISTENCE, against that run's own
settings.json, and (4) compares the normalized rows to `golden-report.txt`.

| Exit | Meaning |
|------|---------|
| 0 | pass |
| 1 | a leak, or the harness itself failed |
| 4 | golden drift, or an `unknown` row `tolerated.txt` does not list |
| 5 | a `CONTAINMENT_WEAKEN` run that was not detected (never a pass) |

It fails closed on its own inputs: stale outputs are removed first, and the run
dies if sandbox-probe fails or writes no report in-sandbox, if the report lacks
a finding type the oracle maps, if it does not list `/etc/passwd` as readable
(the liveness canary: a scan that saw nothing did not run), or if the
supplementary probe's observations are missing a section.

## Fixture home

The suite does not probe the developer's home. `run.sh` points `$HOME` at a
fresh directory that `classify -fixture` fills with fake files
(`cmd/classify/fixture.go`): one for every denyRead and allowRead entry rein
configures under home, srt's default write dirs, and files only the wholesale
home deny hides (`.aws/credentials`, `.docker/config.json`, ...).
`classify -targets` fails if the policy has a home entry the fixture does not
plant, so a new deny or allow-back cannot go unprobed. That check runs against
the real emitted policy on every run; no unit test can reach those lists.

A deny row is judged by walking the whole entry in-sandbox: ANY non-empty file
readable under it is a leak, except inside the binds rein nests there on
purpose (allow-backs, writable binds). So the `<HOME>` row covers every
fixture file no narrower rule names.

rein still needs its real App config and state, so `XDG_CONFIG_HOME` and
`XDG_STATE_HOME` are pinned to the real locations. rein denies those dirs
in-sandbox as usual (the `<XDG_CONFIG>` / `<XDG_STATE>` rows), and writes its
normal audit log there. Nothing else in the real home is written. The rest of
the real home is NOT hidden during this run, since it is no longer `$HOME`:
the suite's own probes and the pinned sandbox-probe run with it readable, and
rein has no knob to deny an extra directory.

Those `<XDG_*>` rows and `/run/user/<UID>` are the only deny rows outside the
fixture. Where such a path has no readable content on a host, its "correctly
unreadable" row proves nothing there; the report says so in a `NOTE:` line.
The same rules are content-tested through their fixture copies
(`<HOME>/.config/gh`, ...).

Every `REIN_*` variable is unset before launch: they change the emitted policy.

What this gives up: a developer's own layout (tools under `~/.nvm`, a symlinked
`~/.nix-profile`, relocated `GNUPGHOME`) is not exercised here, and neither are
the install-chain allow-backs rein adds when srt itself lives under `$HOME`
(`~/.npm-global`). The `--sandbox` journeys run against the real home, and
`TestSandboxPathEnv` covers the symlinked-PATH case.

### Checking the suite itself

`CONTAINMENT_WEAKEN` re-applies knobs after the scrub, to confirm a weakened
sandbox fails (`$HOME` is the fixture). Such a run prints a banner, cannot
update the golden, and never exits 0: if the weakening goes unnoticed it exits 5.

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
  fails the run and is printed with the exact target to add. Hand-edited;
  `REIN_UPDATE_GOLDEN` never touches it, and refuses a golden with a non-`ok` row.
- **Visible unix sockets**: which sockets a host has is arbitrary. A visible
  path is `ok` and not in the golden; the pinned `AF_UNIX` creation row is what
  proves none of them can be connected. Visible under a read deny is a leak.

Golden rows are normalized: `<HOME>` (the fixture), `<WORKTREE>`,
`<XDG_CONFIG>`, `<XDG_STATE>`, and `<UID>` (this user's uid only; another
user's `/run/user/N` stays literal). `_out/report.txt` is the raw report and
`_out/report.normalized.txt` the compared rows.

Probe semantics, decided the hard way:

- **Reads are CONTENT-level, not name-level.** The deny tmpfs legitimately
  lists allow-back scaffolding and tombstones (#150), so a non-empty listing is
  not a leak; a readable non-empty file is. sandbox-probe itself calls a dir
  readable when it can list it, so a denied dir it flags that is EMPTY
  in-sandbox (the tmpfs mount point) is not counted.
- **The ephemeral clone dir is skipped** in the write sweep (discarded by rein
  after the run by design, and per-run).
- **Egress is mostly out of this harness's reach.** rein is the egress proxy
  (#185), so the emitted srt allowlist is empty. Three fixed probes: a denied
  canary (`example.com`), a control that must be reachable (`api.github.com`),
  so a broken proxy or CA cannot pass as "denied", and a direct connection to
  `1.1.1.1:443` that the network namespace must stop.
  Coverage debt: rein's allowlist itself is not swept here, so a default-policy
  change that opens another host passes this suite. The proxy tests and the
  egress journeys cover it.
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
