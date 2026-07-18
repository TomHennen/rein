# Design: rein on nono — implementation plan (P0–P2 + srt removal)

**Status:** Design of record for the `nono` branch. Implementable without
re-deriving decisions. Scope: **Linux**. macOS/Seatbelt is an explicit open gate
(§8), not covered here.

**Evidence base:** `docs/nono-git-push-spike-findings.md` (empirical),
`docs/proposal-rein-on-nono.md` (approved WHAT), `docs/containment-probe-harness.md`
(prober). This doc is the HOW.

> **Architecture is (b), not (c).** nono **tunnels** the agent's GitHub egress to
> rein via its `external_proxy`/`upstream_proxy` CONNECT chain — an opaque
> bidirectional byte tunnel, no buffering. **rein** TLS-terminates, injects, and
> streams. nono does NOT inject (that is the retired (c) `cmd://`/`credential_capture`
> framing — the post-spike reviews killed it: `cmd://` sees only
> `{host,path,method,session}`, never the body, so it downgrades mediation and lets
> small pushes bypass the declare gate). No `cmd://`, no `credential_capture`
> anywhere in this design. If that language reappears in a PR, reject it.

---

## 1. Architecture recap

```
  ┌─ nono sandbox (Landlock + seccomp-notify) ─────────────┐
  │  agent (claude/git/gh)                                  │
  │    git http.proxy ─┐  http.proxyAuthMethod=basic        │
  │                    │  CA trust via env (SSL_CERT_FILE…) │
  └────────────────────┼───────────────────────────────────┘
                       │  nono external_proxy (opaque CONNECT tunnel)
                       │  Proxy-Authorization: Basic <per-session secret>
                       ▼
        rein loopback-TCP listener  (127.0.0.1:<port>)
          ├─ verify proxy-auth (constant-time; 407 on miss)
          ├─ TLS-terminate on SNI, inject token, STREAM body
          ├─ receive-pack declare tap + tier classifier
          └─ upstream ──► github.com / api / uploads
   CDN hosts (codeload/objects/raw) ──► nono upstream_bypass ──► direct (never reach rein)
```

Cited facts (all from the findings, proven on Linux/aarch64+x86_64, real nono 0.68.0,
real github.com):

- **nono tunnels, rein injects → the 16 MiB/chunked cap dissolves.** A small AND a
  **20 MiB chunked `git push` both LANDED** through `nono run` → external-proxy →
  rein MITM (`inner POST git-receive-pack cl=-1`). The cap was an artifact of nono's
  *own* inject path; when nono only tunnels it never buffers the body.
- **git needs `http.proxyAuthMethod=basic`.** nono's external-proxy path uses
  *strict* preemptive connect-auth; git otherwise sends CONNECT with no
  `Proxy-Authorization` and nono aborts. (Sandbox git config sets it.)
- **`upstream_bypass` gives exact-host routing.** With `upstream_bypass:[CDN…]` +
  `upstream_proxy:<rein>`, `github.com` reached rein's MITM while `example.com`/CDN
  went direct (MITM never saw them) → no token on a pre-signed asset URL.
- **`external_proxy.auth` lets rein's listener require a per-session secret.** nono
  sends upstream `Proxy-Authorization` (`external.rs:51`); `ExternalProxyAuth`
  (basic) exists. Closes the loopback-capability regression (§3a).
- **nono hides credentials** (`deny_credentials`): the App key
  (`~/.config/rein-credentials/app.pem`), gh token, ssh keys, bash_history all fail
  `open()` with Permission denied in-sandbox; readable on host.
