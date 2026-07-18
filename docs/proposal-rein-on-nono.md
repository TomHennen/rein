# Project proposal: rein as the credential authority for nono

**Status:** DRAFT — **SUPERSEDED IN PART by two reviews; do NOT green-light as
written.** See `docs/nono-git-push-spike-findings.md` §"Post-spike reviews". In
short: this doc argues architecture **(c)** (nono injects via `cmd://`), which the
reviews show is a *downgrade* of rein's mediation (the `cmd://` seam never sees
request bodies → GraphQL tier-classification and the small-push declare gate both
break). The better candidate is architecture **(b)** (nono = sandbox only; rein's
existing proxy stays the injection layer) — source-plausible but **empirically
unconfirmed** (the `--upstream-proxy` wiring did not route in the spike). Land the
carve-out (fuzz + prober) now; gate the pivot on a real follow-up spike. The
tear-out/keep/new/test sections below are written for (c) and must be re-pointed
at (b) if the follow-up spike confirms it.

Original intent: re-base rein's **sandboxed mode** onto `nono` (Landlock) as the
sandbox substrate, with rein reduced to the credential authority + git-push
relay + opinionated installer. Direct mode (Shape B) is out of scope.

## TL;DR

Stop owning the sandbox. rein becomes: (1) an **opinionated, verified installer**
that sets up nono + the GitHub App + an opinionated profile; (2) the
**credential authority** — mint per-issue, human-approved, short-lived App tokens
via nono's `cmd://` hook; (3) a **minimized, fuzzed git-push relay** for the one
path nono structurally cannot broker. nono provides containment (Landlock),
credential injection for the REST path, and egress control.

**Why:** the spike proved nono's Landlock genuinely protects the App key + host
creds from the agent (the reason to adopt it), its `cmd://` hook is a clean
host-side seam for rein's minting + approval, and its install/containment story
is far simpler than srt+bwrap+AppArmor. The **only** thing nono can't do is carry
a real `git push` (16 MiB `413` + chunked hang, both intentional in its source) —
so rein keeps a relay for that, and nothing else.

## Why now — what the spike established (evidence, not vibes)

- **Containment works:** inside `nono run`, Landlock `deny_credentials` blocks the
  agent from reading `~/.config/rein-credentials/app.pem`, the gh token, ssh keys,
  and history (proven). This is what rein's whole `internal/srt` composition
  (deny-read, seccomp, env allowlist, /dev/tty probe) was built to achieve.
- **The `cmd://` seam fits rein exactly:** nono invokes rein's helper **host-side**
  with per-request `{host, repo-path, method, session}`, where it can reach the App
  key + `/dev/tty` (both denied to the agent) — mint + approval, zero nono changes.
