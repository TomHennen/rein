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

## The composed stack is PROVEN (former #1 gate — RESOLVED)

Empirically, `nono run` (full sandbox mode) → nono external-proxy → rein's proxy
→ real github: a small AND a **20 MiB chunked `git push` both landed** (findings
§"RESOLVED"). It was never a design-boundary refusal (the in-sandbox CONNECT
returns 502, not 403, and nono does reach the external proxy). Two production
requirements fell out, both cheap and profile-carried:
- **`http.proxyAuthMethod=basic`** in the sandbox git config — nono's external-
  proxy path uses strict CONNECT-auth and git doesn't preempt `Proxy-Authorization`
  by default (curl does). rein's opinionated profile sets this.
- rein's proxy should **require proxy-auth** (a per-session secret), carried in
  nono's `external_proxy.auth` (`ExternalProxyAuth` exists in nono) — see the
  listener-capability requirement below.

## Design requirements surfaced by the re-review (must be in P1)

The second review confirmed (b) fixes (c)'s problems but flagged real new items.
**All four were then validated** (empirically / in source — findings
§"Design-requirement validation"); status noted per item:

1. **Listener capability (the socket→TCP regression).** Today the token-injection
   capability is a placement-checked unix socket (`internal/proxy/placement.go`,
   design §5.3). Under (b) rein listens on **loopback TCP** for nono to CONNECT to
   — where any local process could otherwise reach it. Mitigation (required, not
   optional): a **per-session ephemeral port + mandatory proxy-auth secret** on
   rein's listener, carried in the generated profile's `external_proxy.auth`
   (verified: nono supports upstream `Proxy-Authorization`), plus per-session token
   binding. This must replace `placement.go`'s guarantees explicitly.
2. **CA trust is env-based + fail-closed (a nono-model difference).** nono uses
   Landlock with NO mount namespaces, so the review's "bind rein's CA read-only over
   the system trust path" is **not available** — you can't mount-substitute a file.
   CA trust is therefore env/config-based (`SSL_CERT_FILE`/`GIT_SSL_CAINFO`/git
   `http.sslCAInfo`), the same model nono uses for its own intercept CA. It **fails
   closed** (unsetting the CA var → TLS failure, not a bypass). Mitigation:
   `GIT_CONFIG_GLOBAL=/dev/null` pinning + accept the env model (there is no fs-bind
   option under Landlock). Status: validated for the **git `http.sslCAInfo` path**
   (the composed push trusted rein's CA that way); **general env-CA trust for
   arbitrary tools** (`NODE_EXTRA_CA_CERTS` et al. — the #94-style territory) is
   **untested** and stays a P1 item.
3. **Host-routing table (carry srt gap #6 forward) — VALIDATED.** Tested:
   `upstream_bypass` routes bypassed hosts direct while github goes to rein
   (github → rein's MITM; `example.com` in `upstream_bypass` → direct, never touched
   rein). So the profile puts **only the exact inject hosts** (api/github/uploads) on
   the `upstream_proxy` path and the CDN hosts (codeload/objects/raw) in
   `upstream_bypass` → direct, never reaching rein (no token on a pre-signed asset
   URL). rein's per-host-class inject stays the second line.
4. **Egress — direct-TCP blocked (strong), but DNS is an open exfil channel (a
   real residual, NOT full parity with srt).** Tested: direct TCP `connect` to
   non-allowlisted hosts is **blocked with `PermissionError`** — nono enforces egress
   host-scoped via seccomp user-notification (validates every `connect`/`bind`),
   compensating for Landlock's port-only scope; documented + fail-closed (refuses
   proxy-only mode on WSL2). **BUT** the seccomp filter mediates `connect`/`bind`,
   not UDP `sendto`: a direct UDP DNS query to `8.8.8.8:53` is **reachable**, so
   **DNS is a live low-bandwidth exfil channel** that srt's netns blocks — nono is
   weaker here. Required: check whether a nono UDP-egress / `block:true` setting
   closes DNS; if not, document it as a residual (rein's current srt threat model
   assumes no direct egress at all). Keep TCP-bypass **and** a DNS-exfil probe in the
   prober. WSL2 → rein fails closed.

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
- **P1 — Compose + harden:** re-front `internal/proxy` on nono's external-proxy
  (routing already proven end-to-end); implement the four §"Design requirements"
  (listener proxy-auth + per-session binding, CA-via-fs, host-routing table,
  egress-bypass probe); add the prober (launch gate + CI harness). Journeys:
  `git_push_via_nono`, containment diff.
- **P2 — Minimize + fuzz the proxy:** delete dead arms; fuzz `ParseReceivePack` +
  classifier (issue #136); independent security review of the relay.
- **P3 — Cutover:** default srt→nono, delete `internal/srt`; gated on the prober +
  P4 dogfood.
- **P4 — Dogfood** on a throwaway, then wrangle (existing CP6 gate).

## The decision

(b) is the attractive pivot: shed srt's sandbox composition + install friction,
keep a small fuzzed stdlib proxy + the broker, minimizing rein's security-critical
surface. **The load-bearing integration is proven** — a 20 MiB chunked `git push`
lands through the full `nono run` → nono-tunnel → rein-inject → github stack — so
the pivot is no longer gated on an unknown, only on *engineering* the four design
requirements (listener auth, CA-via-fs, host-routing, egress probe) and honest
risks that are now **much smaller** after the design-requirement validation:
direct-TCP egress is blocked (seccomp-notify — the biggest concern largely holds up,
with one caveat), host-routing is exact (`upstream_bypass`), the listener capability
is closeable (per-session proxy-auth, source-confirmed), and the TCB root is smaller
under (b) (nono sees only github ciphertext). The remaining genuine unknowns/gaps:
**the DNS exfil channel** (nono allows UDP DNS; srt's netns blocks it — mitigate or
document), **macOS (Seatbelt) parity** (untested), and **nono pre-1.0 maturity**. Recommendation: **land the carve-out
(#136) now, unconditionally**; then run P0/P1 (behind a flag, srt default), with
**macOS parity** and the **listener per-session proxy-auth** as the hard gates
before P3 cutover. Build-vs-adopt on a pre-1.0 dependency remains Tom's call — and
the Linux empirics now support it strongly.
