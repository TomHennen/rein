# Onboarding UX design — interactive `init` + `doctor --fix`

**Status:** DRAFT for Tom's review (2026-07-05). Written after the CP1–CP4
spine landed and sandboxed mode became the `rein run` default. Not yet built.

## 1. Why now

Sandboxed-by-default changed what "onboarded" means, and today's `rein init`
under-serves it:

- **You finish `init` and still can't run anything.** A session file
  (`~/.config/rein/dev-session.yaml`: repos + issue) is *required* for
  `rein run`, but init only scaffolds one if `REIN_TEST_REPO_A` happens to be
  set — otherwise it prints "create it manually." New user → wall.
- **The sandbox prereqs are now mandatory but silent.** `srt`, `bwrap`,
  `ripgrep`, `socat`, the Ubuntu-24.04 AppArmor profile, and healthy NTP are
  required for the default `rein run`. If they're missing, the *first* run just
  fails. `rein doctor` checks them but nothing walks a new user through them.
- **Per-machine App names are random, so multi-machine attribution is opaque.**
  `rein-<role>-<random10hex>` is globally unique but not human-meaningful; you
  can't tell which machine a `[bot]` commit came from.
- **Decisions are flags nobody discovers** (`--no-alias`, `--skip-audit`, …).

## 2. Division of labor: `doctor` checks + fixes, `init` decides

The core principle — **one source of truth for health checks**:

- **`doctor` = diagnose + (opt-in) fix.** It already owns every health check
  (PATH, shims, key, App creds, session, the sandbox stack, caches) and today
  *routes* to the fix (`re-run \`rein install-shim\``, `rein init`, or a printed
  command). Add an opt-in **remediation mode** (§6) so it can also *apply* the
  safe fixes.
- **`init` = the one-time interactive setup decisions** — session, alias,
  machine label, install-on-repo. `init` **calls doctor's checks** rather than
  reimplementing prereq detection, so the guidance you get during onboarding and
  from `rein doctor` later is literally the same code.

Net: prereq logic lives once (doctor). init orchestrates the decisions.

## 3. Interactive `init` flow

A short guided flow, in order. Every prompt has an **Enter-accepts default**, so
"just hit Enter" is a fast path. **Non-interactive fallback is mandatory** (§7).