- **git push is the one hard wall:** nono's proxy caps request bodies at 16 MiB
  (`reverse.rs:37`, a deliberate DoS guard, issue #554) and never decodes chunked
  request bodies — git chunks every push > 1 MiB, so brokered pushes hang/413. Not
  a bug they'll fix. rein already streams pushes correctly (CP1 relay), so it keeps
  that one component.
- **Install is simpler on nono:** srt needs bwrap + an AppArmor userns profile +
  the Ubuntu 24.04 gate (a documented onboarding blocker). nono is a single
  Landlock binary. rein wrapping a *verified* nono install is a net UX + security
  win over both srt's setup and nono's own `curl | sh`.

## Architecture: division of labor

| Concern | Owner | Mechanism |
|---|---|---|
| Sandbox / containment | **nono** | Landlock profile (`deny_credentials`, fs, per-command) |
| Credential injection (REST/API) | **nono** ← rein | nono intercepts api.github.com; rein mints via `cmd://` |
| Credential minting + per-issue scope + approval | **rein** | `cmd://` helper, host-side, App key local |
| `git push` credential brokering | **rein** | minimized local relay (nono can't do it) |
| Egress control | **nono** ← rein | rein writes a tightened `allow_domain` profile |
| GitHub App onboarding | **rein** | manifest flow (existing) |
| Install/orchestration | **rein** | verified nono fetch + profile gen + CLI drive |
| Containment **assurance** | **rein** | sandbox prober: fail-closed launch gate + CI differential harness (trust-but-verify the substrate) |

Integration boundary = **nono CLI + JSON profile + the `cmd://` subprocess
contract** (the same clean subprocess boundary rein already uses for srt — no
CGo, no SDK; nono's Go SDK is a thin FFI binding and less capable than the CLI,
so we orchestrate the CLI). Pin the nono version; treat the profile schema + CLI
flags as a contract to re-verify on bump (existing srt policy).

## What we TEAR OUT

| Package / area | LOC | Why it goes |
|---|---|---|
| `internal/srt` | ~2030 | nono replaces the entire srt sandbox composition (Build/Validate, denyRead, seccomp, env allowlist, `VerifyConfigApplied`, /dev/tty probe, bwrap preflight). |
| `cmd/rein/run_sandboxed.go`, `sandbox_home.go`, `sandbox_claude_home.go` | large | srt launch + `~/.claude` bind/overlay hardening (#94) → nono profile handles the fs policy. |
| `internal/proxy` REST/GraphQL **injection** path + `classPassthrough`/CDN relay | part of ~2260 | REST injection moves to `cmd://` (nono injects); the CDN passthrough arm is already dead code in sandboxed mode. |
| Most of `internal/proxy` CA management | part | keep only the minimal CA the git-push relay needs. |
| srt-specific doctor checks (`cmd/rein/doctor.go` bwrap/userns/seccomp) | part | replaced by nono health checks. |

## What we KEEP

| Package / area | LOC | Role |
|---|---|---|
| `internal/broker` + `internal/brokercore` | ~730 | mint / scope / approval core — rein's heart. |
| `internal/keystore` | ~370 | App private key, **on-box** (local-first, hard-constraint #6). |
| `internal/githubapp`, `internal/tokencache`, `internal/ghsession` | — | App auth + token minting/caching. |
| `internal/approvals`, `internal/declare`, `internal/issuemeta` | — | human approval + declare-first per-issue scoping (#35). |
| `internal/classify` | — | tier (read/write) classification — **moves** into the `cmd://` mint decision (rein picks the token scope from the request context nono passes). |
| `internal/runbroker`, `runscope`, `session` | — | session identity + scope expansion (#69), adapted to the nono launch. |
| `internal/gitidentity` | — | non-impersonating commit author. |
| `internal/proxy` git-push relay + `receivepack.go` + declare gate (`gate.go`) | part of ~2260 | the one thing nono can't do — **minimized + fuzzed** (below). |
| `internal/appsetup` + `cmd/rein/init.go` | — | GitHub App manifest flow, **extended** to install nono. |
| Direct mode (Shape B) + `proctree*` | — | untouched; separate track. |

## What's NEW

1. **Verified nono install (the `curl | sh` fix).** `rein init` fetches a
   **pinned** nono release binary and verifies it — signature (nono is from the
   Sigstore founder; releases are signed) + SHA-256 digest against the pinned
   expected value — before installing to a rein-managed path. `rein doctor`
   re-checks the pinned digest. Strictly better supply chain than `curl | sh`,
   and a story security teams accept. *(Confirm nono's exact signing mechanism —
   cosign bundle / SLSA provenance — during CP-install.)*
2. **`rein credential-capture` subcommand** — the `cmd://` helper nono invokes
   host-side: reads the `nono.credential-provider.v1` request (host/repo-path/
   method/session), runs the broker core (scope check → approval prompt on
   `/dev/tty` for writes → mint), returns the token as `Basic`/`Bearer`.
3. **Opinionated profile generator** — rein writes the nono profile: `cmd://`
   wiring to `rein credential-capture`, tightened `allow_domain` (restores rein's
   strict egress that nono's stock profile lacks), `open_port` for the git-push
   relay, and the sandbox git-config that points push traffic at the relay.
4. **Repositioned git-push relay** — a standalone local proxy (nono's profile
   confines the sandboxed git to reach *only* it), rebuilt on `httputil.
   ReverseProxy` (stdlib streaming/chunked/Expect) with a thin rein inject +
   receive-pack-tap layer.

## Assurance over a substrate we don't own: the sandbox prober

Ceding containment to nono — pre-1.0, fast-moving, not ours — means rein no longer
controls the sandbox. The sandbox prober (`docs/containment-probe-harness.md`,
written substrate-agnostic for exactly this moment) is how rein keeps assurance,
and under the pivot it is **more central than it was under srt, not optional**.
Two layers:

1. **Launch-time gate (fail-closed, every `rein run`).** The nono analog of the
   `VerifyConfigApplied` we're deleting with `internal/srt`: before exposing the
   agent, rein confirms nono actually applied the expected denials — a sentinel
   read-back (a known file in the deny set must read empty in-sandbox) plus a probe
   that the App key + cred stores are unreadable. If nono didn't confine as
   configured — version drift, a profile bug, a Landlock-unavailable kernel — rein
   **fails closed and refuses to launch**. We keep the *concept* of the launch gate
   even as the srt implementation goes; with a third-party substrate it matters
   more, not less.
2. **CI / golden differential prober (dev + CI + on every nono bump).** Adopt
   `controlplaneio/sandbox-probe` (Apache-2.0, Go, same host-vs-sandbox-diff
   design): run inside `nono run` vs on the host, diff, and classify each
   surviving-reachable observation against rein's config-derived oracle — expected
   denials (`app.pem`, gh, ssh, history, keyring/agent sockets) and expected-open
   (the tightened egress allowlist). Golden-committed; drift = red = re-review.
   This is the check that caught, this session, that nono's stock profile leaves
   egress wide open — exactly the kind of substrate surprise rein must not ship.

The prober is also the **acceptance gate for the P3 cutover**: srt's containment
tests are only safe to delete once the prober proves nono enforces the equivalent
denials on the target platform(s). And it stays test-only / dev-invoked (like
`pyte`), so its license never touches the shipped binary.

## How we'd TEST

The **golden-transcript journey model stays** (`tests/interactive/`): every
behavior-changing PR moves a journey and ships a reviewable golden. But the split
of labor changes what each journey proves.

**KEEP (broker behavior — user-visible path is substrate-agnostic):**
`write_ceremony`, `scope_expansion`, `git_author`, `gh_write`, `push_upstream`,
`multi_repo`, `expansion_404`, `session_commands`, `tmux_popup_approval`,
`credential_boundary`, `app_not_installed`, `realagent_write`, `claude_resume`,
`init_*`/`onboarding`. These test declare → approve → push-lands, which is
preserved; they re-point from srt to the nono-composed stack and regenerate
goldens.

**DROP / DEMOTE (srt-specific containment — now nono's job):**
`sandbox_filesystem` (srt deny-read of cred stores) and the srt-only unit tests
(`VerifyConfigApplied`, seccomp AF_UNIX, /dev/tty self-grant) go away **with**
`internal/srt` — replaced by the **sandbox prober** (its own section above): the
fail-closed launch gate + the differential CI harness verify nono's containment
once and guard it against drift, instead of rein re-deriving it per release.
`sandbox_gh_read_staleness` likely stays (broker cache behavior).

**NEW:**
- `nono_install` — `rein init` fetches + **verifies** (sig + digest) a pinned
  nono, writes the profile; tampered binary fails closed.
- `git_push_via_relay` — a real (chunked, > 16 MiB) `git push` through the
  composed nono+relay stack lands on a throwaway repo (the capability nono can't
  provide alone — the headline journey).
- `cmd_capture_approval` — nono invokes `rein credential-capture`; write token
  gated by the host-tty prompt; in-sandbox agent can't reach it.
- **Fuzzing (the mediation-robustness track):** `go test -fuzz` on
  `ParseReceivePackCommands` (untrusted pkt-line — top target), the tier
  classifier, and `classifyHost`; smuggling/desync probes on the relay. Plus an
  independent security review scoped to the relay + parsers. This is the answer to
  "are we confident in that code" — currently it has unit tests + checkpoint
  reviews but **zero fuzzing**.

Net test surface: **smaller** (srt-composition tests deleted, containment
delegated to one probe) but with a **new, higher-value focus** (fuzz the one
custom security-critical component we keep).

## Risks & open questions

- **The relay is the residual risk.** It's the one hand-rolled component we keep.
  Mitigation: rebuild on stdlib transport + fuzz + independent review (above).
  Non-negotiable: it must fail loud, never the silent hang nono exhibits.
- **nono is pre-1.0, fast-moving.** Pin + re-verify the profile schema/CLI on bump
  (existing srt policy). The `cmd://` schema + 16 MiB cap are stable-ish (the cap
  is intentional, so it won't silently change).
- **Composition cost.** Three processes (nono, rein relay, `cmd://` helper) vs one
  stack. More moving parts; the installer hides it from users.
- **macOS.** nono uses Seatbelt on mac; verify `cmd://` + relay + git-config
  routing compose there (folds into the CP5 mac parity track).
- **Egress default.** nono's stock profile is open; rein's generated profile must
  tighten it (and we should EGRESS-warn on wide sets, as today).
- **Do we file the chunked/Expect issue upstream?** Courtesy + optionality (if
  nono ever streams request bodies, the relay could shrink further), but we do NOT
  depend on it — it's an intentional cap.
- **Direct mode** stays as-is; whether it survives long-term is a separate call.

## Phasing (spine, mirrors the CP discipline)

- **P0 — Install+profile:** verified nono fetch + opinionated profile gen in
  `rein init`; `doctor` nono health. Journey: `nono_install`.
- **P1 — `cmd://` authority + prober:** `rein credential-capture` (broker core +
  approval through it) **and** the sandbox prober — the fail-closed launch-time
  gate plus the `sandbox-probe` differential CI harness. Journeys:
  `cmd_capture_approval`, containment diff. The prober lands here because this is
  the first phase rein actually launches nono, and it's the assurance we need
  before trusting the substrate.
- **P2 — Relay reposition + de-risk:** move git-push relay behind nono, rebuild on
  stdlib transport, add fuzzers + review. Journey: `git_push_via_relay`.
- **P3 — Cut over sandboxed mode:** `rein run` launches nono (not srt); delete
  `internal/srt` + srt journeys. **Gated on the prober** proving nono enforces the
  equivalent denials on the target platform(s). Regenerate kept goldens.
- **P4 — Dogfood** on a throwaway, then wrangle (the existing CP6 gate).

## How we'd develop this

**Isolate the pivot from main until it fully replaces srt.** Per Tom's direction,
this does *not* trickle onto main. Develop on a long-lived integration branch
(`nono`, off main); main keeps shipping srt-based rein unchanged until the pivot
is complete and cut over in one reviewed step.

- **Branch model.** `nono` is the pivot trunk. Each phase (P0–P4) is a normal
  reviewed PR **targeting `nono`**, not main — so we keep per-phase review, green
  CI, and journey goldens *within* the pivot, without ever leaving main in a
  half-pivoted state. The flag that keeps srt default (proposal §"The decision")
  lives on the branch; main never sees it.
- **Keep `nono` synced with main — the one real risk of a long-lived branch is
  drift.** Merge main → `nono` continuously (at least per merged main PR / weekly),
  so the eventual cutover is a small reconciliation, not a big-bang. rein's
  worktree workflow (`.claude/worktrees`) gives the branch its own workspace.
- **The branch stays runnable throughout.** srt remains the default *on the branch*
  until P3; nono is behind the flag. Every phase leaves `go test ./... -race`, vet,
  gofmt, and the journey goldens green — so the branch is always dogfoodable (you
  can run the composed nono stack on the branch long before it's near main).
- **Carve-out: substrate-agnostic hardening goes to main NOW, independently.** Two
  pieces are wins regardless of the pivot and should not wait on `nono`: (a)
  **fuzzing the existing relay/parsers** (`ParseReceivePackCommands`, the
  classifier) — it hardens *today's shipping* code; (b) **adopting `sandbox-probe`
  as a CI check against the current srt setup** (the harness is substrate-agnostic,
  written for exactly this). Landing these on main shrinks the `nono` branch and
  de-risks the relay before it's ever repositioned.
- **The cutover (P3) is one atomic, reviewed PR** merging `nono` → main: flips the
  default srt→nono, deletes `internal/srt` + srt journeys, **gated on P4 dogfood +
  the prober's acceptance** (equivalent denials proven on the target platform(s)).
  Because srt lived until this PR, **rollback = revert it.**
- **Discipline per phase** (unchanged from the CP model): advisor before each
  phase's substantive work; a reviewer subagent (code + security) after each
  phase's implementation; stop-and-surface at every phase gate.

## The decision

Adopting nono is a **forward-looking simplification**: rein sheds ~2000+ LOC of
srt composition and its install friction, gains a stronger/better-maintained
sandbox and a cleaner verified-install story, and concentrates its own code on
what's differentiated (issuance, per-issue scope, approval) plus the one
irreducible relay. The cost is composing two proxies, taking a dependency on a
pre-1.0 tool, and owning + hardening the relay. The spike says the fit is real;
this proposal is the shape. **Green-light P0–P2 as a spike-grade track** (behind
a flag, srt still default) so we can prove the composed stack end-to-end before
committing to the P3 cutover.