- **Direct TCP egress is blocked** by seccomp user-notification (`connect`/`bind`
  validated by nono's supervisor). **UDP is open by default** — ALL UDP — but nono
  routes `sendto`/`sendmsg` to the supervisor too (`sandbox/linux.rs:2064`), so it is
  mediable; the strict policy is unlocated (§3d).
- **Loopback `connect()` policy is UNMEASURED.** The spike only tested *external*
  hosts. Whether seccomp mediates `connect(127.0.0.1:*)` at all is unknown — and it is
  load-bearing two ways: (i) the agent must reach nono's own loopback proxy port, so
  *some* loopback must be allowed; (ii) if loopback is otherwise open, every host
  loopback service (docker API, dev servers, DBs, X11, the tmux server socket — §3e F#2)
  is reachable, a regression from srt's empty netns. Conversely if seccomp *does* mediate
  loopback, a direct hit on rein's port yields `EPERM`, not the 407 the prober expects
  (§3e). **P1 confirm-item:** measure it; decide restriction (Tom-level, like UDP).
- **Integration is CLI + JSON profile, not the SDK.** rein exec's `nono run
  --profile <file> -- <agent>`; no linking.

---

## 2. P0 — verified installer + profile generator + doctor

New package **`internal/nono`**. Files: `installer.go`, `profile.go`, `doctor.go`
(health checks; the cobra wiring stays in `cmd/rein/doctor.go`).

**P0.0 — schema dump FIRST (gates the profile struct).** Before writing `profile.go`,
dump nono 0.68.0's *real* profile/config schema from nono source (the profile struct)
and emit one working, composed `nono run` profile (both auth hops + declare host +
`deny_credentials` + env) that actually launches. Every field name in §2.2
(`allow_domain`, `upstream_bypass`, `external_proxy{…}`, `deny_credentials`, `env`) is
currently inferred from the spike, not from a dumped schema — and the spike drove auth
via the `--upstream-proxy` CLI flag, never a composed profile with the `auth` object set
(§8). The struct shape in §2.2 is provisional until this lands. **Do not code the
generator against guessed field names.**

**"Standalone" caveat.** P0 is *mostly* self-contained, but `doctor.go` needs the
`Check`/`Status` types that today live in `internal/srt/preflight.go`. Reusing them
makes `internal/nono` import `internal/srt`; that undercuts "no srt coupling." Fix by
sequencing the shared-substrate extraction (§5, §7) at/before this work, not by copying.

### 2.1 Verified installer (`internal/nono/installer.go`)

nono's own `nono.sh/install.sh` downloads from
`https://github.com/nolabs-ai/nono/releases/download/<version>/<asset>`, fetches
`SHA256SUMS.txt`, and verifies SHA-256 — **checksum only, no signature** (confirmed
by fetching the script; release assets are 8 tarballs/pkgs + `SHA256SUMS.txt`, no
`.sig`/`.sigstore`/attestation). nono's CI *is* moving toward signing
(`nolabs-ai/agent-sign`, `sigstore-sign` in their release workflow) but publishes no
verifiable signature material on the release yet.

**Trust floor = a rein-vendored SHA-256 digest.** Fetching `SHA256SUMS.txt` over the
same TLS channel as the binary is not independent trust — a channel compromise serves
both. So rein pins the digest **in its own source**, checked into rein's supply chain
(wrangle/SLSA covers rein's binary), and verifies the download against that.

```go
// PinnedVersion is the nono release rein's profile schema + behavior are verified
// against. A mismatch means the profile shape / egress semantics may differ; the
// installer refuses and doctor warns (mirrors srt.PinnedVersion policy).
const PinnedVersion = "0.68.0"

// pinnedDigests maps (version, platform) -> {tarball, binary} lowercase hex SHA-256,
// VENDORED in rein source (the trust floor; never the fetched SHA256SUMS.txt).
// Regenerated by hand on a version bump after out-of-band verification. platform key
// is the rustc target triple nono ships, e.g. "x86_64-unknown-linux-gnu". Two digests:
// tarball gates Install's download; binary is what VerifyInstalled re-hashes on disk.
var pinnedDigests = map[string]map[string]struct{ Tarball, Binary string }{}

type InstallParams struct {
    Version   string // default PinnedVersion
    Platform  string // default detected: <arch>-unknown-linux-gnu
    DestDir   string // rein-managed; default managedNonoDir() (see 2.1.1)
    HTTPGet   func(url string) (*http.Response, error) // injectable for tests
}

// Install downloads the release TARBALL, verifies it, extracts the inner nono
// binary, and atomically places it at DestDir/nono (0o755). Fail-closed: any digest
// mismatch, missing pin, tar path-traversal, or partial download removes temp files
// and returns an error — it NEVER installs an unverified binary. Returns the
// absolute installed path.
func Install(p InstallParams) (string, error)

// VerifyInstalled recomputes the on-disk BINARY's SHA-256 and compares to the pin.
// Used by doctor and by the launch path (§2.3).
func VerifyInstalled(path, version, platform string) error
```

**Extraction is a distinct step (release assets are tarballs, not bare binaries).** The
flow is: download tarball → verify TARBALL digest → extract → **guard tar path-traversal**
(reject `..`/absolute members) → place the inner binary. This means the pin map must
hold **two** digests per (version,platform) — the tarball digest (gates download) *and*
the inner-binary digest (what `VerifyInstalled` re-hashes on disk). Re-hashing the
on-disk binary against a *tarball* digest never matches. Vendor both; `Install` checks
the tarball digest, `VerifyInstalled` checks the binary digest.

Failure handling: unsupported OS/arch → error naming the supported set; no pin for
`(version,platform)` → error "no vendored digest; bump `internal/nono.pinnedDigests`"
(never fall through to trusting `SHA256SUMS.txt`); digest mismatch → error with both
hashes, temp file unlinked. Download is atomic (temp file in `DestDir` + `os.Rename`).

**Signature upgrade path (documented, not built):** when nono publishes sigstore
bundles or GitHub attestations, add `verifySignature()` gating BEFORE the digest
check; the vendored digest stays as belt-and-suspenders. Track as a follow-up issue.

#### 2.1.1 rein-managed path (closes the binary-shadowing gap)

srt was resolved via `exec.LookPath("srt")` — a `$PATH` entry the agent's environment
could shadow. nono is invoked by **absolute path from a rein-managed directory**,
never `LookPath`:

```go
func managedNonoDir() string  // filepath.Join(config.ConfigDir(), "nono", "bin")
func ManagedNonoPath() string  // managedNonoDir()/nono — the ONLY path rein exec's
```

`rein init` calls `Install` into `managedNonoDir()`; `rein run --nono` exec's exactly
`ManagedNonoPath()` after `VerifyInstalled` passes.

### 2.2 Profile generator (`internal/nono/profile.go`)

Emits the exact JSON nono profile rein hands `nono run --profile`. One source of
truth; `cmd/rein/run_nono.go` writes it each launch.

**Struct shape is PROVISIONAL — gated on the P0.0 schema dump (§2).** Two shapes are
unverified and could be wrong: (a) whether `upstream_proxy` (URL) and the auth object
are separate top-level fields or nest under one `external_proxy{url,auth}` object;
(b) whether the per-session secret goes in the profile JSON at all, or nono expects it
via the **OS keyring** (Secret Service/dbus — "keyring-backed" is source-language). If
keyring: headless VMs/CI have no Secret Service, per-run provisioning is a keyring write
per launch, and concurrent runs may collide on the entry — a real P1a integration cost,
not "proxy-local only" (§7). The `env` field's ability to inject arbitrary env is
*itself* unverified in the findings. Resolve all of this in P0.0 before coding.

```go
type Profile struct {
    AllowDomain    []string          `json:"allow_domain"`              // egress allowlist
    UpstreamProxy  string            `json:"upstream_proxy"`            // rein listener "http://127.0.0.1:<port>" (may nest — see above)
    UpstreamBypass []string          `json:"upstream_bypass"`           // CDN hosts → direct
    ExternalProxy  *ExternalProxy    `json:"external_proxy,omitempty"`  // per-session auth
    DenyCredentials []string         `json:"deny_credentials"`          // cred stores to hide — MUST include the profile path itself (§3a)
    Env            map[string]string `json:"env,omitempty"`             // CA-trust + git + HTTPS_PROXY/NO_PROXY (§3b/§3c) — arbitrary-env support UNVERIFIED
    // UDP policy field — name TBD; P1d-gated (§3d). P0 generator ships WITHOUT it
    // (open UDP) behind --nono; the fail-closed field is added when P1d locates it.
}
type ExternalProxy struct {
    Auth *ProxyAuth `json:"auth,omitempty"`
}
type ProxyAuth struct { // basic; nono keyring-backed — see keyring caveat above
    Username string `json:"username"`
    Secret   string `json:"secret"` // per-session; see §3a for provenance
}

type Params struct {
    ListenAddr      string   // "127.0.0.1:<port>" rein's loopback listener
    ProxyAuthUser   string
    ProxyAuthSecret string   // 32 random bytes, base64; generated per run
    ProfilePath     string   // secrets dir — agent-DENIED (in DenyReadPaths). NOT the CA dir.
    CACertPath      string   // SEPARATE agent-READABLE dir — CA PEM the sandbox tools read (§3b)
    ExtraDomains    []string // operator opt-in (api.anthropic.com, npm, …) — egress only, NEVER injected
    DenyReadPaths   []string // credentialDenyReadPaths(stateDir) + App key dir + audit + ProfilePath's dir
    GitConfigEnv    map[string]string // GIT_CONFIG_GLOBAL etc.
}
// Path split is load-bearing (§3a, §3b): the profile JSON holds the per-session secret
// and MUST be agent-unreadable; the CA PEM MUST be agent-readable (tools trust it).
// Same directory ⇒ dir-granular Landlock allow-read on the CA leaks the profile. Two
// distinct dirs (or file-granular rules): secrets-dir denied, ca-dir allow-read.

// Build assembles the profile. Invariants enforced here (fail closed on violation):
//   - AllowDomain = proxy.InjectHosts ∪ proxy.CDNHosts ∪ ExtraDomains ∪ DeclareHost
//   - UpstreamProxy routes; UpstreamBypass = proxy.CDNHosts (direct, never rein)
//   - proxy.InjectHosts + DeclareHost are NOT in UpstreamBypass (must reach rein)
//   - ExtraDomains are NEVER in the inject set (egress-only direct TLS)
//   - Env carries CA trust (§3b) + http.proxyAuthMethod=basic wiring (§3c)
func Build(p Params) (Profile, error)
func (pr Profile) MarshalIndent() ([]byte, error)
```

Host lists stay in `internal/proxy/hosts.go` (`InjectHosts`, `CDNHosts`,
`DeclareHost`) — the profile generator consumes them so the two never drift (same
single-source-of-truth rule srt.Build followed). **`upstream_bypass` = `CDNHosts`
verbatim** — this is the (3c) host-routing property.

### 2.3 doctor nono health checks (`internal/nono/doctor.go`)

Replaces the four srt rows (`srt present/version/seccomp/bwrap userns`). Uses the
`Check`/`Status` types — which today live in `internal/srt/preflight.go` and srt is
NOT deleted until P3, so reusing them in place couples `internal/nono → internal/srt`.
**Extract `Check`/`Status` into a substrate-neutral package first** (the §5/§7
shared-substrate extraction); do not copy or import-couple. Rows:

| Row | Check | Hard? |
|---|---|---|
| `nono present` | `ManagedNonoPath()` exists + executable | fail |
| `nono version` | binary version == `PinnedVersion` | fail |
| `nono digest` | `VerifyInstalled` (on-disk SHA-256 == pin) | fail |
| `nono landlock` | `nono setup --check-only` reports Landlock supported | fail |
| `nono seccomp` | supervisor connect/UDP mediation available (probe or `setup`) | fail |
| `CA trust env` | rein CA PEM readable + non-empty (SystemCA analog) | fail |

`nono setup --check-only` is nono's own health probe (findings runbook step 1). The
launch path (`rein run --nono`) hard-gates the same fail rows and fails closed —
never silently drops to a weaker mode (design §2-3).

---

## 3. P1 — compose: re-front the proxy, CA, routing, UDP, prober

New file **`cmd/rein/run_nono.go`** (`func runNono(cmdline []string) (int, error)`),
the nono analog of `run_sandboxed.go`. `rein run` dispatches to it behind
**`--nono`** (§7); srt stays the default until P3. It reuses the broker/mint/scope
machinery unchanged (`runbroker`, `runscope`, `brokercore`, declare hooks) — only the
*front* of the proxy and the *sandbox launch* differ.

### 3a. Re-front internal/proxy: unix socket → loopback TCP + proxy-auth (defense-in-depth)

Closes the socket→TCP capability regression — but proxy-auth is **defense-in-depth,
NOT the security boundary.** The primary gate is declare + tier-classifier +
downstream token-injection (§3a-mediation below); proxy-auth only removes a redundant
anonymous path to rein's port.

**The regression.** Under srt the proxy listened on a **unix socket** whose parent
dir (0700) + placement-outside-every-bind-mount was the capability gate. A **loopback
TCP port** is reachable by any process that can `connect(127.0.0.1:port)`. Whether nono's
seccomp mediates loopback `connect()` is UNMEASURED (§1) — if it doesn't, the sandboxed
agent can reach rein's port directly, bypassing nono's tunnel.

**Why this is defense-in-depth, not the gate (F3 reconciliation).** Reviews called a
leaked secret "unmediated injection." That is wrong on both counts. (1) *Not unmediated*:
every accepted connection runs the SAME pipeline — TLS-terminate → inject → declare tap →
tier classifier → upstream (§1 diagram). A direct-to-port connection with a forged secret
is still declare-gated, still classified; the token is injected on the rein→github leg
**downstream of the sandbox**, so its value never enters the sandbox regardless of how the
connection authed. (2) *Not a new capability*: the agent already HAS a legit
token-injected tunnel (nono adds the secret host-side); forging the secret buys a
redundant path to the same place. So the real gates are declare + classifier +
downstream-injection + **approval-channel isolation (§3e, the actual crux)** — proxy-auth
is a belt-and-suspenders layer that keeps anonymous in-sandbox processes off rein's port.

**"Full mediation on every accepted connection" is a CONFIRM-ITEM, not an asserted
invariant.** The F3 reasoning above holds only if the pipeline order guarantees declare
tap + classifier run regardless of auth. That is inferred from the diagram, not proven.
P1 must confirm it (and denial semantics — §3a-deny) before relying on it.

**The close.** rein's listener **requires a per-session proxy-auth secret**, carried
by nono in `external_proxy.auth`, sent as `Proxy-Authorization: Basic` on every CONNECT
nono makes to rein. rein verifies it (constant-time, full decoded credential) on the
CONNECT preamble and returns **407** on miss/mismatch/malformed header (fail closed — do
not short-circuit on username or header-presence). The secret:

- **Must be agent-unreadable — via `deny_credentials`/Landlock, NOT file perms.** The
  earlier "0o600 host-side tmp / outside any writable mount" rationale is srt mount
  semantics and buys nothing under nono: nono runs the agent at **rein's own uid**, so a
  0o600 file at that uid is readable by the agent, and Landlock has no mounts. The profile
  JSON is unreadable in-sandbox **only if its path is in the profile's `deny_credentials`
  / Landlock deny set** — and §2.2's `DenyReadPaths` did NOT list it; it now must (and the
  §3e prober must assert it, since this was never measured — the containment probe only
  read the already-denied app.pem/gh/ssh paths). nono injects the `Proxy-Authorization`
  host-side from the profile; the agent never sees it even though its traffic carries it.
- 32 random bytes (crypto/rand), base64, regenerated every run.

**Phasing: P1a is ADDITIVE, deletions are P3.** srt stays the default until the P3
atomic cutover (§5), so the unix-socket front must keep working the whole time. P1a
therefore **adds** the loopback-TCP+auth front alongside the unix-socket front (both
compiled, `Start` selects on which config is set); it does NOT replace or delete the srt
path. All the deletions below (`CheckPlacement`, the unix-socket `Listen` body,
`SocketPath`/`ForbiddenDirs`) move to **P3**. A hard swap at P1a breaks the default
`rein run` on the branch and defeats "rollback = revert one PR" (§5/§7).

**Code changes in `internal/proxy`:**

- **`proxy.go handleConn`** already consumes a CONNECT preamble and supports a direct
  TLS client. Add, in the CONNECT-preamble branch: parse `Proxy-Authorization`,
  `subtle.ConstantTimeCompare` over the **full decoded credential**; on miss/mismatch/
  malformed/absent header write `HTTP/1.1 407 Proxy Authentication Required\r\n\r\n` and
  return (fail closed — no short-circuit on username or header-presence). Plumb the secret
  via a new `Config` field:

  ```go
  // Config (internal/proxy/proxy.go)
  ProxyAuthSecret string // set for the loopback-TCP (nono) front. New() MUST HARD-ERROR
                         // (not warn) if a TCP listener has empty auth. Empty is legal
                         // ONLY for the unix-socket srt front; after P3 no empty-auth-TCP
                         // path remains in the tree.
  ```
  Direct-TLS-no-CONNECT clients (no preamble) are refused under nono (a CONNECT with
  valid auth is mandatory); keep the no-SNI fail-closed as-is.

- **Injection is UNCONDITIONAL OVERWRITE**, not add-if-absent: rein replaces any
  agent-supplied `Authorization` on the request before forwarding upstream. (Live risk is
  low — the agent has no valid token, so suppressing injection is self-denial — but
  overwrite is the stated invariant for defense-in-depth + parser robustness.)

- **`placement.go` — split, deletions deferred to P3:**
  1. **Add** a loopback TCP listener (P1a); do NOT remove the unix-socket `Listen` body
     until P3:
     ```go
     // ListenLoopback binds 127.0.0.1:0 (or a fixed port), returns the listener
     // and the chosen addr. No placement check, no chmod dance — the proxy-auth
     // secret (§3a) is the capability gate now, not filesystem perms.
     func ListenLoopback() (*net.TCPListener, string, error)
     ```
     Bind explicitly to `127.0.0.1` (never `0.0.0.0`) so no other host can reach it.
  2. **Delete `CheckPlacement` + `pathWithin` and the unix-socket `Listen` body — at P3**
     (no bind-mount under nono ⇒ no socket-placement capability to defend). Remove their
     tests then.
  3. **KEEP** `ResolveAbs` + `cleanAbs` — audited: shared symlink primitive still needed
     by `run_nono.go` to resolve the `deny_credentials` set and working tree. Move them to
     a non-placement file (e.g. `internal/proxy/paths.go`) when placement.go is emptied.

- **`runbroker.Config`:** stays **additive** P1a→P3 — keep `SocketPath`/`ForbiddenDirs`
  (unix front) AND add `ListenAddr` + `ProxyAuthSecret` (loopback front); `Start` selects
  on which is set, calling `ListenLoopback` for the nono front. Remove the srt fields at
  P3 only.

`declare.rein.internal` (§3, below) rides the same listener: nono CONNECTs to it via
`upstream_proxy` by hostname, rein terminates with its CA and answers locally
(`classLocalDeclare`), responses token-free. It must be in `allow_domain` and MUST NOT
be in `upstream_bypass` (that would send it direct → unresolvable) nor a real
`allow_domain` egress target.

- **Confirm-item (unspiked):** that nono tunnels an *unresolvable virtual host* by
  CONNECT-host without DNS is ASSERTED, never tested — the spike used real github. nono
  may validate/resolve CONNECT targets or reject an unknown-TLD `allow_domain` entry. The
  declare ceremony is the core of the write gate; if this breaks, P1 stalls. Demote to a
  named P1 confirm-item alongside CA-env and the auth hops.
- **Invariant — declare informs, it cannot drive approval.** The agent CAN reach
  `declare.rein.internal` through the authenticated tunnel (nono adds the secret
  host-side); it is *not* blocked by 407. Safety rests entirely on approval being
  `/dev/tty`-gated out-of-band (§3e) — the declare HTTP surface may *inform* intent but
  MUST NOT be able to *drive* an approval.

**Hard-constraint #2 + declare-denial semantics — RE-DERIVE under (b) (were raised
against the retired (c) path).** Specify all three:

- **Credential helper:** under the MITM-inject model rein injects host-side and there is
  likely no sandbox-side git credential helper. If so, #2's "always return a credential"
  is **moot** — state that explicitly. If any helper survives, the always-return
  invariant must be preserved.
- **Denial semantics (load-bearing, currently undefined):** when a push is NOT approved
  at the declare gate, the agent's git must see a **clean fail-closed HTTP status**, not a
  hang or an empty-credential retry loop. This is what makes the F3 "still-declare-gated"
  argument safe; specify and test it (§3e).
- **Token-leak boundary (named invariant):** rein injects downstream of the sandbox, so
  even a direct-to-port connection yields authenticated *actions*, never the token
  *value* — PROVIDED (a) no inject host reflects the `Authorization` header back (all
  inject hosts are real github, none echo auth — make this an explicit confirm) and
  (b) `ExtraDomains` are never injected (enforced in `Build`, §2.2). No
  attacker-controlled reflection oracle ⇒ no token exfil.

### 3b. CA trust via env (no fs-bind under Landlock; fail-closed)

nono uses Landlock, **no mount namespaces** — so srt's model (bind rein's CA bundle
read-only over the system trust path) is unavailable. CA trust is **env/config based**,
the same mechanism nono uses for its own intercept CA:

