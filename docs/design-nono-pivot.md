# Design: rein on nono (built)

**Status:** Design of record for the nono sandbox — **now the default**. This
describes the design as built: nono runs the agent, srt is deleted, and the P0–P3
work below has landed (the P3 cutover flipped the default and removed srt in one
commit). **Scope: Linux.** macOS uses a different sandbox (Seatbelt) and is an open
item — a caveat in §8, not covered here.

**Evidence:** `docs/nono-git-push-spike-findings.md` (what we tested),
`docs/proposal-rein-on-nono.md` (the approved WHAT),
`docs/design-nono-profile-schema.md` (the exact nono profile fields — authoritative),
`docs/containment-probe-harness.md` (the prober).

## The shape: nono tunnels, rein injects

nono runs the agent in a sandbox and **tunnels** its GitHub traffic to rein as an
opaque byte stream — nono never opens it, buffers it, or adds the token. **rein**
terminates the TLS, injects the GitHub token, streams the body, and reads the push.

We rejected the alternative where nono injects the token. That path only ever sees
`{host, path, method, session}` — never the request body — so it can't run rein's
per-push checks, and small pushes would slip past the approval gate. This design has
no token injection in nono and no `cmd://` or `credential_capture` anywhere. Reject
that language if it shows up in a PR.

---

## 1. Architecture

```
  ┌─ nono sandbox (Landlock + seccomp) ────────────────────┐
  │  agent (claude/git/gh)                                  │
  │    git http.proxy ─┐  http.proxyAuthMethod=basic        │
  │                    │  CA trust via env (SSL_CERT_FILE…) │
  └────────────────────┼───────────────────────────────────┘
                       │  nono's proxy tunnels out (opaque CONNECT)
                       ▼
        rein loopback listener  (127.0.0.1:<port>)
          ├─ terminate TLS on SNI, inject token, STREAM body
          ├─ read the push (declare tap) + classify the request
          └─ upstream ──► github.com / api / uploads
   CDN hosts (codeload/objects/raw) ──► nono sends direct (never reach rein)
```

Proven on Linux (aarch64 + x86_64, real nono 0.68.0, real github.com):

- **No size cap.** A small push and a **20 MiB chunked `git push`** both land through
  `nono run` → nono's proxy → rein → GitHub. The earlier 16 MiB cap came from nono
  buffering the body when *nono* did the injecting; when nono only tunnels it never
  buffers, so there is no cap.
- **git needs one config flag.** nono's proxy demands proxy credentials on every
  CONNECT; plain git doesn't send them and nono drops the connection, so the sandbox
  git config sets `http.proxyAuthMethod=basic`. This is git authenticating to *nono's
  own* proxy, separate from anything on the rein hop (§3a/§3c).
- **Exact-host routing works.** With CDN hosts in `upstream_bypass` and rein in
  `upstream_proxy`, `github.com` reaches rein while other hosts go straight out (rein
  never sees them) — so no token can land on a pre-signed CDN asset URL.
- **nono hides credentials.** With `deny_credentials` on, the App key
  (`~/.config/rein-credentials/app.pem`), the gh token, ssh keys, and bash history all
  fail to open inside the sandbox (readable on the host).
- **No direct TCP out.** nono's supervisor checks every `connect`/`bind` and refuses
  ones that aren't allowed.
- **UDP is open by default — all of it** (DNS, QUIC, anything). nono *can* route UDP
  sends through its supervisor, but the default lets them through and we found no
  setting that tightens it. A real residual (§3d).
- **No loopback port reachable except nono's own proxy.** A sandboxed agent's raw
  `connect()` to an arbitrary `127.0.0.1` port is refused, while going through nono's
  proxy works. This is what lets rein's loopback listener run without a password (§3a).

Integration is **command line + a JSON profile file**, not a library:
`nono run --profile <file> -- <agent>`. No linking.

---

## 2. P0 — installer + profile generator + doctor

Package **`internal/nono`**: `installer.go`, `profile.go`, `preflight.go` (doctor
wiring stays in `cmd/rein/doctor.go`).

**The profile schema (authoritative facts).**
`docs/design-nono-profile-schema.md` is the authoritative field reference and
`docs/nono-profile-sample.json` is a profile that launches. The generator honors:

- The profile is **nested** (`network.*`, `filesystem.*`, `linux.*`, `environment.*`,
  `groups.include`), not a flat struct.
- `upstream_proxy` is a **bare `host:port` string**, not a URL.
- `deny_credentials` is a **policy group** turned on via
  `groups.include: ["deny_credentials"]`, not a list of paths.
- Env vars go in **`environment.set_vars`**.
- The filesystem is default-deny, so the CA cert is granted via `filesystem.read_file`.
  `filesystem.deny` is a no-op on Linux — grant nothing extra rather than deny.