1. **Sandbox health (via doctor's checks).** Run the sandbox-stack checks. If
   anything's missing, show the exact fix and offer doctor's remediation (§6)
   for the safe steps; for the sudo/external ones (apt, AppArmor, npm, NTP),
   print the command and offer to run it *with consent* — never silently.
   Block completion of onboarding on a healthy sandbox (or let the user proceed
   with a loud "sandboxed runs won't work until this is fixed").
2. **Which agent(s) do you use?** `claude` (default) / aider / codex / other.
   Drives **two** things: which alias(es) to install (step 5) and the CP4.5
   egress default (e.g. `claude` → pre-allow `api.anthropic.com`).
3. **Name this machine** `[<hostname>]`. Becomes the App label (§4). Default is
   the hostname *when distinctive*; on a generic hostname (`ubuntu`,
   `localhost`) prompt more insistently since the default won't distinguish
   machines. Always append a short uniqueness guard (§4).
4. **App creation** (existing manifest/browser flow), now named with the label.
5. **First repo + issue → write the session.** *"Which repo should the agent
   work on?"* + *"Which issue backs write-approval?"* Writes
   `dev-session.yaml`. This is the highest-value addition — it's what turns
   "init, then read docs, then hand-write YAML" into "init, then `claude`."
6. **Alias?** *"Run `claude` through rein automatically? [Y/n]"* — edits the
   shell rc (detected from `$SHELL`, confirmable). Print the `\claude` /
   `command claude` bypass right here so opt-out is discoverable.
7. **Install-on-repo.** Offer to open the install page for the repo from step 5;
   fall back to a printed link (§5).

Kept as **smart defaults, not interrogated:** audit App (default *skip* + one
line that it's reserved for audit-comment writeback), PATH symlink (yes), shell
(auto-detect + confirm), git-author label.

## 4. App naming (the machine label)

Constraints that shape this: GitHub App names are **globally unique across all
of GitHub** (the name is the public `github.com/apps/<slug>` URL), and hostnames
are often generic. So a name must be **meaningful AND globally unique**:

- Format: `rein-<role>-<label>-<shortrand>` — e.g. `rein-primary-toms-laptop-a1b2`.
  The label makes it human-recognizable; the short random guard keeps it
  globally unique (and survives two machines sharing a hostname).
- `label` defaults to the sanitized hostname *when distinctive*; on this VM the
  hostname is literally `ubuntu`, which is why the prompt (step 3) matters —
  auto-inference alone is unreliable.
- On collision (GitHub 422 "name taken"), lengthen the random guard and retry.
- Payoff: per-machine commits/Activity read as `toms-laptop[bot]` vs
  `toms-workvm[bot]` — turning distinct-bot-per-machine from a wart into useful
  attribution, and making the "don't copy the key across machines" story a
  positive.

## 5. Browser steps degrade to a link (headless-first)

rein already detects headless (`SSH_CONNECTION` set, no `DISPLAY`/
`WAYLAND_DISPLAY`) and prints instructions instead of opening a browser. Every
browser step in the flow follows the same rule: **the printed link is the
baseline; auto-open is a bonus** when a local display exists. Install-on-repo is
even simpler than App creation — it needs no loopback callback (just visit a URL
to grant the install), so there's **no `ssh -L` dance**; any browser on any
machine works. (App creation keeps its `ssh -L` recipe because it *does* need
the callback to capture credentials.)

## 6. `doctor --fix` (remediation mode)

Today doctor is read-only and routes to fixers. Add an **opt-in** fix mode
(`--fix`, or an interactive "apply this fix? [Y]"). Tiered by safety:

- **Auto-runnable with consent (no privilege):** `install-shim`, refresh the
  PATH symlink, (re)write the session, refresh a stale cache.
- **Show + offer to run, needs privilege/external — NEVER silent:** `apt install
  bubblewrap ripgrep socat`, the AppArmor profile write + reload,
  `npm install -g @anthropic-ai/sandbox-runtime@0.0.63`, enabling NTP. Print the
  exact command; run only on explicit per-step consent (design §4.5: rein must
  not silently `apt`/`npm` on a user's behalf).
- **Guide-only:** anything that needs a human decision (which repo, which
  account).

`init` reuses this mode for its step-1 prereq handling, so there's one
remediation path.

## 7. Guardrails

- **Enter-defaults everywhere** — interactive must not mean slow.
- **Non-interactive fallback is mandatory** — no tty (the headless `ssh -L`
  flow, CI), or `--yes`, → fall back to flags + defaults, never block. `init`
  runs headless today; a hanging prompt would be a regression. All existing
  flags remain as the non-interactive override.
- **No silent privileged/external installs** — consent per step, command shown.
- **Fail closed** — if the sandbox can't be made healthy, say so loudly; don't
  quietly leave the user in a broken-default or unsandboxed state.

## 8. Open decisions (for Tom)

1. **Prompt vs. loud-labeled-default for the machine name?** Leaning: prompt
   (step 3), because hostname inference is unreliable (generic hostnames,
   global-uniqueness collisions).
2. **How hard to gate onboarding on a healthy sandbox** — block completion, or
   allow "finish now, sandbox won't work until fixed"?
3. **Multi-agent aliasing** — alias every agent the user names, or just the
   primary? And do we alias non-`claude` agents at all in v1?
4. **`doctor --fix` scope for v1** — start with the no-privilege tier only
   (shims/symlink/session) and leave the sudo steps guide-only? Or include the
   consented-privileged tier from the start?
5. **Where this sits in the plan** — a "CP4.6 onboarding" checkpoint after
   CP4.5, or fold into CP6 dogfood prep (since dogfooding needs a smooth
   onboarding anyway)?
