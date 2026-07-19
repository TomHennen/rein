# Decision: rein as a sandbox-agnostic credential broker (plug-in, not wrapper)

**Status:** Draft for Tom. Distills the nono-pivot investigation into one set of decisions.
**TL;DR:** Stop trying to *own* the sandbox. rein becomes a **credential broker you plug a
sandbox into** — it mints per-issue scoped tokens, injects them, and runs the declare→approve
ceremony; the sandbox (nono, srt, …) is the user's, configured per rein's snippet and **verified**
by rein's prober. This sheds ~3.4k lines of wrapper + parser code and makes no sandbox claims.

---

## Why this, in one paragraph

We set out to "get out of configuring our own sandbox so we don't worry about bypasses." The
investigation found the opposite of the easy story: srt's injection hook is **not** being
deprecated (that was an overread), and on measured containment **srt is stronger than nono**
(process-table, UDP, `.git`-hooks). nono's real edge is *install ease*, not security. So the win
isn't "swap srt for nono" — it's realizing rein's value is the **broker**, not the sandbox, and
that the broker should be **substrate-agnostic**. rein hides nothing and claims nothing about the
sandbox; it stands behind the credential boundary only.

## What rein *is* (the kept core)

| Piece | Package(s) | Role |
|---|---|---|
| **Minter** | `internal/githubapp`, `internal/broker`, `internal/ghsession` | mint short-lived, per-issue-ceiling, read/write-tiered GitHub App installation tokens |
| **Relay** | `internal/proxy` (minus the parser) | a loopback listener the sandbox tunnels to; TLS-terminate, inject the auth header, **stream** the body opaquely |
| **Ceremony** | `internal/approvals`, `internal/ui/grant` | `rein declare <n>` → human approval → write tier unlocked; revocable in one place |
| **Verifier** | `internal/nono` prober (`selftest`, `verify`) | assert the *user's* sandbox actually confines: creds hidden, egress routed to rein, approval channel isolated, rein's own broker exposes no secret |
| **Audit** | (existing + GitHub-sourced) | reconstruct "what the run wrote" from GitHub's authoritative record (pushes/PRs/comments), keyed to the declared issue |

That's rein. Everything else is how a sandbox gets pointed at it.

## Plug-in, not wrapper

- **Wrapper (today):** `rein run -- claude` installs nono, generates the whole profile, launches
  `nono run`, runs a nono doctor. rein owns install, pack provisioning, profile shape, version churn.
- **Plug-in (proposed):** rein runs as a local broker. The user adds a **snippet** to *their* nono
  (or srt) profile — route GitHub egress to rein's relay, trust rein's CA, `af_unix_mediation`,
  keep `deny_credentials` (their standard profile already has it). rein publishes the snippet
  (`rein profile-snippet nono`) + docs, and **verifies** the running sandbox with the prober before
  trusting it. The user launches their sandbox their way.

**Guarantee → verify.** The one honest shift: the wrapper *generates* the profile, so it
*guarantees* the routing; the plug-in has the user write it, so rein *verifies* it. The containment
prober we already built is exactly that verifier — which is why it earns its keep.

**Why plug-in wins here:** every finding dragged the wrapper deeper into sandbox ownership (the
`claude-code` profile is a registry *pack* the wrapper must provision; nono moves fast; profiles
differ per backend). The plug-in leaves all of that with the user, where it belongs. It's also the
purest form of the product: "bring your own sandbox; rein brokers the credentials."

## The proxy, simplified

Keep the **relay** (git-push credential injection has to stream, and neither nono nor srt can do
dynamic per-request injection — nono's injector buffers and caps at 16 MiB, hangs on chunked). Drop
the **parser** (the receive-pack + GraphQL classifiers — the `#136` fuzz attack surface):

- **Branch floor** (`agent/**`, never `main`/tags) → **GitHub rulesets** (server-side, binds the
  installation token; costs the App `administration:write` for one-time setup).
- **Read/write tiering** → **token scope** (read-only token before declare, write token after; a
  pre-declare mutation just 403s server-side). GraphQL body inspection buys ~nothing the token
  scope doesn't already enforce.
- **"Where it wrote" (audit)** → **GitHub's own record**, which is more complete than the stream
  tap (it also captures the gh-API merges/PRs/comments the receive-pack parser never saw).

Per-issue-*at-write-time* is **audit, not a hard boundary** — rein's stated claim is per-issue
*ceiling* + attribution + audit, not "the token can only write issue N's branch" (the docs confirm
this; GitHub has no issue-scoped tokens). So dropping the parser costs no real claim.

## Merge policy

Keep **agent-merge as the default** (it's genuinely useful). Offer **human-merge-only as an opt-in**:
drop `pull_requests` merge from the write tier + a ruleset protecting `main` the App doesn't bypass.

## Kept vs. shed (concrete)

Roughly **~3.4k non-test LOC** shed or demoted from a 22k branch:

- **Shed → docs + a snippet command:** `cmd/rein/run_nono*` + overlays (~960), `internal/nono`
  installer + profile-generator + nono-doctor (~1.6k).
- **Shed → replaced by rulesets + token tiers:** the parser (`internal/classify` + `proxy/receivepack`+`gate`, ~860).
- **Kept:** minter, relay-minus-parser, ceremony, prober-as-verifier, audit.

Nothing built this cycle is wasted — the relay, minter, prober, and ceremony *are* the broker; the
wrapper code becomes documentation.

## Backend menu (Story 2)

rein is agnostic (Story 1); it ships a snippet + a verified config for each supported sandbox:

- **srt** — recommended for **maximum containment** (namespaces: process-table, UDP, `.git`-hooks all
  contained; stable hook). Cost: npm/Node + the one-time userns/AppArmor setup (#147 automates the
  *scoped* profile).
- **nono** — recommended for **easiest setup** (Landlock+seccomp: no userns tax, single binary).
  Cost: three documented residuals that are the *substrate's*, not rein's — process-table/argv
  visibility, open UDP, `.git`-hooks in an in-place tree.

## Open design work (the two real wrinkles)

1. **Broker lifecycle.** As a plug-in, rein's relay must be running on a stable local endpoint when
   the sandbox launches (a small daemon/service), rather than launched together by the wrapper.
2. **Session identity.** When rein controls the launch it knows "this request belongs to the run
   that declared #123." As a plug-in, requests arrive at a stable port and rein must tie them to a
   run + its declare state — solvable with a per-session token the profile/env carries; needs design.

## Decisions requested (with recommended defaults)

1. **Adopt plug-in over wrapper** as rein's shape. *(Recommend: yes; keep a thin `rein run` wrapper
   as an optional convenience if we want turnkey too.)*
2. **Drop the parser; adopt rulesets + read/write token tiers; source audit from GitHub.** *(Recommend: yes.)*
3. **Merge:** agent-merge default, human-merge-only opt-in. *(Recommend: yes.)*
4. **Backends:** support srt (containment) + nono (ease); rein agnostic. *(Recommend: yes.)*
5. **The `claude-code` pack dependency** (wrapper-only pain): dissolved by plug-in (user owns it).

## What this session already produced (reusable regardless)

A working nono integration on `nono-pivot-design` (installer, profile gen, prober, relay, run path,
journey green) that becomes the **nono backend + snippet + prober**; the `#136` fuzzers merged to
main; a real fix (rein now scrubs ambient GitHub tokens from its own env — a co-located-broker leak
under nono's no-namespace model); and the measured containment ledger + srt-hook verification that
grounded all of the above.
