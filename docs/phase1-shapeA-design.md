# Phase 1 — Shape A (sandbox-composed) design

**Status:** DRAFT for review (2026-06-08). Opens Phase 1. Grounded in the
srt-integration spike (`docs/phase1-srt-spike-findings.md`), design.md
§4.1 / §7.2 / §12.2, and the Phase 0/0.5 implementation. Stop-and-surface
gate before implementation (same discipline as CP4).

## 0. Goal & scope

Close **issue #7** — in Shape B (Phase 0/0.5, credential-helper, no
sandbox) every token channel is reachable from any same-uid process
(env, files, /proc), so a compromised/curious agent can read and
exfiltrate credentials. Shape A puts the agent inside Anthropic's `srt`
sandbox and injects tokens at the network proxy, so **the agent never
holds a token at all** — not in env, not on disk, not in its process
memory.

**This design covers the sandbox-composition spine only:** the broker
daemon, the injecting proxy, srt composition, CA management, and the
filesystem hardening that the spike proved is also required. The other
§7.2 Phase 1 items — broker-as-CA commit signing, audit-comment
writeback, the five-role catalog, single-use/HEAD-pinned write tokens —
are **separate tracks** (PLAN-1 §Later), layered on this spine once it
holds.

**Non-negotiables carried from Phase 0:** fail closed; never silently
degrade; all private-key reads through `internal/keystore`; throwaway
repos until this spine is dogfooded.

## 1. What the spike settled (don't re-litigate)

From `docs/phase1-srt-spike-findings.md`, verified against
`@anthropic-ai/sandbox-runtime@0.0.54` and run end-to-end:

- **The only injection lever is `network.mitmProxy.socketPath`.** srt
  forwards a matched-domain CONNECT to a unix socket as an opaque TLS
  tunnel; rein terminates TLS there with its own CA, injects auth, and
  re-encrypts upstream. `filterRequest` is allow/deny-only;
  `parentProxy`/`tlsTerminate` cannot inject. No in-process TypeScript —
  rein stays a Go process at a socket, configured by a JSON settings file.
- **srt framing:** `CONNECT host:443\r\n\r\n` over the socket → expects
  `200 Connection Established` → then raw client TLS. Terminate with an
  SNI-signed leaf off rein's CA.
- **Injection must be host-aware:** `Bearer <token>` for `api.github.com`;
  `Basic base64("x-access-token:"+token)` for github.com git transport
  (Bearer→401 there).
- **Filesystem is the other half of #7.** srt's default `--ro-bind / /`
  let in-sandbox `gh` read the host's `~/.config/gh/hosts.yml` and use the
  user's *real* login. Network injection alone is insufficient; the
  sandbox MUST `denyRead` ambient credential stores.
- **gh needs a token present to even try:** set a fixed stub `GH_TOKEN` in
  the sandbox so gh issues the request; the proxy overwrites it.
