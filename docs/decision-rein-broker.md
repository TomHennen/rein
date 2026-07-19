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
| **Relay** | `internal/proxy` (minus the parser) | A loopback listener the sandbox tunnels to; TLS-terminate, inject the auth header, stream the body opaquely. Kept for srt and as the portable fallback; on nono a tool-sandbox + credential socket can replace it (see the proxy section) |
| **Ceremony** | `internal/approvals`, `internal/ui/grant` | `rein declare <n>` → human approval → write tier unlocked; revocable in one place |
| **Verifier** | `internal/nono` prober (`selftest`, `verify`) | Assert the *user's* sandbox confines: creds hidden, egress routed as the profile declares (to rein's relay, or straight to GitHub with the token scoped to git), approval channel isolated, broker leaks no secret |
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

**On nono, rein can shed the streaming relay. For srt — and as the portable fallback — it
stays.** A spike verified the load-bearing pieces on real nono 0.68.0 against real GitHub (the
credential-helper wiring is the one part still pending — see the costs below).

The relay exists to inject the auth header into git's byte stream, because no sandbox injects
per-request. But nono has a second egress path that skips injection entirely:

- **Two nono paths, one cap.** The 16 MiB body cap is an artifact of nono's *inject* path,
  where nono MITM-terminates the connection and rewrites the request to add the token — it
  buffers the whole body (hence the cap) and hangs on chunked encoding. On the **opaque
  CONNECT-passthrough** path — git carries its own auth, nono just tunnels the bytes — there is
  **no cap** and no buffering. A 30 MiB incompressible push landed.
- **The agent can't read the token.** A nono *tool-sandbox* scopes the token to git's child
  process, so the outer agent can't read it out: `/proc/*/environ` returns EACCES, and a live
  sweep found 0 leaks. This is *passive*-read closure — see the confused-deputy cost below. Host
  egress is preserved via `allow_domain` plus the CONNECT proxy.

So the shape on nono becomes a **tool-sandbox git/gh policy** plus a **credential-helper
socket** — git's `credential.helper` calls rein for the current-tier token, which is what lets a
mid-run read→write switch happen without a streaming relay. That sheds the relay on nono. It
**stays for srt and as the portable fallback.**

Shedding it loses no *enforcement* capability, because decision 2 already moved everything that
needed rein in the byte path elsewhere: branch floor → rulesets, tiering → token scope, audit →
GitHub's record. No rule left depends on rein terminating the stream. The one real loss is
observability: the relay saw denied attempts (pre-declare 403s, out-of-ceiling pushes) in real
time, and GitHub's record only shows *successful* writes — a minor tripwire we trade away.

**Honest costs of the nono path:**

- The per-run tool-sandbox profile carries the token **on disk** — written outside the agent's
  grants, and verified unreadable by the agent, but on disk nonetheless.
- **Confused-deputy channel.** The sweep proved the agent can't *passively* read the token; it
  did not prove the agent can't make *git itself* hand the token over. Because the token lives in
  git's process, an agent that can steer git — `git credential fill`, a `.git` hook, a
  `-c credential.helper=...` alias — can extract it. The relay never had this exposure (git held
  no real credential under it). Blast radius is bounded: the token is short-lived and repo-scoped,
  and post-declare the agent can already write. Closing the channel means denying agent writes to
  hook/config paths and limiting how often the helper answers — which is part of the fiddly
  grants below, not a free property. This is an **open item, not yet closed.**
- Tool-sandbox grants are **fiddly and per-host:** git-core exec paths, the library closure, CA
  and DNS files. A missing git-core path surfaces as an *auth* error, not an obvious sandbox one.
- The credential-helper socket is **verified in mechanism, not yet in wiring** — and this is the
  **load-bearing gate** for the whole nono shed, not a side cost. Socket forwarding itself is
  proven (ssh-agent), but the git-credential-helper wiring on top of it is a **pending
  verification**; if it doesn't hold, the nono path falls back to the relay.
- **`gh` is a separate delivery path.** `gh` doesn't consult git's credential helper — it reads
  its own token. So the on-disk profile token and the helper-fetched current-tier token need
  reconciling: which token `gh` uses, and how it tier-switches on declare, is open design.

**Commit signing.** A tool-sandbox can forward a signing-agent socket scoped to just `git`, so
the signing key never reaches the agent — same mechanism as the credential socket. Caveat:
forwarding the *developer's* key signs commits as the developer, which conflicts with rein's
non-impersonation attribution. So signing must use a **rein/bot signing identity** — a
rein-managed key registered to the bot — not the user's key. Open design, not a blocker.

**Drop the parser** — the receive-pack and GraphQL classifiers (`internal/classify` +
`proxy/receivepack` + `gate`), the `#136` fuzz attack surface. Each thing it did moves
somewhere better:

- **Branch floor** (`agent/**`, never `main`/tags) → **GitHub rulesets**: server-side, bound
  to the installation token. One-time setup costs the App `administration:write`.
- **Read/write tiering** → **token scope**: read-only token before declare, write token after;
  a pre-declare mutation just 403s server-side. GraphQL body inspection buys ~nothing the scope
  doesn't already enforce.
- **"Where it wrote" (audit)** → **GitHub's own record**: more complete than the stream tap; it
  also catches the gh-API merges, PRs, and comments the receive-pack parser never saw.

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
