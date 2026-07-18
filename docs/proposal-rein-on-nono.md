# Project proposal: rein on nono (architecture B — nono sandboxes, rein's proxy injects)

**Status:** DRAFT for Tom's review (rewritten around architecture **(b)** after
the spike + two reviews; supersedes the earlier `cmd://` draft). Basis:
`docs/nono-git-push-spike-findings.md`. Scope: rein's **sandboxed mode** only;
direct mode (Shape B) untouched.

## TL;DR

Re-base rein's sandbox onto **nono** (Landlock), but keep rein's **own proxy as
the injection layer**. nono runs the sandbox and, for github, acts only as an
**opaque external-proxy tunnel** — it CONNECT-tunnels the agent's github traffic
to rein's proxy without terminating or buffering it. rein's proxy (the existing,
hardened, streaming CP1 relay — **minimized and fuzzed, not rewritten, not
replaced by a library**) TLS-terminates, injects the scoped token, taps the
receive-pack for the declare gate, and streams upstream. rein keeps its broker
(mint / per-issue scope / approval / declare) and adds a verified installer +
opinionated profile generator. rein **deletes `internal/srt`** (~2000 LOC of
sandbox composition).

This is the version that actually **minimizes rein's security-critical code**:
one small stdlib forward-proxy + the broker, on top of a sandbox rein no longer
maintains.

## Why (b), not the earlier (c)

The earlier draft had nono inject via its `cmd://` hook. Two reviews killed it:
`cmd://` never sees request bodies, so GraphQL read/write tiering and the
small-push declare gate both break, and nono's injecting proxy caps bodies at
16 MiB and can't stream chunked pushes. **(b) avoids all of that** by keeping
injection in rein's proxy, which sees full plaintext and streams. Proven in the
spike: a **20 MiB chunked `git push` streamed through `nono proxy --upstream-proxy
<rein>` → rein → real github and landed** (`git-receive-pack cl=-1` at rein). The
cap was an artifact of *nono* injecting; when rein injects, it's gone — for git
push **and** LFS / release-asset / large-REST uploads alike.

## The forcing function (state it honestly)

Install friction alone does not justify a core-substrate swap. The real driver:
**srt is drifting away from the `mitmProxy.socketPath` hook rein depends on** —
srt's newer config format lists custom-proxy support as "not yet supported…
future release" (spike findings). If srt breaks rein's injection hook, rein needs
a new front-end regardless; nono's opaque external-proxy tunnel is a candidate
replacement for exactly that hook. (Caveat: nono is *also* pre-1.0 and churny —
this is choosing between two moving dependencies, not stable→unstable.)

## Architecture (b): division of labor

| Concern | Owner | Mechanism |
|---|---|---|
| Sandbox / containment | **nono** | Landlock profile (`deny_credentials`, fs, seccomp) |
| github egress transport | **nono** | opaque **external-proxy** CONNECT tunnel to rein (no terminate, no buffer) |
| TLS-terminate + inject + stream + declare-tap | **rein** | the existing `internal/proxy` (minimized + fuzzed), fronted by nono's tunnel instead of srt's socket |
| Mint / per-issue scope / approval / declare | **rein** | `internal/broker` etc., App key local |
| Egress control | **nono** ← rein | rein writes a tightened `allow_domain` + `upstream_proxy` profile |
| Containment **assurance** | **rein** | sandbox prober: fail-closed launch gate + CI differential harness |
| Install / orchestration | **rein** | verified nono fetch + profile gen + CLI drive |