- rein writes its CA PEM to the **agent-readable CA dir** (separate from the denied
  profile-secrets dir — §2.2); the profile's `env` sets **all four** CA vars —
  `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`, **`CURL_CA_BUNDLE`** — to it
  (reuse srt's full set, `internal/srt/env.go` `caEnvVars`; dropping `CURL_CA_BUNDLE`
  loses curl/libcurl trust). Extract these constants into the substrate-neutral package
  (§5/§7), don't import-couple to `internal/srt`.
- **#6 — the CA SIGNING key is a private key.** The PEM written here is the CA *cert*
  (public), fine to expose. But rein's MITM CA signing key MUST load through
  `internal/keystore` (Get/Fingerprint), never `os.ReadFile` — confirm the existing
  `proxy/ca.go` path is carried forward unchanged (it is today: keystore-loaded ECDSA
  P-256). (Aside: rein's CA is already ECDSA P-256, so the findings' "nono ring requires
  EC P-256" constraint is satisfied and moot under (b) — nono tunnels, doesn't terminate
  rein's CA. Don't "fix" a non-problem.)
- **Fails closed:** an agent unsetting the CA var gets a TLS *failure* (rein's leaf is
  untrusted), not a bypass — provided there is genuinely no unmediated path to a real
  github cert (see F2/§3c). Fail-closed rests on **egress-allowlist + proxy-auth**, not
  on the config pin below.
