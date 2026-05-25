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

## After Phase 0.5

Phase 0.5 is complete. Decision point for the human:

- Did the install flow Just Work? If yes, Phase 0.5 closes; Phase 1
  starts (sandbox composition + daemon + audit App + proxy).
- Did anything surprise you? Update phase0_findings.md (or open a
  phase0.5_findings.md) before Phase 1.
- Are there design corrections needed (things the design got wrong
  about init / manifest / cross-platform)? Surface clearly with
  proposed fixes.

Phase 0.5 is also when to sweep Phase 0 design corrections into
`docs/design.md`. That's a separate housekeeping pass, not a
checkpoint, but it should land before Phase 1 starts.

## Notes / blockers / design corrections needed

(Append entries here as you work. Format: date — issue — proposed resolution.)

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