Integration boundary = **nono CLI + JSON profile** (no SDK — nono's Go binding is
a thin, less-capable FFI). nono's front-end to rein is a **standard forward
CONNECT** (rein is "the enterprise proxy"), which is why rein's proxy needs no
`cmd://` and why the CLAUDE.md goproxy-rejection rationale (tied to srt's socket)
relaxes — though we still keep stdlib, see "Libraries".

## The one open gate (must close before committing)

**`nono run` (sandbox mode) did not route to the upstream proxy in the spike.**
The standalone `nono proxy --upstream-proxy <rein>` works; `nono run` with the
same setting (CLI flag *and* profile `network.upstream_proxy`) aborts the CONNECT
before contacting rein, and the supervised proxy's reason is not surfaced
(`NONO_LOG`/`NONO_PROXY_LOG`/session dir all silent). Source shows the wiring
*should* be present (`proxy_runtime.rs:2412`). **This is the #1 unknown** — resolve
it (a nono-maintainer question / issue, or the correct supervised config) before
any cutover. If it proves unfixable, the fallback composition to evaluate is
"nono sandbox + rein's proxy as the sandbox's forced `HTTPS_PROXY`, egress-confined
to rein" — but that needs its own spike (nono forces its own proxy env today).

## What we TEAR OUT

| Area | LOC | Why |
|---|---|---|
| `internal/srt` | ~2030 | nono owns the sandbox (Landlock `deny_credentials` proven to hide `app.pem`/gh/ssh/history). |
| `cmd/rein/run_sandboxed.go`, `sandbox_home.go`, `sandbox_claude_home.go` | large | srt launch + `~/.claude` bind/overlay (#94) → nono profile fs policy. |
| srt-specific `doctor` checks (bwrap/userns/seccomp) | part | → nono health checks. |

Note vs the old draft: **`internal/proxy` is NOT torn out** — under (b) it is the
injection layer and stays (minimized + fuzzed). Only its front-end changes (srt
socket → nono external-proxy CONNECT).

## What we KEEP

`internal/broker` + `brokercore` (mint/scope/approval), `keystore` (App key,
on-box), `githubapp`/`tokencache`/`ghsession`, `approvals`/`declare`/`issuemeta`
(#35), `classify` (tier — still body-based, which (b) preserves), `runbroker`/
`runscope`/`session`, `gitidentity`, `appsetup`/`init.go` (extended to install
nono), **`internal/proxy` (minimized + fuzzed)**, and direct mode (Shape B,
untouched).

## What's NEW

1. **Verified nono install (the `curl | sh` fix).** `rein init` fetches a *pinned*
   nono release + verifies signature (Sigstore-founder project; releases signed) +
   SHA-256 digest before installing; `rein doctor` re-checks. (Confirm nono's exact
   signing mechanism during P0.)
2. **Opinionated profile generator** — nono profile: `upstream_proxy` → rein's
   proxy, tightened `allow_domain` (rein's strict egress; nono's stock profile is
   open), fs deny-read parity, CA-trust env so the sandboxed client trusts rein's
   leaf.
3. **Proxy front-end swap** — `internal/proxy` accepts nono's external-proxy
   CONNECT (a normal forward-proxy entry) instead of srt's `mitmProxy.socketPath`.
4. **The sandbox prober** (its own section) as the trust-but-verify layer.

## Assurance: the sandbox prober

Ceding containment to a pre-1.0 third party rein doesn't control makes the prober
(`docs/containment-probe-harness.md`) load-bearing, in two layers:
1. **Launch-time gate** (fail-closed, every run) — the nono analog of the deleted
   `VerifyConfigApplied`: sentinel read-back + App-key-unreadable probe; refuse to
   launch if nono didn't confine as configured.
2. **CI differential harness** — `controlplaneio/sandbox-probe` inside `nono run`
   vs host, classified against rein's config oracle. Test-only (like `pyte`).
It is the **acceptance gate for the P3 cutover**.

## Libraries: keep the hand-rolled stdlib proxy

Research verdict (findings §"Follow-up spike"): **no maintained Go proxy library
fits** — goproxy (you'd gut its CONNECT layer, keep every rein invariant custom,
add a 6.7k-line audit liability), go-mitmproxy (buffers by default — OOM footgun
for streaming pushes), gomitmproxy (GPL-3.0, incompatible with a shipped binary).
Infisical's `agent-vault` — the exact use case — also chose stdlib. rein's
forward-MITM core is ~40 lines of stdlib; the security value (SNI==Host,
no-token-on-response, HTTP/1.1 pin, the pkt-line declare tap) is custom regardless.
**Minimize by deletion + fuzz (issue #136); do not adopt a library, do not rewrite
on `httputil.ReverseProxy` (contradicts CLAUDE.md; retracted).**

## How we'd TEST

Golden-transcript journeys stay. **Keep** the broker journeys (`write_ceremony`,
`scope_expansion`, `git_author`, `gh_write`, `push_upstream`, `tmux_popup_approval`,
`credential_boundary`, `claude_resume`, `init_*`) — declare→approve→push-lands is
substrate-agnostic; re-point srt→nono, regenerate goldens. **Drop** the
srt-containment tests (`sandbox_filesystem`, `VerifyConfigApplied`, seccomp,
/dev/tty) with `internal/srt` → replaced by the prober. **Add**: `nono_install`
(verified fetch), `git_push_via_nono` (the composed nono→rein→github stack, incl.
>16 MiB — the headline), and the **fuzzing track** (issue #136: `ParseReceivePack`
+ classifier). Net test surface shrinks; investment moves to fuzzing the one
custom component we keep.

## Risks & open questions

- **The `nono run` upstream-proxy gate (above)** — the biggest, unresolved.
- **nono is a new TCB root** — it tunnels (not terminates) github, so it sees only
  encrypted CONNECT for that path (good — rein holds the plaintext + tokens). But
  it fully controls the sandbox; a nono compromise = containment loss. Add a
  "nono compromised" row to the threat model: rein still guarantees key custody +
  approval UX, nothing about containment.
- **"Stronger sandbox" is unmeasured** — run the srt-vs-nono channel diff (env,
  sockets, TTY, pid-ns, seccomp, fs, egress) before asserting it; both substrates
  + the probe are available now.
- **nono pre-1.0 churn** — pin + re-verify profile schema/CLI on bump (existing srt
  policy). The `upstream_proxy` field + external-proxy tunnel are stable-shaped.
- **macOS = Seatbelt**, different mechanism; re-derive containment there (CP5 track).
- **Egress default open** in nono's stock profile — rein's generated profile
  tightens it.
- **git proxy auth** — git doesn't preempt Proxy-Authorization; rein's relay path
  must use `--no-auth` on a loopback-only relay or handle 407.
- **Large non-push uploads** (LFS, release assets) — under (b) they flow through
  rein's streaming proxy too, so the cap doesn't apply. (This was a (c)-only risk.)
- **Direct mode** diverges; separate call.

## How we'd develop this

Long-lived `nono` integration branch off main (Tom's direction): phases as PRs
*into* `nono`, kept continuously synced with main to avoid drift, always
green/runnable (srt default on-branch until P3). **Carve-out lands on main NOW,
independently** (fuzz relay + adopt sandbox-probe vs current srt — issue #136):
pure wins, zero dependency, and its outputs feed the decision. Cutover (P3) is one
atomic reviewed PR: flip default srt→nono, delete `internal/srt`, gated on P4
dogfood + prober acceptance; rollback = revert it. Per-phase: advisor before,
reviewer subagent after.

## Phasing

- **P0 — Install + profile:** verified nono fetch, opinionated profile gen,
  `doctor` nono health. Journey: `nono_install`.
- **P1 — Compose + close the gate:** re-front `internal/proxy` on nono's
  external-proxy; **resolve the `nono run` upstream-proxy routing (the open gate)**;
  add the prober (launch gate + CI harness). Journeys: `git_push_via_nono`,
  containment diff. *P1 is where the pivot lives or dies.*
- **P2 — Minimize + fuzz the proxy:** delete dead arms; fuzz `ParseReceivePack` +
  classifier (issue #136); independent security review of the relay.
- **P3 — Cutover:** default srt→nono, delete `internal/srt`; gated on the prober +
  P4 dogfood.
- **P4 — Dogfood** on a throwaway, then wrangle (existing CP6 gate).

## The decision

(b) is the attractive pivot: shed srt's sandbox composition + install friction,
keep a small fuzzed stdlib proxy + the broker, on a stronger-to-install (if
unmeasured-stronger) sandbox — genuinely minimizing rein's security-critical
surface. But it is **gated on one unresolved unknown** (the `nono run`
upstream-proxy routing) and a set of honest risks (new TCB root, pre-1.0 churn,
macOS parity). Recommendation: **land the carve-out (#136) now; make P1's gate a
hard go/no-go spike with kill criteria** — if `nono run` can't be made to route to
rein's proxy, the pivot does not proceed on this design. Do not green-light P3
until P1's gate is closed and the srt-vs-nono channel diff is measured.