- **`GIT_CONFIG_GLOBAL` is a convenience default, NOT a control (F4 correction).** git
  local repo config (`.git/config` in the writable working tree), `-c`, and
  `GIT_SSL_*`/`http_proxy` env all OVERRIDE `GIT_CONFIG_GLOBAL` — so the agent CAN
  override `http.sslCAInfo`/`http.proxy`/`http.sslVerify`. Earlier text claiming the pin
  "so the agent can't override http.sslCAInfo" is wrong; none of those overrides yield a
  mediation bypass because the real controls are egress-allowlist + proxy-auth, and
  local-config override is precisely *how* an agent would attempt the F2 direct-github
  path. Treat the pin as a helpful default only.

Whether the CA PEM path itself must be `allow_read` under Landlock (so the agent's
tools can *read* it) is a **P1 confirm-item**: the profile may need the CA file in an
allow-read set — which is exactly why it must live in its own dir, NOT with the profile
secret (§2.2). Verify empirically for git AND a non-git tool (node) — findings proved
git only.

### 3c. Host-routing (exact hosts → rein, CDN → direct)

Per §2.2: `upstream_proxy` = rein listener; `upstream_bypass` = `CDNHosts`; only
`InjectHosts` + `DeclareHost` reach rein. Sandbox git config sets
**`http.proxyAuthMethod=basic`** (via `GIT_CONFIG_GLOBAL`) so git sends preemptive
proxy-auth to nono's external-proxy path.