- **nono owns `HTTP(S)_PROXY`/`NO_PROXY`** — it points them at its own proxy and
  overrides anything rein sets. rein must not set them; CDN bypass uses
  `upstream_bypass`, not the agent's `NO_PROXY` (§3c).
- **There is no password on rein's listener.** nono 0.68.0's `external_proxy.auth` is
  unimplemented — the schema rejects it and the source hardcodes it off. The design
  doesn't need it (§3a).
- The schema dump **omits** the `filesystem.unix_socket*` grant fields the profile
  actually accepts — don't treat the dump as complete.

### 2.1 Installer (`internal/nono/installer.go`)

nono's own install script downloads a release, fetches `SHA256SUMS.txt`, and checks
the SHA-256. nono 0.68.0 **also** publishes sigstore-backed GitHub build-provenance
attestations (verified: `gh attestation verify` passes on the real tarball, fails on a
tampered byte). rein's installer still uses digest-only — see the decision below.

**rein's trust floor is a SHA-256 digest pinned in rein's own source.** Fetching
`SHA256SUMS.txt` over the same connection as the binary is not independent trust — one
compromise serves both. So rein pins the digest in its own source (covered by rein's
supply-chain hygiene) and verifies the download against that pin.

The pin holds **two** digests per (version, platform): the tarball digest gates the
download; the inner-binary digest is what `VerifyInstalled` re-hashes on disk
(re-hashing the on-disk binary against a *tarball* digest would never match).
`PinnedVersion = "0.68.0"`; the platform key is nono's rustc target triple (e.g.
`x86_64-unknown-linux-gnu`).

`Install` downloads the tarball → verifies its digest → extracts the inner binary
(**rejecting any `..` or absolute tar member**) → verifies the binary digest →
atomically places it at `DestDir/nono` (0o755). Fail-closed: any digest mismatch,
missing pin, tar path-traversal, or partial download deletes temp files and returns an
error — it never installs an unverified binary, and never falls through to trusting
`SHA256SUMS.txt`. `VerifyInstalled` re-hashes the on-disk binary against the pin; it's
used by doctor and the launch path.

