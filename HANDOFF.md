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
> recipe). Current branch for in-progress CP2 work: **`cp2-daemon-core`**.

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

**Pin srt.** The spike verified `@anthropic-ai/sandbox-runtime@0.0.54`. Its
mitm/proxy hooks are undocumented and may move across versions — install
that version and re-verify if you bump (`docs/phase1-srt-spike-findings.md`).

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

## 4. Where CP2 stands — resume here

CP1 is DONE (`git push` through a MITM proven; recipe recorded). CP2 (the
daemon + proxy arm) is in progress on **`cp2-daemon-core`** — its
**foundation packages are landed + tested + pushed**: `internal/brokercore`
(the shared decision core), `internal/classify` (the read/write tier
classifier), and `internal/daemon` (the control-socket + in-memory-cache
skeleton). The NEXT piece is the **proxy arm** (port the CP1 relay, wire it
to classify + brokercore.Core, per-run socket). The precise live status +
what's-next is the **CP2 entry in `PLAN-1.md`'s "Notes" section** — read it
first. Then, in order:

1. `PLAN-1.md` — CP2 section + the "Notes" tail (live status: what's done,
   what's next).
2. `docs/phase1-design.md` §4–§5 — the daemon/proxy/classifier/CA design and
   the open decisions (§5.1 classifier, §5.2/§5.3 per-run socket).
3. The CP2 commits on this branch + any open PR.

**Carry-forward invariants** (don't re-derive): the 6-point relay recipe
(spike-findings "CP1 results"); per-run socket must sit outside every srt
bind-mount (design §5.3); read-tier tokens minted with zero write scopes
are the hard boundary, the classifier is defense-in-depth (§5.1); keep
**direct mode + its existing tests green** through the broker-core
extraction.

## 5. Keeping THIS doc useful

If you change the bring-up steps or the resume state, update this file in
the same commit. The next agent reads it first.
