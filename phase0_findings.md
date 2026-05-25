# Phase 0 — Findings

Anchor for the learnings, design corrections, and open issues that came
out of building Phase 0. Written so Phase 0.5 (and anyone reading later)
can pick up without re-deriving everything from commit messages.

## Summary

Phase 0 succeeded. The credential-helper-based architecture works
end-to-end against an unmodified Claude Code session running in a Linux
VM, against throwaway GitHub repositories. The six PLAN.md checkpoints
landed, plus four follow-on checkpoints (CP3.5, CP3.6, CP3.7, CP5.5)
that closed real holes surfaced during testing. Fifteen GitHub issues
track Phase 0.5 / Phase 1 followups.

The most important property — **TM-G8 (helper always returns a credential
for github.com get)** — held across every path tested, including direct
exposure to Claude. No `gh auth setup-git` regression was observed.

## What got built (chronological)

| CP | What | Validates |
|----|------|-----------|
| 1 | `cmd/spike-token`: mint scoped installation token; verify repo A=200, repo B=404 | jferrl/go-githubauth works; per-repo scope enforced at GitHub's API |
| 2 | `cmd/rein credential-helper` + `internal/broker`: real git credential helper; TM-G8 placeholder on mint failure | Shape B helper path; TM-G8 invariant in place from day 1 |
| 3 | `cmd/rein-git` shim + REIN_GIT_OP env signaling + `/proc` fallback; two-tier read/write tokens | PLAN's helper-can-see-/git-receive-pack mechanism didn't exist; PATH-shim is the Shape B discriminator |
| 3.5 | `rein gh-auth` env file (deprecated) | First attempt at gh coverage |
| 3.6 | gh write token revoke on store/erase; rein-gh shim with lazy refresh; install-shim places shims dir | Effective git-write TTL drops from ~1h to operation-duration; rein-gh handles long sessions |
| 3.7 | Two-tier gh tokens (read cached, write JIT + revoke on exit); `internal/tokencache` + `internal/ghsession` extractions | gh writes get the same revoke discipline as git writes; cached gh-read tier has read-only capability if exfiltrated |
| 4 | `internal/session` YAML loader; `Config.InScope` predicate in broker; scope-ceiling enforcement via `credential.useHttpPath=true` | Per-repo scope refusal returns TM-G8 placeholder with clear log line |
| 5 | `internal/ui/prompt` (TTYPrompter) + `Config.ConfirmWrite` predicate; `session.Issue` field | Human confirmation ceremony works at the helper layer |
| 5.5 | `internal/ui/grant` (layered: cache→tty→tmux popup→helpful stderr); `internal/approvals` (approve-once per session); `rein approval grant/status/clear` subcommands; gh-shim ConfirmWrite gating + denial placeholder | Approve-once-per-session UX; discovery layers cover claude's no-/dev/tty case; gh writes share the same gate; deny-with-GH_TOKEN-placeholder prevents `hosts.yml` fallback |
| 6 | `rein run -- <cmd>`: per-process git config (`GIT_CONFIG_GLOBAL`), PATH-prepend shim dir, env wiring, signal forwarding, cleanup | Phase 0 integration entry point; e2e validated with real claude |

## Design corrections (PLAN.md got these wrong)

These are concrete cases where PLAN.md or the design assumed mechanisms
that don't exist in practice. Captured so they don't get re-derived in
Phase 1.

1. **CP3's "helper sees `/git-upload-pack` vs `/git-receive-pack`"
   does not exist.** Git's credential helper is invoked at the repo-URL
   level, before deciding fetch vs push. The smart-protocol endpoint is
   never in the helper's `path` attribute, even with
   `credential.useHttpPath=true`. The PATH-shim that sets `REIN_GIT_OP`
   in env is the Shape B substitute.

2. **Pre-push hooks fire AFTER refs retrieval, not before.** A hook
   can't be used as the intent signal because reaching the hook
   already requires a write-capable token (the initial `GET
   /info/refs?service=git-receive-pack` 403s with a read-only token,
   aborting the push transport before any hook fires).

3. **CP4 strict scope-check requires `credential.useHttpPath=true`.**
   Without it, the helper sees `path=""` and can only fall back to
   server-side scope enforcement (token scope). `rein install-shim`
   prints the recommended config; `rein run` sets it.