- **Environmental:** Ubuntu 24.04+ needs an AppArmor profile granting
  `userns` to bwrap (or srt won't launch); the VM clock must be NTP-
  disciplined or App-JWT mints 401 (#22, #23).
- **NOT yet proven:** `git push` (POST `git-receive-pack` with a body)
  through the MITM. PLAN-1 CP1 validates this FIRST.

## 2. Architecture

This is design.md §4.1's Shape A diagram, made concrete:

```
  rein run claude
        │ 1. launch
        ▼
  ┌─────────────────────────────┐        ┌──────────────────────────────┐
  │ srt sandbox (bwrap)         │        │ rein daemon (host, user UID) │
  │  agent + git/gh/curl        │        │  - GitHub App config + keystore│
  │  HTTPS_PROXY=localhost:3128 │        │  - session table + scope ceil │
  │  GH_TOKEN=<stub>            │        │  - mint (read-cache / write-JIT)│
  │  GIT_SSL_CAINFO / SSL_CERT  │        │  - human-approval dispatcher  │
  │    _FILE = rein CA          │        │  - hash-chained audit log     │
  │  denyRead: ~/.config/gh,    │        │  ┌─────────────────────────┐  │
  │    ~/.netrc, ~/.git-creds…  │        │  │ proxy arm (MITM)        │  │
  └──────────┬──────────────────┘        │  │  - unix socket          │  │
             │ matched domains            │  │  - TLS-terminate w/ CA  │  │
             │ (github.com, api.github)   │  │  - host-aware inject    │  │
   srt http-proxy ── mitmProxy.socketPath─┼─▶│  - re-encrypt upstream  │  │
             │                            │  └───────────┬─────────────┘  │
  other domains → srt allowlist/deny      └──────────────┼────────────────┘
                                                         ▼  system-root TLS
                                              github.com / api.github.com
```

**Process split.** Two long-lived pieces plus the wrapped agent:
- **`rein` daemon** (host, user UID, unix control socket). Owns App
  config, the keystore, sessions, mint logic, approvals, audit, the CA,
  and the proxy arm. This is `internal/broker`'s logic lifted out of the
  one-shot credential-helper process into a resident server (the Phase 0
  doc comments already anticipate this — `tokencache` "goes away", tokens
  held in memory).
- **proxy arm** — the spike's MITM, productized: listens on the
  `mitmProxy.socketPath`, asks the broker core for the right token per
  (host, method, repo), injects, forwards. In-memory tokens only.
- **wrapped agent** — launched by `rein run` inside srt with the settings
  file below.

**`rein run` in Shape A** stops doing PATH-shim + per-process gitconfig
(Shape B) and instead: ensures the daemon is up, creates a per-run srt
settings file (mitmProxy socket, allowed/denied domains, fs deny-read,
stub GH_TOKEN, CA-trust env), and `exec`s `srt -s <settings> -- <agent>`.

## 3. The injection path (proxy arm)

Per request through the socket:
1. Read the `CONNECT host:443` preamble; reply `200`.
2. `tls.Server` with `GetCertificate` → SNI-signed leaf off rein's CA
   (cache leaves per host).
3. `http.ReadRequest`; derive `(host, method, path→repo)`.
4. Ask the broker core for a token for that tuple. This is where Phase 0's
   logic is **reused, not rewritten**: `InScope` ceiling and `ConfirmWrite`
   human approval apply unchanged. The read/write **tier signal** is
   better than Shape B's shim+proc-tree guess but is NOT a free "method =
   tier" read (see §8.1 — it's a real classifier, moved into the proxy):
   key git writes on the `git-receive-pack` service (NOT the POST verb —
   `git fetch` POSTs to `git-upload-pack` too), and the API needs body
   inspection for GraphQL. A confirmed write triggers the same run-scoped
   approval prompt as today.
5. Inject host-aware auth (Bearer / Basic). Overwrite any inbound auth.
6. Forward upstream over system-root TLS; relay the response.
7. Audit the request (method, host, repo, decision) to the hash-chained
   log.

**TM-G6 (TOCTOU) becomes reachable here:** because the proxy sees the
actual `git-receive-pack` POST, write tokens can be single-use and
HEAD-pinned (design §4.2.5 Shape A) — a separate track, but this is the
layer that enables it.

## 4. Filesystem hardening (the other half of #7)

The srt settings `filesystem` block MUST deny-read the ambient credential
stores the spike found reachable, else the agent can use/exfiltrate the
user's real logins despite network injection:
- `~/.config/gh/` (gh hosts.yml — proven leak), `~/.config/gh-*`
- `~/.netrc`, `~/.git-credentials`, `~/.config/git/credentials`
- `~/.ssh/` (deny-read; pushes go through the proxy, not SSH keys)
- the rein daemon's own key material + control socket dir
- broad-deny `$HOME` then re-allow the working tree (srt's deny-then-allow
  read pattern), so new credential stores don't leak by default.

Open question: a curated denylist vs. broad `$HOME` deny + re-allow. Lean
broad-deny for fail-closed; enumerate the re-allows the agent genuinely
needs (working tree, caches).

## 5. CA management

rein generates a local CA at first run; the proxy mints per-host leaves
off it. The CA **private key** is key material → stored and read through
`internal/keystore` (CLAUDE.md #6), same as App PEMs; never on the Shape B
plaintext path. The CA **cert** is delivered into the sandbox as a trusted
root via env (`GIT_SSL_CAINFO`, `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`,
`CURL_CA_BUNDLE`) — srt does NOT auto-deliver it on the mitm path.

Risk: a rein CA trusted in-sandbox can mint a leaf for *any* host. Bound
it: the proxy only mitm's `github.com`/`api.github.com` (everything else
is srt's normal allow/deny, no rein leaf), and the CA trust env is scoped
to the sandbox process, not the host trust store. Document that the CA key
compromise = ability to MITM the agent's GitHub traffic (not the host's).

## 6. What's reused vs. new

| Concern | Phase 0/0.5 (Shape B) | Phase 1 (Shape A) |
|---|---|---|
| Mint read/write tokens | `githubapp.Client` | **reused** in the daemon |
| Tier signal (read/write) | rein-git shim + proc-tree | **HTTP method/path at proxy** (definitive) |
| Scope ceiling | `sess.Contains` | **reused** |
| Human approval | run-scoped `internal/ui/grant` | **reused**, daemon-dispatched |
| Token delivery | credential-helper stdout / GH_TOKEN | **proxy injection** (agent never sees it) |
| Token storage | `tokencache` file (read), ledger (write, #20) | **in-memory in daemon** (files go away) |
| Audit | helper.log | hash-chained audit log |

The security win is entirely in *delivery + storage*; the mint/scope/
approval brains are already built and carry over.

## 7. Threat-model delta

Closes (vs Shape B): agent reading tokens from env/files/proc (#7);
ambient-credential reuse (fs deny-read); approximate write detection
(now definitive at the wire). Enables later: single-use/HEAD-pinned write
tokens (TM-G6).

Does NOT close / honest limits: **srt is defense-in-depth, not a hard
boundary** (two bypass CVEs in 6 months — §12.2); a sandbox escape
re-exposes Shape B's surface. The approval prompt's `/dev/tty` self-grant
concern (#12) is improved (the agent is sandboxed away from the daemon
socket) but the human-confirmation model is unchanged. macOS uses srt's
`sandbox-exec` path — parity to be validated separately.

### 7.1 The mitm socket is itself a capability

A TLS connection arriving at the proxy carries no `REIN_RUN_ID` — so the
socket connection *is* the only identity the proxy has. Anything same-uid
that can connect to the socket gets authenticated GitHub access at that
session's scope **even without ever seeing the token value**. The token is
hidden (good, #7), but the *capability* is reachable to same-uid processes
exactly as Shape B's token was. This is bounded, not eliminated, by: a
**per-run socket** (each `rein run` gets its own socket path → socket
identity = session identity; see §8.2), 0700 perms, and run-lifetime
teardown. The srt sandbox is what actually keeps non-agent processes off
it; on a shared-uid box outside the sandbox the capability is reachable
(the residual #7 surface the sandbox addresses, not the broker).

## 8. Open questions / risks (ranked)

### 8.1 Tier classification is a real classifier, not a free signal

- **git:** write = the `git-receive-pack` service (`?service=` on info/refs,
  and the `/git-receive-pack` POST path) — NOT "POST = write" (`git fetch`
  POSTs to `git-upload-pack`). Path/service-keyed, reliable.
- **API REST:** method works (GET/HEAD read; POST/PATCH/PUT/DELETE write).
- **API GraphQL:** `gh` uses it heavily and it is ALWAYS `POST /graphql` —
  query vs mutation is only in the body. The proxy terminates TLS so it
  CAN peek the body for `mutation`, but that's a classifier to design +
  test, and it's where Shape B's `rein-gh` classifier (issue #9) must move
  to now that the PATH-shim is gone. Fail closed: unclassifiable → treat as
  write (prompt) rather than silently over-serving.

### 8.2 Per-run socket = session identity (decide in CP2)

Concurrent `rein run`s (Phase 0.5 supports them) share one daemon but need
distinct scope ceilings + approval state. Resolution: each run gets its own
mitm socket path, baked into its srt settings file; the daemon maps
socket → session. This also gives §7.1's capability its run-scoped bound.

### 8.3 Ranked risks

1. **`git push` through the MITM is unproven** (body upload, keep-alive,
   chunked). CP1 de-risks before anything else.
2. **srt API churn** — the mitm/fs options are undocumented in srt's
   README; pin the version, re-verify on bump.
3. **fs denylist completeness** — broad-deny-$HOME vs curated; what the
   agent legitimately needs to read.
4. **Daemon lifecycle** — start/stop, socket perms, single-instance,
   crash recovery; `srt` fallback when unavailable (design §4 says warn
   loudly + run unsandboxed only on throwaways).
5. **macOS parity** (`sandbox-exec`, Secure Enclave CA key).
6. **Performance** — per-request TLS terminate/re-encrypt latency on the
   git hot path.

## 9. Out of scope for THIS design (separate tracks in PLAN-1)

Broker-as-CA commit signing (§4.2.6); audit-comment writeback via the
audit App; the five-role catalog; single-use + HEAD-pinned write tokens
(TM-G6); status app / OS-notification approval channel; Claude Code hooks
as a complementary guard (#21). Each layers on the spine above.
