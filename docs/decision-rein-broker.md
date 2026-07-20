# Decision: rein as a sandbox-agnostic credential broker

**Status:** Draft for Tom. Turns the nono-pivot investigation into one set of decisions.

**TL;DR:** Stop trying to *own* the sandbox. rein becomes a **credential broker you plug a
sandbox into.** It mints per-issue scoped tokens, gets them to the agent's git without leaving
them where the agent can read them out, and runs the declare→approve ceremony. The sandbox (nono, srt, ...) is the
user's — a nono profile (shipped as a **pack**) that starts rein's relay via `session_hooks` and
is **verified** by rein's prober. **nono owns the launch.** This sheds ~1.7k lines of
sandbox-*config* code (profile-gen, installer, doctor) and makes no claims about the sandbox itself.

---

## Why

We set out to "get out of configuring our own sandbox so we don't worry about bypasses." The
investigation found the opposite:

- srt's injection hook is **not** being deprecated — that was an overread.
- On measured containment, **srt is stronger than nono** (process table, UDP, `.git` hooks).
  nono's real edge is *install ease*, not security.

So the win isn't "swap srt for nono." It's that rein's value is the **broker**, not the
sandbox — and the broker should be **sandbox-agnostic**. rein hides nothing and claims nothing
about the sandbox; it stands behind the credential boundary alone.

## What rein is (the kept core)

| Piece | Package(s) | Role |
|---|---|---|
| **Minter** | `internal/githubapp`, `internal/broker`, `internal/ghsession` | Mint short-lived, per-issue-ceiling, read/write-tiered GitHub App installation tokens |
| **Relay + parser** | `internal/proxy` | A loopback listener the sandbox tunnels to: TLS-terminate, inject the auth header, relay the body — and parse the receive-pack / GraphQL to enforce the `agent/**` branch floor. Kept on every backend — the token is injected downstream and never enters the sandbox (the tool-sandbox 'shed it on nono' alternative was disproven: it leaks the token to the agent; see the proxy section) |
| **Ceremony** | `internal/approvals`, `internal/ui/grant` | `rein declare <n>` → human approval → write tier unlocked; revocable in one place |
| **Verifier** | `internal/nono` prober (`selftest`, `verify`) | Assert the *user's* sandbox confines: creds hidden, egress routed to rein's relay, approval channel isolated, broker leaks no secret |
| **Audit** | parser tap + GitHub-sourced | "What the run wrote" from the parser's real-time stream tap (incl. denied attempts) plus GitHub's own record (pushes, PRs, comments), keyed to the declared issue |

That's rein. Everything else is just how a sandbox gets pointed at it.

## Plug-in, not wrapper — nono owns the launch, rein plugs in

The axis that matters is **who owns the sandbox — not rein.** Today `rein run -- claude` installs
nono, generates the whole profile, provisions the `claude-code` pack, launches `nono run`, and
runs a doctor. In the plug-in shape **the user runs `nono run` themselves** and rein is a nono
plug-in that the profile starts on demand.

The mechanism is nono's own: **`session_hooks.before`** — a host-privileged script the profile
runs *before the sandbox child forks* (schema-confirmed; empirically it reaches `/dev/tty` when
nono is launched from a terminal, so it can prompt and drive interactive setup). rein's profile
declares:

1. `network.upstream_proxy: 127.0.0.1:<port>` → **rein's own relay** — so rein's proxy stays in
   the request path and **the receive-pack parser / `agent/**` branch floor survives**;
2. `session_hooks.before` → a rein boot script that **starts rein's relay** for this run;
   `session_hooks.after` → tears it down;
3. CA trust, `deny_credentials`, `af_unix_mediation`.

Then `nono run -p rein-local -- claude` starts rein's proxy as needed. **nono owns the launch;
rein brokers.** Because the hook fires once per `nono run`, it is **one relay per sandbox** — and
the boot script **fails closed on port collision**, so a second concurrent sandbox can't co-point
at the first relay (the one-sandbox-per-relay property is structural, not conventional).

**Distribution is a nono *pack*, but the auto-start hook is a *local* profile rein writes.** A
pack can't carry the hook: `session_hooks.script` takes no path expansion and a signed pack
profile is byte-identical for every user, so it can't encode a per-machine path (confirmed: the
`$HOME` path is rejected, and you can't add `session_hooks` to a digest-verified pulled pack).
The resolution is nono's own `<name>-local` pattern: a rein pack ships the base profile + skill +
docs, and pack **`json_merge` wiring writes a local `rein-local` profile** whose
`session_hooks.script` is the install-time-expanded absolute path (confirmed: `json_merge`
expands `$HOME` inside written content, and a wiring-output profile is outside the digest check so
its hook fires). What a pack **cannot** ship is the two irreducibly rein-side things — the `rein`
binary and the **GitHub App private key** (a per-user secret + browser flow). Those are what
`rein init` provisions.