- **Non-git tools need proxy env.** `gh`, node, python etc. don't read git config —
  the profile `env` must also set `HTTPS_PROXY`/`HTTP_PROXY` (→ nono's proxy) and
  `NO_PROXY` (CDN bypass hosts). The `gh_write` journey depends on this; §2.2's `env`
  ("CA-trust + git config") must include it.
- **F2 — "no unmediated path to github" is unproven; confirm it.** `github.com` is in
  `allow_domain` as an inject host. The spike never tested whether a **direct
  `connect(github.com:443)` that bypasses the proxy** (e.g. agent clears `http.proxy` in
  the writable `.git/config`) is blocked. If `allow_domain` grants direct egress, the
  agent reaches github OUTSIDE rein's MITM. **VERIFIED (2026-07-18): NO hole.** A direct
  `connect(github.com:443)` inside `nono run` (github in `allow_domain`) is **BLOCKED**
  (`PermissionError`) — `allow_domain` grants *proxy-mediated* egress, not direct; seccomp
  blocks the direct connect. So the agent's only path to github is through rein. Keep the
  prober assertion as a **regression guard**, but §3b/§1's "no unmediated path" now holds
  empirically.
- **Two auth hops, do not conflate.** The spike exercised git→nono with `--no-auth`; the
  *authenticated* git→nono credential value (nono's own external-proxy password, if nono
  requires one) needs a P1 end-to-end confirm, distinct from nono→rein
  (`external_proxy.auth`, §3a). The findings' account of these two hops is muddled —
  disentangle them in the P1 full-profile exit gate (§7).

### 3d. UDP policy — DECISION FOR TOM (fail-closed default recommended)

**State:** direct-TCP egress = strong (seccomp-notify blocks it, parity with srt).
**UDP egress is open by default — all UDP** (DNS names, QUIC, arbitrary): tested inside
`nono run`, `sendto` to `8.8.8.8:53/:12345`, `1.1.1.1:443` all succeed. srt's empty
netns blocks this. nono *can* mediate UDP — `sandbox/linux.rs:2064` routes
`sendto`/`sendmsg`/`sendmmsg` to the supervisor — but the default policy allows them
and the **strict setting is unlocated**.

**P1 task:** locate/verify the nono profile field that restricts UDP (candidates:
a `deny`-UDP policy, an egress `block:true` + per-proto allow, or a network-lockdown
mode). Search nono source/docs; test inside `nono run` that DNS-to-resolver still
works while arbitrary UDP is blocked.

**Two options for Tom (present both — do not bake acceptance of the weaker one):**
- **(A) deny-all UDP.** Under this architecture the agent needs NO DNS at all — every
  egress is CONNECT-by-hostname to a loopback proxy that resolves host-side. So deny-all
  is simpler, stricter, closest to srt's empty-netns parity, and eliminates the
  DNS-tunnel exfil channel entirely.
- **(B) deny-UDP except DNS to the configured resolver.** Retains a DNS-tunnel exfil
  channel; only needed if some in-sandbox tool genuinely requires local DNS.

If the strict setting is found → set it fail-closed (lean (A) unless a tool breaks). If
NOT findable before cutover → explicit **accept/reject** decision for Tom in the P1
stop-and-surface AND a **P3 cutover gate** (§5): rein's threat model has historically
weighted credential theft over exfil, but a general UDP exfil channel is a real
regression from srt. Do not silently accept, and do not let cutover slip through with UDP
unresolved.

### 3e. The prober (fail-closed launch gate + CI harness)

Two layers, per `docs/containment-probe-harness.md`, retargeted from srt to nono:

**CRUX — approval-channel isolation (Review's top finding, above F3 and UDP).** srt
isolated the agent with namespaces; **nono has NO namespaces** (Landlock + seccomp only).
The agent runs as an ordinary host process at rein's uid in the same session, so nothing
obviously stops it from (a) opening `/dev/tty` on the shared controlling terminal, or
(b) connecting to the **tmux server socket** and driving the approval popup
(`tmux send-keys`) — self-approving its own declared push **through the legit tunnel,
no secret-forging needed**, silently defeating the one guarantee rein exists to provide
while journeys stay green (they drive approval host-side). The tmux socket + `$TMUX`
appear in NO deny list in this design. **EMPIRICALLY CONFIRMED (2026-07-18) — this is a real hole, and the obvious fix does
NOT work.** Inside `nono run` (default-ish profile), the agent: got `$TMUX` **passed
through** (leaks `/tmp/tmux-<uid>/default`); could NOT `ls` the tmux dir; **but
successfully `connect()`ed to the tmux AF_UNIX socket and `send-keys`'d a command that
executed** (self-approval proven). `/dev/tty` was **blocked** (good — a *tty*-based
approval is safe from the agent). The naive mitigation **fails**: adding
`/tmp/tmux-<uid>` to `filesystem.deny` makes nono **refuse to launch** — *"Landlock
deny-overlap is not enforceable; deny '/tmp/tmux-1000' overlaps allowed parent '/tmp'
(group:system_write_linux)."* Landlock has **no deny-under-allow**, and nono's IPC
restriction (`ipc_mode:shared_memory_only`) only blocks **abstract** sockets, not
filesystem sockets like tmux's. So there is **no simple socket-deny**.

**RESOLVED (2026-07-18) — `af_unix_mediation: "pathname"` closes it AND preserves the
tmux-popup UX; NOT a blocker.** The Landlock fs-deny does not block socket `connect()`
(only read) — but nono ships an opt-in `LinuxConfig.af_unix_mediation` field, **default
`off`** (why the hole was open). Set to **`"pathname"`** it deny-by-default mediates
pathname AF_UNIX connect/bind, requiring explicit `filesystem.unix_socket*` allowlist
grants. **Tested:** with `linux.af_unix_mediation: "pathname"` (no socket allowlist), the
agent's `connect()` to the tmux socket → **Permission denied**, `send-keys` blocked,
while the agent still runs (scratch writes fine); `/dev/tty` already blocked. Crucially
this constrains **only the sandboxed agent** — rein drives the approval popup
**host-side, outside the sandbox**, unaffected, so the **interactive tmux approval UX is
preserved** (the user's constraint).

**P1 work (a config + allowlist-tuning item, not a redesign):** set
`af_unix_mediation: "pathname"` in rein's profile; scrub `$TMUX` (defense-in-depth — the
mediation is the real control); determine the *minimal* pathname unix sockets a real
agent (claude/node/gh — DNS resolver, etc.) legitimately needs and grant ONLY those via
`filesystem.unix_socket*`, **never the tmux/approval socket**; verify the agent still
runs. The prober MUST assert (fail closed otherwise): tmux socket connect **denied**,
`send-keys` denied, `/dev/tty` unopenable, `$TMUX` absent. This preserves rein's #32
non-replayability guarantee under nono.

1. **Launch gate = nono analog of `VerifyConfigApplied`.** A tiny, dep-free,
   in-binary check that runs on every `rein run --nono` BEFORE the agent, and fails
   closed. It launches the probe THROUGH the real `nono run` path (inherits the exact
   profile). This needs a probe **subcommand dispatched inside the nono sandbox** (reuse
   the hidden `rein __sandbox-probe` or a new one) plus the `main.go` wiring — the seam
   `srt/selftest.go RunProbe` uses; `selftest.go` alone is not enough. Asserts:
   - App key / gh / ssh **unreadable**;
   - **profile JSON unreadable** (holds the §3a secret — guards F3; never measured);
   - direct TCP to a non-allowlisted host **blocked**;
   - direct (non-proxy) connect to an `allow_domain` **inject host blocked** (guards F2);
   - **arbitrary loopback ports** unreachable except nono's proxy port (guards the wider
     loopback regression — §1/§3a; conditional on the loopback-policy measurement);
   - **arbitrary UDP blocked** (guards §3d; conditional on the UDP policy being set);
   - **approval channel unreachable:** `/dev/tty` unopenable, tmux socket unconnectable,
     `send-keys` blocked (the crux above);
   - rein's loopback port **rejects unauth'd CONNECT (407)** — *and* a **positive
     control** that the *authenticated* CONNECT is served (else a mis-provisioned secret
     fails the whole stack closed silently in the gate rather than surfacing as a config
     error). NOTE: if the loopback measurement shows seccomp mediates loopback, the
     unauth'd probe gets `EPERM`, not 407 — encode the observed behavior, don't assume.

   Lives in `internal/nono/selftest.go` (mirrors `srt/selftest.go`). Keep it
   bespoke-tiny; do NOT swap an external toolkit into this slot.
2. **Verification harness = `controlplaneio/sandbox-probe`** (Apache-2.0, test/CI
   dep, never linked into the binary — same posture as `pyte`). Run on host + through
   `rein run --nono`, diff, oracle-classify against rein's emitted profile
   (`deny_credentials`, `allow_domain`, inject vs CDN), emit a golden report wired
   into a `tests/interactive/` journey. Drift = red = re-review. This is how we keep
   trusting nono across version bumps.

Land the prober against **current srt** on `main` first (issue #136 sibling) so it
exists before the pivot — it is substrate-agnostic by design.

---

## 4. P2 — minimize + fuzz the proxy

With nono owning the sandbox and CDN going direct via `upstream_bypass`, whole arms
of the proxy are dead — but their **deletion is P3**, not P2. srt has no
`upstream_bypass`, so under the still-default srt path CDN hosts reach rein and need
`classPassthrough`; deleting it at P2 breaks the default (§5/§7 phasing). P2 *fuzzes*
(below, srt-safe); the deletions land in the atomic P3 cutover.

- **Delete `classPassthrough` (P3)** (proxy.go `serveOne`/`relay` CDN-relay arm) **and
  its `classifyHost` case** for `objects/codeload/raw.githubusercontent.com`: under nono,
  CDN hosts never reach rein (they're in `upstream_bypass`). **KEEP the `CDNHosts` list**
  in hosts.go — the profile generator still needs it to populate `upstream_bypass`. Easy
  to over-delete; the list lives on, the relay arm dies. **Update the `hosts.go:24`
  comment** ("must NOT appear in `mitmProxy.domains`") when CDNHosts is repurposed —
  under nono CDN hosts DO go in `allow_domain` AND `upstream_bypass`.
- **Request-body size gate:** dissolving nono's 16 MiB `MAX_REQUEST_BODY` makes rein the
  ONLY body-size gate. The streaming relay must have no unbounded accumulation; add a sane
  upper bound (self-DoS surface, low severity) — confirm this when minimizing.
- Keep the hand-rolled streaming relay (SNI==Host, no-token-on-response, HTTP/1.1
  relay, ContentLength/TransferEncoding copy, no-redirect-follow) and the
  receive-pack **declare tap**. These are rein's security-critical differentiators and
  stay custom (no library fits — findings §library question; goproxy gut-and-audit,
  go-mitmproxy buffers bodies, gomitmproxy is GPL).
- **Fuzz (issue #136):** `go test -fuzz` against `ParseReceivePackCommands`
  (receivepack.go) and the host/tier **classifier** (`classifyHost` + the GraphQL
  read/write tier resolution in gate.go). Seed corpora from the existing
  `receivepack_test.go`/`gate_test.go` cases. This is the surface an adversarial
  in-sandbox agent actually touches — higher value than more containment probing.

Land #136 fuzzing on `main` unconditionally (no new dependency, security win either
way).

---

## 5. srt removal plan

**When:** srt stays fully working behind the default until **P3 — one atomic cutover
PR** that (i) flips the default `rein run` from srt to nono and (ii) deletes srt in the
same commit. **Gated on** (all must be green/resolved):
- dogfood-proven nono (a human ran the real agent through the nono stack, pushed real
  code);
- green prober including the new assertions (§3e): profile-unreadable, F2 direct-github
  blocked, approval-channel isolation (tty + tmux socket + send-keys), loopback policy;
- **UDP policy RESOLVED** — either the fail-closed field is set, or Tom has explicitly
  accepted open UDP (§3d). Cutover must NOT slip through with UDP unresolved.
- green re-pointed journeys.

**Do NOT delete before nono is dogfood-proven.** Rollback = revert the one PR (which is
only true because P1a/P2 were additive — §3a/§4).

**Shared-substrate extraction (do at/before P1a, not P3).** `Check`/`Status`
(preflight.go), the CA-env constants (`caEnvVars`), `ResolveExtraAllowedDomains`
(domains.go), and the `agentenv` helpers must move into substrate-neutral packages so
`internal/nono` never imports `internal/srt` during P0–P2. A small "extract shared
substrate" PR early avoids the import-untangling churn this plan claims to avoid.

**What gets deleted (P3):**

| Target | Verdict | Note |
|---|---|---|
| `internal/srt/*.go` (~3972 LOC incl. tests) | **delete all** | config, env, cabundle, domains, preflight, selftest, socket_*, githard, allowread |
| `cmd/rein/run_sandboxed.go` + `_test.go` | **delete** | replaced by `run_nono.go` |
| `cmd/rein/sandbox_home.go` + tests | **assess** | if the $HOME/overlay construction is srt-mount-specific → delete; nono uses `deny_credentials`, not mount overlays. Likely delete; audit for reusable env-scrub helpers first |
| `cmd/rein/sandbox_claude_home.go` + tests | **P1 replace, NOT assess** | #94's property (agent gets a rein-owned `CLAUDE_CONFIG_DIR`; real `~/.claude` unreadable) is user-facing security AND the real agent needs a *writable* config dir to run at all. Under nono there is no overlay: either `~/.claude` is denied and claude breaks, or it isn't and #94 regresses. Specify the nono-model replacement — per-run `CLAUDE_CONFIG_DIR` + Landlock deny on `~/.claude` — as **P1 work**. `realagent_write`/`claude_resume` journeys depend on it |
| `internal/proxy/placement.go` (`CheckPlacement`/`pathWithin`, unix-socket `Listen` body) | **delete at P3** | additive until then (§3a); `ResolveAbs`/`cleanAbs` MOVED, not deleted |
| doctor srt rows (`srt present/version/seccomp/bwrap userns`) | **replace** (P2/P3) | → nono rows (§2.3) |
| `main.go` srt dispatch + preflight | **replace** | → nono dispatch; flip default |

**Env vars — per-var verdict (these are srt's filesystem model):**

| Env var | Verdict |
|---|---|
| `REIN_SANDBOX_ALLOW_READ` / `REIN_SANDBOX_SHOW_HOME` (allowread.go) | **delete** — srt deny-read/allow-back mount semantics; nono uses `deny_credentials` + Landlock allow-read, a different surface. If an escape-hatch is still wanted, re-introduce under nono semantics as a NEW var, don't carry the srt one |
| `REIN_SANDBOX_ALLOW_UNHARDENED_GIT` + `githard.go` | **assess → likely delete** — git-hardening guarded srt's `.git` **bind-mount** threat (a writable bound `.git` with hooks). Under nono there is no bind-mount; the threat model differs. Confirm the nono threat (can the agent write `.git/hooks` in the working tree and does that matter under nono?) before deleting; if the threat survives, port the check, else delete |
| `REIN_ALLOW_DOMAINS` (domains.go `ResolveExtraAllowedDomains`) | **KEEP** — operator egress opt-in is substrate-agnostic; move the resolver into `internal/nono` (feeds `allow_domain` + `ExtraDomains`, never inject) |
| `REIN_DISABLE_CLAUDE_MCP`, `REIN_REPO_WORKTREES`, `REIN_EPHEMERAL_CLONE_DIR`, `REIN_UPSTREAM_INTENT_FILE`, agent-contract | **KEEP** — not srt-specific; move helpers into a substrate-neutral package (`internal/agentenv` or `internal/nono`) |

**srt-coupled seams to cut (every one):**

- Go imports of `internal/srt`: `cmd/rein/{run_sandboxed,sandbox_home,doctor,main}.go`
  + tests `run_sandboxed_test.go`, `sandbox_home_e2e_test.go`. Each import removed or
  repointed to `internal/nono`.
- `runbroker.Config.SocketPath`/`ForbiddenDirs` (§3a) — cut, → `ListenAddr`/`ProxyAuthSecret`.
- `proxy.Listen` unix-socket / `CheckPlacement` callers.
- doctor `sandboxDoctorChecks` srt.Preflight wiring (`doctor.go:455-500`).
- Banner/preflight helpers in run_sandboxed.go that print srt paths
  (`preflightSrtPath`, `printPreflightAndOK`) — reimplement for nono or drop.
- Tests: `gitidentity_srtexclude_test.go` (name references srt), the srt-containment
  interactive tests (§6).

**End state (srt gone):** `internal/nono` (installer, profile, doctor, selftest) +
`internal/proxy` (streaming inject relay + declare tap + loopback-TCP-front, no
placement, no classPassthrough) + unchanged broker/keystore/runscope. `rein run`
defaults to nono; `--direct` unchanged. ~3972 LOC of srt + the dead proxy arms gone;
the security-critical code rein still owns is the small fuzzed relay.

---

## 6. Testing

- **Re-point broker journeys srt→nono, regenerate goldens.** The user-visible path is
  unchanged (declare/approve/push/scope/git-identity/gh-writes), so the journeys in
  `tests/interactive/journeys/` (write_ceremony, gh_write, scope_expansion, multi_repo,
  push_upstream, git_author, expansion_404, sandbox_gh_read_staleness, realagent_write,
  credential_boundary, claude_resume) run against the nono stack; regenerate the golden
  transcripts via `tests/interactive/run-journeys.sh`. Green = behavior preserved.
- **Drop srt-containment tests; the prober replaces them.** `sandbox_filesystem`
  journey and any srt-mount-specific assertions → the sandbox-probe golden report
  (§3e) against nono.
- **New nono-stack push journey:** real `git push` through `nono run` → nono
  external-proxy → rein → real github (throwaway repo, hard-constraint #1), INCLUDING a
  >16 MiB **chunked** push (the case that proves the cap dissolved). Golden-transcript
  backed.
- **Unit tests:** `internal/nono` installer (digest match/mismatch/missing-pin,
  atomic-place, unsupported platform — inject `HTTPGet`); profile generator (invariants:
  CDN in bypass not inject; inject/declare NOT in bypass; extra domains never injected;
  proxy-auth present); proxy-auth 407 path (`proxy_test.go`).
- **#136 fuzzing:** `ParseReceivePackCommands` + classifier (§4).

`tests/interactive/CLAUDE.md` rules apply: default-keep golden, `SBX|` view-split,
`rein init` setup (not `source ./dev-env`). The new push journey and re-pointed
goldens are the human-reviewable deliverables — green tests alone are insufficient
(CLAUDE.md).

---

## 7. Build / branch plan

Long-lived **`nono` branch**, kept in sync with `main`. `main` keeps shipping srt
until P3. Each phase is one (or few) PR(s) into `nono`.

**Land on `main` NOW, unconditionally (no new runtime dep, wins either way):**
- #136 fuzz the receive-pack parser + classifier.
- The prober adopted against **current srt** (substrate-agnostic).

**PRs into `nono`:**

| PR | Phase | Depends on | Parallelizable |
|---|---|---|---|
| **schema dump** — nono profile schema + one working composed profile | P0.0 | — | **yes**; **gates profile generator** |
| extract shared substrate (`Check`/`Status`, `caEnvVars`, domains resolver, agentenv) | P0 | — | **yes**; before doctor + P1a |
| `internal/nono` installer + pins (tarball+binary digests) | P0 | — | **yes** (standalone) |
| `internal/nono` profile generator | P0 | P0.0 schema + proxy host lists | after P0.0 |
| doctor nono health rows | P0 | installer + substrate extract | after installer |
| proxy re-front: **ADD** loopback TCP + proxy-auth 407 (additive, srt front kept) | P1a | substrate extract | **yes** (proxy-local; parallel with P0) |
| `run_nono.go` + `--nono` flag dispatch | P1 | P0 + P1a | serial (integrates) |
| CA-env + host-routing + git proxyAuthMethod + non-git proxy env | P1b,c | run_nono | with run_nono |
| UDP policy locate/decide | P1d | — | **yes** (research); **profile generator gains the UDP-deny field only after this** |
| claude config-dir nono replacement (#94) | P1 | run_nono | with run_nono |
| prober retarget to nono (launch gate + dispatch + harness) | P1e | run_nono | after run_nono |
| proxy **fuzz** (classPassthrough deletion deferred to P3) | P2 | — | **yes** (parallel; srt-safe) |
| **P3 cutover:** flip default + delete srt + all deferred deletions | P3 | ALL above + dogfood + green prober + UDP resolved | atomic, serial, LGTM-gated |

**Flag:** `rein run` defaults to srt through P0–P2; `--nono` opts into the new stack.
P3 flips the default and deletes srt in one commit. `--direct` is untouched throughout.

**Concurrency:** P0.0 schema dump gates the profile generator; the substrate extraction
gates doctor + P1a. After those, P0 (installer, profile), P1a (additive proxy front),
P1d (UDP research), P2 (fuzz) proceed in parallel — different files, no ordering.
`run_nono.go` integration (P1) serializes them; the prober and cutover follow. **No
srt-breaking deletion lands before P3** (§3a/§4 additive), which is what keeps
"rollback = revert one PR" true.

Each phase **stops-and-surfaces** at its gate per the current PLAN's discipline; P1d
(UDP) and P3 (cutover) are explicit Tom decisions.

---

## 8. Risks carried from findings

- **Approval-channel isolation (was TOP risk — §3e crux) — CONFIRMED then RESOLVED
  (2026-07-18).** Confirmed a real hole: the sandboxed agent connected to the tmux socket
  and drove `send-keys` to self-approve (fs-deny does NOT block socket `connect()`;
  `/dev/tty` was already blocked). RESOLVED by `linux.af_unix_mediation: "pathname"`
  (default `off`) — tested to block the agent's tmux connect while rein (host-side) keeps
  the popup. Downgraded from blocker to a P1 config + unix-socket-allowlist-tuning item
  (§3e). Still needs the prober assertion + the minimal-allowlist determination.
- **UDP exfil** (§3d): open by default, mediable-but-strict-setting-unlocated. Explicit
  Tom decision (deny-all vs deny-except-DNS); a regression from srt's empty-netns until
  closed. Must be resolved at the P3 gate, not slip through.
- **Loopback policy unmeasured** (§1/§3a): if seccomp doesn't mediate `connect(127.0.0.1)`,
  every host loopback service is reachable (regression); if it does, the prober's 407
  check sees `EPERM`. Measure; Tom-level restrict decision if open.
- **F2 anonymous direct-github** (§3c): unproven whether a proxy-bypassing direct connect
  to an `allow_domain` inject host is blocked. No credential, so anonymous access not
  token theft, but a mediation hole. P1 confirm + prober.
- **declare virtual-host tunneling** (§3a): that nono tunnels an unresolvable host by
  CONNECT-name is asserted, never spiked. If it breaks, the write gate stalls.
- **claude config-dir under nono** (§5): no overlay under nono; #94 needs an explicit
  per-run `CLAUDE_CONFIG_DIR` + `~/.claude` deny replacement or the agent can't run / #94
  regresses.
- **macOS / Seatbelt:** entirely untested. nono uses a different sandbox on macOS.
  This doc is **Linux-scoped**; a macOS cutover is a separate open gate requiring its
  own spike (containment parity, CA-env behavior, proxy-auth) BEFORE default-flip on
  macOS. Do not assume Linux results transfer.
- **nono pre-1.0 churn:** nono moves fast (0.68.0; profile schema fields shift — e.g.
  `deny_domain`, `platform_overrides` landed in 0.68.0). Mitigations: `PinnedVersion` +
  vendored digest + the prober's golden report re-run on every bump (the pin-and-
  re-verify policy made mechanical). We trade one moving dependency (srt) for another.
- **General-env CA untested:** CA-env proven for **git only**; a non-git tool (node/
  python) reading rein's CA under Landlock is a P1 confirm-item (§3b), incl. whether the
  CA file needs an allow-read entry.
- **Listener proxy-auth not yet exercised:** the `external_proxy.auth` send path is
  source-confirmed (`external.rs:51`) but the keyring plumbing was NOT run end-to-end in
  the spike (and may require an OS keyring — §2.2). P1a must exercise nono→rein
  authenticated CONNECT for real (valid secret → served; wrong/absent → 407). This is
  **defense-in-depth, not the security boundary** (F3): the real gates are declare +
  classifier + downstream-injection + approval-channel isolation. Forging the secret buys
  a redundant, still-declare-gated path, never the token value or an unapproved push.
- **Full composed profile never run:** the spike's end-to-end proof used `--no-auth` on
  the rein hop, open UDP, no declare host, no tightened profile — the *proven*
  composition is not the *production* composition. Make "**full production profile, all
  knobs on** (both auth hops + UDP policy + declare host + `GIT_CONFIG_GLOBAL` +
  `deny_credentials`), real `git push` + real `gh` write" the explicit **P1 exit gate**,
  so the scattered deltas are forced to converge in one run.
- **New TCB trust root:** nono TLS-terminates nothing rein cares about (rein does the
  github TLS), but nono is now the sandbox + tunnel; "stronger sandbox than srt" was
  asserted, never measured. The prober is how we substantiate/monitor it.
