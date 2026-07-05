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
  `ripgrep`, `socat` (Linux). **Pin srt 0.0.63** (bumped from 0.0.54 on
  2026-07-05; latest, and what CP3 builds + re-verifies against — the
  `mitmProxy.socketPath` schema is byte-identical 0.0.54→0.0.63 and 0.0.63
  is still the old config format, so the injection lever is intact). The
  spike originally verified 0.0.54; CP3's live e2e is the bump re-verify.
  Upstream's main-branch README still describes a new config format where
  custom-proxy support is "not yet supported … future release" — the
  `mitmProxy.socketPath` hook will move eventually. Track upstream; expect
  one migration; standing policy is pin + re-verify-on-bump (design §7).
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

**Status: DONE (2026-07-05).** Proxy arm landed + reviewed (code + security)
+ live-verified against real github.com. See the 2026-07-05 Notes entries.
Architecture changed mid-checkpoint: **in-process per run, not a resident
daemon** (Tom's decision) — the daemon skeleton stays as unwired shelf code;
`internal/runbroker` is the host. `curl`/`git`-through-the-proxy read/write
tiering + scope ceiling + run-scoped approval all verified live.

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

- 2026-07-05 — **CP2 proxy arm DONE** (supervised agent team). Delivered:
  `internal/proxy` (TLS-terminating injecting MITM on a per-run unix socket —
  the CP1 6-point relay recipe productized: ALPN http/1.1 pin, ContentLength/
  TransferEncoding copy, ErrUseLastResponse, DisableCompression, 100-continue,
  chunked bodies; SNI==Host, §4.3 host-class inject, per-SNI leaf cache,
  keystore-backed CA, plain redacted audit log) and `internal/runbroker` (the
  in-process per-run host — see the pivot note below). #10 (mint the full
  session repo set) and #11 (one RepoFromPath helper) fixed. Two code + two
  security review passes; all confirmed findings fixed (keep-alive body-drain
  race, BareRepoNames normalization, missing inbound I/O deadlines, 100-continue
  ordering, log control-char sanitization, SNI normalization, fail-closed
  nil-approval guard). **Live gate PASSED** against real github.com on the
  throwaway (`live_test.go`, REIN_LIVE-gated): read-Bearer 200, read-git 200,
  write-receive-pack 200 (proves push perm), out-of-scope → local 403 no
  egress, zero token leak client-side. Minted tokens revoked on cleanup.
  Follow-ups filed: #30 (closed — prompt now names full session scope),
  #31 (CA/leaf rotation, deferred).

- 2026-07-05 — **DESIGN CORRECTION (Tom): v1 spine is in-process per run,
  NOT a resident daemon.** Each `rein run` hosts the broker core + proxy in
  its own out-of-sandbox process; the write-approval prompt is in-process on
  the foreground tty. Removed from the spine: the daemon control socket +
  protocol, daemon lifecycle, and **CP4's daemon→foreground approval relay**;
  issue #12's sandboxed-mode analogue closes structurally (no control channel
  exists to reach). `internal/daemon` stays on the branch as **unwired shelf
  code** for the later tracks that motivated it (status app, OS-notification
  approvals, one-time biometric key unlock, shared cross-run token cache) — a
  daemon returns only when one of those lands. Security equivalence argued and
  accepted: tokens live in a same-uid out-of-sandbox process's memory either
  way; same-uid residual + crash-orphan behavior identical (design §5.3/§7).
  **CP4 shrinks** to run-scoped approvals + session expiry. `phase1-design.md`
  §4 ("Three pieces"), §5.2, §5.5 carry daemon language superseded by this —
  correction banner added at the design doc top; full prose sweep folded into
  the #25 rename sweep.