4. **CP5's "single prompt presented to human for the push" was
   per-write, which is UX-hostile for productive sessions.** Approve-
   once-per-session with a configurable TTL (default 4h, matching
   design §4.2.2's `default_read_ttl`) lands the right ergonomic and
   aligns with design §2.2's session-start ceremony framing.

5. **CP5's `/dev/tty` mechanism is unavailable inside claude's Bash
   tool calls** (claude detaches the controlling tty from its
   subprocesses). Layered discovery (tty → tmux popup → helpful
   stderr + out-of-band grant) handles all three cases.

6. **GitHub's `POST /app/installations/{id}/access_tokens` does not
   accept a custom expiration.** Returned tokens always have ~1h TTL,
   regardless of design §4.2.5's 5-minute write target. The 5-minute
   effective window is achieved broker-side via `DELETE
   /installation/token` on the credential-helper's store/erase action.

7. **gh writes bypass the git credential helper entirely** (gh reads
   `GH_TOKEN` env or `~/.config/gh/hosts.yml`). Without coverage, an
   agent could TM-G8-displace at the API layer by running `gh auth
   login`. The gh shim's `runWrite` path now gates through the same
   approval flow as the helper; on denial, sets
   `GH_TOKEN=rein-placeholder-denied` to prevent fallback to the user's
   `hosts.yml`.

## Honest Shape B limits (observed empirically)

The design's §5.1 threat table is honest about Shape B being weaker
than Shape A. CP6 e2e confirmed specific weaknesses:

1. **TM-G5 (human confirmation) is bypassable by an agent with shell
   access.** An agent that can read `dev-session.yaml` knows the issue
   number; it could run `rein approval grant` itself and answer the
   prompt. Observed in CP6 e2e only because the test used `! rein
   approval grant` from claude (a user convenience, not an agent
   attack) — but the same path is available to a determined agent.
   Issue #12 (nonce-via-tty) marginally hardens. Phase 1 sandbox is
   the architectural fix.

2. **Tokens are reachable from any UID-shared process.** `GH_TOKEN`
   env, the read cache file at `~/.local/state/rein/cache/`, the
   pre-mint env file, are all readable by any process running as the
   user. Standard Unix assumption. Phase 1 sandbox closes this by
   running the agent without UID-shared access to these files.

3. **The agent's `/dev/tty` situation is implementation-dependent.**
   Claude detaches `/dev/tty` from its Bash tool subprocess but
   preserves it for the `!` shell escape. A different agent might do
   either differently. Layer 4 (helpful stderr) is the only reliably-
   working channel for agent-driven writes.

4. **`$TMUX` propagation depends on launch context.** If `rein run` is
   launched from a shell without `$TMUX` set (user attached to tmux
   later), claude's bash subprocess won't have it either, and the
   tmux-popup layer won't fire. `rein doctor` (Phase 0.5 CP2) should
   surface this.

## What's verified working

- Scope-ceiling enforcement at helper layer
- Two-tier git tokens (read cached ~1h, write JIT + revoke-on-store
  yielding ~operation-duration effective TTL)
- Two-tier gh tokens (read cached, write JIT + revoke-on-gh-exit)
- Approve-once-per-session with file-based approval cache
- Layered grant discovery: tty → tmux popup → helpful stderr
- Session-based scope (`~/.config/rein/dev-session.yaml`)
- gh deny placeholder (prevents `hosts.yml` fallback)
- `rein run` per-process git config (no global pollution)
- TM-G8 invariant across every refusal path (mint failure,
  scope refusal, confirm denial, prompt cancellation, prompter
  error, panic recovery)
- Full e2e against Claude Code in a Linux VM: clone, commit, push
  succeeded; agent did NOT run `gh auth setup-git`; agent did NOT
  bypass any operation we expected to intercept

## GitHub issues filed during Phase 0

Each tracks a real gap or followup. Numbers link to
github.com/TomHennen/rein/issues/N.

**Phase 1 / sandbox-required**:
- #6 — `pull_requests:write` grants PR review/approve; SLSA
  `counts_as_reviewer: false` is the policy answer
- #7 — Shape B token leakage via env/files/proc; sandbox closes
- #15 — Failed self-grant + retry-401 UX confusion; status app fixes

**Phase 0.5 (this plan)**:
- #8 — macOS proc-tree fallback for `DetectWrite`
- #14 — rein binary not on PATH in fresh terminals; `rein init`
  addresses

**Phase 0.5 or Phase 1 (bandwidth-dependent)**:
- #12 — Nonce-via-tty hardening for `rein approval grant`
- #13 — UX: `rein approval grant` from inside claude's `!` escape
  (partly addressed by CP5.5's tmux popup fallback in grant
  subcommand; works in tmux but not without)

**Code cleanup**:
- #9 — gh classifier table pinned to gh 2.67; needs drift detection
- #10 — Multi-repo sessions: mint scoped only to `sess.Repos[0]` but
  `Contains` accepts all entries; CP4+ correctness issue
- #11 — Dedup `broker.pathToRepo` / `session.normalizeRepo`

## Open questions for Phase 1

These are design-level questions Phase 1 has to answer; not bugs in
Phase 0. Listed for future-me / reviewers.

- **Daemon vs in-process broker.** Today every helper invocation
  re-parses session, re-reads caches, re-builds the App token source.
  Phase 1's daemon would hold these in memory; helper becomes a thin
  RPC client. Audit log + approval lifecycle gets cleaner.

- **Sandbox composition mechanism.** Design §3.5 names `srt` (Anthropic's
  research preview). Two CVEs in 6 months mean we treat it as defense-
  in-depth, not a hard boundary. Alternative: greywall (§3.5 mention).
  Phase 1 picks one and integrates.

- **Status app channel for prompts.** Design §2.2 mentions "terminal,
  OS notification, or a rein status app." For Phase 1 with sandbox
  composition, the in-tty channels go away; some out-of-band UI is
  needed. Could be: GTK/Qt status icon on Linux, native menu bar on
  macOS, or a tiny local web UI the user opens.

- **Audit App integration.** Phase 0 has no audit comment writeback.
  Design §4.2.4's split (primary + audit App) is implementable today —
  audit App's installation token in a separate broker-controlled
  variable, agent never sees it. Phase 1.

- **Mint rate-limit handling.** Empirical: GitHub returns transient
  "Bad credentials" 401s when mint rate gets high (presumably
  anti-abuse). Phase 1 needs caching (per-session write tokens reused
  across pushes within a window), backoff, batch revokes. Tracked
  here, not as an issue.

- **Real cross-platform support.** macOS proc-tree fallback (#8) is
  Phase 0.5. Windows is not currently considered. Design §4.2.1.1's
  Windows Credential Manager mention is the closest reference.

- **Commit signing (broker-as-CA).** Design §4.2.6's v1 default.
  Phase 0 doesn't sign commits. Phase 1 should land this for any
  SLSA Source Track ambitions.

## Empirical surprises worth knowing

These are observations from running things, not bugs:

- **GitHub's installation-token mint has secondary rate limits.**
  Multiple-mints-per-minute can trip transient 401 "Bad credentials"
  even with valid App JWTs. Resolves on its own in 5-60 minutes.
  Affects testing-heavy days; less likely in production use where
  one-mint-per-push is the normal cadence.

- **`git push` can print "fatal: the remote end hung up
  unexpectedly" followed by "Everything up-to-date"** when transport
  layer retries succeed after a transient failure. Cosmetic; the push
  actually completed. Has not been root-caused — likely git's HTTP/2
  vs HTTP/1.1 fallback or a transient TLS hiccup.

- **gh forks itself internally for some subcommands.** Our shim's
  `REIN_GH_SHIM_ACTIVE` sentinel short-circuits re-entrant
  invocations to avoid double-minting + double-prompting per
  user-visible gh call. Verified to halve API pressure for write ops.

- **`/dev/tty` is reachable from claude's `!` shell escape but not
  from its Bash tool subprocess.** Asymmetric. The shell-escape uses
  a real subshell with controlling-tty inheritance; the Bash tool
  detaches. Means: a human can manually run `rein approval grant`
  via `!` (works); an agent doing the same via its Bash tool would
  hit ErrNoTTY (good — prevents trivial agent-self-grant). Confirmed
  in CP6 e2e logs.

- **`include.path` in git's config layers user preferences correctly
  while letting rein's `credential.*` overrides win** (via the
  empty-helper-resets-list rule per gitcredentials(7)). `rein run`
  relies on this so the wrapped process keeps the user's aliases,
  editor, signing config, etc.

## Reference: file layout as of Phase 0 close

```
cmd/
  rein/            — the rein binary (credential-helper, install-shim,
                     gh-auth, approval {status|clear|grant}, run)
  rein-git/        — PATH-shim that sets REIN_GIT_OP=read|write|unknown
  rein-gh/         — PATH-shim that gates gh writes through ConfirmWrite +
                     two-tier mint + revoke-on-exit
  spike-token/     — CP1 spike (kept; not absorbed yet)
internal/
  approvals/       — approve-once-per-session: Record{Signature, ...}
  broker/          — credential-helper protocol + scope-check + tier
                     selection + read cache + revoke + ConfirmWrite hook
  config/          — env-var loader, StateDir, ConfigDir
  ghsession/       — EnsureFresh: shared read-token cache for rein-gh +
                     `rein gh-auth`
  githubapp/       — go-githubauth wrapper: MintReadOnlyToken,
                     MintWriteToken, MintGhReadOnlyToken,
                     MintGhSessionToken, RevokeToken
  session/         — Session{ID, Role, Repos, Issue, Created}, YAML
                     loader, LoadOrFallback, Contains
  tokencache/      — Entry{Token, ExpiresAt}, atomic read/write
  ui/grant/        — ObtainApproval (4 layers), Grant (subcommand),
                     TmuxRunner
  ui/prompt/       — Prompter, TTYPrompter, StubPrompter,
                     Request, ErrNoTTY, ErrCancelled
docs/
  design.md        — pre-Phase-0 design; needs sweep before Phase 1
PLAN.md            — Phase 0 plan; checkpoints CP1-CP6
phase0_findings.md — this file
```