**Install journey (the target):**
1. `nono pull rein` — verified (sigstore/rekor); installs the base profile + skill + docs; wiring
   writes the local `rein-local` profile with the machine-path hook.
2. First `nono run -p rein-local -- claude` — the boot hook runs, sees rein isn't set up, **prompts
   for consent** on `/dev/tty`, fetches + verifies rein's signed binary (its existing SLSA
   channel — not shipped in the pack) and drives `rein init` (App creation), then starts the
   relay. Later runs just start the relay.

`rein run -- claude` survives only as an *optional* turnkey convenience (rein owns the launch);
it is no longer the primary shape.

**Security posture to state plainly:** this means rein's pack plants a host-privileged
`session_hooks.before` into user-space that nono runs on every `nono run`, *outside* pack
digest-verification — guarded by pack signer provenance (sigstore/rekor, org==namespace) + the
script's file-ownership checks, and backstopped by the first-run consent gate. Appropriate for
rein's *own* deliberately-pulled signed pack; not something to hide.

**Build/test without publishing anything.** The substance needs no pack: the relay, parser,
`session_hooks` boot, consent gate, and `rein init` all run under a hand-written **local**
profile (`nono run -p ./rein-local.json`) driven by the existing pexpect journeys. The
pack-specific bit (wiring writes `rein-local`) is validated last against a **local registry**
(`nono pull --registry http://localhost:3000`). Nothing reaches the public registry until it is
ready for users.

## The proxy: relay and parser, both kept

**Keep the relay — on every backend.** A spike tried to shed the relay on
nono by delivering the token to git *inside* the sandbox (a nono tool-sandbox policy + a
credential socket). It works mechanically (no cap, mid-run tiering) but **fails on security,
decisively**: *any token delivered into the sandbox is extractable by the agent that controls
the sandbox.* Two independent leaks, one unmitigable — the agent `nc -U`'d the credential socket
and got the raw write token (nono's egress confinement is **TCP-only** on Linux; `filesystem.deny`
does **not** block an AF_UNIX `connect`), and `git credential fill` makes git print even a *baked*
token. The relay is safe precisely because it injects **downstream** of the sandbox behind a
loopback-TCP front (which nono *does* gate): the token value **never enters the sandbox**, so
there is nothing to `nc` or `git credential fill` out. So the relay **stays**, nono and srt alike.
The lasting value of the tool-sandbox detour is a hard **negative result that vindicates
downstream injection.**

**Keep the parser too — it is the universal branch floor.** The receive-pack and GraphQL
classifiers (`internal/classify` + `proxy/receivepack` + `gate`) enforce, on the byte stream,
that the agent's work lands on an `agent/**` branch and never a raw push to `main` or a tag. That
floor is the reviewable-artifact guarantee: even with agent-merge on, work goes through a branch
+ PR, not straight to `main`. The parser enforces it on **every repo, for free**, and its one
real downside — parsing attacker-controlled bytes — is already **de-risked by the `#136`
fuzzing** (merged to main).

We considered replacing the parser with **GitHub rulesets** (server-side branch protection bound
to the token) and **declined**. Rulesets would only ever be defense-in-depth *behind* the parser,
and buying them is expensive:

- **They widen rein's own blast radius.** Creating a ruleset costs the App
  `administration:write` — a permission that can reconfigure a repo's protections. rein's App
  today is `contents`/`issues`/`pull_requests:write` + `metadata:read`, **no admin**; adding
  admin means a leaked App key could rewrite branch protection, not just push branches. **Keeping
  the App admin-free is a deliberate design principle for a security tool** — minimize the
  permissions the broker itself holds.
- **They aren't universal.** Rulesets require **GitHub Pro for private repos** — verified: even a
  *read* of a private repo's rulesets returns `403 "Upgrade to GitHub Pro or make this repository
  public"`. Solo developers on private free repos are a core rein audience and simply can't use
  them. The parser has no such gate.
- **They add code.** An `internal/ruleset` path plus a `MintAdminToken` — more surface, for
  marginal defense-in-depth behind a floor we already enforce.

So the parser **stays**, rulesets are **not** adopted for the branch floor, and the relay's other
jobs are unchanged:

