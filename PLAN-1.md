# PLAN-1.md — Phase 1 (Shape A: sandbox composition)

**Goal:** Close issue #7. Run the agent inside the `srt` sandbox with a
resident `rein` daemon injecting short-lived, scoped GitHub tokens at the
network proxy, so the agent never holds a credential in env/files/proc.
Tom dogfoods it on a throwaway, then on `wrangle`, for two weeks without
reverting to a PAT (design §7.2 hypothesis).

**Design of record:** `docs/phase1-shapeA-design.md` (spine) +
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
  `ripgrep`, `socat` (Linux). Pin the srt version.
- Ubuntu 24.04+: AppArmor profile granting `userns` to `/usr/bin/bwrap`
  (see spike findings) — or srt won't launch. `rein init` should detect
  and guide (folds into #22/#23 work).
- Healthy NTP (#23).

## Spine checkpoints

### CP1 — Validate `git push` through a MITM  (de-risk the #1 unknown)

**Estimate:** 0.5 day. **No sandbox, no daemon yet** — just the question
the spike left open.

- Stand up the spike's Go MITM (host-aware Basic inject) on a unix socket
  and front it directly (or via a minimal http-proxy CONNECT) — no srt.
- **Mint a WRITE-tier token** for this test (the spike used a read-only
  `rein gh-auth` token — a `git push` with it 403s regardless of the MITM,
  which would masquerade as a relay bug). Inject Basic `x-access-token:`.
- Do a real `git push` of a commit to a throwaway repo THROUGH it: prove
  the POST `git-receive-pack` body upload, keep-alive across info/refs →
  receive-pack, and chunked transfer all survive the
  `ReadRequest → forward → relay` loop. Confirm the pushed commit lands.
- Also confirm `git clone`/`ls-remote` (read path) green with a live token.

**Success:** a commit pushed through the MITM appears on the throwaway
repo; no hangs/truncation on the body upload.
**Gate:** if push needs more than header injection (e.g. streaming/Expect:
100-continue handling), capture it here before the daemon design hardens.

### CP2 — Daemon skeleton + proxy arm

**Estimate:** 3-4 days.

- New resident `rein` daemon: unix control socket (0700 dir, uid-checked),
  single-instance, start/stop, holds App config + keystore + sessions in
  memory. Lift `internal/broker` mint/scope/approval logic into it
  (`tokencache` files → in-memory).
- Proxy arm: the CP1 MITM, productized into the daemon — per-request it
  asks the broker core for a token, host-aware inject, audit each request
  to a hash-chained log.
- **Per-run socket = session identity** (design §8.2): each `rein run` gets
  its own mitm socket path; the daemon maps socket → session for scope +
  approval. 0700 perms, run-lifetime teardown — the socket is a capability
  (design §7.1), bound this way.
- **Tier classifier** (design §8.1), NOT "method = tier": git keys on the
  `git-receive-pack` service; API REST on method; GraphQL needs body peek
  (`mutation` vs `query`) — this is where the Shape B `rein-gh` classifier
  (#9) moves. Fail closed (unclassifiable → prompt).
- CA: generate at first run; key via `internal/keystore`; leaves per host.

**Success:** daemon up; `curl`/`git` pointed at the proxy socket get
injected tokens; read/write tiering + scope ceiling + run-scoped approval
all fire from the proxy. Unit-tested.

### CP3 — srt composition

**Estimate:** 2-3 days.

- `rein run` (Shape A path): ensure daemon up; emit a per-run srt settings
  file (mitmProxy.socketPath, allowed/denied domains, **fs deny-read of
  credential stores**, stub `GH_TOKEN`, CA-trust env); `exec srt -s … --
  <agent>`.
- Filesystem hardening: broad-deny `$HOME` read + re-allow working tree;
  explicit deny `~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.ssh`,
  daemon key material.
- `srt`-unavailable fallback: loud warning + unsandboxed only on throwaway
  (design §4); fail closed otherwise.

**Success:** `rein run -- bash -c 'gh api …; git clone …; git push …'`
inside srt works end-to-end via proxy injection; the token is absent from
the sandbox env/proc AND the agent cannot read the host's gh login
(deny-read verified).

### CP4 — Session & approval integration in Shape A

**Estimate:** 2 days.

- Session start/scope negotiation mediated by the daemon (proxy intercepts
  the agent's session calls per design §4; broker pops human confirm).
- Reuse run-scoped approvals (#20/0a02043) under the daemon; clear on
  agent exit; revoke write tokens on exit (the #20 intent, now native in
  the daemon — closes the loop #20 left for Phase 1).
- Automatic session expiry: idle, hard TTL, agent-process exit.

**Success:** concurrent runs isolated; approval prompts fire correctly;
tokens revoked promptly on agent exit (in-memory, no ~1h floor).

### CP5 — macOS parity

**Estimate:** 2-3 days (needs a Mac).

- srt `sandbox-exec` path; CA key via macOS Keychain / Secure Enclave
  (`sks`) where available; verify mitm socket + injection identical.

**Success:** CP3 e2e passes on macOS.

### CP6 — Dogfood

- Tom runs Shape A on a throwaway for a few sessions, then on `wrangle`.
- **GATE — explicit human approval required:** `wrangle` is the FIRST use
  on a real repo. The throwaway-only constraint has held since Phase 0
  (CLAUDE.md hard-constraint #1). Crossing it is Tom's conscious decision,
  made only after CP1-CP4 are green and the spine has run clean on
  throwaways — not something this plan grants by reaching CP6.
- Hypothesis (design §7.2): two weeks on `wrangle`, no PAT fallback under
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

## Notes / blockers / design corrections needed

(Append as you work. Format: date — issue — resolution.)

- 2026-06-08 — Spike verified the srt boundary; see
  `docs/phase1-srt-spike-findings.md`. Key correction to design.md §12.2:
  only `mitmProxy.socketPath` can inject (not `filterRequest`/`parentProxy`).
  Two new requirements it surfaced: host-aware auth (Bearer API / Basic git)
  and filesystem deny-read of ambient credential stores. `git push` through
  the MITM is the one unproven path → CP1.
