# Phase 1 design — credentials the agent can't read

**Status:** DRAFT for review (2026-06-08; revised 2026-06-11 after
multi-lens review). Opens Phase 1.

Self-contained: problem and requirements first, then the design. The
integration spike (`docs/phase1-srt-spike-findings.md`) is referenced as
evidence but not assumed read.

## 1. The problem

Today rein hands the agent a real GitHub token for each operation — as a
git credential helper, and via a `gh` wrapper. The token is short-lived
and scoped to one repo, but it is a **real, usable token the agent's own
process can read**: from its environment, rein's on-disk token cache, or
its own memory.

Per-issue scoping and human-approval-for-writes (Phase 0/0.5) limit what
a leaked token can do. They do not stop an adversarial (prompt-injected)
agent from reading the token and using or exfiltrating it directly. "The
agent can read its own credential" is the gap that matters (issue #7).

## 2. Requirements

Stated independently of mechanism.

**Functional**
- The agent can clone, fetch, push, and use `gh` within its session's
  scope, with no token handling by the developer.
- Access scoped per issue/session; writes require human confirmation
  (both as today).

**Security**
- **The agent's process can never read a usable GitHub credential** — not
  in its environment, a file, or its own memory. This is the headline
  requirement (the #7 gap), a property of the agent's reach, not of any
  particular mechanism.
- The agent also cannot reach the developer's **ambient** credentials
  (their own `gh` login, SSH keys, `~/.netrc`, stored git credentials,
  keyrings, agent sockets). Hiding rein's token is pointless if the agent
  can grab the human's instead.
- Write authorization stays human-confirmed and non-replayable (the agent
  can't pre-answer or self-answer the prompt).
- **Fail closed:** if we cannot satisfy the above, refuse the operation —
  never silently fall back to handing the agent a token.

**Operational**
- Tolerable latency on the git path (the agent hits it constantly).
- Linux and macOS.
- If the protection mechanism is unavailable, degrade **loudly and
  safely** — never silently hand the agent a token on a real repo.

**Non-goals** (later tracks, §8): commit signing, audit-comment
writeback, the five-role catalog, single-use / HEAD-pinned write tokens.

## 3. Approach

Run the agent inside a **sandbox with no direct network access**. All its
GitHub traffic goes through a **local proxy rein controls**. The agent
sends ordinary git/`gh` requests carrying *no* credentials; the proxy adds
the credential on the wire, at the last hop. The credential is added
inside rein's process, *outside* the sandbox, so nothing the agent can
read ever contains it.

We call this **sandboxed mode**. The Phase 0/0.5 credential-helper path
becomes **direct mode** — retained as a clearly-marked fallback for
environments without a sandbox, never the default once sandboxed mode
works.

> Naming: earlier docs call these "Shape A" (sandboxed) and "Shape B"
> (direct). The repo-wide rename is tracked in **#25**.

The sandbox is Anthropic's `sandbox-runtime` (`srt`) — the sandbox Claude
Code itself ships with — composing with a maintained tool rather than
building one. The srt-specific surface is deliberately thin (§4.4): the
daemon, proxy, and classifier are sandbox-agnostic, so srt can be swapped
for another no-egress sandbox without redesign.

## 4. How it works

Three pieces:

**The broker daemon.** A long-running local rein process owned by your
user account. Holds the GitHub App credentials, session table, the
token-minting + scope-ceiling + approval logic (carried over from today's
code), and an audit log. Tokens live only in its memory.

**The injecting proxy** (part of the daemon). The sandbox routes the
agent's GitHub-bound traffic to it over a per-run unix socket. Per
request, the proxy classifies read vs. write (§5.1), checks scope and
approval, injects the credential, and forwards to GitHub. Because the
proxy sees the actual `git push`, it is also where the stronger write
protections (§8: single-use, branch-pinned tokens) attach later — which
direct mode never could.

**The sandbox.** `srt` runs the agent with no direct network egress, so
the agent's only route out is the proxy. `rein run` generates a per-run
configuration: traffic routing, filesystem denials (§4.2), a scrubbed
environment (§4.2), and the proxy's CA certificate to trust (§5.4).

### 4.1 Injection invariants

The proxy's safety rests on these; they are requirements, not
implementation details.

- **One identity source.** The agent controls both the TLS SNI and the
  plaintext `Host:` header. The proxy derives *both* the upstream
  connection *and* the injection decision from the SNI, and rejects any
  request whose `Host` does not match it — otherwise the agent could open
  a connection "to github.com" and steer an injected token to an
  attacker-chosen host.
- **Exact-match injection allowlist, per host class** (§4.3). Inject only
  on hosts that take credentials; never on CDN/asset hosts.
- **No credential on the response path.** Nothing the proxy sends toward
  the sandbox — error bodies, reflected headers, redirect locations, debug
  output — may contain the token. Audit-log entries redact tokens, and the
  log file is in the sandbox deny-read set.
- **HTTP/1.1 only.** The proxy pins ALPN to `http/1.1`
  (`tls.Config.NextProtos`); git/curl/gh all fall back. Go's
  `http.ReadRequest` relay loop cannot parse h2, and the spike only worked
  because Go's `tls.Server` happens not to offer h2 by default — pin it
  explicitly so a refactor doesn't reintroduce it.

### 4.2 Hiding the developer's own credentials

Sandboxing the network is not enough; ambient credentials leak through
three channels, and all three must be closed:

- **Filesystem.** By default the sandbox can read the home directory. The
  spike confirmed the failure concretely: `gh` in-sandbox silently picked
  up the host's stored login from `~/.config/gh` and authenticated with
  it. Default-deny `$HOME` reads and re-allow only the working tree, with
  explicit denials for `~/.config/gh`, `~/.netrc`, `~/.git-credentials`,
  `~/.ssh`, and rein's own key material — so a store we didn't think of
  doesn't leak by default.
- **Environment.** The sandbox environment is a **strict allowlist** (CA
  trust vars, stub `GH_TOKEN`, PATH/locale), not host passthrough.
  Passthrough would hand the agent every env-resident secret on the host
  (`GITHUB_TOKEN`, `ANTHROPIC_API_KEY`, `AWS_*`, …) — exactly the class
  the filesystem denials exist to block.
- **Sockets outside `$HOME`.** The Secret Service keyring (D-Bus,
  `/run/user/<uid>`), `ssh-agent`, and `gpg-agent` grant credential *use*
  without any file read. Scrub `DBUS_SESSION_BUS_ADDRESS`,
  `SSH_AUTH_SOCK`, `GPG_AGENT_INFO` etc. and do not mount
  `/run/user/<uid>` into the sandbox. (On Linux, rein's own keyring
  backend may be the Secret Service — reachable via that bus if left
  mounted.)

### 4.3 GitHub host classes

GitHub operations span more hosts than `github.com`/`api.github.com`,
and they need different treatment:

| Host | Treatment |
|---|---|
| `api.github.com` | intercept + inject `Bearer` |
| `github.com` (git smart-HTTP) | intercept + inject `Basic x-access-token:` |
| `uploads.github.com` (release assets up) | intercept + inject `Bearer` |
| `objects.githubusercontent.com` | allow egress, **never inject** (pre-signed S3-style URLs — injecting leaks the token and can break the request) |
| `codeload.github.com`, `raw.githubusercontent.com` | allow egress, **never inject** (reached via tokenized redirects; fail-closed choice — direct authenticated raw fetches of private content will fail) |
| everything else GitHub-bound | deny (fail closed) |

Auth scheme is **host-aware** (spike-verified: Bearer → 401 on the git
transport; Basic → 200). Redirects from github.com to the CDN hosts
arrive at the proxy as fresh connections and must classify into the
never-inject class.

### 4.4 `srt` specifics (implementation detail; evidence in the spike)

Skippable; records why the mechanism looks this way. Validated
empirically in the spike doc.

- `srt` exposes exactly one hook that lets an external process *modify*
  requests: it forwards matched-domain traffic as an opaque tunnel to a
  unix socket rein owns. rein terminates TLS there with its own CA,
  injects, and re-encrypts upstream. srt's other hooks are allow/deny
  only.
- CA trust is delivered via env vars (`GIT_SSL_CAINFO`, `SSL_CERT_FILE`,
  `NODE_EXTRA_CA_CERTS`); srt does not do this automatically. **The
  bundle must contain system roots + rein's CA** — `SSL_CERT_FILE`
  *replaces* the default roots, so a rein-only file breaks every allowed
  non-GitHub HTTPS destination in-sandbox (including the agent's own API
  endpoint). macOS is different — see §5.4.
- `gh` won't send requests unless it believes it is logged in: the
  sandbox sets a stub `GH_TOKEN`; the proxy overwrites it.
- Ubuntu 24.04+ restricts unprivileged user namespaces; `srt` needs an
  AppArmor profile for `bwrap` or it won't start. `rein doctor`/`run`
  must preflight this (and clock skew, #22) and guide the fix
  (loud-degrade, §2).

## 5. Key design decisions & open questions

### 5.1 Read/write classification: scope is the boundary, the classifier is defense-in-depth

The proxy must decide read (cached read token) vs. write (mint + require
approval). The **hard boundary is token scope, not classification: read-
tier tokens are minted with zero write permissions**, so a misclassified
write fails at GitHub rather than executing silently. The classifier sits
above that backstop and is real work, not "POST = write":

- **git:** the write signal is the `git-receive-pack` *service* — and it
  appears first on the advertisement request
  (`GET /info/refs?service=git-receive-pack`), which GitHub 403s without
  push permission. Classify that GET as write tier; note the approval
  prompt therefore fires before any pack body exists (constrains the
  later commit-inspecting tracks in §8 to the second request). `git
  fetch` also POSTs (to `git-upload-pack`) — method is not the signal.
- **REST:** method works (GET/HEAD read; POST/PATCH/PUT/DELETE write).
- **GraphQL:** always `POST /graphql`; mutation-ness lives in the body,
  which the proxy can read (it has plaintext). The check must resolve the
  *selected* operation (shorthand `{...}` queries, multi-operation
  documents + `operationName`, batched arrays) — not substring-match
  `mutation`. This is where direct mode's `gh` classifier (#9) moves.
- **Fail closed:** unclassifiable ⇒ treated as write ⇒ prompt.

### 5.2 One proxy socket per run = the session's identity

Connections carry no session id, and Phase 0.5 supports concurrent
`rein run`s with independent scope/approval. Resolution: each run gets
its own proxy socket path, baked into its sandbox config; the daemon maps
socket → session.

### 5.3 The proxy socket is itself a capability

Anything that can connect to the socket gets authenticated GitHub access
at that session's scope, **without ever seeing a token**. Bounded by: the
per-run socket (§5.2), run-lifetime teardown, and permissions — a
**filesystem** unix socket (Linux abstract-namespace sockets bypass file
permissions and are forbidden), dir `0700`, socket `0600`.

Be precise about who that stops: it gates **other-uid** processes. A
same-uid process *on the host* can still connect — the socket lives
outside the sandbox, so the sandbox does nothing to protect it. This is
the same-uid residual of #7 (design.md TM-G4 class), mitigated only by
host hygiene (`ptrace_scope`, separate UIDs), not by this design. Note
TM-G4's bearer-rotation mitigations don't apply here: the proxy is
in-daemon, and the socket's only auth *is* filesystem permissions.

### 5.4 The proxy's certificate authority

rein generates a local CA; the **private key is stored/read through
`internal/keystore`** (CLAUDE.md hard-constraint #6). Compromise scope:
the CA is trusted only for the agent's traffic, and the proxy intercepts
only the §4.3 hosts — "can intercept the agent's GitHub traffic," not the
host's.

**macOS caveat (affects gh):** Go's `crypto/x509` on darwin uses the
platform verifier and ignores `SSL_CERT_FILE` (and env-delivered trust
generally) on every macOS version (open Go proposal golang/go#77865; not
landed). So env-delivered trust covers git/curl but **not Go clients
like `gh`**. The
expected path is adding rein's CA to the user keychain with trust
settings — weaker than "sandbox-only trust" and recorded as such; the
exposure still requires the CA *private key*, which never leaves the
keystore. Validated when a Mac is available (PLAN-1 CP5).

### 5.5 Approvals: the channel must be outside the sandbox

Non-replayability is a property of the **channel**, not a free
consequence of sandboxing. Two invariants:

- **The daemon's control socket (where approvals are granted) is never
  reachable from inside the sandbox.** Only the per-run proxy socket is
  mounted. Otherwise the agent self-grants over the same path it uses for
  traffic — issue #12 relocated, not fixed.
- The prompt itself needs a human-reachable surface: the daemon has no
  tty, and the host terminal is occupied by the agent TUI (open question
  from phase0_findings, Phase 0 CP5).
  Decision: the daemon relays the prompt to the foreground `rein run`
  process, which reuses the Phase 0 layered flow (tty → tmux popup →
  stderr instructions). The richer status-app/OS-notification channel
  stays a later track (§8). #12's nonce-via-tty remains open for *direct*
  mode only.

## 6. What we reuse vs. build new

The security win is entirely in credential *delivery and storage*;
minting, scoping, and approval logic carry over.

| Concern | Today (direct mode) | Phase 1 (sandboxed mode) |
|---|---|---|
| Mint read/write tokens | `githubapp.Client` | **reused** in the daemon |
| Read vs. write signal | git PATH-shim + proc-tree guess | request inspected at the proxy (§5.1) |
| Scope ceiling | `sess.Contains` | **reused** |
| Human approval | run-scoped approval flow | **reused**, daemon-dispatched (§5.5) |
| Token delivery | handed to git/`gh` (agent can read it) | **added at the proxy** (agent never sees it) |
| Token storage | on-disk cache + write-token ledger | **in daemon memory** (no disk) |
| Audit | `helper.log` | hash-chained audit log (redacted, deny-read) |

Direct mode keeps working throughout: its broker core becomes a client of
the same daemon logic, and its tests stay green (PLAN-1 CP2).

## 7. Risks & limits

- **The sandbox is defense-in-depth, not a hard boundary.** Two `srt`
  sandbox-escape fixes in the last six months. An escape re-exposes
  direct mode's surface. One layer, not a guarantee.
- **`git push` through the proxy is unproven** — the one untested path
  (request-body upload through the relay loop). PLAN-1 CP1 validates it
  before the daemon is built around it.
- **`srt` API churn — concrete, not hypothetical.** The
  `mitmProxy.socketPath` hook is typed but undocumented in 0.0.54 (latest
  as of 2026-06-11; spike-verified), and upstream's README describes a
  **new configuration format in which custom-proxy support "is not yet
  supported … will be added in a future release"** — so the hook will
  move, though upstream clearly intends bring-your-own-proxy as a real
  feature. Mitigations (pin, track, file upstream) in PLAN-1
  prerequisites.
- **Revocation residual moves, it doesn't vanish.** Direct mode's #20
  disk ledger could sweep orphaned write tokens after a killed run;
  in-memory daemon tokens can't be revoked if the *daemon* dies (revoke
  is authenticated by the token itself). Daemon-crash orphans live to
  native TTL (~1h). Accepted, stated.
- **macOS parity:** different sandbox backend (`sandbox-exec`) *and* a
  different CA-trust path (§5.4). Validated separately; not on the
  dogfood critical path.
- **Latency:** TLS terminate + re-encrypt is per *connection* (keep-alive
  amortizes), but three known footguns are design choices, not
  measurements: cache per-host leaf certs (ECDSA P-256) rather than
  minting per connection; one shared upstream transport for connection
  pooling; `DisableCompression` on the relay so Go doesn't silently
  gunzip and break framing. Then measure (CP3).

## 8. Out of scope for this design (later tracks)

Layer on the spine once it holds, each tracked separately: single-use +
branch-pinned write tokens (reachable now the proxy sees the push);
broker-as-CA commit signing; audit-comment writeback via the audit App;
the five-role permission catalog; a status-app / OS-notification approval
channel; Claude Code hooks as a complementary guard (#21).