- **Read/write tiering** → **rein injects the current-tier token** (read pre-declare, write
  after; a pre-declare mutation 403s server-side). The mid-run switch is trivial and safe on the
  relay — rein just flips which token it injects, per request, and the token never being in the
  sandbox is exactly what makes that safe (it was the sandbox-side delivery that leaked).
- **"Where it wrote" (audit)** → the parser taps writes on the stream in **real time, including
  denied attempts**, and **GitHub's own record** supplements it (it also catches gh-API
  merges/PRs/comments the receive-pack tap never sees), keyed to the declared issue. Keeping the
  parser preserves the real-time denied-attempt visibility a GitHub-record-only source would lose.

The relay + parser are **universal**: both nono and srt route GitHub egress to rein's relay, and
`gh`/REST/GraphQL ride the same relay (small requests, no cap).

**Commit signing.** Forwarding a signing-**agent** socket (ssh-agent/gpg-agent) is *not* the same
as delivering a token: the agent protocol signs on request but never reveals the key, so the key
isn't `nc`-extractable even though the socket is reachable. Two caveats remain: the forwarded
agent can be *used* to sign arbitrary things, so scope it to `git` (tool-sandbox `intercept`); and
the signer must be a **rein/bot identity** — forwarding the *developer's* key signs commits as the
human, breaking non-impersonation. Open design, not a blocker.

**Issue binding is audit, not a hard boundary.** rein's claim is a per-issue *ceiling* plus
attribution plus audit — not "the token can only write issue N's branch." GitHub has no
issue-scoped tokens, so even the kept parser never enforced a hard per-issue write boundary; that
was never a claim rein made.

## Merge policy

Keep **agent-merge as the default** — it's genuinely useful. Offer **human-merge-only as an
opt-in.** A GitHub App's `pull_requests:write` **bundles** the merge capability — you can't keep
PR creation but subtract merge — so the parser's branch floor can't stop the agent from merging;
only a server-side rule can. Human-merge-only is therefore enforced by a **ruleset protecting
`main` that the App doesn't bypass**, and it is set up **by the user** — rein still mints no admin
token and gains no `administration:write`, so the broker stays **admin-free even here**. It
consequently only works on repos that support rulesets (**public or Pro-private**), and it stays
an opt-in, not the default — the common path never touches rulesets.

## Kept vs. shed

Roughly **~1.7k non-test lines** of sandbox-*config* code shed from a ~22k branch (recompute at
build; the reorientation sheds config ownership, not the launch):

- **Shed → docs + a pack + a boot command:** `internal/nono` profile-generator (~550), installer
  (~365), doctor/preflight (~303), and the home overlays (`sandbox_claude_home` +
  `sandbox_gh_home`, ~215) — all now the user's profile / the rein pack. `cmd/rein/run_nono.go`
  (~590) is largely superseded: nono owns the launch, so what remains is a small
  **`session_hooks` boot/stop path** (start relay on the fixed port → export env → teardown) plus
  the consent-gated first-run setup.
- **Kept:** minter, the relay **with its parser** (`internal/classify` + `proxy/receivepack` +
  `gate`, ~860 — the universal branch floor), ceremony, prober-as-verifier (the ~1.6k `selftest` +
  `verify`, now with the end-to-end round-trip fix), audit — the broker itself. **No ruleset code
  is adopted, and the App manifest does not gain `administration:write`.** Nothing built this cycle
  is wasted; the shed config code becomes documentation and the pack.

The relay + parser stay on **every** backend (nono and srt alike) — they carry the universal
branch floor and the downstream injection that keeps the token out of the sandbox.

## Backends

rein is agnostic. It ships a snippet and a verified config for each supported sandbox:

