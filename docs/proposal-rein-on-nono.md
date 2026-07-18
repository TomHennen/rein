# Proposal: run rein's sandbox on nono

**Status:** Draft for Tom. Sandboxed mode only; direct mode unchanged. Full
evidence is in `docs/nono-git-push-spike-findings.md`.

## The idea

rein does two jobs today: it runs the agent in a sandbox (`srt`), and it brokers
GitHub credentials (mint, scope, approve) through its own proxy. This proposal
keeps the broker and hands the *sandbox* to `nono`. nono runs the agent and
tunnels its GitHub traffic to rein's proxy; rein's proxy still injects the token.

rein deletes ~2000 lines of sandbox code (`internal/srt`) and keeps a small proxy
plus the broker. The point: less security-critical code for rein to own, on a
sandbox someone else maintains.

## Why now

`srt` is moving away from the hook rein uses to inject credentials (its newer
config marks custom-proxy support "not yet supported... future release"). If that
hook goes, rein needs a new one anyway, and nono provides one. Caveat: nono is pre-1.0 and also moves fast — this trades
one moving dependency for another, not stable for unstable.

## Who does what

| Job | Owner |
|---|---|
| Sandbox the agent (files, network, credentials) | nono |
| Tunnel GitHub traffic to rein, unopened | nono |
| Terminate TLS, inject the token, stream, read the push | rein's proxy |
| Mint / scope / approve / declare the issue | rein's broker |
| Install and configure nono | rein |
| Check the sandbox actually confined things | rein (prober) |

rein drives nono through its command line and a JSON config file — no library, no
linking.

## What changes in rein

**Remove:** `internal/srt` and the srt launch code (~2000 lines). nono's sandbox
now hides the credentials from the agent (tested — it does).

**Keep:** the broker, the keystore (the App key stays on the laptop), and the
proxy — but smaller. The proxy stays for two reasons: nono can't inject into a
real `git push`, and even where nono *could* inject (small requests), it never
reads the request body — so rein's approval and per-issue checks would be skipped.
rein has to see the traffic to enforce its rules.

**Add:**
- A verified installer: `rein init` downloads a pinned, signature-checked nono
  instead of `curl | sh`.
- A config generator: writes nono's profile to route GitHub to rein, tighten
  egress, and trust rein's CA.
- The prober: confirms nono confined things, and fails closed if it didn't.

## What we tested (Linux, real GitHub)

Works:
- A **20 MiB `git push`** goes the whole way: agent → nono → rein (inject +
  stream) → GitHub. This was the main risk, and it passed.
- nono hides the App key, gh token, and ssh keys from the agent.
- The agent **can't open a direct TCP connection out** — nono blocks it at the
  syscall level (its supervisor checks every connection).
- rein can route **only** GitHub to itself and send CDN hosts straight out, so no
  token leaks onto a CDN URL.

Open / not done:
- **UDP is open by default** — all UDP, not just DNS — so an agent could exfiltrate
  over UDP. nono's code routes UDP sends to its supervisor too, so it can *likely*
  be filtered, but we haven't found or verified the stricter setting — we'd locate
  it or make an explicit decision to accept the risk. (srt blocks this; nono's
  default doesn't. rein's threat model has cared more about credential theft than
  exfil — worth revisiting.)
- **CA trust** is proven for git; not yet for other tools (they need an env var).
- **A password on rein's proxy** (so only nono can use it) is supported by nono
  but not yet wired up.
- **macOS** uses a different sandbox (Seatbelt) — untested.

## Should we use a proxy library?

No. Checked the options: none fit a forward proxy that streams and injects, and
one is GPL. Infisical's agent-vault (the same use case) also hand-rolled on the Go
standard library. The spike's stand-in proxy was ~40 lines of standard library, so
the core rein needs is small; the value is rein's own rules (which host gets which
token, reading the push). Keep it and fuzz it (#136).

## How we'd build it

Work on a long-lived `nono` branch, kept in sync with main. main keeps shipping
the srt version until the whole thing is done; then one reviewed PR flips the
default and deletes srt. Rollback is reverting that PR.

Land two things on main now, no matter what: fuzz the proxy's push parser and add
the prober against the current srt (#136). Both are security wins with no new
dependency.

Phases: install → compose + harden → shrink and fuzz the proxy → cut over →
dogfood.

## How we verify it works

We already have live "journey" tests (`tests/interactive/`) that walk the real
user path — declare an issue, approve, push, check the push lands — against a
checked-in golden transcript. Those are the backbone here:

- **Keep** the broker journeys (declare, approve, push, scope, git identity, gh
  writes). The user-visible path doesn't change, so we point them at the nono
  stack instead of srt and regenerate the goldens. Green journeys = behavior
  preserved.
- **Drop** the srt-specific containment tests; the **prober** replaces them —
  it checks, against nono, that the sandbox really hides credentials and blocks
  egress (run in CI and as a fail-closed check at launch).
- **Add** one new journey for the full nono stack (a real `git push` through
  nono → rein → GitHub, including a large chunked push), plus the fuzzing from
  #136.
- Existing unit/e2e tests for the broker and proxy stay; add unit tests for the
  new installer and config generator.

The prober is also how we keep trusting nono across its version bumps.

## Future work (not now)

nono already stores credentials in the **OS keychain** — something rein doesn't do
today (rein keeps the App key in a file with uid/mode checks). Putting rein's App
key (and minted tokens) in the OS keychain / Secure Enclave, via nono's mechanism
or directly, is a real improvement we could adopt later. Noting it here — we can
expand once the core pivot works.

## The decision

The hard part works: rein can broker a real `git push` through nono, and nono's
containment holds for credentials and direct TCP. The open risks are UDP exfil,
macOS, and nono's maturity. The carve-out (#136) is worth doing either way.
Whether to adopt a pre-1.0 dependency is Tom's call — the Linux results support
it, with those caveats.
