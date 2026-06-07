# PLAN-0.5.md — Phase 0.5 (Onboarding & operator UX)

**Goal:** A new developer on a new Linux or macOS machine can go from
`git clone TomHennen/rein` to a successful `claude` push against their
own GitHub App in under 10 minutes, with one `rein init` flow that
includes creating the GitHub App(s) via manifest flow. No silent
bypasses from skipped or podged setup steps.

**Time budget:** 11-15 hours, several sessions.

**Pre-phase:** Read `phase0_findings.md`. It anchors the design
corrections, Shape B limits, and open Phase 1 questions that Phase 0
produced. Phase 0.5 builds on those rather than re-deriving them.

## Status (last updated 2026-06-07)

| CP | Status | Commit | Notes |
|---|---|---|---|
| CP1 | **done** | `b0ac6e8` | `rein init` local scaffolding (dirs, shim install, ~/.local/bin symlink, env validation, real read-only mint check, session-file scaffold). |
| CP2 | **done** | `0a470e8` | `rein doctor` — 8 read-only checks; color/no-color/CLICOLOR_FORCE; rate-limit-aware mint hint. |
| CP3 | **done** | `d78a097` | shell-rc alias (bash/zsh/fish) with managed BEGIN/END block, foreign-alias guard, duplicate-block self-heal, fish autoload location. Unit-tested. |
| CP4 | **done** | (this commit) | Manifest-flow DESIGN doc at `docs/init-manifest-design.md`; companion research at `docs/rein-manifest-flow-research.md`. Reviewer pass complete (5 should-fix items applied). Ready for CP5. |
| CP5 | **done** | `5243bd7` | Manifest flow in `internal/appsetup`; `internal/keystore` (FileKeystore + SingleFileKeystore, uid+mode checks); mint paths refactored onto Keystore (CLAUDE.md hard-constraint #6); `--owner`/`--skip-audit`/`--force`/`--port` flags; env-var bridge; headless `ssh -L` auto-hint. `cmd/spike-token` removed. **Gate passed (2026-06-06):** fresh `rein init` → both Apps created, PEMs 0600, `state.json` audit_done, fingerprint matches GitHub UI, `rein doctor` green, real mint → `git ls-remote` succeeded (classic 40-char token; opaque handling fine). Manual/no-forwarding fallback deferred (#19). |
| CP6 | **done** | (this commit) | macOS proc-tree fallback via build-tagged `proctree_{linux,darwin,other}.go`. Linux unchanged; darwin uses `ps -ax`-snapshot walk (no cgo); other platforms get a no-op stub so cross-compile stays green. **Tom needs to run macOS e2e to fully close GitHub issue #8.** |
| Turnkey | **done** | `6c31f7b` | Post-CP5 follow-on (pulled the Stage-2 install-poll forward at owner's direction — it's core to the value prop). `config.ResolveApp` read-only resolver (env → state.json+keystore → fail closed); `rein run` eagerly fetches + caches the installation id (`GET /repos/{owner}/{repo}/installation`, App-JWT) so a new user needs ZERO `REIN_APP_*` env vars. **Gate passed (2026-06-06):** with all `REIN_APP_*` unset, `rein run -- git ls-remote <repo>` succeeded purely from `state.json` + keystore + auto-fetched install-id (138434780 cached). Plus Shape B hardening: GH_TOKEN/GITHUB_TOKEN scrubbed from the wrapped child env; TM-G8 no-config path returns the placeholder + `rein doctor` diag (#7). |
| CP7 | **done** | `fead9f1` | `README.md` — clone-to-first-push onboarding for the zero-env-var turnkey flow: install, browser App creation, App install, required session binding, daily `claude` use, headless `ssh -L`, troubleshooting (`rein doctor` + log paths), honest Shape B limits, cleanup. Reviewer pass applied (session required; fish `command claude` bypass; HTTPS-remotes-only footgun; PATH-not-set; full cleanup). 150 lines. |

### Post-Phase-0.5 hardening (2026-06-07, all on `main`)

| Item | Commit | Notes |
|---|---|---|
| git-push write-token fix | `5bdd8dd` | Pre-existing Phase 0 bug: the helper revoked the write token on git's `store` (which fires mid-push) → every HTTPS `git push` 401'd. Now revoke on `erase` only; a pushed token lives to its native ~1h TTL. Tighter bounding via run-exit revoke → #20. |
| Run-scoped write approvals | `0a02043` | Approvals scoped to a `rein run` invocation (per-run nonce + files, run→session lookup for the popup / other-terminal grant, clear-on-exit, orphan sweep, fail-closed without `REIN_RUN_ID`). Concurrent sessions isolated. Gate met: live concurrent run + live exit-isolation + isolation unit tests. |
| `REIN_SESSION_FILE` fix | `0ffda7a` | An explicitly-set-but-missing session file now hard-errors instead of silently falling back to the env session. |
| design.md sweep | `a525f6d` | Phase 0/0.5 corrections folded into the design of record; approval/revoke text corrected to run-scoped + revoke-on-erase; Claude-Code-hooks consideration noted (#21). |

**Working repos:** Throwaway repos only (same as Phase 0). The init
flow will help the user point at their own throwaways during setup;
no Phase 0.5 step should touch a real repo.

## How to work this plan

1. Do checkpoints in order. Each builds on the previous.
2. At each checkpoint: implement, test, run, **stop and surface to
   human**, wait for verification. Same discipline as Phase 0.
3. Between checkpoints, run free — refactor, polish, add tests, but
   don't expand scope.
4. If something surprises you (design wrong, library doesn't behave,
   GitHub API behaves unexpectedly), stop and surface rather than
   working around it. Append to the "notes / blockers" section
   below as you go.
5. The `rein doctor` check from CP2 is the primary tool when
   something feels broken. Use it; trust its output.

## Checkpoint 1 — `rein init` basics + local scaffolding

**Estimate:** 1-2 hours.

**Goal:** A `rein init` subcommand that does the local-only pieces of
setup: state/config dirs, shim install, rein-on-PATH, env validation,
session scaffolding. Assumes the GitHub App is already created
(manifest flow comes in CP4-5).

**Implementation:**

- New subcommand `rein init` in `cmd/rein/main.go`.
- Idempotent: re-running is a no-op for already-done steps.
- Steps:
  1. Create `$XDG_STATE_HOME/rein/` and `$XDG_CONFIG_HOME/rein/`
     with 0700 perms.
  2. Call `installShim` (already implemented) to place
     `{git, gh, rein}` shims in the shim dir.
  3. Symlink `rein` → `~/.local/bin/rein` (on default PATH for most
     Linux/macOS distros via XDG conventions). Idempotent re-link
     if symlink already points where we expect.
  4. Validate `REIN_APP_CLIENT_ID`, `REIN_APP_PRIVATE_KEY_PATH`,
     `REIN_APP_INSTALLATION_ID`, `REIN_TEST_REPO_A` are set.
  5. Mint a real read-only token against the configured App to
     prove the credentials work. On failure, surface the underlying
     error (e.g., `401 Bad credentials` likely means the env vars
     don't match the actual App).
  6. If `~/.config/rein/dev-session.yaml` doesn't exist, scaffold
     a starter from `REIN_TEST_REPO_A` with `issue: 1` and a
     unique session ID. Don't overwrite an existing file.
  7. Print a "you're set up" summary: shim dir, session file, what
     to run next.

**Success criterion:** Running `rein init` on a freshly-cloned and
built rein checkout produces a working setup; subsequent `rein run
-- bash -c 'git config --get-all credential.https://github.com.helper'`
shows rein's helper.

**What to skip:** App manifest flow (CP4-5), shell-rc alias (CP3),
doctor command (CP2).

**Output to human:** "init basics work; here's what gets created on
disk and what gets validated; ready for CP2."

---

## Checkpoint 2 — `rein doctor`

**Estimate:** 1-2 hours.

**Goal:** A diagnostic command that surfaces "why isn't this working"
in seconds. Replaces the manual log-spelunking + path-checking dance
that consumed time during CP6 e2e.

**Implementation:**

- New subcommand `rein doctor` in `cmd/rein/main.go`.
- Run a sequence of checks, each printing green/yellow/red status with
  a one-line explanation. Don't abort on failures — run them all,
  summarize at the end.
- Checks:
  1. **rein binary on PATH** — is `which rein` something that exists
     and matches the running binary's path?
  2. **Shim binaries fresh** — compare `bin/{rein,rein-git,rein-gh}`
     mtimes to `~/.local/state/rein/shim/{rein,git,gh}` mtimes;
     warn if shims are older.
  3. **App private key readable** — `os.Stat` + mode check on
     `$REIN_APP_PRIVATE_KEY_PATH`.
  4. **App credentials valid** — mint a real read-only token; report
     success / 401 / other.
  5. **Session file loads** — `session.LoadOrFallback` succeeds;
     report the source (file vs env-fallback) and the repos.
  6. **`$TMUX` propagates to a helper probe** — spawn a tiny child
     process that calls into the broker helper code, have it log
     whether `$TMUX` was present in its env. Catches the
     "podged-env-no-tmux" case that bit CP6.
  7. **Approval cache state** — read `approvals.Path`; show valid /
     expired / absent.
  8. **gh-shim cache state** — same for `ghsession.ReadCachePath`.
- Exit 0 if all green; 1 if any red.

**Success criterion:** Running `rein doctor` on a known-good setup
shows all green; deliberately breaking one thing (e.g., revoking the
App's installation) flips that check to red with a clear error.

**What to skip:** Anything that requires running an actual git/gh op
(those are real ops; doctor should be a passive check).

**Output to human:** "doctor surfaces these N conditions; pretty
output looks like this; here's a deliberate-break demo."

---

## Checkpoint 3 — Shell integration (alias claude default)

**Estimate:** 1 hour.

**Goal:** `rein init` writes `alias claude='rein run -- claude'` to the
user's shell rc by default. Removes the "remember to type `rein run --`"
step that's been a real papercut.

**Implementation:**

- In `rein init`, after the local scaffolding, add a shell-integration
  step.
- Detect shell from `$SHELL` (or fall back to `~/.bashrc` if unknown).
  Support bash, zsh, fish.
- Append `alias claude='rein run -- claude'` (and equivalent for fish)
  to the appropriate rc file, surrounded by `# BEGIN rein` /
  `# END rein` markers so future re-runs can update in place rather
  than appending duplicates.
- Document in stderr: "alias added. Type `\claude` (with backslash) to
  bypass for one invocation."
- `--no-alias` flag to opt out.
- `--shell=fish` to override detection.

**Success criterion:** After `rein init` on a fresh shell rc, opening
a new shell and typing `claude` invokes `rein run -- claude`; typing
`\claude` invokes plain claude.

**What to skip:** Aliases for anything other than claude (e.g.,
cursor/codex come in Phase 1 when we know what they need).

**Output to human:** "alias added to <rc-file>; verified the
`\claude` escape works; opt-out flag tested."

---

## Checkpoint 4 — App manifest flow DESIGN

**Estimate:** 1-2 hours.

**Goal:** Design the GitHub App manifest flow before implementing.
Stop-and-surface gate; same discipline as Phase 0.

**Deliverable:** A short design doc at `docs/init-manifest-design.md`
(~2 pages) covering:

- **Manifest schema for both Apps in one init flow.** Primary App
  permissions per design §4.2.2 (`scan`/`triage`/`implement`/`review`/
  `release` roles' union, or the safest minimum we can get away with
  for v1). Audit App permissions: just `issues: write` per design
  §4.2.4.
- **Callback server lifecycle.** Pick a free port; bind 127.0.0.1
  only; close after the callback completes or times out.
- **State nonce.** GitHub's manifest flow includes a `state`
  parameter for CSRF protection; generate a fresh random nonce
  per init.
- **Error handling.** Timeout if user doesn't complete in N minutes;
  user closes browser; manifest rejected by GitHub; private key
  download fails. Each path with concrete recovery.
- **Key storage timing.** When does the private key hit disk? With
  what perms? What if the write fails after GitHub has created the
  App?
- **Repo install deep-link.** After Apps are created, generate the
  URL `https://github.com/apps/<slug>/installations/new` so the user
  can install on chosen repos with one click each.
- **Two browser-open events.** Per Tom's direction: do both Apps in
  one `rein init` invocation, but it's acceptable to open the
  browser twice (one per App). Guide the user with clear "first the
  primary App, then the audit App" copy.
- **Security considerations.** The callback server is a brief
  privilege-elevated path; what could a malicious local process do
  to it during the short window?

**Process:** Write the design alone first. Then run a focused
review subagent against it (same pattern as Phase 0 reviewer
passes). Only escalate to Tom if the subagent surfaces a hard
tradeoff or open question. Otherwise proceed to CP5.

**Success criterion:** Design doc exists, reviewer subagent
approves, no blocking questions surfaced.

**Output to human:** Either "design at `docs/init-manifest-design.md`,
reviewer approved, proceeding to CP5" or "open questions: [list];
need your direction before CP5."

---

## Checkpoint 5 — App manifest flow IMPLEMENTATION

**Estimate:** 3-4 hours.

**Goal:** Build the manifest flow per CP4's design. Wire into `rein
init` as the first-run experience (when App env vars are absent).

**Implementation:**

- Manifest flow lives in `internal/appsetup/` (new package).
- `rein init`'s flow becomes:
  1. If App env vars already set AND `app.pem` exists AND mint
     succeeds → skip manifest flow, go to local scaffolding (CP1).
  2. Else → run manifest flow for primary App, then for audit App.
  3. Then local scaffolding.
  4. Then offer alias setup (CP3).
- Manifest flow steps:
  1. Generate state nonce + manifest JSON
  2. Bind callback server on `127.0.0.1:<free-port>`
  3. Print URL to user; open browser via `xdg-open` /
     `open` / `start` per platform
  4. Wait for GitHub's POST callback (with timeout)
  5. Receive `code` + `state`; validate state
  6. Exchange code → App info via
     `POST /app-manifests/{code}/conversions`
  7. Write private key to
     `~/.config/rein-credentials/app-<role>.pem` (0600)
  8. Write App ID + Client ID + Installation ID to
     `~/.config/rein/env` (sourceable)
  9. Print deep-link to install URL; wait for user confirmation
     that they've installed on the repos they want
- Both Apps run sequentially in one `rein init`. Clear copy: "step
  1/2: primary App"; "step 2/2: audit App."
- Stretch: `rein init --skip-audit` for users who don't want the audit
  App (Phase 0.5 doesn't use it yet).

**Success criterion:** A fresh checkout + `rein init` produces a
working primary + audit App, both with private keys on disk, env
vars saved; subsequent `rein doctor` shows all green; subsequent
`rein run -- bash -c 'git push'` to a throwaway repo succeeds with
human approval.

**Open questions to surface:**
- Does the manifest flow work for users behind corporate proxies
  (where `open browser` might not work)? Fallback to "paste this URL
  in your browser" is straightforward.
- Are there organizations that disable App creation by individual
  users? If so, the manifest flow returns an error; we surface and
  document.

**Output to human:** Either "manifest flow works; both Apps created
+ saved + installed; full setup-to-push e2e under 10 min" or
"hit [X]; need direction."

---

## Checkpoint 6 — macOS proc-tree fallback

**Estimate:** 2-3 hours.

**Goal:** Close issue #8. The Linux-only `/proc/<pid>/{cmdline,status}`
walk in `detectFromProcTree` (cmd/rein/main.go) doesn't work on macOS.
Without this, macOS users without the rein-git shim in PATH get a
silent "default to read" instead of the write-detection that Linux
users get.

**Implementation:**

- Build tags: split `cmd/rein/proctree_linux.go` and
  `cmd/rein/proctree_darwin.go`. Common interface in `cmd/rein/proctree.go`.
- Linux implementation: existing `/proc` walk, unchanged.
- macOS implementation: shell out to `ps -o pid,ppid,args` for the
  ancestor chain (avoids cgo, no libproc dependency). Walk the table
  same as the Linux path.
- `unsupported_other.go` build-tagged stub for any other OS, returns
  false.

**Success criterion:** On macOS, running `git push` to a throwaway
repo without the shim in PATH still triggers the write-tier mint
(via process-tree detection). Test by an actual macOS run by the
user, since CI doesn't run macOS in this project yet.

**What to skip:** Windows support. (Different platform, different
process-tree mechanisms, not on the Phase 0.5 priority list.)

**Output to human:** "Linux unchanged; macOS path added; macOS test
run shows write detection works via proc-tree fallback."

---

## Checkpoint 7 — README onboarding walkthrough

**Estimate:** 1 hour.

**Goal:** A real `README.md` (or `docs/onboarding.md`) that takes a
new user from "git cloned the repo" to "claude pushed a commit"
without asking us anything.

**Implementation:**

- Single doc, top-to-bottom, no branching narrative.
- Sections:
  1. **What rein is** (one paragraph; link to design.md for depth)
  2. **Prerequisites** (Go 1.22+, gh CLI for testing, GitHub account
     with permission to create Apps, two throwaway repos)
  3. **Install** (`go build ./...`, `./bin/rein init`)
  4. **Initial setup walkthrough** (the manifest flow's user
     experience, what they'll click, what they'll see)
  5. **Daily use** (just type `claude`; the alias does the rest)
  6. **Troubleshooting** (run `rein doctor`; check `helper.log` /
     `gh-shim.log` locations; common issues like "not in tmux so
     popup didn't fire")
  7. **Phase 0.5 known limits** (link to `phase0_findings.md`'s
     Shape B section)
- Keep it under 250 lines. If it's longer, refactor into multiple
  docs.

**Success criterion:** A new developer can complete the install →
push flow following the README alone, without our help. Test by
having Tom (or anyone) try a "fresh-checkout, follow the README,
no asking me questions" run.

**Output to human:** "README at <path>; tried walking through it
fresh; here's the time-to-first-push."

---

## After Phase 0.5 — CLOSED (2026-06-07)  ←— RESUME HERE

Phase 0.5 is complete and validated. The install flow Just Works
(turnkey, zero `REIN_APP_*` env vars; CP5 + Turnkey gates passed live).
The design sweep into `docs/design.md` landed (`a525f6d`). Plus the
post-0.5 hardening above (revoke-on-erase, run-scoped approvals).

**Next session picks up at Phase 1:** sandbox composition via `srt` +
daemon/proxy split + audit App writeback + commit signing (broker-as-CA).
See `docs/design.md` §7.2.

**Open followups (do alongside / before the relevant Phase 1 work):**

- **#20** — **DONE (gate passed 2026-06-07).** Revoke write tokens on
  `rein run` child-exit. Per-run append-only ledger `writes/<run-id>.jsonl`
  (helper appends each minted write token via a new
  `broker.Config.RecordWrite` hook, guarded on `REIN_RUN_ID`); `rein run`
  best-effort revokes still-valid entries on child-exit, clears the ledger;
  `approvals.List`/`ClearRun`/`Sweep` extended to cover the ledger (orphan
  reap uses the ledger mtime so a live no-bound-issue run isn't clobbered).
  Unit + race + concurrency tests pass; design.md C6 swept. **Gate met:**
  `/tmp/issue20-manual-test.sh` on a throwaway repo showed the minted write
  token live mid-run (200) and revoked-dead after `rein run` exit (401);
  ledger cleared on exit. Residual (accepted): SIGINT/SIGKILL of `rein run`
  skips the exit-revoke → those tokens live to native ~1h TTL. Keychain-
  backed token storage was considered and declined (the OS keyring is
  reachable by any same-uid process, so it doesn't address #7; at-rest
  protection is the Phase 1 in-memory daemon's job, not this 0.5 bridge).
- **#19** — headless GitHub App creation fallback (URL-param prefill +
  `rein import-pem`) for boxes where `ssh -L` isn't possible.
- **#12** — nonce-via-tty so an agent can't self-answer the approval
  prompt (Shape B TM-G5 hardening; the sandbox is the real fix).
- **#7** — Shape B token reachability (env/files/proc); closed by the
  Phase 1 sandbox, tracked there.
- **#21** — Claude Code hooks as a complementary guard/audit layer.
- **#8** — macOS proc-tree e2e (needs a real Mac).

**Environmental (not code, but it bit us):** this dev VM's clock is
erratic (drifted >60s ahead → App-JWT mints 401 "Bad credentials"; later
~13min behind → JWT expired). Ensure NTP is solid before any GitHub App
work, or mints intermittently fail in ways that look like auth bugs.

## Notes / blockers / design corrections needed

(Append entries here as you work. Format: date — issue — proposed resolution.)

- 2026-05-25 — CP2 step 6 "$TMUX child probe" deliberately simplified to a
  direct env check in `checkTmuxEnv` (`cmd/rein/doctor.go`). The spec's
  literal child-probe would catch a future regression where doctor itself
  scrubs env before spawning children, but nothing in `cmd/rein` does
  that today and Go's `exec.Command` inherits env automatically. The
  real concern from phase0_findings — `$TMUX` unset at `rein run` launch
  time — is what the env check actually surfaces, since doctor sees the
  same env the user's shell would pass to `rein run`. If env-scrubbing
  ever lands in doctor or the wrapper, swap in a 10-line child probe.

- 2026-05-25 — CP3 used `# BEGIN rein-credentials managed block` / `# END
  rein-credentials managed block` markers instead of the PLAN's
  illustrative `# BEGIN rein` / `# END rein`. The longer form is
  collision-safer for shared rc files; no behavior change.

- 2026-05-25 — CP3 writes the fish alias to
  `$XDG_CONFIG_HOME/fish/functions/claude.fish` (the fish autoload
  location, one function per file) rather than `config.fish`. This
  integrates with fish's `functions --erase` / `funced` UX and avoids
  re-evaluating the function on every shell startup. BEGIN/END markers
  remain inside the file so re-runs can recognize it as rein's vs a
  user-authored function.

- 2026-05-26 — CP4 design ratified by reviewer pass. Five should-fix
  items were applied to the design doc before this commit: parent-dir
  fsync after rename (closes the durability window the original draft
  named but didn't fully close); UI-only PEM-write-failure recovery
  (no longer points at a `rein import-pem` command that doesn't ship
  in CP5 — that's a Stage 2 polish followup tracked in §Out-of-scope);
  explicit env-var bridge state-transition table (six rows, covers
  Phase 0 dev path and post-manifest steady state); explicit
  `--skip-audit` + `--resume` semantics (skipped audit leaves
  `phase: primary_done`, can be added later without `--force`); App-
  name entropy bumped 24→40 bits.

- 2026-05-26 — CP6 darwin proc-tree `ps` shell-out hardened: pinned
  `LC_ALL=C` and `PATH=/bin:/usr/bin`, explicit `cmd.Stdin = nil`.
  Defense-in-depth (the call runs inside the credential helper); also
  immunizes the column-parser against locale-dependent `ps` output
  variations. The Linux helper-log line for a positive proctree match
  now appends `(platform=linux)` — minor format change worth flagging
  if anyone scrapes the helper log.

- 2026-05-26 — `docs/rein-manifest-flow-research.md` checked in as the
  empirical companion to CP4's `docs/init-manifest-design.md`. It is
  long-form context (818 lines) sourced from a deep-research pass;
  the design doc references its section numbers for anything the
  design itself doesn't restate.

- 2026-05-30 — CP5 added an undocumented `assertNoOrphanPEM` safety
  guard in `internal/appsetup/flow.go`: when state.json has no record
  for a role but a PEM exists in the keystore, refuse to create a new
  App (otherwise we'd orphan the prior App at GitHub since there's no
  delete API). The guard isn't in `docs/init-manifest-design.md` —
  worth a sentence in any future revision. Initially, the guard
  contradicted the design's PEM-write-failure recovery (which tells
  the user to place the PEM and re-run `--resume`); fixed in second
  CP5 reviewer pass by persisting a partial `AppRecord` to state.json
  on `Keystore.Set` failure (init-manifest-design.md §138 assumed
  exactly this — "the partial state in `state.json` makes it
  discoverable"). On `--resume`, the partial record lets the flow
  skip the role's step and adopt the user-placed PEM without tripping
  the orphan guard.

- 2026-06-06 — Headless/remote gap in the manifest flow surfaced while
  smoke-testing CP5 in the (headless) dev VM. The loopback callback
  needs a browser that can reach `127.0.0.1:<port>` on the rein host;
  on an SSH-only box there is none. Researched against GitHub docs: the
  manifest flow cannot be made callback-free (the `code` is delivered
  only via redirect, never shown on a GitHub page). Resolution: (1)
  SSH `-L` port-forward is the preferred remote path and keeps the
  automated, safe PEM import; (2) a URL-parameter-prefill + manual
  key-import fallback is the last resort, with the same-UID-boundary
  security analysis + safe-handling rules now written into
  `docs/init-manifest-design.md` (new "Safe handling of the App private
  key" + "Headless / remote fallback" sections). The fallback is NOT
  built in CP5 (overlaps the Stage 2 `rein import-pem` deferral);
  tracked as a followup issue. The "copy the code from a failed-load
  address bar" trick was evaluated and rejected (undefined behavior).
  CP7 onboarding must surface the safe-PEM-handling rules verbatim.

- 2026-06-07 — Pre-existing **revoke-on-store** bug found via the first
  real HTTPS `git push` e2e through the broker: git calls the helper's
  `store` mid-push (after info/refs, before receive-pack), so revoking
  there killed the write token and every push 401'd. Phase 0 had only
  ever validated writes via `rein-gh`, never a raw `git push`, so it was
  never caught. Fixed to revoke-on-`erase` (`5bdd8dd`); residual: a
  pushed token now lives to ~1h native TTL until #20 (run-exit revoke).

- 2026-06-07 — **Approval model** changed global-4h → run-scoped
  (`0a02043`). The single global approval.json let a second concurrent
  run reuse/clobber the first's approval and lingered after exit. Now
  per-run files keyed by `REIN_RUN_ID`, cleared on exit, swept if
  orphaned. Driving the live test myself was blocked by the `/dev/tty`
  approval gate (TM-G5 working as intended — an agent can't self-answer);
  human did the interactive part, automated tests + a pty-driven
  exit-isolation run covered the rest.

- 2026-06-07 — **Clock-skew failure mode** (environmental, not rein): a
  VM clock >~60s ahead of GitHub makes App-JWT mints 401 "Bad
  credentials" (iat in GitHub's future); far behind expires the JWT.
  Presents as an auth/App-deleted failure; it's the clock. This VM
  drifts erratically — keep NTP solid. A `rein doctor` clock-skew check
  was considered and dropped as gold-plating.

- 2026-06-07 — `REIN_SESSION_FILE` pointing at a missing file silently
  fell back to the env session (looked like the chosen session was
  active when it wasn't); now a hard error (`0ffda7a`).

- 2026-06-07 — **#20 exit-revoke implemented** (first Phase 1 followup;
  uncommitted, pending human manual e2e). Design notes: (1) persisting the
  write-token VALUE to disk is unavoidable — GitHub's revoke is
  `DELETE /installation/token` authenticated BY the token, no revoke-by-id,
  so the revoking process (`rein run`, a different process from the
  short-lived helper) must hold the value. Bounded: 0600, deleted on exit,
  adds nothing a same-uid process couldn't already reach during the run
  (#7). (2) The ledger is a THIRD per-run artifact alongside the approval
  files; an append-only `.jsonl` with `O_APPEND` so concurrent in-run
  pushes don't corrupt it. (3) Reviewer-surfaced + fixed: a "writes-only"
  run (write-capable role with `issue: 0` → `ConfirmWrite` nil → no
  run-context written) has no pid/timestamp for the orphan sweep, so a
  concurrent launch's `Sweep` would insta-reap its live ledger and skip the
  exit-revoke (degrading to the ~1h floor — not a leak, not a TM-G8 break).
  Fixed by judging such runs against the ledger mtime + the 24h backstop.
  Residual (accepted): SIGINT/SIGKILL of `rein run` skips the exit-revoke;
  those tokens live to native ~1h TTL — the same floor as before #20.

## Tooling requests

(If you find this VM is missing something for Phase 0.5 specifically,
propose changes here. Format: date — what's needed — why.)

---

## Out of scope for Phase 0.5 (do not implement, even if tempted)

- Daemon / broker split (Phase 1).
- Audit comment writeback (Phase 1; the audit App created in CP4-5
  is registered but unused in Phase 0.5).
- Commit signing (broker-as-CA per design §4.2.6) — Phase 1.
- srt sandbox composition (Phase 1; the entire architectural
  defense surface for Shape B's TM-G5 weakness).
- Status app / OS notification channel (Phase 1).
- Mint rate-limit handling (Phase 1; track here in findings but
  defer fixes).
- Windows support (later phase if there's demand).
- Multi-issue sessions (Phase 1+; CP4+ correctness work tracked in
  issue #10).
- Per-call API approval (Phase 1 proxy).
- Public Sigstore integration (Phase 2+ per design §11.6).
- Anything requiring Apple Developer ID / codesigning / notarization
  (out of Phase 0.5 scope; macOS support here is "build runs" not
  "distribution-quality binary").