**Decision (locked): digest-only for the cutover; attestation verify is the upgrade
path (#142).** For a *pinned* version, a vendored in-source digest is already the
strongest trust root — a fully compromised release (forged attestations included)
can't pass rein's pin without editing rein's SLSA-covered source. Attestation
verification would add a large `sigstore-go`-class dependency, a supply-chain decision
worth making deliberately. Tradeoff: digest-only gives no *independent* install-time
provenance, so on a version bump whoever regenerates the pin verifies the new bytes
out of band (`gh attestation verify` is the right tool for that manual step).

#### 2.1.1 rein-managed path (closes binary-shadowing)

srt was found via
`exec.LookPath("srt")`, a `$PATH` entry the agent's environment could shadow. nono is
run by **absolute path** from `managedNonoDir()` = `<ConfigDir>/nono/bin`, never
`LookPath`. `rein init` installs there; `rein run` runs exactly that path after
`VerifyInstalled` passes.

### 2.2 Profile generator (`internal/nono/profile.go`)

`Build` emits the exact JSON profile handed to `nono run --profile`; `run_nono.go`
writes it on each launch. The struct mirrors nono's nested schema
(`Profile{Groups, Network, Linux, Filesystem, Environment}`). Key field facts, all
enforced against permissive mistakes:

- `Network.Block` is false (true is incompatible with proxy mode).
- `AllowDomain` = `InjectHosts ∪ CDNHosts ∪ ExtraDomains ∪ DeclareHost`.
- `UpstreamProxy` routes to rein; `UpstreamBypass` = `CDNHosts` verbatim (direct,
  never rein). No auth field exists — don't emit `external_proxy.auth`.
- `InjectHosts` + `DeclareHost` are **never** in `UpstreamBypass` (they must reach
  rein). `ExtraDomains` are **never injected** (egress-only direct TLS).
- `Linux.AfUnixMediation = "pathname"` (the approval-channel isolation control, §3e).
- `Filesystem.ReadFile` grants the CA PEM; `Filesystem.UnixSocket` starts **empty** and
  never lists the tmux/approval socket.
- `Environment.SetVars` carries the four CA vars + `GIT_CONFIG_*` — and **not**
  `HTTP(S)_PROXY`/`NO_PROXY`/`PATH`/`NONO_*` (nono owns those).

`Build` fails closed on any violation. Host lists (`InjectHosts`, `CDNHosts`,
`DeclareHost`) stay in `internal/proxy/hosts.go`; the generator reads them so the
profile and the proxy never drift. The CA PEM lives in its own agent-readable dir — a
dir-wide read grant shouldn't drag other files in with it.

### 2.3 doctor nono health rows (`internal/nono/preflight.go`)

`Preflight` returns the rows `rein run` hard-gates on; doctor prints them read-only.

| Row | Check | Hard fail? |
|---|---|---|
| `nono present` | `ManagedNonoPath()` exists + on-disk digest == pin | yes |
| `nono profile validate` | generated rein profile passes `nono profile validate` | yes |
| `nono af_unix_mediation` | pinned nono's schema accepts `af_unix_mediation:"pathname"` | yes |
| `rein CA` | rein CA PEM present + non-empty | warn (minted lazily) |
| `loopback proxy port` | a 127.0.0.1 TCP port is bindable | warn |

Landlock/seccomp host support isn't a separate doctor row: the launch-gate containment
probe (§3e) fails closed if the sandbox isn't actually enforcing, which subsumes a
static host-capability check. The `Check`/`Status` types are the neutral
`internal/sandboxutil` ones (extracted so `internal/nono` never imported
`internal/srt`). The launch path hard-gates the same rows and fails closed — never a
silent drop to a weaker mode.

---

## 3. P1 — compose: proxy front, CA, routing, UDP, prober

**`cmd/rein/run_nono.go`** (`runNono`) is the nono run path, reusing the
broker/mint/scope/declare/approval spine unchanged (`startRunBroker`, run_broker.go);
only the *front* of the proxy and the *sandbox launch* differ.

### 3a. Proxy front: loopback TCP, no password

Under srt, rein's proxy listened on a **unix socket** whose parent-directory
permissions (plus being outside every bind-mount) kept the agent out. A **loopback TCP
port** is different: normally anything that can `connect(127.0.0.1:port)` reaches it.
The question was whether moving to TCP opened a hole.

**It doesn't, because nono blocks it.** A nono-sandboxed agent's raw `connect()` to an
arbitrary loopback port is refused, while going through nono's own proxy works. nono
lets the agent reach only nono's proxy on loopback, so **the agent cannot reach rein's
listener directly.** rein's listener therefore needs no password — nono is the only
thing that can reach it. (nono 0.68.0 also can't carry a proxy password:
`external_proxy.auth` is unimplemented. We don't need it.)

**Even a direct hit wouldn't hand over the token** (defense in depth). Every accepted
connection runs the same pipeline: terminate TLS → inject token → read the push
(declare tap) → classify → upstream. The token is injected on the rein→GitHub leg,
**downstream of the sandbox**, so its value never enters the sandbox no matter how the
connection arrived. Reaching the port another way buys no capability — the agent
already has a legitimate token-injected tunnel through nono, so it would just be a
second path into the same declare-gated, classified pipeline. The real gates are:
declare + classifier + downstream injection + **approval-channel isolation** (§3e — the
crux). *Caveat to keep verified:* that the pipeline runs the declare tap and classifier
on *every* accepted connection is inferred from the diagram, not separately proven.

**Additive build; deletions came at P3.** srt stayed the default until the P3 cutover,
so P1a **added** the loopback-TCP front alongside the unix-socket front (both compiled;
`Start` picks based on which config is set) rather than replacing it. That's what kept
"rollback = revert one PR" true across the branch. The unix-socket placement machinery
was removed only at the P3 cutover.

**`internal/proxy` shape:**

- **`proxy.go handleConn`** consumes a CONNECT preamble and also supports a direct TLS
  client; under nono a CONNECT is mandatory. The no-SNI case stays fail-closed. No
  proxy-auth parsing (nono is the only reachable client; any `Proxy-Authorization` is
  drained and ignored).
- **Injection is an unconditional overwrite**, not add-if-absent: rein replaces any
  `Authorization` the agent supplied before forwarding upstream. (Low risk — the agent
  has no valid token — but overwrite is the stated invariant.)
- **`ListenLoopback`** binds `127.0.0.1` explicitly (never `0.0.0.0`/`::1`) with no
  placement check — nono's loopback mediation is the capability gate, not filesystem
  perms. The srt-era `CheckPlacement`/`pathWithin` and the unix-socket `Listen` body
  were removed at P3; `ResolveAbs`/`cleanAbs` were kept (still used to resolve the
  working tree).

`declare.rein.internal` (a `LocalHost`) rides the same listener: nono CONNECTs to it by
hostname, rein terminates with its CA and answers locally (token-free). It must be in
`allow_domain` and must **not** be in `upstream_bypass` (that would send it direct →
unresolvable). *Caveat:* that nono tunnels an *unresolvable* virtual host by its CONNECT
hostname without doing DNS was asserted from the spike (which used real github); the
declare journeys exercise it, but it remains the heart of the write gate to keep an eye
on across nono bumps.

**Invariant — declare informs, it cannot approve.** The agent *can* reach
`declare.rein.internal` through the tunnel. Safety rests entirely on approval being
gated out-of-band on the terminal (§3e): the declare HTTP surface may *state* intent but
must never *drive* an approval.

**Hard-constraint #2 + denial semantics.**

- **Credential helper:** rein injects host-side, so there is no sandbox-side git
  credential helper — #2's "always return a credential" is **moot** here.
- **Denial semantics:** when a push is not approved at the declare gate, the agent's
  git gets a **clean fail-closed HTTP status**, not a hang or an empty-credential retry
  loop.
- **Token-leak boundary:** rein injects downstream of the sandbox, so even a
  direct-to-port connection yields authenticated *actions*, never the token *value* —
  provided (a) no inject host reflects the `Authorization` header back (all inject hosts
  are real github, none echo auth — asserted, not separately confirmed) and (b)
  `ExtraDomains` are never injected (enforced in `Build`). No reflection oracle ⇒ no
  token exfil.

### 3b. CA trust via env (fail-closed)

nono uses Landlock and has **no mount namespaces**, so srt's trick (bind rein's CA
bundle over the system trust path) isn't available. CA trust is **env/config based**:

- rein writes its CA PEM to the agent-readable CA dir, and `set_vars` points **all
  four** CA vars at it — `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`, and
  **`CURL_CA_BUNDLE`** (dropping `CURL_CA_BUNDLE` loses curl/libcurl trust). These
  constants live in the neutral `internal/sandboxutil` package.
- **Hard-constraint #6:** the PEM written here is the CA *cert* (public), fine to
  expose. rein's MITM CA *signing key* still loads through `internal/keystore`
  (Get/Fingerprint), never `os.ReadFile` — `proxy/ca.go` carries that path forward (a
  keystore-backed ECDSA P-256 key). (rein's CA is already ECDSA P-256, so the old "nono
  ring requires EC P-256" worry doesn't apply — nono tunnels, it doesn't terminate
  rein's CA.)
- **Fails closed:** an agent that unsets the CA var gets a TLS *failure* (rein's leaf is
  untrusted), not a bypass — there's no unmediated path to a real github cert (§3c).
  Fail-closed rests on the egress allowlist + nono's mediation, not on the git-config
  default below.
- **`GIT_CONFIG_GLOBAL` is a convenience default, not a control.** git's local repo
  config, `-c` flags, and `GIT_SSL_*`/`http_proxy` env all override it, so the agent
  *can* override `http.sslCAInfo`/`http.proxy`/`http.sslVerify`. That's fine: none of
  those overrides yields a bypass, because the real controls are the egress allowlist
  and nono's mediation. Overriding local config is exactly *how* an agent would try the
  direct-github path — which is blocked (§3c).

*Caveat:* the spike proved CA trust for git only. A non-git tool (node/python) reading
rein's CA under Landlock — including whether the CA file needs an explicit read grant —
was not separately spiked; verify for both git and a non-git tool (§8).

### 3c. Host routing (exact hosts → rein, CDN → direct)

`upstream_proxy` = rein's listener; `upstream_bypass` = `CDNHosts`; only `InjectHosts`
+ `DeclareHost` reach rein. The sandbox git config sets **`http.proxyAuthMethod=basic`**
so git sends its proxy credentials up front to nono's proxy. (nono injects a
`nono:<token>` basic credential into the agent's `HTTPS_PROXY`; that token authenticates
only to nono's own proxy and is fine for the agent to see. The GitHub token is injected
downstream by rein and never enters the sandbox.)

- **Non-git tools need proxy env — nono provides it.** `gh`, node, and python don't read
  git config, but nono sets their `HTTPS_PROXY` (nono owns those vars, §2.1). rein must
  not set `HTTP(S)_PROXY`/`NO_PROXY`; nono overrides them anyway. CDN bypass is
  `upstream_bypass`, not the agent's `NO_PROXY` (nono pins that to
  `localhost,127.0.0.1`).
