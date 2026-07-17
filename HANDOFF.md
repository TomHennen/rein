# HANDOFF — continuing Phase 1 on another machine

You are an agent picking up Phase 1 work from a `git clone` on a fresh
machine, and you need to **run and test live** (mint tokens, push to a
throwaway repo, run `srt`). This doc gets you from a bare clone to a
runnable machine, then points you at where the work stands.

No secrets are transferred: you create your **own** GitHub App via `rein
init` (the manifest flow), so nothing from the origin machine needs to move
except this repo.

> **Resume pointer:** the full sandboxed-mode spine **and** the first two
> interactive-`init` checkpoints are **merged to `main`** — work from
> `main`, not a feature branch. Read `PLAN-1.md` (the §"Notes" tail is the
> live state), `docs/phase1-design.md` (design of record), and §4 below for
> what's done and what's next. Active branch: **`main`**.

> **⚠️ Do NOT `source ./dev-env`.** It hardcodes the *origin author's* now-dead
> GitHub App (`REIN_APP_CLIENT_ID`/`_ID`/`_INSTALLATION_ID`), a key path that
> won't exist on your box, and throwaway repos that don't exist. Worse, those
> `REIN_APP_*` vars **override** the App that `rein init` provisions for you, so
> sourcing it makes `rein doctor` fail on `app key` / `mint failed`. On a fresh
> machine, rely entirely on `rein init` + the managed keystore (`state.json` +
> `~/.config/rein/*.pem`); **no `REIN_APP_*` env vars are needed or wanted.**
> (`dev-env` is legacy Phase-0 scaffolding; cleaning it up is tracked with the
> broader `REIN_TEST_REPO_A` deprecation, #40.)

## 0. What's where

- **In git (you have it):** all design/plan docs, the broker code, the CP1
  relay recipe (`docs/phase1-srt-spike-findings.md` → "CP1 results").
- **NOT in git (you set up locally):** your GitHub App (via `rein init`),
  the env prerequisites below. The CP1 spike binary lived in `/tmp` on the
  origin machine and is gone — it is fully reconstructable from the recipe,
  but CP2 productizes it into the daemon anyway, so you won't need it.

## 1. Environment bring-up (Linux)

Arch note: the origin machine is **aarch64** (Apple VZ guest). Commands
below are arch-agnostic except where noted.

### 1a. System deps for `srt`

`srt` (Anthropic's sandbox-runtime) needs `bubblewrap`, `ripgrep`, `socat`,
and Node. Install directly (NOTE: a stale `cli.github.com` apt repo key can
make `apt-get update` fail and break `&&` chains — install without gating
on `update`, or fix the key first):

```
sudo apt-get install -y bubblewrap ripgrep socat
# Node 20+ if absent (nodesource or distro). Then srt:
npm install -g @anthropic-ai/sandbox-runtime   # pin the version; see below
```

**Pin srt.** Current pin is `@anthropic-ai/sandbox-runtime@0.0.63` (bumped
from 0.0.54 on 2026-07-05; CP3 builds + re-verifies against it). Its
mitm/proxy hooks are undocumented and may move across versions — install
the pinned version and re-verify if you bump (`docs/phase1-srt-spike-findings.md`).

### 1b. AppArmor profile for bwrap (Ubuntu 24.04+ ONLY)

Ubuntu 24.04+ sets `kernel.apparmor_restrict_unprivileged_userns=1`, which
strips capabilities from unprivileged user namespaces, so `bwrap` fails
(`setting up uid map: Permission denied`). Check:

```
bwrap --unshare-user --uid 0 --bind / / -- true   # if this errors, you need the profile
```

Fix surgically (grants `userns` to bwrap only — do NOT disable the sysctl
system-wide; that weakens the whole box). Create `/etc/apparmor.d/bwrap`:

```
abi <abi/4.0>,
include <tunables/global>

profile bwrap /usr/bin/bwrap flags=(unconfined) {
  userns,
  include if exists <local/bwrap>
}
```

Then: `sudo apparmor_parser -r /etc/apparmor.d/bwrap`. Re-run the bwrap
check above; it should now succeed with the sysctl still on. (Non-Ubuntu
distros usually don't need this; some ship a setuid bwrap that sidesteps
it. macOS uses `sandbox-exec`, no bwrap — but macOS is deferred, see
design §5.4.)

### 1c. Clock / NTP (critical — or GitHub App mints 401)

App-JWT mints fail `401 Bad credentials` when the clock is >~60s off
GitHub's; on VMs this is a recurring trap (#22, #23). Ensure NTP is
healthy and STAYS on:

```
sudo apt-get install -y chrony && sudo chronyc makestep && chronyc tracking
```

`chronyc tracking` should show `System time : ~0 seconds`. Do NOT leave NTP
disabled. Sanity check vs GitHub:

```
date -u; curl -sI https://api.github.com | grep -i '^date:'   # should be within seconds
```

### 1d. Go + build

Go 1.26.5+ (`go.mod`; the directive was bumped to 1.26.5 on 2026-07-09 to
clear two stdlib CVEs — #41). Build the binaries — **use `-o bin/`**; a bare
`go build ./...` compiles to cache and produces NO `./bin/rein`, so every
command below would fail:

```
go build -o bin/ ./...
go test ./...            # all green on a clean clone before you start
```

## 2. Create your own GitHub App + first mint

Follow `README.md` (the clone-to-first-push onboarding) — it covers
`rein init` (manifest flow: creates your primary + audit App in a browser,
installs them on a throwaway repo), then `rein doctor`. Summary:

```
./bin/rein init --repo <owner>/<your-throwaway-repo>   # App creation + scaffold a session
./bin/rein doctor                                       # checks; 'app key' must be [ok]
```

- `rein init` is now **interactive** (see README "First-time setup"): it
  scaffolds `~/.config/rein/dev-session.yaml` from `--repo owner/name` (or an
  interactive prompt), and on a real tty asks whether to add the `claude` alias
  (opt-in; or pass `--alias`). Headless/CI/`--yes` never blocks. It no longer
  reads `REIN_TEST_REPO_A` for scaffolding (#40) — pass `--repo`.
- Use a **throwaway repo you own** (hard-constraint #1 still holds). After
  `init`, **install the App on that repo** via the deep-link `init` prints, or
  `doctor` will note `install-id not cached` and the first `rein run` will fetch
  it.
- The scaffolded session is **repo-only**: reads work immediately; writes are
  agent-declared per run (#35, built): no file edit at all — from inside the
  wrapped run, `rein declare <n>` (a REAL issue on the repo), approve the Form A
  prompt on your terminal, then push to `agent/<n>/<nonce>` (sandboxed mode
  enforces that convention; direct mode cannot see refs — documented delta).
  A leftover `issue:` field is IGNORED with a loud warning.
- init now also **soft-blocks on an unhealthy sandbox**: it finishes but prints
  a loud per-check warning; `--require-sandbox` makes it hard-fail. Enforcement
  is still at `rein run` (fails closed).
- If `doctor` shows `app credentials: 401`, it's almost always the clock
  (§1c), not the App. If it shows `app key`/`mint failed`, check you didn't
  `source ./dev-env` (see the warning at the top).

## 3. Verify you can do what CP1 proved

Before continuing CP2, confirm the machine can mint + push, reproducing the
CP1 result (`docs/phase1-srt-spike-findings.md` → "CP1 results"):

- Write a minimal session to `/tmp/cp1-session.yaml`:

  ```yaml
  id: cp1-check
  role: implement
  repos:
    - <owner>/<your-throwaway-repo>
  ```
- Read-tier mint with no new code (the helper serves reads without any
  ceremony):
  `printf 'protocol=https\nhost=github.com\npath=<owner>/<repo>.git\n\n' | REIN_SESSION_FILE=/tmp/cp1-session.yaml ./bin/rein credential-helper get`
  → the `password=` line is a READ token (a `rein-placeholder-*` value means
  config/mint trouble — run `rein doctor`).
- **Write mints are declaration-gated now (#35):** the helper returns the
  fail-closed placeholder for any write until an issue is declared+confirmed
  inside a `rein run` — there is deliberately no side-door write mint anymore.
  To verify the full write path, run `docs/35-manual-test.sh <real-issue>` (a
  guided live gate: declare, Form A confirm, verified push).

If that works, your machine is fully set up.

## 4. Where the spine stands — resume here

**The full sandboxed-mode spine (CP1–CP4.5) is MERGED to `main`** (PR #34,
2026-07-08) and live-verified. On top of it, the **interactive-`init` onboarding
work has started** and two checkpoints are merged (CP1 #39, CP4.6 #42 — see
"Recent work" below). The remaining spine checkpoint is CP6 (dogfood), which
needs Tom's explicit go-ahead.
CP1 proved `git push` through a MITM. CP2 landed the **proxy arm**
(`internal/proxy` — TLS-terminating injecting MITM on a per-run unix socket —
+ `internal/runbroker`, the in-process per-run host) on the CP2 foundation
(`internal/brokercore`, `internal/classify`, and `internal/daemon` — the last
**unwired shelf code**). CP3 landed **srt composition**: `internal/srt` (typed
0.0.63 settings, strict env allowlist, system+rein CA bundle, preflight, the
two fail-open self-tests) + `cmd/rein/run_sandboxed.go` (`rein run --sandbox`).
All reviewed (code + security) and **live-verified against real srt 0.0.63 +
real github.com**: CP2 via `REIN_LIVE=1 go test ./internal/proxy -run Live`;
CP3 via `REIN_SANDBOX_E2E=1 go test ./internal/srt -run E2E` (self-tests) and a
supervisor live `rein run --sandbox` gate (injection, scope-ceiling 403, cred
hiding incl. XDG-relocated stores, env scrub, audit redaction).

**Architecture note (don't re-derive):** the spine is **in-process per run,
NOT a resident daemon** (Tom's decision, 2026-07-05). No control socket, no
approval relay; `internal/daemon` is shelf code for later tracks. Write
approval is **run-scoped** (approve once per run, covers the session's whole
repo set until token expiry). srt pin is **0.0.63** (bumped from 0.0.54). The
`--sandbox` flag is CP3 opt-in; CP4 makes sandboxed the default where srt is
healthy. Details in `PLAN-1.md` Notes (2026-07-05) + the correction banner atop
`docs/phase1-design.md`.

CP4 added **session & approval integration**: git author identity
(`internal/gitidentity` — sandboxed commits author as "<name> (via rein)" + the
App-bot noreply email, not the developer; `~/.gitconfig` leak closed), session
expiry (`internal/runbroker/expiry.go` — idle 30m / hard TTL 4h, revoke +
proxy-close on expiry), the **default-mode flip** (`rein run` sandboxes by
default; `--direct`/`--no-sandbox` for direct behind a loud banner; fail closed
if srt unhealthy), and the approval-non-replayability verification (srt
`--new-session` severs the controlling tty → in-sandbox /dev/tty unopenable, a
per-launch self-test enforces it; #32 downgraded). Reviewed + supervisor
live-gate passed (bare `rein run` sandboxes; a real commit authored as the
delegated identity; /dev/tty ENXIO in-sandbox; `--direct` banner).

### Recent work since the spine merged (2026-07-08/09)

Post-#34, work moved to **interactive onboarding** plus fixes. All merged to
`main`:

- **`init` CP1 (#39):** `rein init` scaffolds a **repo-only** `dev-session.yaml`
  from `--repo`/a prompt with a mandatory non-interactive fallback (never blocks
  headless). No more "create the session by hand" wall.
- **`init` CP4.6 (#42):** sandbox-health **soft-block** (loud warning + fixes,
  exit 0; `--require-sandbox` to hard-fail) and the **opt-in `claude` alias**
  (default off; `--alias`, or a tty `[y/N]` prompt; `--no-alias` wins).
- **Grant TUI fix (#37, closes #36):** the write-approval prompt now prefers the
  **tmux popup** over inline `/dev/tty` when `$TMUX` is set (default), so it no
  longer corrupts a full-screen agent TUI. `REIN_APPROVAL=tty|popup` overrides.
- **Stdlib CVE bump (#41):** `go` directive → 1.26.5.

**Design-conformance audit is RUNNING in a separate session** (fan-out vs.
`design.md`). Expect it to file divergence issues + generated tests; fold them
into the plan when it reports. One divergence is already tracked:

- **#35 — issue scoping.** design.md wants the issue **agent-declared at runtime**
  (`agent/{{issue}}/{{nonce}}` push ref) + **human-confirmed** (`type_issue_number`);
  Phase 1 shipped a static `sess.Issue`. Decisions A–F are recorded on #35 (A:
  follow the design; onboarding `init` deliberately does NOT prompt for an issue).
  This is the biggest open design follow-up.
- **#40 — deprecate `REIN_TEST_REPO_A`** from production paths (it still drives
  the env-fallback session in `cmd/rein/*` + `internal/config`). A test var must
  not drive production scope. CP1 already removed it from `init` scaffolding.

### What's next (pick with Tom)

1. **More `init` slices** (`docs/onboarding-ux-design.md`, "CP4.7+"): machine-label
   App naming (decided: prompt pre-filled with the detected hostname, editable —
   §4), install-on-repo (§5), `doctor --fix` remediation (no-privilege tier only
   for v1 — §6). Same subagent-implement + independent-review flow used for
   CP1/CP4.6.
2. **#35 agent-declared issue** — the real runtime change (proxy extracts the
   issue from the push ref; approval prompt shows the issue title/repo). Security-
   sensitive; see the A–F decisions on #35 first.
3. **CP6 — dogfood** (needs Tom's explicit go-ahead; no CP5 on the Linux spine —
   macOS is the off-spine CP5 track). GATE: `wrangle` is the FIRST real-repo use;
   throwaway-only hard-constraint #1 has held since Phase 0, and crossing it is
   Tom's conscious decision. Plan (PLAN-1 CP6): sandboxed mode on a throwaway for
   a few sessions, then on `wrangle`, testing the design.md §7.2 hypothesis. Before
   dogfood: durable VM time-sync (#23) and re-verify the srt pin.

**The write path needs a tty — but NOT a human.** (This used to say "needs a human
tty"; that was stale and it cost us: agents kept parking write-path verification
for Tom.) The pexpect suite in `tests/interactive/` hands `rein` a real pty and
**is** the human stand-in — it answers the Form A prompt just as a developer
would. So an agent can and **should** self-verify the whole ceremony, autonomously:

Setup is the **`rein init` world**, not `source ./dev-env` — dev-env is the dead-App
footgun this HANDOFF's top banner warns about. `rein init` configures the App +
a dev-session; a journey resolves its throwaway with `resolve_throwaway_repo`
(`REIN_JOURNEY_REPO` → the configured dev-session → `REIN_TEST_REPO_A` only as a
labeled legacy shortcut), so journeys don't depend on `REIN_TEST_REPO_A` special-
casing (#40).

```sh
# once per machine: rein init (see the env prereqs above); this box's legacy
# shortcut is `source ./dev-env`, but the documented path is init.
python3 -m tests.interactive.journeys.write_ceremony.journey   # the ceremony journey (creates + closes its own issue)

# the gated test_*.py take an issue via env (they don't self-create one):
gh issue create --repo <throwaway> --title "..." --body "..."   # declare FETCHES a real issue
REIN_ITEST_ISSUE=<n> REIN_ITEST_TITLE_ISSUE=<n> REIN_ITEST_TITLE_WORD=<word-in-title> \
  tests/interactive/run.sh                              # write_approval + confirm_shows_title + init + realagent
```

The security model is untouched: the **sandboxed** agent has no tty at all and
still cannot self-approve; pexpect drives only the **host-side** prompt. The
`docs/cp*-manual-test.sh` scripts remain as human walkthroughs, but they are no
longer the *only* way to verify writes. A manual script is genuinely required for
exactly one thing: the **browser** flow (GitHub App *creation* via the manifest —
`scripts/cp5-manifest-manual-test.sh`).

Read `PLAN-1.md` Notes tail for the full live status + every design decision.

Three prior open questions are CLOSED (Tom, 2026-07-05; see PLAN-1 Notes):
stop-condition (b) → CONTINUE; srt-upstream issue → track locally, file
post-going-public; `--direct` → informational banner is final.

**Carry-forward invariants** (don't re-derive): the 6-point relay recipe
(spike-findings "CP1 results"); per-run socket must sit outside every srt
bind-mount (design §5.3 — `proxy.CheckPlacement` enforces it); read-tier tokens
minted with zero write scopes are the hard boundary, the classifier is
defense-in-depth (§5.1); keep **direct mode + its existing tests green**; audit
redaction is by token VALUE, never by pattern (the new `ghs_APPID_JWT` format
breaks prefix/length regexes); rein must fail closed around srt's two silent
fail-opens (invalid config, missing seccomp) — the self-tests enforce this.

## 5. Keeping THIS doc useful

If you change the bring-up steps or the resume state, update this file in
the same commit. The next agent reads it first.
