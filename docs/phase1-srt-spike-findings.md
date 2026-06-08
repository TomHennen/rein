# Phase 1 srt-integration spike — findings

**Status:** in progress (de-risking the Go-broker ↔ srt boundary before
the Shape A design + PLAN-1). Throwaway spike per Tom's direction
(2026-06-07). Closes the central unknown behind issue #7 (Shape B: tokens
reachable from any same-uid process) — the fix is Shape A: agent inside
the `srt` sandbox, token injected at the proxy, never in the agent's
env/files/proc.

## What was verified (against the real package, not docs)

Installed `@anthropic-ai/sandbox-runtime@0.0.54` and read the shipped
TypeScript declarations (`dist/sandbox/*.d.ts`). The integration-relevant
surface:

### The only token-injection lever is `network.mitmProxy.socketPath`

`sandbox-config.d.ts` network config exposes four interception-relevant
options:

| Option | Shape | Can inject an `Authorization` header? |
|---|---|---|
| `mitmProxy` | `{ socketPath: string, domains: string[] }` | **YES** — see below |
| `filterRequest` | `FilterRequestCallback` | No — allow/deny only |
| `tlsTerminate` | `{ caCertPath?, caKeyPath? }` | No — only feeds `filterRequest` |
| `parentProxy` | `{ http?, https?, noProxy? }` | No — opaque CONNECT tunnel |

