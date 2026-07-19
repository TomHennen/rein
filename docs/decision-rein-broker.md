# Decision: rein as a sandbox-agnostic credential broker

**Status:** Draft for Tom. Turns the nono-pivot investigation into one set of decisions.

**TL;DR:** Stop trying to *own* the sandbox. rein becomes a **credential broker you plug a
sandbox into.** It mints per-issue scoped tokens, gets them to the agent's git without leaving
them where the agent can read them out, and runs the declare→approve ceremony. The sandbox (nono, srt, ...) is the
user's — configured from rein's snippet and **verified** by rein's prober. This sheds ~2.9k lines
of wrapper and parser code and makes no claims about the sandbox itself.

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
| **Relay** | `internal/proxy` (minus the parser) | A loopback listener the sandbox tunnels to; TLS-terminate, inject the auth header, stream the body opaquely. Kept on every backend — the token is injected downstream and never enters the sandbox (the tool-sandbox 'shed it on nono' alternative was disproven: it leaks the token to the agent; see the proxy section) |
| **Ceremony** | `internal/approvals`, `internal/ui/grant` | `rein declare <n>` → human approval → write tier unlocked; revocable in one place |
| **Verifier** | `internal/nono` prober (`selftest`, `verify`) | Assert the *user's* sandbox confines: creds hidden, egress routed to rein's relay, approval channel isolated, broker leaks no secret |
| **Audit** | existing + GitHub-sourced | Reconstruct "what the run wrote" from GitHub's own record (pushes, PRs, comments), keyed to the declared issue |

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

## The proxy, simplified

**Keep the relay — on every backend — but drop the parser.** A spike tried to shed the relay on
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

What the relay does *not* need is the **parser**. Drop the receive-pack and GraphQL classifiers
(`internal/classify` + `proxy/receivepack` + `gate`, the `#136` fuzz surface). The relay becomes
**inject-the-current-tier-header + stream opaquely** — it never parses attacker-controlled bytes.
Enforcement moves off the byte stream, none of it needing rein to read the body:

- **Branch floor** (`agent/**`, never `main`/tags) → **GitHub rulesets** (server-side, bound to
  the token; one-time setup costs the App `administration:write`).
- **Read/write tiering** → **rein injects the current-tier token** (read pre-declare, write
  after; a pre-declare mutation 403s server-side). The mid-run switch is trivial and safe on the
  relay — rein just flips which token it injects, per request, and the token never being in the
  sandbox is exactly what makes that safe (it was the sandbox-side delivery that leaked).
- **"Where it wrote" (audit)** → **GitHub's own record**, more complete than the stream tap (it
  catches gh-API merges/PRs/comments the receive-pack parser never saw). The one real loss:
  real-time *denied-attempt* visibility — GitHub's record shows only successful writes.

So the parser goes (attack surface removed), the relay stays and *simplifies* to inject-and-stream,
and it's **universal**: both nono and srt route GitHub egress to rein's relay. `gh`/REST/GraphQL
ride the same relay (small requests, no cap).

**Commit signing.** Forwarding a signing-**agent** socket (ssh-agent/gpg-agent) is *not* the same
as delivering a token: the agent protocol signs on request but never reveals the key, so the key
isn't `nc`-extractable even though the socket is reachable. Two caveats remain: the forwarded
agent can be *used* to sign arbitrary things, so scope it to `git` (tool-sandbox `intercept`); and
the signer must be a **rein/bot identity** — forwarding the *developer's* key signs commits as the
human, breaking non-impersonation. Open design, not a blocker.

Tying a write to its issue was always **audit, not a hard boundary.** rein's claim is a per-issue
*ceiling* plus attribution plus audit — not "the token can only write issue N's branch." GitHub
has no issue-scoped tokens, so the parser never enforced that anyway. Dropping it costs no real
claim.

## Merge policy

Keep **agent-merge as the default** — it's genuinely useful. Offer **human-merge-only as an
opt-in:** drop `pull_requests` merge from the write tier, plus a ruleset protecting `main` that
the App doesn't bypass.

## Kept vs. shed

About **~2.9k non-test lines** shed from a ~22k branch:

- **Shed → docs + a snippet command:** `cmd/rein/run_nono.go` + home overlays (~810);
  `internal/nono` installer + profile-generator + doctor (~1.2k).
- **Shed → replaced by rulesets + token tiers:** the parser (`internal/classify` +
  `proxy/receivepack` + `gate`, ~860).
- **Kept:** minter, relay-minus-parser, ceremony, prober-as-verifier (the ~1.6k `selftest` +
  `verify`), audit — the broker itself. Nothing built this cycle is wasted; the shed wrapper
  code becomes documentation.

The relay stays in this count: it's kept for srt and as the portable fallback. The nono path can
run without it (proxy section), but that's an *optional* nono-only saving, not part of the ~2.9k.

## Backends

rein is agnostic. It ships a snippet and a verified config for each supported sandbox:

- **srt** — recommended for **maximum containment.** Its namespaces contain the process table,
  UDP, and `.git` hooks; the hook is stable. Cost: npm/Node plus the one-time userns/AppArmor
  setup (#147 automates the *scoped* profile).
- **nono** — recommended for **easiest setup.** Landlock + seccomp: no userns tax, single
  binary. Cost: three documented residuals that are the **substrate's, not rein's** —
  process-table/argv visibility, open UDP, and `.git` hooks in an in-place tree.

## Open design work (two real wrinkles)

1. **Broker lifecycle.** As a plug-in, rein's relay must already be running on a stable local
   endpoint when the sandbox launches — a small daemon or service, rather than launched together
   by the wrapper.
2. **Session identity.** When rein controls the launch, it knows "this request belongs to the
   run that declared #123." As a plug-in, requests arrive at a stable port and rein must tie them
   to a run and its declare state. Solvable with a per-session token the profile or env carries —
   but it needs design.

## Decisions requested

Each with a recommended default:

| # | Decision | Recommend |
|---|---|---|
| 1 | Adopt **plug-in over wrapper** as rein's shape | **Yes** — keep a thin `rein run` wrapper as an optional convenience if we want turnkey too |
| 2 | **Drop the parser;** adopt rulesets + read/write token tiers; source audit from GitHub | **Yes** |
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
