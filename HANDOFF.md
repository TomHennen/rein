# HANDOFF — continuing Phase 1 (CP2) on another machine

You are an agent picking up Phase 1 work from a `git clone` on a fresh
machine, and you need to **run and test live** (mint tokens, push to a
throwaway repo, run `srt`). This doc gets you from a bare clone to a
runnable machine, then points you at where the work stands.

No secrets are transferred: you create your **own** GitHub App via `rein
init` (the Phase 0.5 manifest flow), so nothing from the origin machine
needs to move except this repo.

> **Resume pointer:** read `PLAN-1.md` (the §"Notes" tail has the live
> state) and `docs/phase1-design.md` (design of record) +
> `docs/phase1-srt-spike-findings.md` (esp. the "CP1 results" relay
> recipe). Active branch: **`cp2-daemon-core`** (CP1+CP2 done, unpushed;
> CP3 is next — see §4).

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

Go 1.26+ (`go.mod`). Build the binaries — **use `-o bin/`**; a bare
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
./bin/rein init      # browser App creation + install on a throwaway repo
./bin/rein doctor     # all checks green; 'app credentials' must be [ok]
```

- Use a **throwaway repo you own** (hard-constraint #1 still holds for CP2).
  **Export `REIN_TEST_REPO_A=<owner>/<your-throwaway-repo>` yourself** before
  `rein init` — init *reads* it to scaffold a session but does not set it, and
  `dev-env`'s hardcoded value is the origin author's repo, not yours.
- If `doctor` shows `app credentials: 401`, it's almost always the clock
  (§1c), not the App.
- A minimal session file (no bound issue) is handy for non-interactive
  write-token mints during testing — see how CP1 did it (below).

## 3. Verify you can do what CP1 proved

Before continuing CP2, confirm the machine can mint + push, reproducing the
CP1 result (`docs/phase1-srt-spike-findings.md` → "CP1 results"):

- Write a minimal **no-issue** session to `/tmp/cp1-session.yaml` (omitting
  `issue:` is what disables the write-approval prompt — a session WITH an
  `issue:` would prompt on `/dev/tty` and the mint below would hang):

  ```yaml
  id: cp1-check
  role: implement
  repos:
    - <owner>/<your-throwaway-repo>
  ```
- Mint a write token with no new code (the helper honors `REIN_GIT_OP=write`
  + `REIN_SESSION_FILE`):
  `printf 'protocol=https\nhost=github.com\npath=<owner>/<repo>.git\n\n' | REIN_SESSION_FILE=/tmp/cp1-session.yaml REIN_GIT_OP=write ./bin/rein credential-helper get`
  → the `password=` line is the write token.
- Isolation check: `curl -u "x-access-token:<tok>" 'https://github.com/<owner>/<repo>/info/refs?service=git-receive-pack'` → **200** means push perm.

If that works, your machine is fully set up.

## 4. Where the spine stands — resume here

**CP1, CP2, CP3, and CP4 are DONE on `cp2-daemon-core` (unpushed as of
2026-07-05). The full sandboxed-mode spine is built and live-verified; the only
remaining checkpoint is CP6 (dogfood), which needs Tom's explicit go-ahead.**
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

**NEXT is CP6 — dogfood (needs Tom's explicit go-ahead; there is no CP5 on the
Linux spine — macOS parity is the off-spine CP5 track).** The GATE: `wrangle` is
the FIRST real-repo use; the throwaway-only hard-constraint #1 has held since
Phase 0, and crossing it is Tom's conscious decision after the spine runs clean
on throwaways — not something reaching CP6 grants. Plan (PLAN-1 CP6): Tom runs
sandboxed mode on a throwaway for a few sessions, then on `wrangle`, testing the
design.md §7.2 hypothesis (two weeks, no PAT fallback under deadline pressure).
Before dogfood: durable VM time-sync (#23) and re-verify the srt pin. Read:

1. `PLAN-1.md` — CP6 section + the full 2026-07-05 Notes tail (the whole spine's
   live status + every design decision made building it).
2. `docs/phase1-design.md` — the 2026-07-05 correction banner + §7.2 hypothesis.
3. The CP4 commits (`git log 46b892d..HEAD`); `cmd/rein/run_sandboxed.go`.

**The write path needs a human tty** — verify with `docs/cp3-manual-test.sh`
(read path is autonomous) and the CP4 identity/push check `docs/cp4-manual-test.sh`.

Three things still want Tom's input (PLAN-1 Notes 2026-07-05): the stop-condition
(b) re-read (Claude Code shipped first-party masking); whether to file the staged
srt-upstream BYO-proxy issue (Tom: hold until CP3/dogfood — now reached); and
whether `--direct` should have a harder gate than its informational banner
(footgun-on-real-repo is currently possible by design).

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