- **srt** — recommended for **maximum containment.** Its namespaces contain the process table,
  UDP, and `.git` hooks; the hook is stable. Cost: npm/Node plus the one-time userns/AppArmor
  setup (#147 automates the *scoped* profile).
- **nono** — recommended for **easiest setup.** Landlock + seccomp: no userns tax, single
  binary. Cost: three documented residuals that are the **substrate's, not rein's** —
  process-table/argv visibility, open UDP, and `.git` hooks in an in-place tree.

## Plug-in architecture (the two wrinkles, resolved)

The design worried about two wrinkles — a standing daemon and a session-identity token. Grounding
them in the code dissolved both. Today the broker is **in-process, one relay per run, with
identity implicit** (one proxy = one run; no on-the-wire run discriminator). The reorientation
keeps that property; it does **not** introduce a resident multi-run daemon (multiplexing runs over
one endpoint is the only shape that needs a wire discriminator, and nono can't carry one —
`external_proxy.auth` is unimplemented; `internal/daemon` stays shelved).

**Lifecycle — `session_hooks.before` starts the relay (see "Plug-in, not wrapper").** nono owns
the launch; the profile's `before` hook starts rein's relay on the **fixed port** the profile's
`upstream_proxy` names, once per `nono run`. Fixed port + **fail-closed-on-collision** in the boot
script makes one-relay-per-sandbox *structural* — a second concurrent sandbox can't bind the port
and can't co-point at the first relay. `session_hooks.after` tears the relay down; the hook is
non-fatal, so a failed boot fails closed on credentials (dead `upstream_proxy` → no token → no
writes), which rein health-checks and surfaces rather than running silently un-brokered.

**Session identity stays implicit.** One relay = one session = one `runID` = one confirmed-issue
file the relay re-reads per request. No per-session bearer token (it couldn't ride anyway).

### Security properties and fixes (from the fable review)

The relay is **unauthenticated on the wire** and that is a deliberate, bounded choice — but the
old claim that "reaching the port buys no capability" is **wrong** and is retracted. Token
*secrecy* holds (the value is injected on the rein→GitHub leg, never enters the sandbox — nothing
to `nc -U` or `git credential fill` out). But the relay's job is to *authorize the connector's
requests*: any process that reaches the port gets its own requests decorated with the current-tier
token and executed against GitHub. That is **action authority**, not nil capability. Consequences
and the fixes that ship with the reorientation:

- **Different-uid reach (multi-user box).** Loopback TCP is not uid-gated, but the App key *is*
  (keystore, uid + mode `0o077`). So a different local uid could get write-tier GitHub *actions*
  injected without ever reading the key — the relay would be a weaker boundary than the keystore
  in front of it. **Fix: uid-gate the loopback relay to same-uid peers**, for parity with the
  keystore's uid discipline. (Implementation note: it's a TCP front, so this is a
  `/proc/net/tcp` peer-uid check, not `SO_PEERCRED`; confirm feasibility during build.)
- **Shared-session blast radius is worse than "bounded."** If two connectors share one relay
  (a second sandbox pointed at the same port; or a co-resident host process), the second inherits
  the approved write tier. The write token is **installation-wide, floored only to `agent/**` by
  the parser** — issue binding is audit-only (see above) — so the second connector can write
  `agent/**` on **any repo the installation covers**, and its writes are **laundered under the
  session's issue attribution**, poisoning the audit trail. The `session_hooks` fixed-port +
  fail-closed-on-collision removes the second-sandbox vector *structurally* (the second can't bind
  the port); the uid-gate narrows the co-resident vector to same-uid (who already holds the key).

**Two build-time must-dos** (requirements, not open questions):

- **Prober must prove the rein leg is live — today it does not.** The shipped positive control
  only TCP-*connects* to nono's own proxy; it never confirms a request reached *rein*, so it
  **cannot detect a dead/mis-provisioned rein listener** (the current claim in
  `design-nono-pivot.md` that it can is an overclaim to correct). Fix: the probe must make an
  **end-to-end round-trip** through nono→rein to a rein-served endpoint, fail closed on a bad
  config, and the prober's scope must include verifying the user's **`deny_credentials`** (now the
  user's profile) — because a forgotten route fails closed *only* if the agent holds no independent
  credential. Caveat: verify runs *after* launch, so it bounds — not eliminates — a
  launch→probe window; the `before` hook narrows it by starting the relay before the child forks.
- **Approval must fail closed at declare time, not session start.** The prompt fires at
  `declare`, not at start, so a session that starts attached and then loses its terminal (SSH
  drop, window close, backgrounding) has nowhere to prompt. Requirements: re-evaluate channel
  reachability **at declare time**; a dead-tty **EOF is a denial** (fall back to read tier — which
  still satisfies "the helper always returns a credential"), never a hang, never auto-approve;
  prefer the **tmux popup** (it survives detach; the foreground terminal does not).

**Open empirical must-verify.** nono's behavior when `upstream_proxy` is set-but-unreachable, and
`block:false + allow_domain(github)` with no upstream_proxy, is documented nowhere in-repo or in
the nono skill. It changes only the *degradation mode* (agent can't work vs. works un-brokered),
not the credential boundary, but it is load-bearing for the fail-closed story — measure it, don't
assert it.

## Tradeoffs and options considered

The design space we explored, and why we landed where we did. A record, not a re-argument.

| Question | Options considered | Landed on | Why |
|---|---|---|---|
| **Sandbox** | **srt** — stronger containment (namespaces contain the process table, UDP, and `.git` hooks; the injection hook is stable — the "srt is deprecating the hook" worry was an overread), cost: npm/Node + a one-time userns/AppArmor tax · **nono** — easier install (single binary, no userns tax), weaker containment (three documented residuals) | **Multi-backend menu; rein stays agnostic** | rein's value is the broker, not the sandbox. The measured ledger put srt ahead on containment and nono ahead on install ease, so ship both — srt for max containment, nono for easiest setup — with a verified snippet + prober for each. |
| **Token delivery** | **Relay / downstream injection** — token injected outside the sandbox · **in-sandbox tool-sandbox delivery** — a credential socket inside nono · **nono `cmd://` inject** · **SSH / deploy keys** | **Relay / downstream injection (kept)** | In-sandbox delivery is **disproven**: the agent `nc -U`'s the credential socket, and `git credential fill` leaks even a *baked* token — a negative result that *vindicates* downstream injection (the token never enters the sandbox). `cmd://` caps at 16 MiB and is body-blind. Deploy keys aren't ephemeral (lifecycle churn). |
| **Branch floor** | **Parser** — on-stream `agent/**` enforcement · **GitHub rulesets** — server-side branch protection | **Parser (kept); rulesets declined** | The parser is **universal and free** on every repo, and its attack surface is de-risked by `#136` fuzzing. Rulesets would only be defense-in-depth *behind* it, but cost the App `administration:write` (widening the broker's blast radius past its `contents`/`issues`/`pull_requests:write` + `metadata:read` today), are Pro-gated for private repos (verified 403), and add an `internal/ruleset` + `MintAdminToken` path. Keeping the App **admin-free** wins. |
| **Merge policy** | **Agent-merge default** · **human-merge-only** | **Agent-merge default; human-merge opt-in** | Agent-merge is genuinely useful. Human-merge is hard to enforce cleanly — GitHub bundles merge into `pull_requests:write` — so blocking it needs a server-side ruleset; that makes human-merge-only an opt-in that only works on rulesets-capable repos (public / Pro-private). |
| **Commit signing** | Forward the **developer's** signing key · a **rein/bot signing identity** | **rein/bot identity** (scoping mechanics still open design) | Forwarding a signing *agent* socket is safe (the key isn't extractable — the agent protocol signs but never reveals it), but signing as the *developer* breaks non-impersonation. |
| **Issue binding** | Hard **per-issue write boundary** · **per-issue ceiling + attribution + audit** | **Ceiling + audit, not a hard boundary** | GitHub has no issue-scoped tokens, so the parser never enforced "this token can only write issue N's branch" anyway. rein's honest claim is a per-issue *ceiling* plus attribution plus audit. |
| **Shape** | **Wrapper** — rein owns install / profile / launch · **plug-in, `rein run` thin launch** — rein execs the user's sandbox · **plug-in, nono owns launch** — rein starts from the profile via `session_hooks` | **Plug-in, nono owns the launch** (`nono run -p rein-local`; `session_hooks.before` starts rein's relay); `rein run` survives as optional turnkey | rein's value is the broker, not the sandbox. `session_hooks` starts rein's relay per `nono run` (one-per-sandbox, fixed-port fail-closed) while keeping rein's proxy + parser in the path — the truest plug-in, distributed as a nono pack, without shipping the binary or the App key. |

## Decisions requested

Each with a recommended default:

| # | Decision | Recommend |
|---|---|---|
| 1 | Adopt **plug-in** as rein's shape — **nono owns the launch** (`nono run -p rein-local`), rein's relay started from the profile via `session_hooks.before` (keeps proxy + parser; one-relay-per-sandbox via fixed-port fail-closed); distributed as a nono pack; `rein run` survives as optional turnkey | **Yes** |
| 2 | **Keep the parser** as the universal branch floor (de-risked by `#136`); **do not adopt rulesets** for the branch floor — they'd cost the App `administration:write` and are Pro-gated for private repos; keep the App admin-free; audit from the parser tap + GitHub's record | **Yes** |
| 3 | **Merge:** agent-merge default, human-merge-only opt-in | **Yes** |
| 4 | **Backends:** support srt (containment) + nono (ease); rein stays agnostic | **Yes** |
| 5 | The **`claude-code` pack dependency** (wrapper-only pain) | Dissolved by plug-in — the user owns it |

## What this session already produced (reusable regardless)

- A working nono integration on `nono-pivot-design` (installer, profile gen, prober, relay, run
  path, green journey) — becomes the **nono backend + snippet + prober.**
- The `#136` fuzzers, merged to main.
- A real fix: rein now **scrubs ambient GitHub tokens from its own env** — closing a
  co-located-broker leak under nono's no-namespace model.
- The measured containment ledger + srt-hook verification that grounded all of the above.
