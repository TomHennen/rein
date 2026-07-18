# Design note: sandbox containment probe harness (adopt `sandbox-probe`)

**Question (Tom):** replace rein's bespoke sandbox self-checks with well-trodden
existing software; keep the tiny in-binary launch gate. This note records the
decision from that conversation so implementation has a spec to react to.

> **Status:** DRAFT for review. Substrate-agnostic on purpose — if the sandbox
> substrate moves off srt (see the parallel nono evaluation), the harness
> retargets without redesign, because it measures the boundary empirically
> rather than asserting srt-specific config.

## Recommendation

Two layers, different rules:

1. **Launch gate — keep bespoke and tiny, in-binary.** `internal/srt/selftest.go`
   (`RunProbe` / `VerifyConfigApplied`) stays: a dependency-free, fail-closed
   check compiled into rein that runs on every `rein run`. Do NOT swap an
   external pentest toolkit into this slot — it would turn a ~200-line stdlib
   check into a runtime dependency on a third-party binary (version skew,
   must-be-installed, a new supply-chain surface), which is a bad trade for the
   one gate that must always fire. Widen its channel coverage if useful; keep it
   in the binary.

2. **Verification harness — adopt `github.com/controlplaneio/sandbox-probe`.**
   For the broad, periodic, golden-reviewed enumeration (dev + CI + on every srt
   version bump), use sandbox-probe rather than growing our own enumerator. It is
   Go, Apache-2.0, and its core methodology is exactly ours: run on the host
   unconfined, run again inside the sandbox, diff the two reports. It already
   enumerates the channels we care about (sensitive readable paths incl.
   SSH/cloud creds/cookies, DNS + external connectivity, ports + unix sockets,
   processes + parents, mounts, uid/gid, proxy config, runtime detection).

Neither layer touches the **mediation-robustness** axis (fuzzing the proxy /
tier classifier / declare parser — the surface we *deliberately* expose). That
is a separate track; see "Out of scope" below.

## Why not replace the launch gate too

The launch gate and the harness have different requirements:

| | Launch gate (`VerifyConfigApplied`) | Verification harness (new) |
|---|---|---|
| Runs | every `rein run`, fail-closed | dev / CI / on srt bump, golden-reviewed |
| Requirement | dep-free, fast, in-binary | may be heavy; external deps fine |
| Verdict | keep bespoke-tiny | adopt sandbox-probe |

sandbox-probe as a **test/CI dependency invoked as an external process** is the
same posture as `pyte` in `tests/interactive/` (CLAUDE.md): never linked into or
shipped with the Go binary, so its Apache-2.0 license is a check-before-adopt,
not a distribution concern (hard-constraint #4). Runtime-shipping it would be
where license + supply chain actually bite — which is the other reason the
launch gate stays bespoke.

## What stays bespoke even in the harness

sandbox-probe supplies the *enumeration*. Two pieces have no off-the-shelf
equivalent and remain a thin rein layer:

- **The config-derived oracle.** A reachable host or a readable file is only a
  *leak* relative to intent. sandbox-probe cannot know rein's `denyRead` set,
  host classes (`internal/proxy` `InjectHosts`/`CDNHosts`), env allowlist, or
  the mitm-socket placement invariant. The oracle consumes rein's emitted
  sandbox config and classifies each observation expected/unexpected. This is
  the valuable part and it is small. Examples of "expected, not a leak":
  `api.anthropic.com` reachable (egress-only, never injected); a CDN host
  getting direct TLS with no token; the working tree writable.
- **The golden-report wiring.** The classified report is a checked-in,
  human-reviewable artifact wired into a `tests/interactive/` journey. Drift =
  red = re-review, same discipline as the existing golden transcripts. An srt
  bump that reopens a channel flips a row red and forces re-review — this is the
  "pin + re-verify on bump" policy (PLAN-1) made mechanical.

## Method

1. Run sandbox-probe on the host, unconfined → baseline report.
2. Run it through the **real** launch path (`rein run -- <sandbox-probe>`) so it
   inherits the exact scrubbed env, seccomp, and binds the agent gets — not a
   bespoke launcher that would measure a different sandbox.
3. Diff. The delta is what confinement actually removed.
4. Oracle classifies each surviving-reachable observation against rein config.
5. Emit the golden report; commit; journey guards drift.

## Channel coverage (oracle classification target)

| Channel | Probe (sandbox-probe unless noted) | Oracle: expected iff |
|---|---|---|
| Network egress | connect/DNS to allowed / denied / inject / CDN hosts | reachable ⟺ in `allowedDomains`; token present ⟺ in `mitmProxy.domains` |
| Proxy-socket placement | is the mitm socket inside any bind mount? (rein-specific) | must sit outside every srt mount (CP2 invariant) |
| Filesystem read | sensitive-path scan + rein sentinel | all cred stores + rein state/key/audit in `denyRead` |
| Filesystem write | writable-path scan | only working tree + `CLAUDE_CODE_TMPDIR` |
| Env vars | scan for `ANTHROPIC_API_KEY`, `AWS_*`, real `GH_TOKEN`, `DBUS_*`, `SSH_AUTH_SOCK`, `GPG_AGENT_INFO` | only the env allowlist survives |
| IPC / sockets | `socket(AF_UNIX)`; `/run/user/<uid>`; abstract-ns | seccomp-blocked; `/run/user` unmounted |
| Process ns | host PIDs / `/proc` leakage; parent process | pid-namespaced |
| TTY | `/dev/tty`, `TIOCSTI` (rein `RunProbe` already) | unopenable (`--new-session`) |
| Caps / seccomp | caps held; filter active | minimal set |

## Limits (state loudly)

- **Enumeration ≠ soundness.** A positive finding is gold; "found nothing" only
  means "none of the channels we know to check." This is a regression + drift
  detector, not a proof of confinement.
- **Overt only.** Covert / side channels (timing, `/proc` counters, cache,
  contention) are a separate, harder problem — out of scope, not implied.
- **Dual-use.** A leak enumerator is a breakout-recon tool pointed at our own
  sandbox. The corpus enumerates and reports; it must not grow into an
  exploit/escape kit (CLAUDE.md #5).

## Out of scope (separate track: mediation robustness)

The surface rein *intentionally* exposes — the hand-rolled MITM relay
(`ReadRequest → inject → resp.Write`, hop-by-hop stripping, `Expect:
100-continue`, chunked framing, redirect suppression), the read/write tier
classifier (GraphQL operation resolution), and the declare channel +
receive-pack ref parser — is not tested by containment probing. That is a
**fuzzing** track: Go-native `go test -fuzz` against the relay loop, classifier,
and parser first; HTTP request-smuggling/desync tooling
(smuggler.py, http-garden, Burp HTTP Request Smuggler) against the relay's
framing. Higher value than the containment side (it is the part an adversarial
in-sandbox agent actually touches), and tracked separately.
