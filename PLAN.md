# PLAN.md — Phase 0

**Goal:** Demonstrate the credential-helper round-trip with one GitHub App, one role (`implement`), unsandboxed. Success = `rein run -- claude` in a throwaway repo, claude pushes commits, broker mints scoped credentials JIT, no PAT involved.

**Time budget:** A weekend. If a checkpoint is taking significantly longer than its estimate, surface the surprise to the human before continuing.

**Working repos:** Two throwaway repos identified by `$REIN_TEST_REPO_A` and `$REIN_TEST_REPO_B` env vars. App config is in `$REIN_APP_CLIENT_ID`, `$REIN_APP_ID`, `$REIN_APP_INSTALLATION_ID`. App private key is at `$REIN_APP_PRIVATE_KEY_PATH`.

**Dev environment.** Development happens directly in this Linux VM. Source `./dev-env` at the start of each session to load REIN_* env vars. If additional tooling is needed (Go libraries, system packages), add a note under "tooling requests" at the bottom of this file.

## How to work this plan

1. Do checkpoints in order. Each builds on the previous.
2. At each checkpoint: implement, test, run, **stop and surface to human**, wait for verification.
3. Between checkpoints, run free — refactor, polish, add tests, but don't expand scope.
4. If something surprises you (design wrong, library doesn't behave, GitHub API behaves unexpectedly), stop and surface rather than working around it.
5. Update this file with notes, blockers, and "design corrections needed" as you go.

## Checkpoint 1 — Device Flow + scoped installation token

**Estimate:** 2-3 hours.

**Goal:** Prove the GitHub App integration works end-to-end in Go. A small program that:
1. Loads the App ID and private key from env vars (`REIN_APP_ID`, `REIN_APP_PRIVATE_KEY_PATH`).
2. Signs a JWT for the App.
3. Calls `POST /app/installations/$REIN_APP_INSTALLATION_ID/access_tokens` with `repository_ids` (the repo A ID, looked up from `$REIN_TEST_REPO_A`) and minimal `permissions` (`contents: read, metadata: read`).
4. Uses the resulting token to call `GET /repos/$REIN_TEST_REPO_A` (should 200) and `GET /repos/$REIN_TEST_REPO_B` (should 404, because token is not scoped to it).
5. Prints both status codes.

**Success criterion:** Repo A returns 200, repo B returns 404. The scoping mechanism is verified working in real Go code, matching validation §12.3 findings.

**Code location:** `cmd/spike-token/main.go`. Single-file spike; not part of the final binary.

**What this validates:** That `jferrl/go-githubauth` does what we need, that the App config is right, that `repository_ids` actually enforces scope.

**What to skip:** No CLI structure, no daemon, no broker logic. Just: prove the mint works.

**Output to human:** "Spike at `cmd/spike-token/main.go` mints a scoped token; repo A=200, repo B=404. Ready for Checkpoint 2."

---

## Checkpoint 2 — Credential helper that mints on demand

**Estimate:** 3-4 hours.

**Goal:** A real git credential helper binary that, when invoked by git, mints a fresh read-only installation token and returns it via the credential-helper protocol.

**Implementation:**
- New binary at `cmd/rein/main.go` with a `credential-helper` subcommand.
- The subcommand reads the git credential protocol from stdin (host, protocol, path, etc.), determines whether the request is for github.com, mints a fresh installation token for the configured repo, and writes the credential back to stdout per the git credential helper protocol.
- Tokens are minted on demand; no caching yet (Checkpoint 3 will add caching).
- **Always return a credential** (TM-G8). Even if no session is active, return a placeholder read-only credential rather than erroring or returning empty.
- Configure git to use this helper for github.com: `git config --global credential.https://github.com.helper "/path/to/rein credential-helper"`. (For Phase 0 we configure git globally for this user on this VM; later checkpoints will scope it to the wrapped process.)
- Test: `git clone https://github.com/<owner>/agentcreds-validation-a.git` should succeed using broker-minted creds.

**Success criterion:** `git clone` against the throwaway repo succeeds. Helper logs show it was invoked, what credentials it returned, what was minted from GitHub.

**Code location:**
- `cmd/rein/main.go` — CLI entry point.
- `internal/broker/broker.go` — the broker logic (token minting, session lookup).
- `internal/githubapp/client.go` — wrapper around go-githubauth.

**What this validates:** The Shape B credential-helper path (design §6.2) works mechanically. The TM-G8 "always return a credential" property is in place from the start.

**Edge cases to handle:**
- Helper invoked for non-github.com host → return empty credential block (signals "I don't handle this host").
- Helper invoked for `store` or `erase` action → no-op, exit 0 (we don't persist credentials anywhere).
- Token mint fails → log error, return the placeholder credential anyway (TM-G8).

**Output to human:** "Helper at `cmd/rein` mints tokens for github.com on demand. `git clone` works against the throwaway repo. Helper logged at <path>."

---

## Checkpoint 3 — Two-tier tokens (read + JIT write)

**Estimate:** 3-4 hours.

**Goal:** Extend the broker to distinguish read vs write operations. Read operations use a cached session read token (1 hour TTL); write operations (specifically `git push`) get a freshly minted single-use write token (5 min TTL, no caching).

**Implementation:**
- In-memory session table with one hardcoded session for now (no session lifecycle yet).
- The credential helper inspects the git operation context (CRED helper protocol gives us the host + path; pushes hit `/git-receive-pack`, fetches hit `/git-upload-pack`).
- For reads: return cached token if non-expired; mint a new read token if needed.
- For writes: mint a fresh single-use write token (`contents: write`); don't cache.
- Test: `git push` against the throwaway repo (after committing a change) should succeed. A second push should mint a fresh token (verify in logs).

**Success criterion:** Both `git fetch` (uses cached read token) and `git push` (uses freshly minted write token) succeed. Logs show distinct token minting for each operation type. A read token captured during fetch fails when used for a push (verify by inspecting headers).

**What this validates:** The two-tier model from design §4.2.5 works in practice.

**Open questions to surface:**
- Does GitHub actually accept the read-scoped token for `/git-upload-pack`? Confirmed in validation §12.3 but worth double-checking against the real git client.
- Is the 5-minute write TTL achievable, or does GitHub round up to 1 hour minimum? Try 5m and see.

**Output to human:** "Two-tier tokens working. Fetch uses cached read token; push mints fresh write token. Logs at <path>."

---

## Checkpoint 4 — Hardcoded session with scope ceiling

**Estimate:** 2-3 hours.

**Goal:** Introduce the session abstraction. One hardcoded session at startup that names one repo (validation-a). The broker only mints tokens for that repo; requests for any other repo are refused.

**Implementation:**
- `internal/broker/session.go` — Session type with an ID, role, scope ceiling (list of repos), and timing fields.
- The broker loads a hardcoded session at startup from `~/.config/rein/dev-session.yaml`.
- Credential helper checks the requesting host/path against the session's scope ceiling before minting. Out-of-scope requests get a placeholder credential (TM-G8) plus a clear log line indicating the refusal.
- No human confirmation prompt yet (that's Checkpoint 5).
- Test: `git clone https://github.com/<owner>/agentcreds-validation-a.git` succeeds. `git clone https://github.com/<owner>/agentcreds-validation-b.git` fails with auth error (helper returned placeholder).

**Success criterion:** In-scope operations succeed; out-of-scope operations fail with a clear error in the broker logs.

**What this validates:** The scope-ceiling enforcement from design §4.2.2 works before any UI is built.

**Output to human:** "Session-based scope ceiling enforced. Cloning validation-a succeeds; cloning validation-b fails. Logs show the refusal."

---

## Checkpoint 5 — Human confirmation prompt

**Estimate:** 3-4 hours.

**Goal:** Add the human-in-the-loop ceremony for write operations. When `git push` is about to mint a write token, the broker prompts the user (via a separate terminal/notification) and waits for them to type the issue number before approving.

**Implementation:**
- `internal/ui/prompt.go` — abstraction for confirmation prompts. v0 implementation uses stdin/stderr on a separate file descriptor (since the credential helper's stdout is consumed by git, we need a different channel for human interaction). Probably: write the prompt to `/dev/tty` and read response from `/dev/tty`.
- Prompt format matches design §2.2: show the role, repo, branch, and ask for non-replayable input (issue number).
- On approval: mint the write token. On Ctrl-C or wrong input: deny.
- The hardcoded session config now includes an issue number alongside the repo.
- Test: with a session bound to issue 1 in validation-a, `git push` triggers a prompt. Type "1" → push succeeds. Type "2" → push fails. Ctrl-C → push fails.

**Success criterion:** The confirmation mechanic from design §2.2 works end-to-end. Non-replayable input (issue number) is required.

**What this validates:** The TM-G5 mitigation (human confirmation with non-replayable input) is implementable in practice.

**Open question to surface:**
- What does the prompt UX feel like in practice? Does typing the issue number every push feel right? Surface a 1-paragraph reaction to the human after testing.

**Output to human:** "Confirmation prompt working. `git push` triggers prompt asking for issue number. Right number proceeds; wrong number or Ctrl-C denies. Reaction to UX: <your reaction>."

---

## Checkpoint 6 — Full Phase 0 integration

**Estimate:** 3-5 hours.

**Goal:** End-to-end test with a real Claude session on this VM. `rein run -- claude` (or equivalent) launches claude in an environment where the credential helper is in place. Claude is given a real-ish task (modify a file, commit, push). The broker handles the full lifecycle.

**Implementation:**
- `rein run` subcommand: sets up the credential helper config in a way that's local to the wrapped process (not global git config), launches the wrapped command, monitors it.
- The hardcoded session is bootstrapped at `rein run` start.
- All previous behaviors (scope ceiling, two-tier tokens, confirmation prompt) compose.
- Test prompt for claude: *"Make a change to README.md adding a line that says 'tested at <timestamp>', commit it, and push the branch."*
- Watch what happens. Claude should: invoke the helper for fetch (works silently), make the change, commit (no GitHub call), attempt push (triggers prompt), push succeeds after human types the issue number.

**Success criterion:** A claude session completes the task. Single prompt presented to human (for the push). Push succeeds. Logs show the full lifecycle: session start, credential helper invocations, prompt, write token mint, push success.

**What this validates:** Phase 0 success criterion from design §7.1. The full credential-helper-based architecture works end-to-end with an unmodified `claude`.

**Critical thing to watch:**
- Does claude run `gh auth setup-git` at any point? If yes, TM-G8 has a regression — investigate.
- Does claude bypass the helper for any operation (e.g., direct API calls instead of git)? If yes, document the bypass; some bypasses are fine, others may need additional coverage.
- Does the prompt land in a place where the human will see it? If the human misses the prompt, the UX is broken — surface the issue.

**Output to human:** A short writeup: did Phase 0 succeed? What surprised you? What does the human need to know before deciding on Phase 1?

---

## After Checkpoint 6

Phase 0 is complete. Decision point for the human:

- Did the round-trip work? If yes, proceed to Phase 1 (see design §7.2).
- Did anything fundamentally surprise you? Update design doc and CLAUDE.md before Phase 1.
- Are there design corrections needed (things the design got wrong)? Surface them clearly with proposed fixes.

Phase 0 is also when to clean up: the spike at `cmd/spike-token/` is throwaway; absorb anything reusable into `internal/githubapp/` and delete the spike.

## Notes / blockers / design corrections needed

(Append entries here as you work. Format: date — issue — proposed resolution.)

- 2026-05-24 — Checkpoint 1 spike used `REIN_APP_CLIENT_ID` (string Client ID, matches design §4.2.4's recommendation for new apps) rather than the int64 `REIN_APP_ID` named in PLAN.md's CP1 description. Both work; `go-githubauth.NewApplicationTokenSource` is generic over `int64 | string`. Resolution: prefer Client ID throughout; PLAN wording was loose, no design change needed.
- 2026-05-24 — CP1 spike scoped the installation token via `Repositories: []string{"<name>"}` rather than `RepositoryIDs: []int64{...}` (PLAN wording). Names avoid an extra `GET /repos` lookup and the API enforces scope identically. Resolution: name-based scoping is the default unless we need IDs for a specific reason later.
- 2026-05-25 — CP3 design correction. PLAN's stated discriminator ("CRED helper protocol gives us the host + path; pushes hit `/git-receive-pack`, fetches hit `/git-upload-pack`") does not exist. Empirically verified that git invokes the credential helper at the repo-URL level *before* deciding fetch vs push; with `credential.useHttpPath=true` the helper sees `path=owner/repo.git`, never the smart-protocol endpoint. Three options were evaluated (process-tree introspection / always-write single-tier / defer to Shape A); initially resolved to **Option K**: a `pre-push` hook writes a consume-once intent file the helper reads.
- 2026-05-25 — Option K failed in e2e testing. Pre-push hooks fire AFTER git successfully reads refs from the remote, not before. With a read-only token, `GET /info/refs?service=git-receive-pack` returns 403 (verified with curl using Basic auth: read token gets 200 on upload-pack but 403 on receive-pack), so git aborts the push transport before the hook can run. The hook-as-intent-signal pattern requires the helper to first return a write-capable token to GET refs — which defeats the purpose of two-tier.
- 2026-05-25 — Final CP3 mechanism (after external research input): **PATH-shim primary + process-tree introspection fallback**. A small Go binary at `cmd/rein-git/` is placed earlier in $PATH than the real git; it parses argv past global options, sets `REIN_GIT_OP=read|write|unknown` based on the subcommand, then execs the real git. The env var propagates through git → git-remote-https → credential helper (verified empirically: `REIN_GIT_OP=write` set before `git push` is visible in the helper's `os.Environ()`). The helper reads the env var; if absent or `unknown`, it falls back to walking `/proc/<pid>/{cmdline,status}` for `git push`/`git send-pack` in the ancestor chain (Linux-only; macOS support is libproc later). Default if neither signal yields write: read. TM-G8 unchanged — this is a routing signal, not a security boundary; misdetection causes a wrong-tier mint, not a security breach. The role's permissions ceiling remains the authoritative boundary.
- 2026-05-25 — Spotted in CP3 research: `gh` CLI ops (issues, PRs, merges) bypass the git credential helper entirely. Without coverage, agents will fall back to `gh auth login` and TM-G8-displace us at the API layer. Punted to a new CP3.5 to keep CP3 reviewable. Approach: rein writes installation token to `~/.config/gh/hosts.yml` at session start, refreshes before 1h expiry, backs up + restores any pre-existing file on enter/exit. Phase 1 sandbox+proxy makes the hosts.yml rewrite unnecessary.
- 2026-05-25 — CP3 open-question answer (PLAN CP3 asked "Is the 5-minute write TTL achievable, or does GitHub round up to 1 hour minimum?"). Empirical answer: **GitHub returns ~1h on installation-token mints regardless of any requested expiration**. The `POST /app/installations/{id}/access_tokens` endpoint has no `expires_in` parameter — TTL is fixed at 1h. Confirmed against the real API during CP3 e2e (write-tier mint logged `ttl=1h0m0s`). Resolution: the design's 5-minute write TTL goal is not enforceable at the GitHub API layer. The "single-use, JIT" property is enforced *broker-side* (no caching of write tokens; mint per push). Phase 1 may need broker-side revocation timers (call `DELETE /installation/token` after the push completes) to approximate the 5-minute effective window.

## Tooling requests

(If you find this VM is missing something, propose changes here rather than installing system packages silently. Format: date — what's needed — why.)

---

## Out of scope for Phase 0 (do not implement, even if tempted)

- Daemon mode (Checkpoint 6's `rein run` launches the broker in-process; daemon split is Phase 1).
- Audit comment writeback (Phase 1).
- Commit signing (Phase 1).
- srt sandbox composition (Phase 1).
- The full role catalog (only `implement` for Phase 0; other roles in Phase 1).
- Multi-issue sessions (Phase 1).
- Issue ID / URL redirect tracking for TM-G6 (Phase 1).
- The five-minute write-token revocation timer (Phase 1; for Phase 0 just rely on TTL expiry).
- Anything Sigstore-related.
- Anything that requires Apple Developer ID, codesigning, or notarization.
