# Decision: rein as a sandbox-agnostic credential broker

**Status:** Draft for Tom. Turns the nono-pivot investigation into one set of decisions.

**TL;DR:** Stop trying to *own* the sandbox. rein becomes a **credential broker you plug a
sandbox into.** It mints per-issue scoped tokens, gets them to the agent's git without leaving
them where the agent can read them out, and runs the declare→approve ceremony. The sandbox (nono, srt, ...) is the
user's — configured from rein's snippet and **verified** by rein's prober. This sheds ~2.0k lines
of wrapper code (if we adopt the plug-in shape) and makes no claims about the sandbox itself.

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

## Plug-in, not wrapper

- **Wrapper (today):** `rein run -- claude` installs nono, generates the whole profile,
  launches `nono run`, runs a nono doctor. rein owns install, pack provisioning, profile
  shape, and version churn.
- **Plug-in (proposed):** rein runs as a local broker. The user adds a **snippet** to *their*
  nono (or srt) profile — route GitHub egress to rein's relay, trust rein's CA,
  `af_unix_mediation`, keep `deny_credentials` (their standard profile already has it). rein
  publishes the snippet (`rein profile-snippet nono`) plus docs, and **verifies** the running
  sandbox with the prober before trusting it. The user launches the sandbox their way.

**The one honest shift: guarantee → verify.** The wrapper *generates* the profile, so it
*guarantees* the routing. The plug-in has the user write it, so rein *verifies* it. The
containment prober we already built is exactly that verifier — which is why it earns its keep.

**Why plug-in wins:** every finding dragged the wrapper deeper into sandbox ownership — the
`claude-code` profile is a registry *pack* it must provision, nono moves fast, profiles differ
per backend. The plug-in leaves all that with the user, where it belongs — the purest form of
the product: bring your own sandbox; rein brokers the credentials.

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

About **~2.0k non-test lines** shed from a ~22k branch — **if the plug-in shape is adopted**:

- **Shed → docs + a snippet command:** `cmd/rein/run_nono.go` + home overlays (~810);
  `internal/nono` installer + profile-generator + doctor (~1.2k).
- **Kept:** minter, the relay **with its parser** (`internal/classify` + `proxy/receivepack` +
  `gate`, ~860 — the universal branch floor), ceremony, prober-as-verifier (the ~1.6k `selftest`
  + `verify`), audit — the broker itself. **No ruleset code is adopted, and the App manifest does
  not gain `administration:write`.** Nothing built this cycle is wasted; the shed wrapper code
  becomes documentation.

The relay + parser stay on **every** backend (nono and srt alike) — they carry the universal
branch floor and the downstream injection that keeps the token out of the sandbox. The ~2.0k is
only the wrapper, and only if we move to the plug-in shape.

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
them in the code dissolved both. Today the broker is **in-process, one relay per `rein run`, with
identity implicit** (one proxy = one run; no on-the-wire run discriminator). The reorientation
keeps that property and just *decouples the relay from launching the sandbox* — it does **not**
introduce a resident multi-run daemon.

1. **Broker lifecycle — a per-session relay, not a daemon.** `rein session start` reuses today's
   per-run relay (`runbroker.Start`) on an **ephemeral `127.0.0.1:0` port** and prints the launch
   routing instead of exec-ing the sandbox itself. It lives from `session start` to `session end`
   / idle-TTL. The user then launches *their* sandbox pointed at it. The one already-standing
   thing rein reuses is the persistent trust material (App key + proxy CA); `internal/daemon`
   stays shelved. A resident multi-run daemon is explicitly rejected: multiplexing runs over one
   endpoint is the *only* shape that needs a wire discriminator, and nono can't carry one
   (`external_proxy.auth` is unimplemented). Don't build the thing that manufactures the problem.

2. **Session identity — stays implicit.** One relay = one session = one `runID` = one
   confirmed-issue file; the read→write flip is still a file the relay re-reads per request. No
   per-session bearer token (it couldn't ride anyway). Concurrency = multiple `rein session start`
   on different ephemeral ports; nono's loopback mediation isolates them.

**How the ephemeral port reaches a user-owned profile — without rein writing the profile.** nono
exposes `--upstream-proxy <host:port>` / `NONO_UPSTREAM_PROXY=` (and `--allow-domain` /
`--upstream-bypass`) as launch-time overrides. So the per-session port travels on the user's
*launch command* (`NONO_UPSTREAM_PROXY=127.0.0.1:<port> nono run -p their-profile …`), and their
profile stays **static and entirely theirs**: CA trust (stable path), `allow_domain` (inject hosts
+ `declare.rein.internal`), `upstream_bypass` (CDN), `af_unix_mediation`, `block:false`. rein
injects **nothing** into the profile. (nono's profile fields do **not** interpolate arbitrary host
env — expansion is a fixed variable set on path/`set_vars` fields only — so the launch-flag
override, not profile interpolation, is what makes an ephemeral port work.)

**The one residual, and it's the honest "verify" tax.** rein can no longer *guarantee*
one-sandbox-per-relay the way owning the launch did. If a user *deliberately* points two sandboxes
at one running relay, both share the one approved write session (agent B inherits agent A's write
tier). An ephemeral fresh-port-per-session makes this a deliberate misuse, not the default — and
the containment prober asserts it, plus the docs warn. Anyone wanting the old *guarantee* keeps
the thin `rein run` wrapper (decision #1), which still owns the launch. Guarantee vs. verify is
exactly the wrapper-vs-plug-in axis.

**Two build-time must-dos** (not open questions — requirements):
- **Prober invocation.** The verify half of guarantee→verify needs a concrete hook: `rein session
  start` (or a `rein verify`) runs the prober against the launched sandbox before the agent does
  real work, and fails closed on a bad config. Not a silent gap.
- **Approval fails closed.** With rein no longer owning a terminal, the in-sandbox `rein declare`
  surfaces its host-side approval via the **tmux popup** (already built, af_unix-mediated) or the
  **foreground `rein session start` terminal**. Detached + no-tmux has nowhere to prompt — that
  must be a clear fail-closed error at `session start`, never a hang.

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
| **Shape** | **Wrapper** — rein owns install / profile / launch · **plug-in** — the user owns the sandbox, rein brokers | **Plug-in** (a thin wrapper stays optional) | Every finding dragged the wrapper deeper into sandbox ownership (registry packs, fast-moving profiles, per-backend shape). The plug-in leaves that with the user; rein stands behind the credential boundary alone and *verifies* the sandbox with the prober. |

## Decisions requested

Each with a recommended default:

| # | Decision | Recommend |
|---|---|---|
| 1 | Adopt **plug-in over wrapper** as rein's shape | **Yes** — keep a thin `rein run` wrapper as an optional convenience if we want turnkey too |
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