- **No unmediated path to github — VERIFIED.** A direct `connect(github.com:443)` that
  bypasses the proxy (e.g. the agent clears `http.proxy` in the writable `.git/config`)
  is **blocked**: `allow_domain` grants *proxy-mediated* egress, not direct, and seccomp
  refuses the direct connect. The agent's only path to github is through rein; the prober
  keeps this as a regression guard.
- **Two auth hops — don't conflate.** Hop 1 is git → nono (nono's own proxy credential,
  `http.proxyAuthMethod=basic`). Hop 2 is nono → rein (a plain CONNECT tunnel, no auth —
  §3a).

### 3d. UDP policy — accept the residual, document it

Direct TCP egress is strongly blocked (seccomp refuses it, same as srt). **UDP egress is
open by default — all of it** (DNS, QUIC, arbitrary): inside `nono run`, `sendto` to
`8.8.8.8:53`, `8.8.8.8:12345`, and `1.1.1.1:443` all succeed. srt's empty network
namespace blocked this. nono *can* route UDP sends through its supervisor
(`sandbox/linux.rs:2064` routes `sendto`/`sendmsg`/`sendmmsg`), but the default allows
them.

**No clean fix under nono.** There is no fine-grained UDP control: the only knob is
`network.block` (all-or-nothing), and `block: true` is **incompatible with proxy mode**
(nono errors — domain filtering requires proxy mode), so it can't be combined with rein's
proxy. Landlock doesn't touch UDP either. Under the proxy mode rein requires, **UDP stays
open** — a general exfil channel that srt's empty namespace blocked. Unlike the loopback
question (which nono's mediation closes), no nono config fixes this.

**Decision (locked): accept the residual, document it.** rein's threat model weights
credential theft over data exfil, and the credential boundary is intact: the agent can't
get a token (proven) and can't exfil over TCP (blocked). What it *can* do is send out
data it already sees (the working tree) over UDP. A prompt-injected agent could exfiltrate
the working tree over UDP (it could already read that tree; it just gains an out-of-band
send path srt denied). The credential the whole product exists to protect never leaks.
The prober **reports** UDP as open (loud warning, not a launch failure) so it's never
silent.

**Mitigation deferred:** a host-side firewall (nftables) blocking UDP egress from the
sandboxed process — but scoping it to just the agent is hard under nono (same uid, no
network namespace; would need a cgroup nono exposes). Tracked as future work.

### 3e. The prober (fail-closed launch gate + CI harness)

Two layers, per `docs/containment-probe-harness.md`, retargeted from srt to nono.

**The crux — approval-channel isolation.** srt isolated the agent with namespaces. nono
has **no namespaces** (Landlock + seccomp only), so the agent runs as an ordinary host
process at rein's uid in the same terminal session. Nothing *obviously* stops it from
(a) opening `/dev/tty` on the shared terminal, or (b) connecting to the **tmux server
socket** and driving the approval popup with `tmux send-keys` — self-approving its own
push through the legit tunnel while the journey tests stay green.

**We confirmed a real hole, and the obvious fix doesn't work.** Inside `nono run` with a
default-ish profile the agent got `$TMUX` passed through (which leaks the socket path);
it could not `ls` the tmux dir, but it **did `connect()` to the tmux socket and
`send-keys` a command that ran** — self-approval proven. (`/dev/tty` was blocked — a
tty-based approval is safe.) The naive fix fails: adding `/tmp/tmux-<uid>` to
`filesystem.deny` makes nono **refuse to launch** (Landlock has no "deny under an allowed
parent"), and nono's shared-memory IPC restriction only blocks *abstract* sockets, not
filesystem sockets like tmux's.

**Resolved by `linux.af_unix_mediation: "pathname"`.** nono ships this field, default
`off` (which is why the hole was open). Set to **`"pathname"`**, it deny-by-default
mediates pathname AF_UNIX connect/bind; sockets must be granted back via
`filesystem.unix_socket*`. Tested: with `"pathname"` and no socket allowlist, the agent's
`connect()` to the tmux socket is refused, `send-keys` is blocked, and the agent still
runs fine. This constrains **only the sandboxed agent** — rein drives the approval popup
**host-side, outside the sandbox**, so the interactive tmux approval UX is preserved.

The profile sets `af_unix_mediation: "pathname"`, scrubs `$TMUX`/`$TMUX_PANE`
(defense-in-depth — the mediation is the real control), and starts the unix-socket
allowlist **empty**, never granting the tmux/approval socket. Cost of the empty allowlist
is low: DNS, `getent`, git, curl-through-the-proxy, node, and gh all still work; nono
blocked 6 non-essential unix-socket ops and nothing essential broke. The allowlist grows
only if a specific tool needs a specific socket. This preserves rein's non-replayability
guarantee (#32) under nono.

**Layer 1 — launch gate** (`internal/nono/selftest.go` + `verify.go`, the nono
counterpart of srt's `VerifyConfigApplied`). A tiny, dependency-free, in-binary check
that runs on every `rein run` *before* the agent and fails closed. It launches the probe
*through* the real `nono run` path (a hidden `rein __nono-probe` subcommand) so it
inherits the exact profile, and asserts:
- App key / gh / ssh **unreadable**;
- direct TCP to a non-allowlisted host **blocked**;
- direct (non-proxy) connect to an inject host **blocked** (guards §3c);
- **arbitrary loopback ports unreachable except nono's proxy** (the control that lets
  rein's listener run without a password — §3a);
- **arbitrary UDP** reported (open is a warning, not a fail, unless policy opts in — §3d);
- **approval channel unreachable:** `/dev/tty` unopenable, tmux socket unconnectable,
  `send-keys` blocked, `$TMUX` absent;
- **positive control:** an authorized request through nono's tunnel reaches the listener
  and is served — so a mis-provisioned `upstream_proxy` port or a dead listener surfaces
  as a config error instead of silently passing every negative check.

The gate keeps a strict errno discipline: a negative channel is "ok" **only** on an
explicit denial (EPERM/EACCES); success ⇒ leak; anything else ⇒ unknown, never "ok" (a
down network must not read as containment). It reports booleans/errnos, never credential
contents, and never attempts to break out (CLAUDE.md #5). It is a regression + drift
detector for the channels the spike measured, not a proof of confinement.

**Layer 2 — verification harness = `controlplaneio/sandbox-probe`** (Apache-2.0, a
test/CI dependency never linked into the binary — same posture as `pyte`). Run on the
host and through `rein run`, diffed and classified against the emitted profile, emitting
a golden report wired into a `tests/interactive/` journey. Drift = red = re-review — how
we keep trusting nono across version bumps. The prober was landed against **current srt**
first (it's substrate-agnostic) so it existed before the pivot.

---

## 4. P2 — minimize + fuzz the proxy

With nono owning the sandbox and CDN going direct, whole arms of the proxy became dead
and were removed at the P3 cutover (they had to stay while srt was still the default,
since srt has no `upstream_bypass` and CDN hosts *did* reach it):

- **`classPassthrough` deleted** — the CDN-relay arm and its `classifyHost` case for
  `objects/codeload/raw.githubusercontent.com`: under nono, CDN hosts never reach rein
  (they're in `upstream_bypass`). The **`CDNHosts` list stays** in `hosts.go` — the
  profile generator needs it to populate `upstream_bypass`; only the relay arm died.
- **Request-body size gate:** with nono's 16 MiB body cap gone, rein is the only
  body-size gate; the streaming relay must not accumulate the body unbounded (a
  low-severity self-DoS surface).
- **Kept:** the hand-rolled streaming relay (SNI==Host, no token on the response,
  HTTP/1.1 relay, ContentLength/TransferEncoding copy, no-redirect-follow) and the
  receive-pack **declare tap**. These are rein's security-critical differentiators; no
  library fits (goproxy needs a gut-and-audit, go-mitmproxy buffers bodies, gomitmproxy
  is GPL).
- **Fuzz (#136):** `go test -fuzz` against `ParseReceivePackCommands` (receivepack.go)
  and the host/tier classifier (`classifyHost` + the GraphQL read/write tier resolution
  in gate.go), seeded from `receivepack_test.go`/`gate_test.go`. This is the surface an
  adversarial in-sandbox agent actually touches. #136 fuzzing was landed on `main`
  unconditionally (no new dependency, a security win either way).

---

## 5. Removing srt (done)

**srt is deleted.** It stayed fully working behind the default until the **P3 atomic
cutover PR**, which (i) flipped the default `rein run` from srt to nono and (ii) deleted
srt in the same commit. Rollback = revert that one PR — which worked only because P1a/P2
were additive (§3a/§4). The cutover was gated on:
- dogfood-proven nono (a human ran the real agent through the nono stack and pushed real
  code);
- a green prober including the §3e assertions (direct-github blocked, approval-channel
  isolation — tty + tmux socket + send-keys, loopback-only-nono-proxy);
- **UDP policy resolved** — accepted open UDP, documented (§3d);
- green re-pointed journeys.

**Shared substrate was extracted first** (before P1a): `Check`/`Status`, the CA-env
constants, the extra-egress resolver, and the `agentenv` helpers moved into the neutral
`internal/sandboxutil` / `internal/agentenv` packages so `internal/nono` never imported
`internal/srt` during P0–P2.

**What was deleted (P3):**

| Target | Note |
|---|---|
| `internal/srt/*.go` (~3972 LOC incl. tests) | config, env, cabundle, domains, preflight, selftest, socket_*, githard, allowread |
| `cmd/rein/run_sandboxed.go` + test | replaced by `run_nono.go` |
| srt `$HOME`/overlay construction | nono uses `deny_credentials`, not mount overlays; reusable env-scrub helpers were salvaged first |
| `internal/proxy/placement.go` (`CheckPlacement`/`pathWithin`, unix-socket `Listen` body) | additive until P3; `ResolveAbs`/`cleanAbs` were moved, not deleted |
| doctor srt rows | → nono rows (§2.4) |
| `main.go` srt dispatch + preflight | → nono dispatch; default flipped |

The `claude` config-dir property (#94) was **replaced, not dropped**: under nono there's
no overlay, so the agent gets a rein-owned, writable `CLAUDE_CONFIG_DIR` (and gh a
`GH_CONFIG_DIR`) while the real `~/.claude` / `~/.config/gh` stay hidden by default-deny
fs. The `realagent_write`/`claude_resume` journeys depend on it.

**Env-var verdicts (these were srt's filesystem model):**

| Env var | Verdict |
|---|---|
| `REIN_SANDBOX_ALLOW_READ` / `REIN_SANDBOX_SHOW_HOME` | **deleted** — srt deny-read/allow-back mount semantics; nono uses `deny_credentials` + Landlock allow-read. If an escape-hatch is wanted later, add a NEW var under nono semantics rather than carrying the srt one |
| `REIN_SANDBOX_ALLOW_UNHARDENED_GIT` + `githard.go` | **deleted** — guarded srt's `.git` **bind-mount** threat; under nono there's no bind-mount. See the `.git`-hooks residual in §8 |
| `REIN_ALLOW_DOMAINS` | **kept** — operator egress opt-in, substrate-agnostic; the resolver moved to `internal/sandboxutil` (feeds `allow_domain` + `ExtraDomains`, never inject) |
| `REIN_DISABLE_CLAUDE_MCP`, `REIN_REPO_WORKTREES`, `REIN_EPHEMERAL_CLONE_DIR`, `REIN_UPSTREAM_INTENT_FILE`, agent-contract | **kept** — not srt-specific; helpers moved to `internal/agentenv` |

**End state:** `internal/nono` (installer, profile, doctor, selftest) + `internal/proxy`
(streaming inject relay + declare tap + loopback-TCP front, no placement, no
classPassthrough) + unchanged broker/keystore/runscope. `rein run` defaults to nono;
`--direct` is unchanged. ~3972 LOC of srt plus the dead proxy arms are gone; the
security-critical code rein still owns is the small, fuzzed relay.

---

## 6. Testing

- **Broker journeys re-pointed srt→nono, goldens regenerated.** The user-visible path is
  unchanged (declare/approve/push/scope/git-identity/gh-writes), so the journeys in
  `tests/interactive/journeys/` (write_ceremony, gh_write, scope_expansion, multi_repo,
  push_upstream, git_author, expansion_404, sandbox_gh_read_staleness, realagent_write,
  credential_boundary, claude_resume) run against the nono stack. Green = behavior
  preserved.
- **srt-containment tests dropped; the prober replaces them** — the sandbox-probe golden
  report (§3e) against nono.
- **New nono-stack push journey:** a real `git push` through `nono run` → nono's proxy →
  rein → real github (throwaway repo, hard-constraint #1), **including a >16 MiB chunked
  push** (the case that proves the cap is gone). Golden-transcript backed.
- **Unit tests:** `internal/nono` installer (digest match/mismatch/missing-pin,
  atomic-place, unsupported platform — inject `HTTPGet`); profile generator (invariants:
  CDN in bypass not inject; inject/declare not in bypass; extra domains never injected).
- **#136 fuzzing:** `ParseReceivePackCommands` + classifier (§4).

`tests/interactive/CLAUDE.md` rules apply: default-keep golden, `SBX|` view-split, `rein
init` setup. The new push journey and the re-pointed goldens are the human-reviewable
deliverables — green tests alone are not enough.

---

## 7. How it was built

Work happened on a long-lived **`nono` branch**, kept in sync with `main`; `main` kept
shipping srt until the P3 cutover. Each phase was one or a few PRs. Two things landed on
`main` unconditionally (no new runtime dependency, wins either way): the #136 fuzzing and
the prober adopted against current srt.

| PR | Phase | Depends on |
|---|---|---|
| nono profile schema + one working profile | P0.0 | — (gated the profile generator) |
| extract shared substrate (`Check`/`Status`, CA env, domains resolver, agentenv) | P0 | — (before doctor + P1a) |
| `internal/nono` installer + pins (tarball + binary digests) | P0 | — |
| `internal/nono` profile generator | P0 | P0.0 schema + proxy host lists |
| doctor nono health rows | P0 | installer + substrate extract |
| proxy re-front: add loopback TCP front (additive; srt front kept) | P1a | substrate extract |
| `run_nono.go` + dispatch | P1 | P0 + P1a |
| CA-env + host-routing + git proxyAuthMethod | P1b,c | run_nono |
| UDP policy decide | P1d | — |
| claude config-dir nono replacement (#94) | P1 | run_nono |
| prober retarget to nono (launch gate + dispatch + harness) | P1e | run_nono |
| proxy fuzz (classPassthrough deletion deferred to P3) | P2 | — |
| **P3 cutover:** flip default + delete srt + all deferred deletions | P3 | all above + dogfood + green prober + UDP resolved |

The additive discipline (no srt-breaking deletion before P3, §3a/§4) is what kept
"rollback = revert one PR" true. Later, three small follow-ups closed gaps found in
dogfooding: **gap 1** — `rein init` installs the pinned nono runtime; **gap 2** — a
`GH_CONFIG_DIR` overlay so `gh` starts in-sandbox; **gap 3** — the agent contract is
briefed under nono.

---

## 8. Risks and residuals

- **Approval-channel isolation — was the top risk; confirmed then resolved.** The
  sandboxed agent connected to the tmux socket and drove `send-keys` to self-approve
  (`filesystem.deny` does not block socket `connect()`; `/dev/tty` was already blocked).
  Closed by `linux.af_unix_mediation: "pathname"`, which blocks the agent's tmux connect
  while rein keeps the popup host-side. The prober asserts it (§3e); the minimal
  unix-socket allowlist starts empty.
- **UDP exfil (§3d):** open by default, no nono config fixes it. Accepted and documented —
  a data-confidentiality regression from srt's empty namespace. The prober reports it
  loudly.
- **`.git`-hooks host-RCE (#64) — RESIDUAL under nono.** srt ro-bound
  `<tree>/.git/hooks` + `.git/config` so a prompt-injected agent could not plant a hook
  (or `core.fsmonitor`/`core.pager`) that runs AS THE DEVELOPER on the host at their next
  git op. `run_nono` grants the working tree AND every mapped checkout fully writable via
  nono `--allow`, `.git` included, with no read-only carve-out — and Landlock has no "deny
  under an allowed parent" (same limit as the tmux-socket deny), so srt's mechanism
  **cannot be ported as-is**. The threat **survives** for mapped/real checkouts, and the
  containment prober has no write-confinement channel to catch it. The cutover deleted
  `internal/srt/githard.go`; the threat surviving + port being infeasible under Landlock
  is the recorded confirmation. Options for Tom: accept + document like UDP; refuse to
  bind a mapped checkout writable (throwaway-clone-and-push only); or a host-side
  mitigation (fs watch / a nono write-confinement primitive if one lands). **To decide
  before nono runs on non-throwaway checkouts.**
- **F2 anonymous direct-github (§3c):** confirmed blocked — a proxy-bypassing direct
  connect to an inject host is refused by seccomp. The prober keeps it as a regression
  guard.
- **declare virtual-host tunneling (§3a):** that nono tunnels an unresolvable host by its
  CONNECT name is asserted from the spike and exercised by the declare journeys, not
  independently spiked; it's the heart of the write gate, so keep it verified across nono
  bumps.
- **claude config-dir under nono (§5):** no overlay under nono; #94 is met by a per-run
  `CLAUDE_CONFIG_DIR` + `~/.claude` hidden by default-deny fs. The
  `realagent_write`/`claude_resume` journeys guard it.
- **macOS / Seatbelt:** entirely untested. nono uses a different sandbox on macOS. This
  doc is Linux-scoped; a macOS cutover is a separate open gate needing its own spike
  (containment parity, CA-env behavior, and re-verifying the loopback-mediation property
  the passwordless listener depends on — proven only via Linux seccomp so far) before
  flipping the default there. Don't assume Linux results transfer.
- **nono is pre-1.0 and moves fast** (0.68.0; profile fields shift — `deny_domain`,
  `platform_overrides` landed in 0.68.0). Mitigations: `PinnedVersion` + vendored digest +
  re-running the prober's golden report on every bump. We traded one moving dependency
  (srt) for another.
- **CA trust untested beyond git (§3b):** proven for git only; a non-git tool
  (node/python) reading rein's CA under Landlock — including whether the CA file needs an
  explicit read grant — was not separately spiked, so it remains a confirm-item.
- **Full composed profile:** the end-to-end spike proof used a no-auth rein hop, open UDP,
  no declare host, and an untightened profile — the *proven* composition was not the
  *production* one. The production profile (nono→rein tunnel + UDP policy + declare host +
  `GIT_CONFIG_GLOBAL` + `deny_credentials` + `af_unix_mediation`) is what ships and what
  the journeys + launch-gate prober exercise on every run.
- **nono is a new trust root.** nono TLS-terminates nothing rein cares about (rein does
  the github TLS), but it is now the sandbox and the tunnel. "Stronger sandbox than srt"
  is asserted, not measured — the prober is how we substantiate and monitor it.