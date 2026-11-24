# Containment probe harness (issue #136B)

A differential dev/CI check that verifies rein's **current srt sandbox** actually
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
- **Unix sockets** — judged by what the sandbox can *do*: creating an `AF_UNIX`
  socket, or connecting to any socket sandbox-probe found, is a leak. A socket
  path that is merely visible is `ok`, as is expected on any host (docker, the
  nix daemon, tmux, systemd).
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
(host run vs sandbox run) into it — the mapping lives in `cmd/classify` (see
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

## How to run (wired end-to-end, #141)

```sh
go install github.com/controlplaneio/sandbox-probe@latest   # once per box
go build -o bin/ ./cmd/...
tests/containment/run.sh                    # check against expected.txt
```

`run.sh` (1) runs `sandbox-probe scan --fast` on the host, (2) captures the
emitted settings.json via `REIN_SRT_SETTINGS_COPY` and derives probe TARGETS
from it (`classify -targets`: allowlists, deny paths, srt's own default write
paths — the substrate-chosen set that leaked in #153), (3) runs sandbox-probe
plus the target probes through the REAL `rein run --sandbox` launch, (4) checks
write PERSISTENCE host-side, (5) classifies everything against that run's own
settings.json, and (6) checks the normalized rows against `expected.txt`
(mismatch = exit 4; any leak = exit 1).

## Expectations (`expected.txt`)

There is no per-box golden. One committed file says what the report must look
like on **every** developer machine, and `run.sh` fails on any row it does not
account for. Each line is `class | verdict | kind | target | reason [| note]`:

| Class       | Rule                                                              |
|-------------|-------------------------------------------------------------------|
| `required`  | must appear everywhere (the denies rein always configures, the env denylist, the write sweep, the planted read sentinel) |
| `ifexists`  | must appear exactly when the path has content on this host (the curated toolchain allow-backs) |
| `optional`  | depends on what is installed; present or absent                   |
| `oneof:<g>` | at least one row of the group (same check, host-dependent outcome)|
| `triaged`   | an `unknown` row a human reviewed; the note says why it is fine    |

What still fails, on any machine:

- a `required` row missing (a probe silently dropped), or an `ifexists` path
  that is on the host but no longer allowed back;
- a listed row with a different verdict or reason (`CHANGED`);
- any row not listed at all: a new allow-back, a new write result, or an
  `unknown` nobody triaged (`UNEXPECTED` / `UNTRIAGED`).

`leak` and `regression` can never be listed, an `unknown` must be `triaged` with
a note, and the `*` target is accepted only for `optional | ok | socket` (those
rows are connect-tested on every run, so they need no per-path review). To
accept a new row, review it and add a line by hand; there is no auto-adopt.

Rows are normalized so they compare across machines: `<HOME>`, `<WORKTREE>`,
`<UID>` (this user's uid only; another user's `/run/user/N` stays literal),
`<PID>`, `<RUNID>`, `<HEX>`, and the srt-mux socket herd collapsed to one row.
`_out/report.txt` is the raw report, `_out/report.normalized.txt` the compared
rows, and `_out/observations.json` records which file each deny was read through.

Probe semantics, decided the hard way:

- **Reads are CONTENT-level, not name-level.** A denied dir is probed through a
  host-sampled file inside it, and dir readability means "a real file readable
  within depth 3" — the deny tmpfs legitimately lists allow-back scaffolding and
  tombstones (#150), so a non-empty listing is not a leak. The row is reported
  under the deny ENTRY, not the sampled file, so it reads the same everywhere.
  `run.sh` also plants `~/.rein-containment-read-sentinel` (removed on exit) so
  the home deny is always judged against known content.
- **Token placement is probed only where there is observable evidence**: the
  api.github.com rate-limit heuristic (anonymous = 60; the injected read token
  raises it). Other inject hosts are skipped rather than guessed; their token
  placement stays covered by the proxy unit tests.
- **GH_TOKEN in-sandbox is rein's deliberate stub**, not a secret; only a
  non-stub value counts as present.
- **The ephemeral clone dir is skipped** in the write sweep (discarded by rein
  after the run by design, and per-run).
- **Egress is mostly out of this harness's reach.** rein is the egress proxy
  (#185), so the emitted srt allowlist is empty and only the denied canary
  (`example.com`) is probed. rein's own egress policy is covered by the proxy
  tests and the egress journeys, not here.

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