- 2026-07-05 — **Write-approval granularity: RUN-SCOPED for both git and
  GraphQL** (Tom's decision, resolving a security-review finding). A deeper
  security pass found the code memoized approval per-repo for git but blanket
  ("" key) for repo-less/GraphQL writes. Tom's model: the human approves the
  agent for the session's issue/repo set once; then good-to-go until token
  expiry; reads never prompt. This matches design §5.3 ("approvals are
  run-scoped, not per-request") and the #10 full-set token (the first approval
  already mints a token valid across the whole session set, so per-repo
  re-prompting was only an awareness ping, never a scope gate). Code brought in
  line: the per-repo memo collapsed to a single per-run "writes approved" flag;
  no GraphQL body parsing needed. Scope ceiling still enforced upstream of the
  prompt (out-of-scope repos refused before confirmWrite). design §5.1's
  read/write bullet should read run-scoped, not per-repo (fold into the sweep).

- 2026-07-05 — **Audit log: plain append-only + token-redacted, NOT
  hash-chained** (team simplification, v1). Rationale: the same-uid host
  residual (§5.3) means a local attacker who can write the log can rewrite an
  unanchored chain, so chaining buys tamper-evidence against nearly nobody; the
  in-sandbox agent is already denied read/write of the log by the CP3 fs rules.
  Re-add chaining with an external anchor if/when the audit-writeback track
  lands. Redaction is **by VALUE** (the minted token strings), never by
  pattern — GitHub is rolling out a new `ghs_APPID_JWT` installation-token
  format (~520 chars, variable, staged from 2026-04-27), so `ghs_`-prefix/
  length regexes rot. (rein source has no token-shape assumptions today —
  verified.)

- 2026-07-05 — **advisor() unavailable** in this background session (MCP tool
  not connected); compensated with dual reviewer subagents (code + security,
  two passes each) on the CP2 diff before surfacing. Re-check advisor
  availability next session.

- 2026-07-05 — **Stop-condition (b) partially triggered — needs Tom's
  read before CP3.** Claude Code shipped first-party local credential-injection
  plumbing: `sandbox.credentials` `"mode": "mask"` (v2.1.187+ deny, v2.1.199+
  mask) substitutes a per-session sentinel for env credentials and re-injects
  the real value at the sandbox proxy for allowlisted hosts, on the new
  experimental `network.tlsTerminate`; Managed Agents (cloud beta, ~2026-06-09)
  does the same with vault keys. What has NOT shipped: minting short-lived
  issue-scoped App tokens, scope ceilings, write approvals — masking re-injects
  the user's *existing long-lived* token, the exact primitive rein replaces.
  Read: the injection plumbing is now commodity; rein's moat is the brokering
  semantics. Not a hard §0(b) stop as worded, but the delta shrank — decide
  consciously before investing CP3+. **Positioning now documented** in
  `phase1-design.md` §5.6: masking protects against *theft*, rein additionally
  limits *contemporaneous capability* (scope + read/write + write-approval);
  rein's blast radius is bounded by *App installation* (not account reach like
  a PAT), and with no `gh auth login` there's no user token on disk at all;
  agent-agnostic is real but bounded (local CLI subprocess / HTTPS / CA-env).
  Tom (2026-07-05): don't file the srt upstream issue until CP3/dogfood gives a
  working integration to point at — draft stays staged.

- 2026-07-05 — **srt upstream (researcher, 0.0.63 tarball-diffed):**
  `network.mitmProxy.socketPath` schema is byte-identical 0.0.54→0.0.63 and
  still undocumented (README still says custom proxy "not yet supported" in the
  new config format — docs contradict code). NEW CONSTRAINT: srt's
  `tlsTerminate` and `mitmProxy` are mutually exclusive at config level, and
  first-party masking is built on `tlsTerminate` — rein's hook now competes
  with upstream's own injection path (displacement risk real). No upstream
  issue covers BYO-proxy; a draft is staged (needs Tom's go-ahead to file — it
  is outward-facing). **Pin BUMPED to 0.0.63 (Tom, 2026-07-05)** — build CP3
  against current for the sandbox-escape fixes (srt is our defense-in-depth
  boundary) and let CP3's live e2e serve as the bump re-verify, since the
  injection lever is schema-stable and 0.0.63 is still the old config format.
  Infisical Agent Vault is now a single Go binary with a built-in MITM
  proxy (v0.39.0) but still no GitHub opinionation — stop-condition (c) half.


- 2026-06-14 — **CP2 foundation landed** on `cp2-daemon-core`
  (`d452925..0c5f600`), all built + tested + pushed:
  - **`internal/brokercore`** — the decision core extracted from
    `broker.handleGet`: `Serve(ctx, Request) → Credential`, always non-empty
    (TM-G8); scope → approval → mint/cache; `ReadCache` interface. Direct
    mode + every existing test green (public broker API unchanged). Reviewed;
    hardened to fail-closed on nil mint.
  - **`internal/classify`** — tier classifier (design §5.1), fail-closed to
    Write: github.com keys on git service (receive-pack=write), REST on
    method, GraphQL peeks the body (literals/comments stripped). Where #9
    moves.
  - **`internal/daemon`** — `MemReadCache` + same-uid control-socket skeleton
    (0700 dir / 0600 socket / SO_PEERCRED; single-instance; ping stub).
    Reviewed; `-race` green. NOT yet wired into `cmd/`.
  - **NEXT (resume here):** the **proxy arm** — port the CP1 relay (6-point
    recipe, spike-findings "CP1 results") + call `classify` then
    `brokercore.Core.Serve`, host-aware inject; **per-run socket** (= session
    identity) + the **placement-outside-bind-mounts** check (§5.3). Then: CA
    management (keystore-backed, per-host leaves); daemon control methods for
    token/approval requests over the socket; `rein run` → daemon + srt
    composition (CP3); #10 (multi-repo mint scope) still open. The CP1 spike
    MITM (`/tmp`, ephemeral) is the relay reference; it is also captured as
    prose in spike-findings.

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
