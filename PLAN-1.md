# PLAN-1.md — Phase 1 (sandboxed mode)

**Goal:** Close issue #7. Run the agent inside the `srt` sandbox with a
resident `rein` daemon injecting short-lived, scoped GitHub tokens at the
network proxy, so the agent never holds a credential in env/files/proc.
Tom dogfoods it on a throwaway, then on `wrangle`, for two weeks without
reverting to a PAT (design.md §7.2 hypothesis).

**Design of record:** `docs/phase1-design.md` (spine) +
`docs/phase1-srt-spike-findings.md` (what the spike proved). Read both
before starting. Don't re-derive the srt integration — it's verified.

**Discipline (same as Phase 0/0.5):** checkpoints in order; at each one
implement → test → run → **stop and surface to human** → wait for
verification. Between checkpoints, refactor freely; don't expand scope.
Surface surprises in the Notes section rather than working around them.
Call `advisor()` before substantive work and before declaring a
checkpoint done. Spawn a reviewer subagent after each checkpoint.

**Constraints:** throwaway repos only until the spine is dogfooded
(CP6). Fail closed. All key reads through `internal/keystore`. NTP must
be healthy before any GitHub App work (#23) or mints 401 intermittently.

## Environmental prerequisites (one-time, per machine)

- `srt` installed (`@anthropic-ai/sandbox-runtime`), plus `bwrap`,
  `ripgrep`, `socat` (Linux). **Pin srt 0.0.54** (latest as of 2026-06-11;
  what the spike verified). Upstream's main-branch README describes a new
  config format where custom-proxy support is "not yet supported … future
  release" — the `mitmProxy.socketPath` hook will move. Track upstream;
  expect one migration; consider filing an upstream issue describing
  rein's use case (design §7).
- Ubuntu 24.04+: AppArmor profile granting `userns` to `/usr/bin/bwrap`
  (see spike findings) — or srt won't launch. `rein init` should detect
  and guide (folds into #22/#23 work).
- Healthy NTP (#23).

## Spine checkpoints

### CP1 — Validate `git push` through a MITM (de-risk the #1 unknown)

**Status: DONE (2026-06-14).** `git push` (small + 2 MiB chunked + force-
chunked), `git ls-remote`, `curl`, and `gh` all relay correctly through the
spike MITM to github.com with write-token injection; commits land. The
relay-hygiene recipe + the reviewer-caught redirect bug are recorded in
`docs/phase1-srt-spike-findings.md` ("CP1 results") for CP2 to productize.

**Estimate:** 0.5 day. **No sandbox, no daemon yet** — just the question
the spike left open.

- Stand up the spike's Go MITM (host-aware Basic inject) on a unix socket
  and front it directly (or via a minimal http-proxy CONNECT) — no srt.
- **Mint a WRITE-tier token** for this test (the spike used a read-only
  `rein gh-auth` token — a `git push` with it 403s regardless of the MITM,
  which would masquerade as a relay bug). Inject Basic `x-access-token:`.
- Do a real `git push` of a commit to a throwaway repo THROUGH it: prove
  the POST `git-receive-pack` body upload and keep-alive across info/refs →
  receive-pack survive the `ReadRequest → forward → relay` loop. Confirm
  the pushed commit lands.
- **Force the chunked case** — git only chunks above `http.postBuffer`
  (1 MiB default), so a small push goes Content-Length and would leave
  chunked relay untested. Push a >1 MiB blob and/or
  `git -c http.postBuffer=1024 push`. Test both small and large.
- **Pin ALPN to HTTP/1.1** (`tls.Config.NextProtos = ["http/1.1"]`).
  `http.ReadRequest` can't parse h2; the spike only worked because Go's
  `tls.Server` doesn't offer h2 by default. Verify git, curl, AND gh all
  fall back and transit the relay (design §4.1).
- Relay hygiene: handle `Expect: 100-continue` (reply 100 before reading
  the body — git won't send it to github.com, but a raw relay deadlocks if
  it ever appears); `DisableCompression` on the upstream transport so Go
  doesn't transparently gunzip and break framing.
- Also confirm `git clone`/`ls-remote` (read path) green with a live token.

**Success:** commits (small and >1 MiB) pushed through the MITM appear on
the throwaway repo; no hangs/truncation; gh/git/curl all work over the
pinned HTTP/1.1 relay.
**Gate:** if push needs more than the above, capture it here before the
daemon design hardens.

### CP2 — Daemon skeleton + proxy arm

**Estimate:** 3-4 days.

- New resident `rein` daemon: unix control socket (0700 dir, uid-checked),
  single-instance, start/stop, holds App config + keystore + sessions in
  memory. **Extract `internal/broker` mint/scope/approval logic into a
  core both modes share** — direct mode's helper becomes a client of the
  same logic (or keeps the file path explicitly); either way direct mode
  keeps working and its tests stay green through the refactor.
  `tokencache` files → in-memory. **Fix #10 here** (mint scope is
  `Repos[0]` but `Contains` accepts all) — don't lift the bug into the
  daemon.
- Proxy arm: the CP1 MITM, productized into the daemon — per-request it
  asks the broker core for a token, host-aware inject per the §4.3 host
  classes (inject api/github/uploads; **never inject** the CDN hosts),
  SNI==Host enforcement (design §4.1), audit each request to a
  hash-chained log (token-redacted).
- Token-mint hygiene at proxy rate: cache minted tokens per session/tier,
  backoff on GitHub rate limits (phase0_findings flagged this; per-request
  minting at the proxy makes it acute).
- **Per-run socket = session identity** (design §5.2): each `rein run` gets
  its own mitm socket path; the daemon maps socket → session for scope +
  approval. Filesystem socket only (no abstract namespace), dir 0700,
  socket 0600, run-lifetime teardown — the socket is a capability
  (design §5.3), bound this way. **Placement check:** the socket must sit
  outside every srt bind-mount (srt mounts the working dir), or in-sandbox
  processes get direct, unmediated access — verify at launch, fail closed.
- **Tier classifier** (design §5.1), NOT "method = tier": git keys on the
  `git-receive-pack` service **including the advertisement**
  (`GET /info/refs?service=git-receive-pack` ⇒ write tier); REST on
  method; GraphQL resolves the *selected* operation (shorthand queries,
  `operationName`, batched arrays — not substring match) — this is where
  the direct-mode `rein-gh` classifier (#9) moves. Fail closed
  (unclassifiable → prompt). **Backstop is scope, not the classifier:**
  read-tier tokens carry zero write permissions.
- CA: generate at first run; key via `internal/keystore`; **cache leaves
  per host** (ECDSA P-256) and share one upstream transport for pooling
  (design §7 latency).

**Success:** daemon up; `curl`/`git` pointed at the proxy socket get
injected tokens; read/write tiering + scope ceiling + run-scoped approval
all fire from the proxy; direct-mode test suite still green. Unit-tested.

### CP3 — srt composition

**Estimate:** 2-3 days.

- `rein run` (sandboxed-mode path): ensure daemon up; emit a per-run srt settings
  file (mitmProxy.socketPath, the §4.3 host classes as allowed/denied
  domains, **fs deny-read of credential stores**, stub `GH_TOKEN`,
  CA-trust env); `exec srt -s … -- <agent>`.
- Filesystem hardening: broad-deny `$HOME` read + re-allow working tree;
  explicit deny `~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.ssh`,
  daemon key material, audit log. Do not mount `/run/user/<uid>` (D-Bus /
  Secret Service / agent sockets — design §4.2).
- **Environment allowlist, not passthrough** (design §4.2): CA vars, stub
  `GH_TOKEN`, PATH/locale only. Scrub `DBUS_SESSION_BUS_ADDRESS`,
  `SSH_AUTH_SOCK`, `GPG_AGENT_INFO`, and everything else.
- **CA bundle = system roots + rein CA** (`SSL_CERT_FILE` replaces the
  defaults — design §4.4). Verify an allowed non-GitHub HTTPS domain
  still works in-sandbox.
- **Preflight in `rein doctor` + `rein run`:** srt present + pinned
  version, `bwrap` userns/AppArmor health (Ubuntu 24.04 gate), clock skew
  (#22). Loud, actionable errors — this is the loud-degrade requirement,
  with an implement/test cycle, not just a prerequisites note.
- `srt`-unavailable fallback: loud warning + unsandboxed only on throwaway
  (design §2-3); fail closed otherwise.
- **Measure git-path latency** through the proxy vs. direct (design §7);
  record the numbers here.

**Success:** `rein run -- bash -c 'gh api …; git clone …; git push …'`
inside srt works end-to-end via proxy injection; the token is absent from
the sandbox env/proc; the agent cannot read the host's gh login
(deny-read verified) NOR reach keyring/ssh-agent sockets; a non-GitHub
allowed domain works; preflight catches a broken userns config with a
useful message; latency recorded.

### CP4 — Session & approval integration (sandboxed mode)

**Estimate:** 2 days.

- Session start/scope negotiation mediated by the daemon: scope is bound
  at `rein run` launch (socket = identity, design §5.2); scope *changes*
  mid-run pop a human confirm via the §5.5 channel.
- **Approval channel (design §5.5):** daemon relays the prompt to the
  foreground `rein run`, which reuses the Phase 0 layered flow
  (tty → tmux popup → stderr). Verify the invariant that the daemon
  **control socket is not reachable in-sandbox** — only the per-run proxy
  socket is (else #12 relocates instead of closing).
- Reuse run-scoped approvals (#20/0a02043) under the daemon; clear on
  agent exit; revoke write tokens on exit (the #20 intent, now native in
  the daemon — closes the loop #20 left for Phase 1).
- Automatic session expiry: idle, hard TTL, agent-process exit.
- Default-mode UX: sandboxed becomes the `rein run` default where srt is
  healthy; direct mode behind an explicit flag with a loud banner.

**Success:** concurrent runs isolated; approval prompts fire correctly
from inside a sandboxed run; an in-sandbox attempt to grant fails;
tokens revoked promptly on agent exit (in-memory — no ~1h floor on
normal exit; daemon-crash orphans live to TTL, accepted per design §7).

### CP5 — macOS parity (parallel track — NOT on the dogfood spine)

**Estimate:** 2-3 days. **Gated on Mac availability**; runs whenever a
Mac exists, before or after CP6. Linux-only dogfood is explicitly
acceptable. Bundle the outstanding direct-mode macOS e2e verification
(from #8, closed) into the same Mac session.

- srt `sandbox-exec` path; verify mitm socket + injection.
- **CA trust is NOT identical to Linux** (design §5.4): Go clients (gh)
  ignore `SSL_CERT_FILE` on darwin — expected path is user-keychain trust
  for rein's CA, with the risk note recorded. git/curl env-var trust to
  be verified per build (SecureTransport vs OpenSSL libcurl).
- CA key via macOS Keychain / Secure Enclave (`sks`) where available.

**Success:** CP3 e2e passes on macOS, including gh via the keychain-trust
path.

### CP6 — Dogfood

- Tom runs sandboxed mode on a throwaway for a few sessions, then on `wrangle`.
- **GATE — explicit human approval required:** `wrangle` is the FIRST use
  on a real repo. The throwaway-only constraint has held since Phase 0
  (CLAUDE.md hard-constraint #1). Crossing it is Tom's conscious decision,
  made only after CP1-CP4 are green and the spine has run clean on
  throwaways — not something this plan grants by reaching CP6. (CP5 is
  not a precondition; Linux-only is fine.)
- Hypothesis (design.md §7.2): two weeks on `wrangle`, no PAT fallback under
  deadline pressure.

## Later tracks (layer on the spine; sequence after CP3-CP4 hold)

- **Single-use + HEAD-pinned write tokens (TM-G6)** — now reachable because
  the proxy sees `git-receive-pack`.
- **Broker-as-CA commit signing** (§4.2.6): local CA at init, per-session
  delegation certs, gitsign.
- **Audit-comment writeback** via the audit App (created but unused since
  CP5 of Phase 0.5).
- **Five-role catalog** (replace the coarse `isWriteCapableRole`).
- **Claude Code hooks** as a complementary guard (#21).
- **Status app / OS-notification approval channel.**

## Open-issue disposition (where each followup lands)

- **#7** — the goal of this plan; closed by CP3+CP4 (same-uid host
  residual stays open, design §5.3).
- **#6** (`pull_requests:write` confers approve) — five-role-catalog
  territory; later track.
- **#8** (macOS proc-tree, direct mode; closed) — e2e verification still
  outstanding; bundle with CP5's Mac session.
- **#9** (gh classifier drift) — superseded for sandboxed mode by the
  CP2 proxy classifier; stays open for direct mode.
- **#10** (Repos[0] mint scope) — fixed in CP2's broker-core extraction.
- **#11** (consolidate pathToRepo/normalizeRepo) — fold into CP2's
  extraction touch.
- **#12** (nonce-via-tty) — sandboxed mode closes it structurally (§5.5
  control-socket invariant, verified in CP4); stays open for direct mode.
- **#13/#14** (grant can't reach tty from `!` shell; rein not on PATH) —
  direct-mode/onboarding UX; #13's sandboxed-mode analogue is handled by
  the §5.5 relay-to-foreground design.
- **#15** (stale state on failed self-grant) — direct-mode UX; untouched
  by this plan.
- **#19** (headless App creation) — independent of the spine; do
  opportunistically.
- **#21** (Claude Code hooks) — later track.
- **#22/#23** (clock skew doctor / durable NTP) — preflight lands in CP3;
  durable VM time-sync (#23) before CP6 dogfood.
- **#25** (Shape A/B rename sweep) — after this PR merges, before CP2
  touches the affected code comments.

## Notes / blockers / design corrections needed

(Append as you work. Format: date — issue — resolution.)

- 2026-06-14 — **CP1 done.** `git push` through a Go MITM proven (small +
  2 MiB chunked); the load-bearing fix was copying `ContentLength` +
  `TransferEncoding` onto the upstream request (`http.NewRequest` with an
  opaque body zeroes them — the GET-works/POST-breaks trap). Reviewer caught
  a CP2-blocker: the upstream client followed redirects, which swallows
  3xx, 502s redirected POSTs, and drops injected auth cross-host (TM-G6's
  301 chain would hit this) — fixed with `http.ErrUseLastResponse`. Full
  relay recipe in spike-findings "CP1 results". gh works via `SSL_CERT_FILE`
  on Linux (validates design §5.4). Spike code is in `/tmp` (ephemeral);
  CP2 reimplements the relay in the daemon from the recipe.

- 2026-06-08 — Spike verified the srt boundary; see
  `docs/phase1-srt-spike-findings.md`. Key correction to design.md §12.2:
  only `mitmProxy.socketPath` can inject (not `filterRequest`/`parentProxy`).
  Two new requirements it surfaced: host-aware auth (Bearer API / Basic git)
  and filesystem deny-read of ambient credential stores. `git push` through
  the MITM is the one unproven path → CP1.
- 2026-06-11 — Multi-lens review of the design PR (security / technical /
  plan). Major additions: injection invariants (SNI==Host, host classes,
  response-path hygiene, HTTP/1.1-only relay — design §4.1/§4.3), the
  environment + socket halves of ambient-credential hiding (§4.2),
  scope-as-backstop classifier framing (§5.1), approval-channel decision
  (§5.5), macOS CA-trust divergence (§5.4, CP5 off the spine), srt
  config-format migration risk (0.0.54 pin), CP1 ALPN/chunked additions,
  CP2 direct-mode regression + #10, CP3 env-allowlist/preflight/latency.
