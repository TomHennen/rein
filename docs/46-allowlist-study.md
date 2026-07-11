# Decision study: pivot from deny-read list to a read-allowlist (issue #46 follow-up)

**Question (Tom, on #46):** instead of denylisting credential stores, should the sandbox
allowlist what the agent may read (project dir, /tmp, system paths), accepting that it
"might be too difficult for users" in exchange for safety?

## Recommendation

**Do this: deny-read `$HOME` wholesale; allow back a short, justified list of safe
subpaths; keep `/` readable; keep the existing targeted cred-store denials layered on
top as belt-and-suspenders.** This is the middle path phase1-design.md:145-158 already
prescribes, and srt 0.0.63 supports it natively today (§1 — no upstream changes).

**Do NOT do: a full root-level allowlist.** srt could express it, but enumerating every
system path per distro (/opt, /snap, /nix, brew) is the version of this idea that is
genuinely "too difficult for users," for little extra credential-side gain (§3).

**When something the agent needs is missing: it breaks loudly; there are NO
interactive allow prompts** — see §2a for exactly what the user experiences and why
prompts are deliberately excluded.

> **Status (2026-07-11, post-study):** Tom decided **default-ON from the start** (not
> the opt-in-first rollout in §5 — superseded), with `REIN_SANDBOX_ALLOW_READ` +
> a loud kill switch as the escape hatches and no interactive prompt. Decisions
> recorded on issue #59; implementation is on the #59 branch, in review.

---

## 1. What srt 0.0.63 can express (verified in shipped source)

All from `@anthropic-ai/sandbox-runtime@0.0.63` (`npm root -g`), `dist/sandbox/linux-sandbox-utils.js`:

- **Base is a readable root:** `--ro-bind / /` (line 615). There is no "start from
  nothing" mode; read policy is deny-then-allow-back.
- **denyRead dir → `--tmpfs` (empty, writable); denyRead file → `--ro-bind /dev/null`**
  on the symlink-resolved target (lines 569, resolveSymlinkDenyDest 539-560).
- **allowRead = allow-back-within-deny.** Settings `allowRead` maps to internal
  `readConfig.allowWithinDeny` (line 751) and is re-bound `--ro-bind` on top of an
  ancestor deny tmpfs (lines 577-595). So "deny $HOME except X" is directly expressible.
- **allowWrite survives a deny over its ancestor:** write binds wiped by a denyRead tmpfs
  are automatically re-bound read-write (lines 570-575). A working tree under a denied
  `$HOME` keeps working with no extra config.
- **Deny stays authoritative where it matters:** deeper denies are applied after
  shallower ones (shallow-first sort, lines 788-801), so `denyRead: [$HOME, ~/.config/gh]`
  + `allowRead: [~/.config]` still hides `~/.config/gh`. A dir-level allowRead never
  un-denies an explicitly listed file (exact-match-only rule, lines 805-812).
- **Even `denyRead: ["/"]` is supported** — expanded into per-child denies of `/`
  (skipping /proc /dev /sys, lines 762-777). So a *full* root allowlist is expressible
  too; the blocker for that model is enumeration, not srt capability.

**rein-side state:** the typed struct already models `AllowRead`
(internal/srt/config.go:76) but `Build` hardwires it empty (config.go:194) and `Params`
has no field for it (config.go:84-125). Audit #44/D6 confirms: "the *designed* widening
vector (a read-allowlist, phase1-design.md:148-151) does not exist." One `Validate` rule
must be reworked: config.go:288-297 rejects any allowWrite under a denyRead ("would be
tmpfs'd") — factually wrong for srt 0.0.63, which re-binds it. Under the new model the
working tree legitimately sits under the `$HOME` deny.

## 2. The enumeration problem (Tom's "too difficult for users?")

Hiding `$HOME` breaks, in rough order of severity:

1. **The agent's own installation.** Claude Code is typically npm-installed under
   `~/.npm-global` or `~/.nvm` (true on this dev box: `npm root -g` =
   `/home/admin/.npm-global`) — binary and node runtime vanish; nothing runs. rein must
   auto-derive this: `exec.LookPath(cmdline[0])` + `EvalSymlinks`, allow back the
   install prefix. Solvable mechanically, must ship in v1 of the flip.
2. **Agent config/credentials.** `~/.claude/.credentials.json` + settings must stay
   readable (CP4.5 already curates this set in run_sandboxed.go:576-606); invert it into
   allow-backs while keeping the history/projects sub-denies (srt ordering makes that
   composition work, §1). *Brokering these agent/tool credentials through rein itself —
   so the agent's own API key gets the same short-lived-injected treatment as GitHub
   creds — is the post-Phase-1 non-GitHub-adapter track, tracked as issue #4.*
3. **Toolchains and caches:** `~/.nvm`, `~/.cargo`/`~/.rustup`, `~/go`, `~/.local/bin`,
   `~/.local/share`, `~/.pyenv`, `~/.npm`, `~/.cache` (go-build/pip/uv). Losing caches
   isn't just slow — package registries are egress-denied by default, so a cold cache
   can mean *builds fail entirely* unless the operator opened egress.
4. **Long tail:** dotfile-managed shell/git config, sibling repos under `$HOME`, tool
   configs (`~/.config/ripgrep` etc.). Mostly cosmetic; and because a denied dir is an
   empty *writable* tmpfs, tools that insist on writing dotfiles don't crash — writes
   just evaporate (mildly confusing, non-fatal).

The papercut stream is real but bounded and one-time-per-machine: failures are loud
(ENOENT), and the escape hatch is one env var. A full root allowlist multiplies this by
distro variance (/opt, /snap, /nix, linuxbrew, /var oddities) for little credential-side
gain — that is the model that is genuinely "too difficult for users."

### 2a. What a miss looks like to the user — and why there are NO allow prompts

Concretely, when the allowlist misses something the agent needs:

1. The agent's tool fails **inside the sandbox** — a denied dir reads as an empty
   (writable) tmpfs, so the tool reports "config not found" / ENOENT and the agent says
   so in its transcript. Nothing hangs, nothing is silently degraded.
2. The human recognizes it from the **run banner**, which states up front that `$HOME`
   is hidden and prints the exact remediation to copy-paste:
   `REIN_SANDBOX_ALLOW_READ=/abs/path rein run --sandbox -- ...` (or the loud
   `SHOW_HOME` kill switch for "just make it work while I file the gap").
3. The operator adds the path once; recurring gaps graduate into the shipped
   auto-derived defaults. Discovery is one-time per machine/tool, not per run.

**Deliberately NOT built: an interactive "agent wants to read X — allow? [y/N]"
prompt.** Exactly for the failure mode Tom predicted: under a papercut stream, humans
train themselves to answer y, and the prompt becomes a rubber stamp that launders
credential reads through the same reflex ("~/.aws? y"). A prompt would also hand the
agent an active social-engineering channel (it can *cause* the prompt at a moment of
its choosing). Loud failure + out-of-band, human-initiated widening keeps the human
decision slow and the default closed. Revisit only with dogfood friction data, per
issue #59.

## 3. Security gain vs the maintained denylist

The #46 class is exactly "unknown-unknown same-uid credential stores," and those land in
`$HOME`/`$XDG_*` with near-certainty.

> **Why do they all wind up under `$HOME`? (ELI5)** A per-user credential has to live
> somewhere that (a) the program can write without root, (b) survives reboot, and
> (c) other users can't read. On Linux the only directory that ships with all three
> properties is your home directory — everything else is root-owned (`/etc`, `/opt`),
> shared (`/tmp`), or wiped (`/run`). So every tool either uses an old-style dotfile
> (`~/.aws`, `~/.npmrc`) or follows the XDG Base Directory spec, which is just the
> modern convention for *where under `$HOME`* to put things (`~/.config` for config,
> `~/.local/share` for data — keyrings live there, `~/.cache` for caches; each
> overridable by an `$XDG_*` env var, which is why rein resolves those too). "Near-"
> certainty because exceptions exist — system-wide creds in `/etc` (root-owned, already
> unreadable to our uid) and secrets on other mounts (the `/mnt/dev` residual, §3) —
> which is why `/` staying readable is listed as a residual, not declared safe. Currently readable in-sandbox and closed wholesale
by deny-$HOME: `~/.local/share/keyrings` (#46 itself), `~/.aws`, `~/.azure`,
`~/.kube/config`, `~/.docker/config.json`, `~/.npmrc` (publish tokens), `~/.pypirc`,
`~/.cargo/credentials.toml`, `~/.gem/credentials`, `~/.terraform.d`, browser
cookie/password stores (`~/.mozilla`, `~/.config/google-chrome`), `~/.password-store`,
plus every future tool's token file. The maintained denylist can never win this race;
deny-$HOME changes the failure mode from "unlisted store leaks" to "unlisted tool breaks
loudly" — precisely rein's fail-closed posture (design §2).

Tom's second #46 comment (rein's own on-disk PATs/token caches): already denied via
`stateDir` + `ConfigDir` (run_sandboxed.go:568-573); under the new model they're *also*
outside `$HOME`'s allow-backs — structural instead of enumerated.

**Residuals with `/` still readable:** stray secrets in `/tmp` (agent needs /tmp; srt's
seccomp AF_UNIX block already neuters /tmp agent sockets), project trees on other mounts
(e.g. `/mnt/dev` sibling repos with `.env` files — note for the dogfood, since this box
keeps repos there), world-readable `/etc` oddities (rare; root-owned secrets are already
unreadable by uid). These are the Phase-2 argument for the full allowlist; srt's
root-deny expansion keeps that door open without upstream work.

## 4. Interactions

- **D6 (#44, symlink concern):** every allowRead entry is a new widening vector — apply
  the same fix D6 prescribes for `REIN_SANDBOX_WORKDIR`: `EvalSymlinks` all widening
  paths before overlap checks. One shared fix.
- **Authoritative denials (design §4.2):** keep the current `credentialDenyReadPaths`
  list verbatim; Validate additionally rejects (or carves out) any allowRead that equals
  or contains a denied cred store, per phase1-design.md:154-158. srt's ordering already
  protects the dir-deny case (§1), so this is defense-in-depth, not the only guard.
- **Validate rework:** allowWrite-under-denyRead becomes legal for the managed `$HOME`
  deny (srt re-binds it); keep rejecting overlaps with credential-store denies.
- **Self-test:** extend `VerifyConfigApplied` with a `$HOME`-side sentinel so the flip is
  proven applied every launch, same pattern as today.

## 5. Rollout — SUPERSEDED (kept for the record)

> Tom decided (2026-07-11, issue #59) to skip step 1's opt-in phase and ship
> **default-ON immediately**, given the current single-developer/throwaway-repo user
> base: discovery-by-dogfood beats an opt-in ceremony. The escape hatches and the
> no-prompt rule stand as written below.

1. **Now (with the #46 targeted fix already landing):** plumb `Params.AllowRead`,
   Validate rework, D6 EvalSymlinks, auto-derived allow-backs (agent install chain,
   ~/.claude carve-outs, working tree via existing write bind). Opt-in:
   `REIN_SANDBOX_HIDE_HOME=1`; widening via `REIN_SANDBOX_ALLOW_READ` (comma list) and a
   session `allow_read:` field — mirroring the REIN_ALLOW_DOMAINS / allow_domains
   pattern, with the same loud-warning treatment for broad entries.
2. **CP6 dogfood with it ON;** collect papercuts into the auto-derived defaults.
3. **Flip the default;** keep `REIN_SANDBOX_ALLOW_HOME=1` as a loud, banner-warned
   escape hatch (never silent).

**Effort:** step 1 ≈ 1-2 days (struct/plumbing/tests/self-test/docs; most machinery
exists). Dogfood burn-in 1-2 weeks calendar. Default flip is a one-line change plus docs.
Full root allowlist: not now; revisit post-Phase-1 with dogfood data.