- **`mitmProxy`**: for a host whose domain matches `domains`, srt's HTTP
  proxy forwards the CONNECT as an **opaque byte tunnel** to the unix
  socket at `socketPath` (`http-proxy.d.ts`: "instead of opening an opaque
  byte tunnel … Mutually exclusive with getMitmSocketPath"). srt does
  **not** terminate TLS on this path — the external process at the socket
  receives the raw client TLS bytes and must terminate TLS itself (with a
  CA the in-sandbox client trusts), inspect/mutate the plaintext HTTP,
  inject the credential, and re-encrypt upstream. **This is rein's hook.**

- **`filterRequest` (`FilterRequestCallback`)**: `(request: Request) =>
  Promise<RequestDecision>` where `RequestDecision = { action:
  'allow'|'deny', reason? }`. Decision-only — the `Request` is read-only
  to the callback and `RequestDecision` has no headers/mutation field.
  Throwing or rejecting **denies** (fail-closed — good security contract,
  wrong tool for injection). It is also an in-process TS callback, so it
  could not host a clean Go-external boundary anyway.

- **`tlsTerminate`**: srt terminates HTTPS in-process using a CA
  (`{caCertPath, caKeyPath}`, or ephemeral), then runs `filterRequest` on
  the decrypted request. Since `filterRequest` can't mutate, this does not
  enable injection. Mutually exclusive with `mitmProxy` (sandbox-manager
  rejects both).

- **`parentProxy`**: upstream HTTP proxy for **direct-connect** (non-mitm)
  traffic; tunnelled via CONNECT, so a parent only ever sees the encrypted
  `CONNECT host:443` — never plaintext headers. Cannot inject.

### Design correction to §12.2

§12.2 lists `SandboxManager`, `FilterRequestCallback`,
`network.mitmProxy.socketPath`, and `parentProxy` as the integration
surface. Sharpened by this spike: **`FilterRequestCallback` and
`parentProxy` cannot inject a credential** (decision-only / opaque tunnel
respectively). The single injection lever is **`mitmProxy.socketPath`** —
an external MITM that rein (Go) owns. §12.2 should be amended so the
design doesn't imply filterRequest/parentProxy are injection points.

## The integration model (Go stays Go, no in-process TS)

```
agent (claude/gh/git) inside srt sandbox — no direct egress
        │  HTTPS to github.com / api.github.com
        ▼
srt HTTP proxy  (mitmProxy.domains match)
        │  opaque byte tunnel over unix socket (socketPath)
        ▼
rein Go MITM (the broker / its proxy arm)
        │  - terminate TLS with rein's CA (leaf per host)
        │  - inject Authorization: Bearer <minted scoped token>
        │  - re-encrypt to the real upstream (system roots)
        ▼
github.com / api.github.com
```

- **No in-process TypeScript required.** srt is configured by a JSON
  settings file (`mitmProxy`, `allowedDomains`) and launched via its CLI;
  rein is a separate Go process at the unix socket. The boundary is a
  socket, not an FFI — clean for a Go broker.
- **CA trust:** because rein (not srt) terminates TLS on the mitm path,
  srt does **not** auto-deliver a CA. rein's CA must be trusted by the
  in-sandbox client via env (the sandbox passes these through): git →
  `GIT_SSL_CAINFO`; node → `NODE_EXTRA_CA_CERTS`; curl/others →
  `SSL_CERT_FILE` / `CURL_CA_BUNDLE`.
- **Token never enters the sandbox.** It is added by rein at the socket,
  outside the sandbox boundary — closing the Shape B env/files/proc
  reachability that issue #7 is about.

## Spike RESULT — boundary PROVEN (2026-06-08)

Ran the agent inside `srt` (AppArmor profile in place) with the Go MITM at
`mitmProxy.socketPath`. The token lived ONLY in the MITM's env, never in the
sandbox. Evidence (MITM logs the real upstream status + redacted inbound/
outbound auth at the injection point):

| Case | In-sandbox auth | MITM injected | Upstream | Conclusion |
|---|---|---|---|---|
| curl, inject=OFF (neg. control) | none | none | **401** | no token in sandbox → fails |
| curl, inject=ON | none | `Bearer …` | **200** | injected at socket → succeeds |
| gh, no `GH_TOKEN` | host login (see below) | overwritten | 200 | — |
| gh, dummy `GH_TOKEN` | `token <dummy>` | overwritten → real | **200** | MITM OVERWRITE works |
| git `info/refs`, `Bearer` | none | `Bearer …` | 401 | wrong scheme (see finding 2) |
| git `info/refs`, `Basic` (direct, live token) | n/a | `Basic x-access-token:…` | **200** | right scheme |

The 401→200 transition on the negative control vs injected curl, with
`inbound-auth=""` throughout, is the core proof: **the agent never holds the
token; it is added at the proxy socket outside the sandbox** — exactly the
Shape A property issue #7 needs. The srt CONNECT framing was confirmed
empirically (srt writes `CONNECT host:443` to the socket, expects `200`,
then pipes the raw client TLS — the MITM replies 200 then TLS-terminates
with an SNI-signed leaf off rein's CA).

### Additional findings the spike surfaced

1. **gh has no clean "refuses without a token" behavior here — it silently
   used the USER's stored login.** With no `GH_TOKEN`, `gh` read the host's
   `~/.config/gh/hosts.yml` (a real 0600 oauth token, ~212 bytes) because
   srt's default `--ro-bind / /` makes it readable in-sandbox. So Shape A's
   **filesystem** config MUST `denyRead` credential stores
   (`~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.config/gh/hosts.yml`,
   the git credential cache) — network injection alone does not stop an
   agent from reading (and exfiltrating) the user's ambient gh login. This
   is the filesystem half of #7, complementing the network half.

2. **Token injection must be HOST-AWARE.** `api.github.com` accepts
   `Authorization: Bearer <installation-token>`; github.com's git smart-HTTP
   transport does NOT — it needs `Authorization: Basic base64(x-access-token:
   <token>)` (Bearer → 401, Basic → 200, both verified directly with a live
   token). The proxy must pick the scheme per host. This matches what the
   Shape B credential helper already does (username `x-access-token`).

3. **gh `GH_TOKEN` override + MITM overwrite is the gh path.** A dummy
   `GH_TOKEN` makes gh attempt the request; the MITM overwrites the dummy
   Authorization with the real token → 200. (The Shape A integration can set
   a fixed stub `GH_TOKEN` in the sandbox so gh always proceeds.)

### What is NOT yet proven (do not let "boundary proven" overreach)

- **`git push` through the MITM is completely untested** — even directly,
  outside the sandbox. Everything git-side here is the read path (`GET
  /info/refs`, a GET). A push is `POST …/git-receive-pack` with a request
  BODY (often chunked), plus keep-alive across the two requests on one
  tunnel. The MITM's `http.ReadRequest → client.Do → resp.Write` loop with
  a real upload body is a different beast from a GET and is the path rein
  exists to broker. **This is the #1 unknown for implementation** — validate
  a real `git push` survives the MITM (a ~10-min direct check, no sandbox
  needed) BEFORE committing the design's git-transport section.
- **`git ls-remote` end-to-end through the sandbox** was not captured green
  — blocked only by the clock drift below. The auth-scheme mechanism is
  proven (MITM injects correct host-aware `Basic`, confirmed via
  `outbound-auth`; direct `Basic` → 200 with a live token), but the through-
  sandbox green is pending a stable clock.

### Clock: fix durably before any more GitHub work

This VM's clock drifted ~66 min mid-session — **because the earlier skew fix
disabled NTP** (`timedatectl set-ntp false` + manual `date -s`), and an
Apple VF guest then drifts freely. Repeated manual `date -s` will not hold
and keeps invalidating tokens mid-task (it bit this spike three times). Before
Phase 1 dogfooding, restore real time sync (working chrony/NTP with
`makestep`, or VZ host-time sync) and DO NOT leave NTP disabled.

## srt socket framing — CONFIRMED

CONFIRMED empirically: srt writes an HTTP `CONNECT api.github.com:443`
preamble to the unix socket (first bytes logged as `"CONNECT "` /
`434f4e4e45435420`), expects `HTTP/1.1 200 Connection Established`, THEN
pipes the raw client TLS ClientHello. The MITM therefore: reads+consumes
the CONNECT line(s), replies 200, then `tls.Server`-terminates using a leaf
generated from the ClientHello SNI (so one listener serves both
github.com and api.github.com), reads the plaintext HTTP, injects, and
forwards upstream over system-root TLS.

## Environmental gate hit (2026-06-07): unprivileged userns restricted

srt runs the agent under unprivileged `bubblewrap`. This dev VM is an
**Apple Virtualization Framework guest** (`systemd-detect-virt` → `apple`,
aarch64) on a kernel with **`kernel.apparmor_restrict_unprivileged_userns
= 1`** (the Ubuntu 24.04 default hardening). That strips capabilities from
unprivileged user namespaces, so `bwrap` fails at the most basic step:

```
$ bwrap --unshare-user --uid 0 --bind / / -- true
bwrap: setting up uid map: Permission denied
$ bwrap --unshare-net --bind / / -- true
bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted     # what srt hit
```

`unprivileged_userns_clone=1` and `max_user_namespaces=63333` are both set
— the blocker is purely the AppArmor restriction, not a missing kernel
feature. Fix (one of):

- `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0` (lift for
  the session; persist via `/etc/sysctl.d/`), **or**
- install/enable an AppArmor profile that grants `bwrap` userns (the
  `bwrap-userns-restrict` profile pattern).

Until then the **end-to-end run (git/gh/curl inside srt → MITM → github) is
blocked**, but everything else is staged and ready: the Go MITM
(`/tmp/rein-srt-spike/`) builds, srt v0.0.54 + a working aarch64 `rg` are
installed, and the baseline + mitm `srt` settings files are written.

**Note for Phase 1 / onboarding:** real users on Ubuntu 24.04+ will hit
this same restriction. `rein doctor` should check
`apparmor_restrict_unprivileged_userns` (and basic `bwrap` health) and tell
the user how to enable it, or Shape A silently fails to launch.

## Staged spike artifacts (throwaway, in /tmp)

- `/tmp/rein-srt-spike/main.go` + `mitm` — the Go unix-socket MITM (adaptive
  CONNECT-framing sniff, SNI leaf-signing, overwrite-inject Authorization,
  `-inject=false` negative-control mode).
- `/tmp/rein-srt-spike/settings-{baseline,mitm}.json` — srt configs.
- `/tmp/rein-srt-spike/bin/rg` — aarch64 ripgrep (srt dependency).
- `/tmp/srt-verify/` — srt v0.0.54 install used for type verification + CLI.

Planned e2e once unblocked: anonymous in-sandbox call → **401** (negative
control), injected → **200** at the MITM (logged at the injection point);
covering `curl`, `git ls-remote`, and `gh api` (gh needs a dummy in-sandbox
`GH_TOKEN` to attempt the request — the MITM's overwrite-inject handles it).

## Caveats already known

- srt is **defense-in-depth, not a hard boundary** — two sandbox-bypass
  CVEs in the last 6 months (design §12.2). #7 is *substantially* closed
  by Shape A, not eliminated.
- These mitm/tlsTerminate/filterRequest options are **undocumented in the
  srt README** (typed but no public examples). Integration leans on the
  shipped `.d.ts` + source, so it is exposed to churn across srt versions
  — pin the version and re-verify on bump.
