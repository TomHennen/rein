# Phase 1 design — credentials the agent can't read

**Status:** DRAFT for review (2026-06-08; revised 2026-06-11 after
multi-lens review). Opens Phase 1.

> **2026-07-05 correction (CP2 implementation).** The v1 spine is
> **in-process per run, not a resident daemon** (Tom's decision). The
> "broker daemon" in §4 ("Three pieces"), and the daemon/control-socket
> language in §5.2 and §5.5, are superseded: each `rein run` hosts the
> broker core + injecting proxy in its own out-of-sandbox process and
> prompts for write-approval on its own foreground tty — there is no
> resident daemon and no separate control socket on the spine (so #12's
> sandboxed-mode analogue closes structurally, and CP4's approval relay is
> dropped). `internal/daemon` remains as unwired shelf code for later
> tracks (status app, OS-notification approvals, biometric key unlock,
> shared cross-run cache). Also: **write-approval is run-scoped** across
> both git and GraphQL (§5.3's wording, not §5.1's "per-repo" — the #10
> full-set token makes per-repo re-prompting an awareness ping, not a scope
> gate). Full prose sweep folds into the #25 rename. See `PLAN-1.md`
> Notes (2026-07-05) for the reasoning.

> **2026-07-11 corrections (design-conformance audit, issue #44 §3).**
> Five claims were overtaken by deliberate, recorded implementation
> choices; dated correction notes now sit at each claim site below
> rather than rewriting the prose: CDN hosts never reach the proxy
> (§4.3); the proxy is not the only egress route (§4, "The sandbox");
> the sandbox env allowlist contents are a superset of §4.2's
> parenthetical (§4.2); write-token values do touch disk during a run
> (§5.3 caveat + §6 table note). The fifth is direct-mode-only and its
> stale claim lives in `design.md` §4.2.5 ("served ... on demand for
> the session TTL"), left in place for the #25 sweep: the Shape-B
> read-token cache is a global on-disk file with no session id and can
> serve a later session until token expiry (~1h). Full rationale +
> audit matrix row ids: `PLAN-1.md` Notes (2026-07-11).

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

> **2026-07-11 correction (#44 §3, `P-req-PROXY-ONLY-EGRESS-ROUTE`).**
> "The agent's only route out is the proxy" is no longer literally
> true. Sandbox egress has three routes: the 3 credentialed hosts
> through rein's injecting proxy; the 3 CDN hosts over direct srt TLS
> (see the §4.3 correction); and an operator-widened extra-egress set —
> session `allow_domains:`, `REIN_ALLOW_DOMAINS`, and a built-in
> `api.anthropic.com` default — added in CP4.5 so a real agent can
> reach its own API endpoint at all (decided and reviewed in
> `PLAN-1.md` Notes, 2026-07-05 CP4.5 entries). Every non-proxy route
> is egress-only and never injected (`srt.Build` appends extras to
> `allowedDomains` only, never `mitmProxy.domains`; `Validate` rejects
> wildcard/CDN injectors and inject-host/extra overlaps), so the claim
> narrows to: **the proxy is the only route that carries credentials**,
> and the extra-egress set is the operator's declared exfiltration
> surface (loud EGRESS WARNING on wildcards / large sets).

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
  it. Default-deny `$HOME` reads and re-allow a **configurable read-
  allowlist** — default the working tree, but the user can widen it (e.g.
  a sibling-projects dir like `~/dev`, so the agent can see other projects)
  when they legitimately want that. Independent of the allowlist, always
  deny `~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.ssh`, and
  rein's own key material. These live at `~/.*`, not under a projects dir,
  so widening project access never re-exposes them, and the default stays
  safe. rein keeps these denials **authoritative**: because srt's read
  rule is allow-over-deny, rein refuses (or carves out) an allowlist that
  would otherwise swallow a credential store — a broad `~` allow can't
  silently un-hide `~/.config/gh`.
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

> **2026-07-11 correction (#44 §3, `P-hide-ENV-ALLOWLIST-CONTENTS`).**
> The strict-allowlist *mechanism* above holds (env_test.go pins that
> no name outside the set survives), but the parenthetical contents
> drifted: `internal/srt/env.go` also passes `HOME` and `TERM` from the
> parent (usability, not secrets — settled at CP3, `PLAN-1.md` Notes
> 2026-07-05 "Env allowlist settled") and sets rein-owned values for
> `GIT_AUTHOR_*`/`GIT_COMMITTER_*` and
> `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` (CP4 non-impersonating
> authorship — these *hide* the developer's `~/.gitconfig` identity
> rather than leak it) plus the per-run `CLAUDE_CODE_TMPDIR` scratch
> pointer. `env.go`'s allowlist and doc comments are the normative
> inventory; the parenthetical above is illustrative.

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

> **2026-07-11 correction (#44 §3,
> `P-req-ALL-GH-TRAFFIC-VIA-REIN-PROXY`).** The last sentence is
> superseded: CDN redirects never arrive at the proxy. `internal/srt`
> places the CDN hosts in srt's `allowedDomains` only — never in
> `mitmProxy.domains` — so `codeload`/`objects`/`raw` traffic gets a
> direct srt TLS tunnel and bypasses rein entirely. Never-inject is
> enforced by *routing*, one layer earlier than this table assumed.
> Deliberate: it keeps injection structurally impossible on pre-signed
> URLs and keeps rein off the bulk-download path. Stated honestly:
> those 3 hosts sit outside rein's audit/policy plane, and the proxy's
> `classPassthrough` relay arm (`internal/proxy`) is dead code in
> sandboxed operation — recorded and deliberately retained as
> defense-in-depth should a CDN host ever be routed to the socket.

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

### 4.5 Installation & onboarding

Sandboxed mode adds prerequisites the user must have: `srt` (an npm
package), its system dependencies (`bwrap`, `ripgrep`, `socat` on Linux),
the Ubuntu-24.04 AppArmor profile (§4.4), and healthy NTP (#23). We do
**not** assume they are present, and we do **not** silently install them —
rein can't safely `apt`/`npm install` on a user's behalf. Instead, extend
the Phase 0.5 onboarding machinery: `rein doctor` detects what's missing
and prints the exact fix; `rein init` walks the user through setup (and
may offer to run a step with explicit consent). If a prerequisite is
missing at `rein run` time, **fail closed** and point at `rein doctor` —
never silently drop to direct mode on a real repo (§2). Detailed
checkpoint work is the CP3 preflight (PLAN-1).

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

Crucially, this residual is **not reachable by the agent** — even though
the agent can spawn processes. A process the agent forks stays inside the
sandbox (same namespaces, no direct egress), and the proxy socket is
**never bind-mounted into the sandbox**. So the agent and all its
descendants can only reach the capability through srt's mediated proxy,
where the classifier and write-approval run; they cannot open the socket
directly and bypass them. This is a *hard invariant that nothing enforces
for free*: srt bind-mounts the working directory, so rein must place the
socket outside every srt bind-mount (in the spike it held only because the
socket sat in `/tmp`, outside the bound working dir). Make it a launch-time
check (PLAN-1 CP2/CP3) — if the socket ever lands under a bound path, the
agent gets direct, unmediated access, silently.

Unmediated access otherwise requires being *outside* the sandbox: a
sandbox escape (the defense-in-depth caveat, §7) or separate, pre-existing
same-uid malware. Versus direct mode, that malware is **better on net but
not strictly dominant.** Better: there is no token value to steal, the
capability can't leave the machine, and the first write of a run is still
approval-gated. Not strictly: because approvals are **run-scoped, not
per-request** (Phase 0.5), malware that reaches the socket *during an
already-approved run* rides that approval and can push without a fresh
prompt for the rest of the run's lifetime — which can outlast a
direct-mode write token's ~1h TTL. So: direct mode leaks a short-lived
token replayable anywhere; sandboxed mode leaks a machine-bound,
run-lifetime capability you can't exfiltrate. A real improvement, not a
clean dominance — worth stating honestly.

> **2026-07-11 caveat (#44 §3,
> `P-dec-5.3-NO-EXFILTRATABLE-TOKEN-VALUE`).** "There is no token value
> to steal" holds for the proxy capability but is not absolute: the
> issue-#20 exit-revoke ledger (`writes/<run-id>.jsonl`, mode 0600 in a
> 0700 dir) persists each minted write token's raw value on disk for
> the run's duration — in **both** modes (sandboxed parity added in the
> CP3 fix pass F2; `PLAN-1.md` Notes 2026-07-05). The trade is
> deliberate: GitHub revoke is authenticated by the token itself (no
> revoke-by-id — design.md gap C6), so revoking at run exit, after the
> short-lived minting process is gone, requires the value to cross
> processes via disk. Same-uid host malware can therefore read a
> replayable write token *during a run*; bounded by the file modes,
> deletion at run exit (a SIGKILLed run's orphaned ledger is swept at
> next launch without revoke — those tokens live to the native ~1h
> TTL, the accepted floor), the session scope ceiling, and deny-read
> from inside the sandbox. Read tokens in sandboxed mode stay in
> per-run memory.

### 5.4 The proxy's certificate authority

rein generates a local CA; the **private key is stored/read through
`internal/keystore`** (CLAUDE.md hard-constraint #6). Trust is delivered
**only inside the sandbox**, via env vars (`GIT_SSL_CAINFO`,
`SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`) — never the host trust store. So
the CA vouches only for the agent's traffic, and only for the §4.3 hosts:
"can intercept the agent's GitHub traffic," not the host's. The private
key never leaves the keystore.

**v1 is Linux-only, on purpose.** On Linux this env-delivered trust covers
git, curl, AND `gh`: Go honors `SSL_CERT_FILE`/`SSL_CERT_DIR` on Linux, so
no host-level trust is needed anywhere.

**macOS is deferred — not solved with a keychain CA.** Go's `crypto/x509`
on darwin uses the platform verifier and ignores `SSL_CERT_FILE` (every
macOS version; open Go issue golang/go#77865), so env trust would cover
git/curl but not `gh`. The obvious workaround — adding rein's CA to the
user keychain — is **rejected for v1**: a keychain-trusted CA is trusted
host-wide, the browser included, which is more trust than a security tool
should ask a user to install (even name-constrained, it's the wrong
default). This is almost entirely a Go-on-darwin quirk, not a rein
problem; even `srt` delivers its own CA via the same env vars and hits the
same wall. macOS support is therefore **off the dogfood spine** (PLAN-1
CP5; Linux-only dogfood satisfies the CP6 gate). When macOS is revisited,
the preferred route avoids a CA entirely — point `gh` (and other Go tools)
at a **plaintext local rein endpoint** so there is nothing to trust; rein
does the real TLS to GitHub. To be designed then.

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

### 5.6 Positioning: what rein protects that credential-masking doesn't

Since this design was drafted, Claude Code shipped first-party credential
**masking** (`sandbox.credentials` `mode: mask` on the experimental
`network.tlsTerminate`): the sandboxed command sees a per-session sentinel,
and the sandbox proxy substitutes the real value on requests to `injectHosts`.
This makes the injection *plumbing* first-party — so it's now the closest
alternative — but it protects a narrower property than rein, and the gap is
the point of the whole project. Three distinctions, stated precisely:

- **Masking protects against theft; rein additionally limits *contemporaneous
  capability*.** Masking guarantees the agent never holds the credential in
  readable form, so it can't be exfiltrated for later or off-machine use. But
  during the run, every request the agent makes to an `injectHost` carries the
  **full power of the injected credential** — a prompt-injected agent can push,
  delete, or merge anything that credential can reach, *right now*. Masking has
  no per-request capability restriction (`injectHosts` limits *which hosts*,
  not *what the credential may do*, and it substitutes one static value fixed
  at launch). rein classifies each request (§5.1), enforces the session's scope
  ceiling, and gates writes on human approval — so even a fully-hijacked
  in-session agent is boxed to reads within scope until a human approves a
  write. rein shrinks the blast radius of an *in-session* compromise, not only
  the value of a *stolen* token.

- **Blast radius is bounded by App installation, not by account reach.** rein's
  long-lived secret is a GitHub App private key, and every token it mints is
  bounded by where the App is *installed*. A developer who belongs to several
  orgs installs the App only on the repos the agent should touch; the key —
  even if it leaked — can never mint a token for an org the App isn't installed
  on. Contrast a PAT or `gh auth login` token, scoped to the **user** and thus
  reaching every repo and org that account can access (masking-a-PAT inherits
  that full reach). Installing rein's App narrowly, and never running
  `gh auth login` so **no user-scoped token sits on disk at all**, removes the
  cross-org vector entirely — arguably rein's single strongest property.
  Within the installed repos each minted token narrows further to the session's
  declared repos and to read-or-write permissions (installation-token
  `Repositories` + `Permissions`), so three bounds stack: installed repos ⊇
  session repos ⊇ per-request tier.

  This is *not* a claim that the machine holds no long-lived secret: the App
  private key is one, read on every mint through `internal/keystore`. The claim
  is **no long-lived secret reachable by the agent** — the key lives with the
  broker, outside the sandbox, deny-read from within. Same-uid host compromise
  stays the residual (§5.3 / design.md TM-G4); the installation bound caps what
  that residual can reach in *other* orgs at zero, and a hardware-backed key
  (`sks`; keystore is the swap point) is the second-order hardening for the key
  file itself.

- **Agent-agnostic — at the network boundary, with bounds.** rein injects at
  the OS + network boundary, not inside the agent, so it needs no per-agent
  integration and isn't tied to one vendor's tool (unlike masking, which lives
  in Claude Code's own settings/proxy). That agnosticism is real but bounded:
  it fits **local CLI agents launched as a subprocess** (not IDE/desktop or
  cloud agents), requires **HTTPS** GitHub remotes (SSH bypasses the proxy and
  is denied — §4.2), and requires the agent's HTTP client to honor the standard
  CA-trust env vars (§5.4). Most CLI agents shell out to `git`/`gh`, which are
  covered — but "works with any agent" is a *design property proven so far only
  for the git/curl/gh toolchain* (CP1/CP2), not a verified breadth claim. Some
  tools also need a stub credential to attempt a request at all (the `gh`
  `GH_TOKEN` stub). The README threat model (#5) should carry this positioning
  in user-facing terms.

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

> **2026-07-11 correction (#44 §3):** the "in daemon memory (no disk)"
> cell is superseded twice over — there is no daemon (top banner,
> 2026-07-05), and write-token values touch disk in the exit-revoke
> ledger (§5.3 caveat). Sandboxed-mode read tokens are per-run
> in-memory (`runbroker`'s `MemCache`); the ledger is the one
> deliberate disk exposure.

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
