# Design: rein on nono — build plan (P0–P2 + removing srt)

**Status:** Design of record for the `nono` branch. You can build from this without
re-deciding anything. **Scope: Linux.** macOS (which uses a different sandbox,
Seatbelt) is an open item, covered only as a caveat in §8.

**Evidence:** `docs/nono-git-push-spike-findings.md` (what we tested),
`docs/proposal-rein-on-nono.md` (the approved WHAT), `docs/design-nono-profile-schema.md`
(the exact nono profile fields — authoritative), `docs/containment-probe-harness.md`
(the prober). This doc is the HOW.

## The shape: nono tunnels, rein injects

nono runs the agent in a sandbox and **tunnels** the agent's GitHub traffic to rein as
an opaque stream of bytes — nono does not open it, buffer it, or add the token. **rein**
terminates the TLS, injects the GitHub token, streams the body, and reads the push.

We rejected the alternative where nono injects the token itself. That path only ever
sees `{host, path, method, session}` — never the request body — so it can't run rein's
per-push checks, and small pushes would slip past the approval gate. This design has no
token injection in nono, and no `cmd://` or `credential_capture` anywhere. If that
language shows up in a PR, reject it.

---

## 1. Architecture recap

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

What we proved on Linux (aarch64 + x86_64, real nono 0.68.0, real github.com):

- **The old size cap is gone.** A small push and a **20 MiB chunked `git push` both
  landed** through `nono run` → nono's proxy → rein → GitHub. The 16 MiB cap we saw
  earlier came from nono buffering the body when *nono* did the injecting. When nono only
  tunnels, it never buffers, so there is no cap.
- **git needs one config flag.** nono's proxy demands proxy credentials up front on every
  CONNECT. Plain git doesn't send them and nono drops the connection, so the sandbox git
  config sets `http.proxyAuthMethod=basic`. (This is git authenticating to *nono's own*
  proxy — a different thing from anything on the rein hop; see §3a/§3c.)
- **Exact-host routing works.** With CDN hosts in `upstream_bypass` and rein in
  `upstream_proxy`, `github.com` reached rein while other hosts went straight out (rein
  never saw them). So no token can land on a pre-signed CDN asset URL.
- **nono hides credentials.** With the `deny_credentials` group on, the App key
  (`~/.config/rein-credentials/app.pem`), the gh token, ssh keys, and bash history all
  fail to open inside the sandbox (readable on the host).
- **The agent can't open a direct TCP connection out.** nono's supervisor checks every
  `connect`/`bind` and refuses ones that aren't allowed.
- **UDP is open by default — all of it** (DNS, QUIC, anything). nono *can* route UDP
  sends through its supervisor too, but the default policy lets them through and we
  couldn't find a setting that tightens it. This is a real residual — see §3d.
- **The agent can't reach any loopback port except nono's own proxy.** Tested: a
  sandboxed agent's raw `connect()` to an arbitrary `127.0.0.1` port is refused, while
  going through nono's proxy works. This is what lets rein's loopback listener run
  without a password (§3a).

Integration is **command line + a JSON profile file**, not a library. rein runs
`nono run --profile <file> -- <agent>`. No linking.

---

## 2. P0 — verified installer + profile generator + doctor

New package **`internal/nono`**: `installer.go`, `profile.go`, `doctor.go` (the cobra
wiring stays in `cmd/rein/doctor.go`).

**P0.0 — the nono profile schema is DONE (2026-07-18).** See
`docs/design-nono-profile-schema.md` (authoritative field reference) and
`docs/nono-profile-sample.json` (a profile that actually launches). Code the generator
against those, not against any earlier guess. The key facts the generator must honor:

- The profile is **nested** (`network.*`, `filesystem.*`, `linux.*`, `environment.*`,
  `groups.include`), not a flat struct.
- `upstream_proxy` is a **bare `host:port` string**, not a URL.
- `deny_credentials` is a **policy group** you turn on via
  `groups.include: ["deny_credentials"]`, not a list of paths.
- Env vars go in **`environment.set_vars`**.
- The filesystem is default-deny, so the CA cert is granted via `filesystem.read_file`.
  `filesystem.deny` does nothing on Linux — grant nothing extra rather than trying to
  deny things.
- **nono owns `HTTP(S)_PROXY`/`NO_PROXY`** — it points them at its own proxy and
  overrides anything rein sets. So rein must not set them, and CDN bypass is done with
  `upstream_bypass`, not the agent's `NO_PROXY` (§3c).
- **There is no way to put a password on rein's listener.** nono 0.68.0's
  `external_proxy.auth` is unimplemented — the schema rejects it and the source
  hardcodes it off. This design does not need it (§3a).
- The schema dump also **omits** the `filesystem.unix_socket*` grant fields that the
  profile really accepts — don't treat the dump as complete.

**One coupling to sequence carefully.** `doctor.go` needs the `Check`/`Status` types
that today live in `internal/srt/preflight.go`. Reusing them in place would make
`internal/nono` import `internal/srt` — the opposite of what we want. Fix by extracting
the shared pieces into a neutral package first (§5), not by copying.

### 2.1 Verified installer (`internal/nono/installer.go`)

nono's own install script downloads a release from
`https://github.com/nolabs-ai/nono/releases/download/<version>/<asset>`, fetches
`SHA256SUMS.txt`, and checks the SHA-256 — **checksum only, no signature**. (nono's CI
is moving toward signing, but the releases carry no signature material yet.)

**rein's trust floor is a SHA-256 digest pinned in rein's own source.** Fetching
`SHA256SUMS.txt` over the same connection as the binary is not independent trust — one
compromise serves both. So rein pins the digest in its own source (covered by rein's own
supply-chain hygiene) and verifies the download against that pin.

```go
// PinnedVersion is the nono release this profile schema + behavior are verified
// against. A mismatch means the shape or the egress semantics may differ; the installer
// refuses and doctor warns (same policy as srt.PinnedVersion).
const PinnedVersion = "0.68.0"

// pinnedDigests maps (version, platform) -> {tarball, binary} lowercase-hex SHA-256,
// vendored in rein's source (the trust floor — never the fetched SHA256SUMS.txt).
// Regenerated by hand on a version bump after out-of-band verification. The platform
// key is nono's rustc target triple, e.g. "x86_64-unknown-linux-gnu". Two digests
// because releases are tarballs: the tarball digest gates the download, the inner-binary
// digest is what VerifyInstalled re-hashes on disk.
var pinnedDigests = map[string]map[string]struct{ Tarball, Binary string }{}

type InstallParams struct {
    Version  string // default PinnedVersion
    Platform string // default detected: <arch>-unknown-linux-gnu
    DestDir  string // rein-managed; default managedNonoDir() (see 2.1.1)
    HTTPGet  func(url string) (*http.Response, error) // injectable for tests
}

// Install downloads the release tarball, verifies its digest, extracts the inner nono
// binary, and atomically places it at DestDir/nono (0o755). Fail-closed: any digest
// mismatch, missing pin, tar path-traversal, or partial download deletes temp files and
// returns an error — it NEVER installs an unverified binary. Returns the installed path.
func Install(p InstallParams) (string, error)

// VerifyInstalled re-hashes the on-disk binary and compares to the pin. Used by doctor
// and by the launch path (§2.3).
func VerifyInstalled(path, version, platform string) error
```

**Extraction is its own step** because releases are tarballs, not bare binaries: download
tarball → verify tarball digest → extract → **reject any `..` or absolute tar member** →
place the inner binary. That's why the pin holds **two** digests per (version, platform):
the tarball digest gates the download, the binary digest is what `VerifyInstalled`
re-hashes. Re-hashing the on-disk binary against a *tarball* digest would never match.

Failures: unsupported OS/arch → error naming the supported set; no pin for
(version, platform) → error "no vendored digest; bump `internal/nono.pinnedDigests`"
(never fall through to trusting `SHA256SUMS.txt`); digest mismatch → error with both
hashes and the temp file deleted. Download is atomic (temp file in `DestDir` + rename).

**Signature upgrade path (documented, not built):** when nono publishes sigstore bundles
or GitHub attestations, add a signature check *before* the digest check; keep the
vendored digest as belt-and-suspenders. Track as a follow-up issue.

#### 2.1.1 rein-managed path (closes the binary-shadowing gap)

srt was found via `exec.LookPath("srt")` — a `$PATH` entry the agent's environment could
shadow. nono is run by **absolute path from a rein-managed directory**, never `LookPath`:

```go
func managedNonoDir() string  // filepath.Join(config.ConfigDir(), "nono", "bin")
func ManagedNonoPath() string  // managedNonoDir()/nono — the ONLY path rein runs
```

`rein init` installs into `managedNonoDir()`; `rein run --nono` runs exactly
`ManagedNonoPath()` after `VerifyInstalled` passes.

### 2.2 Profile generator (`internal/nono/profile.go`)

Emits the exact JSON profile rein hands to `nono run --profile`. One source of truth;
`cmd/rein/run_nono.go` writes it on each launch.

The struct mirrors nono's real (nested) schema. See `docs/design-nono-profile-schema.md`
for the field-by-field detail; the shape is:

```go
type Profile struct {
    Schema      string      `json:"$schema,omitempty"`
    Meta        *Meta       `json:"meta,omitempty"`
    Groups      Groups      `json:"groups"`      // Include: ["deny_credentials", ...]
    Network     Network     `json:"network"`
    Linux       Linux       `json:"linux"`
    Filesystem  Filesystem  `json:"filesystem"`
    Environment Environment `json:"environment"`
}
type Groups struct {
    Include []string `json:"include"` // MUST include "deny_credentials" (a policy group, not a path list)
    Exclude []string `json:"exclude,omitempty"`
}
type Network struct {
    Block          bool     `json:"block"`                     // false — true is incompatible with proxy mode
    AllowDomain    []string `json:"allow_domain"`              // InjectHosts ∪ CDNHosts ∪ ExtraDomains ∪ DeclareHost
    UpstreamProxy  string   `json:"upstream_proxy,omitempty"`  // "127.0.0.1:PORT" — bare host:port, no scheme, no auth
    UpstreamBypass []string `json:"upstream_bypass,omitempty"` // = CDNHosts (nono sends direct); needs UpstreamProxy set
    // No auth field exists. Do not attempt external_proxy.auth — validate rejects it, it's unimplemented.
}
type Linux struct {
    AfUnixMediation string `json:"af_unix_mediation,omitempty"` // "pathname" — the approval-channel isolation control (§3e)
}
type Filesystem struct {
    ReadFile   []string `json:"read_file,omitempty"`   // CA PEM path (fs is default-deny, so grant read)
    UnixSocket []string `json:"unix_socket,omitempty"` // allowlist under af_unix_mediation; start EMPTY, never tmux
}
type Environment struct {
    SetVars map[string]string `json:"set_vars,omitempty"` // arbitrary env; do NOT set PATH/NONO_*/HTTP(S)_PROXY/NO_PROXY — nono owns those
}
```

```go
type Params struct {
    ListenAddr    string   // "127.0.0.1:<port>" — rein's loopback listener
    ProfilePath   string   // where the generated profile is written
    CACertPath    string   // agent-READABLE dir — the CA PEM the sandbox tools read (§3b)
    ExtraDomains  []string // operator opt-in (api.anthropic.com, npm, …) — egress only, NEVER injected
    GitConfigEnv  map[string]string // GIT_CONFIG_* wiring
}

// Build assembles the profile. Invariants enforced here (fail closed on violation):
//   - AllowDomain = proxy.InjectHosts ∪ proxy.CDNHosts ∪ ExtraDomains ∪ DeclareHost
//   - UpstreamProxy routes to rein; UpstreamBypass = proxy.CDNHosts (direct, never rein)
//   - InjectHosts + DeclareHost are NOT in UpstreamBypass (they must reach rein)
//   - ExtraDomains are NEVER injected (egress-only direct TLS)
//   - SetVars carries the four CA vars + GIT_CONFIG_* — and NOT HTTP(S)_PROXY/NO_PROXY (nono owns those, §3c)
func Build(p Params) (Profile, error)
func (pr Profile) MarshalIndent() ([]byte, error)
```

The CA PEM must go in a dir the agent **can** read (the sandbox tools trust it), separate
from anything secret. Under nono there is no per-session proxy secret to hide anymore
(§3a), but keeping the CA in its own dir is still the right habit — a dir-wide read grant
shouldn't drag other files in with it.

Host lists stay in `internal/proxy/hosts.go` (`InjectHosts`, `CDNHosts`, `DeclareHost`);
the generator reads them so the two never drift. **`upstream_bypass` = `CDNHosts`
verbatim** — that's the exact-host routing property (§3c).

### 2.3 doctor nono health checks (`internal/nono/doctor.go`)

Replaces the four srt rows. Uses the `Check`/`Status` types — which today live in
`internal/srt/preflight.go`. srt isn't deleted until P3, so **extract those types into a
neutral package first** (§5) rather than importing srt from `internal/nono`.

| Row | Check | Hard fail? |
|---|---|---|
| `nono present` | `ManagedNonoPath()` exists + executable | yes |
| `nono version` | binary version == `PinnedVersion` | yes |
| `nono digest` | `VerifyInstalled` (on-disk SHA-256 == pin) | yes |
| `nono landlock` | `nono setup --check-only` reports Landlock supported | yes |
| `nono seccomp` | supervisor connect/UDP mediation available | yes |
| `CA trust env` | rein CA PEM readable + non-empty | yes |

`nono setup --check-only` is nono's own health probe. The launch path
(`rein run --nono`) hard-gates the same rows and fails closed — it never silently drops
to a weaker mode.

---

## 3. P1 — compose: re-front the proxy, CA, routing, UDP, prober

New file **`cmd/rein/run_nono.go`** (`func runNono(cmdline []string) (int, error)`), the
nono counterpart of `run_sandboxed.go`. `rein run` reaches it behind **`--nono`**; srt
stays the default until P3. It reuses the broker/mint/scope machinery unchanged — only
the *front* of the proxy and the *sandbox launch* differ.

### 3a. Re-front the proxy: unix socket → loopback TCP (no password needed)

Under srt, rein's proxy listened on a **unix socket** whose parent-directory permissions,
plus being placed outside every bind-mount, were the thing that kept the agent out. A
**loopback TCP port** is different: normally anything that can `connect(127.0.0.1:port)`
can reach it. The question was whether moving to a TCP port opened a hole the agent could
walk through.

**It doesn't, because nono blocks it.** Tested: a nono-sandboxed agent's raw `connect()`
to an arbitrary loopback port is refused, while going through nono's own proxy works.
nono lets the agent reach only nono's proxy on loopback — **the agent cannot reach rein's
listener directly.** So rein's listener needs no password: nono is the only thing that
can reach it. (nono 0.68.0 also *can't* carry a proxy password even if we wanted one —
`external_proxy.auth` is unimplemented. We don't need it.)

**Even a direct hit wouldn't hand over the token** — this is the defense-in-depth story,
worth keeping straight. Every connection rein accepts runs the same pipeline:
terminate TLS → inject token → read the push (declare tap) → classify → upstream. The
token is injected on the rein→GitHub leg, **downstream of the sandbox**, so its value
never enters the sandbox no matter how the connection got there. And it buys no new
capability: the agent already has a legitimate token-injected tunnel through nono;
reaching the port another way would just be a second path to the same declare-gated,
classified pipeline. So the real gates are: declare + classifier + downstream injection +
**approval-channel isolation (§3e — the actual crux).**

**One thing to confirm in P1:** that the pipeline really does run the declare tap and
classifier on *every* accepted connection regardless of how it arrived. That's inferred
from the diagram, not yet proven. Confirm it (and the denial semantics below) before
leaning on it.

**Phasing: P1a is additive; deletions are P3.** srt stays the default until the P3
cutover, so the unix-socket front must keep working the whole time. P1a **adds** the
loopback-TCP front alongside the unix-socket front (both compiled; `Start` picks based on
which config is set). It does not replace or delete the srt path. A hard swap here would
break the default `rein run` on the branch and defeat "rollback = revert one PR."

**Code changes in `internal/proxy`:**

- **`proxy.go handleConn`** already consumes a CONNECT preamble and supports a direct TLS
  client. Under nono a CONNECT is mandatory; keep the no-SNI fail-closed as-is. No
  proxy-auth parsing is needed (nono is the only reachable client).

- **Injection is an unconditional overwrite**, not add-if-absent: rein replaces any
  `Authorization` the agent supplied before forwarding upstream. (The risk is low — the
  agent has no valid token, so suppressing injection would only deny itself — but
  overwrite is the stated invariant for robustness.)

- **`placement.go` — add now, delete at P3:**
  1. **Add** a loopback TCP listener (P1a); keep the unix-socket `Listen` body until P3:
     ```go
     // ListenLoopback binds 127.0.0.1:0 (or a fixed port) and returns the listener and
     // the chosen addr. No placement check, no chmod dance — nono's loopback mediation
     // is the capability gate now (§3a), not filesystem perms.
     func ListenLoopback() (*net.TCPListener, string, error)
     ```
     Bind explicitly to `127.0.0.1` (never `0.0.0.0`) so no other host can reach it.
  2. **Delete `CheckPlacement` + `pathWithin` and the unix-socket `Listen` body — at P3**
     (no bind-mount under nono ⇒ no socket-placement gate to defend). Remove their tests
     then.
  3. **Keep** `ResolveAbs` + `cleanAbs` — still needed by `run_nono.go` to resolve the
     working tree. Move them to a non-placement file (e.g. `internal/proxy/paths.go`)
     when `placement.go` is emptied.

- **`runbroker.Config`:** stays additive P1a→P3 — keep `SocketPath`/`ForbiddenDirs` (unix
  front) and **add** `ListenAddr` (loopback front); `Start` selects on which is set.
  Remove the srt fields at P3.

`declare.rein.internal` (§3, below) rides the same listener: nono CONNECTs to it by
hostname, rein terminates with its CA and answers locally, token-free. It must be in
`allow_domain` and must NOT be in `upstream_bypass` (that would send it direct →
unresolvable).

- **Confirm-item (untested):** that nono tunnels an *unresolvable* virtual host by its
  CONNECT hostname without doing DNS. The spike used real github, so this is asserted, not
  proven. nono might try to resolve or reject an unknown-TLD entry. The declare ceremony
  is the heart of the write gate; if this breaks, P1 stalls. Track it as a named P1
  confirm-item.
- **Invariant — declare informs, it cannot approve.** The agent *can* reach
  `declare.rein.internal` through the tunnel. Safety rests entirely on approval being
  gated out-of-band on the terminal (§3e): the declare HTTP surface may *state* intent but
  must never be able to *drive* an approval.

**Hard-constraint #2 + denial semantics.** Specify all three under this model:

- **Credential helper:** rein injects host-side, so there is likely no sandbox-side git
  credential helper at all. If so, #2's "always return a credential" is **moot** — say so
  explicitly. If any helper survives, the always-return invariant must be kept.
- **Denial semantics (load-bearing, currently undefined):** when a push is *not* approved
  at the declare gate, the agent's git must get a **clean fail-closed HTTP status**, not a
  hang or an empty-credential retry loop. This is what makes "still declare-gated" safe;
  specify and test it (§3e).
- **Token-leak boundary:** rein injects downstream of the sandbox, so even a direct-to-port
  connection yields authenticated *actions*, never the token *value* — provided (a) no
  inject host reflects the `Authorization` header back (all inject hosts are real github,
  none echo auth — make this an explicit confirm) and (b) `ExtraDomains` are never
  injected (enforced in `Build`). No reflection oracle ⇒ no token exfil.

### 3b. CA trust via env (no bind-mount under Landlock; fail-closed)

nono uses Landlock and has **no mount namespaces**, so srt's trick (bind rein's CA bundle
read-only over the system trust path) isn't available. CA trust is **env/config based**,
the same way nono trusts its own intercept CA:

- rein writes its CA PEM to the agent-readable CA dir, and the profile's `set_vars` points
  **all four** CA vars at it — `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`,
  and **`CURL_CA_BUNDLE`** (keep the full set from srt's `caEnvVars`; dropping
  `CURL_CA_BUNDLE` loses curl/libcurl trust). Extract these constants into the neutral
  package (§5); don't import srt.
- **Hard-constraint #6 — the CA *signing* key is a private key.** The PEM written here is
  the CA *cert* (public), fine to expose. But rein's MITM CA signing key must still load
  through `internal/keystore` (Get/Fingerprint), never `os.ReadFile` — confirm the
  existing `proxy/ca.go` path is carried forward unchanged (it already loads a
  keystore-backed ECDSA P-256 key). (Aside: rein's CA is already ECDSA P-256, so the old
  "nono ring requires EC P-256" worry doesn't apply — nono tunnels, it doesn't terminate
  rein's CA.)
- **Fails closed:** an agent that unsets the CA var gets a TLS *failure* (rein's leaf is
  untrusted), not a bypass — because there's no unmediated path to a real github cert
  (§3c confirms this). Fail-closed rests on the egress allowlist + nono's mediation, not on
  the git config default below.
- **`GIT_CONFIG_GLOBAL` is a convenience default, not a control.** git's local repo config
  (`.git/config` in the writable working tree), `-c` flags, and `GIT_SSL_*`/`http_proxy`
  env all *override* it, so the agent *can* override `http.sslCAInfo`/`http.proxy`/
  `http.sslVerify`. That's fine: none of those overrides yields a bypass, because the real
  controls are the egress allowlist and nono's mediation. Overriding local config is
  exactly *how* an agent would try the direct-github path — which is blocked (§3c). Treat
  the pin as a helpful default only.

Whether the CA PEM path itself needs an explicit read grant under Landlock (so the tools
can read it) is a **P1 confirm-item** — which is exactly why it lives in its own dir.
Verify for git **and** a non-git tool (node); the spike proved git only.

### 3c. Host-routing (exact hosts → rein, CDN → direct)

`upstream_proxy` = rein's listener; `upstream_bypass` = `CDNHosts`; only `InjectHosts` +
`DeclareHost` reach rein. Sandbox git config sets **`http.proxyAuthMethod=basic`** so git
sends its proxy credentials up front to nono's proxy (nono injects a
`nono:<token>` basic credential into the agent's `HTTPS_PROXY` — that token authenticates
only to nono's own proxy and is fine for the agent to see; the GitHub token is injected
downstream by rein and never enters the sandbox).

- **Non-git tools need proxy env — but nono provides it.** `gh`, node, and python don't
  read git config. They still get proxied because **nono sets their `HTTPS_PROXY` for
  them** (nono owns those vars — §2.2). rein must *not* set `HTTP(S)_PROXY`/`NO_PROXY`;
  nono overrides them anyway. CDN bypass is `upstream_bypass` (nono → direct), not the
  agent's `NO_PROXY` (nono pins that to `localhost,127.0.0.1`).
- **No unmediated path to github — VERIFIED (2026-07-18).** `github.com` is in
  `allow_domain` as an inject host. We tested whether a **direct
  `connect(github.com:443)` that bypasses the proxy** (e.g. the agent clears `http.proxy`
  in the writable `.git/config`) is blocked. **It is blocked** — `allow_domain` grants
  *proxy-mediated* egress, not direct, and seccomp refuses the direct connect. So the
  agent's only path to github is through rein. Keep the prober assertion as a regression
  guard.
- **Two auth hops — don't conflate them.** Hop 1 is git → nono (nono's own proxy
  credential, `http.proxyAuthMethod=basic`). Hop 2 is nono → rein (a plain CONNECT tunnel,
  **no auth** — §3a). These are separate; the P1 exit gate (§7) exercises both end to end.

### 3d. UDP policy — DECISION FOR TOM (recommend fail-closed if possible)

**State:** direct TCP egress is strong (seccomp refuses it, same as srt). **UDP egress is
open by default — all of it** (DNS, QUIC, arbitrary): tested inside `nono run`, `sendto`
to `8.8.8.8:53`, `8.8.8.8:12345`, and `1.1.1.1:443` all succeed. srt's empty network
namespace blocked this. nono *can* route UDP sends through its supervisor, but the default
allows them and the strict setting couldn't be found.

**Investigated (2026-07-18) — no clean fix under nono; a real residual, Tom's call.**
There is **no fine-grained UDP control** in nono: the only knob is `network.block`
(all-or-nothing), and `block: true` is **incompatible with proxy mode** (nono errors:
domain filtering requires proxy mode), so it can't be combined with rein's proxy. Landlock
doesn't touch UDP either. So under the proxy mode rein requires, **UDP stays open** — a
general exfil channel that srt's empty namespace blocked. Unlike the loopback question
(which nono's mediation closes), there's no nono config that fixes this.

**Decision for Tom (a genuine accept/block choice, not a config task):**
- **Accept the residual.** rein's threat model weights credential theft over data exfil,
  and the credential boundary is intact: the agent can't get a token (proven) and can't
  exfil over TCP (blocked). What it *can* do is send out data it already sees (the working
  tree) over UDP. For rein's core purpose — the agent never holds a credential — this
  holds. UDP exfil is a data-confidentiality regression from srt, documented.
- **Block on it / mitigate later.** A host-side firewall (nftables) blocking UDP egress
  from the sandboxed process — but scoping it to just the agent is hard under nono (same
  uid, no network namespace; would need a cgroup nono exposes). Non-trivial, maybe needs
  nono support. Not for the first cutover.

**P3 cutover gate:** this decision must be made explicitly (accept + document, or block),
not slipped through.

### 3e. The prober (fail-closed launch gate + CI harness)

Two layers, per `docs/containment-probe-harness.md`, retargeted from srt to nono.

**The crux — approval-channel isolation.** srt isolated the agent with namespaces. nono
has **no namespaces** (Landlock + seccomp only), so the agent runs as an ordinary host
process at rein's uid in the same terminal session. Nothing *obviously* stops it from
(a) opening `/dev/tty` on the shared terminal, or (b) connecting to the **tmux server
socket** and driving the approval popup with `tmux send-keys` — self-approving its own
push through the legit tunnel, silently defeating the one guarantee rein exists to
provide, while the journey tests stay green (they drive approval host-side).

**Confirmed a real hole (2026-07-18), and the obvious fix doesn't work.** Inside
`nono run` with a default-ish profile: the agent got `$TMUX` passed through (which leaks
the socket path); it could not `ls` the tmux dir; but it **did `connect()` to the tmux
socket and `send-keys` a command that ran** — self-approval proven. `/dev/tty` was blocked
(good — a tty-based approval is safe). The naive fix fails: adding `/tmp/tmux-<uid>` to
`filesystem.deny` makes nono **refuse to launch**, because Landlock has no "deny under an
allowed parent," and nono's shared-memory IPC restriction only blocks *abstract* sockets,
not filesystem sockets like tmux's.

**Resolved (2026-07-18) — `linux.af_unix_mediation: "pathname"` closes it and keeps the
tmux popup UX.** nono ships an opt-in `af_unix_mediation` field, default `off` (which is
why the hole was open). Set to **`"pathname"`**, it deny-by-default mediates pathname
AF_UNIX connect/bind, so sockets must be granted back explicitly via
`filesystem.unix_socket*`. **Tested:** with `af_unix_mediation: "pathname"` and no socket
allowlist, the agent's `connect()` to the tmux socket is refused, `send-keys` is blocked,
and the agent still runs fine. Crucially this constrains **only the sandboxed agent** —
rein drives the approval popup **host-side, outside the sandbox**, so the interactive tmux
approval UX is preserved.

**P1 work (config + allowlist tuning, not a redesign):** set
`af_unix_mediation: "pathname"` in the profile; also scrub `$TMUX` (defense-in-depth — the
mediation is the real control); figure out the *minimal* set of pathname unix sockets a
real agent (claude/node/gh — DNS resolver, etc.) genuinely needs and grant only those via
`filesystem.unix_socket*`, **never the tmux/approval socket**; verify the agent still runs.
**Cost tested (empty allowlist): low** — DNS, `getent`, git, curl-through-the-proxy, node,
and gh all still worked; nono blocked 6 non-essential unix-socket ops and nothing
essential broke. So the allowlist can **start empty** and grow only if a specific tool
needs a specific socket (P1 must verify the *real* `claude` agent runs clean). The prober
must assert (fail closed otherwise): tmux socket connect **denied**, `send-keys` denied,
`/dev/tty` unopenable, `$TMUX` absent. This preserves rein's non-replayability guarantee
(#32) under nono.

1. **Launch gate = nono counterpart of `VerifyConfigApplied`.** A tiny, dependency-free,
   in-binary check that runs on every `rein run --nono` *before* the agent and fails
   closed. It launches the probe *through* the real `nono run` path so it inherits the
   exact profile. That needs a probe **subcommand dispatched inside the nono sandbox**
   (reuse the hidden `rein __sandbox-probe` or add one) plus the `main.go` wiring — the
   same seam `srt/selftest.go RunProbe` uses. Asserts:
   - App key / gh / ssh **unreadable**;
   - direct TCP to a non-allowlisted host **blocked**;
   - direct (non-proxy) connect to an `allow_domain` **inject host blocked** (guards §3c);
   - **arbitrary loopback ports unreachable except nono's proxy** (this is the control
     that lets rein's listener run without a password — §3a);
   - **arbitrary UDP blocked** (guards §3d; only once a UDP policy is set);
   - **approval channel unreachable:** `/dev/tty` unopenable, tmux socket unconnectable,
     `send-keys` blocked (the crux above).

   Lives in `internal/nono/selftest.go` (mirrors `srt/selftest.go`). Keep it bespoke and
   tiny; do not swap an external toolkit into this slot.
2. **Verification harness = `controlplaneio/sandbox-probe`** (Apache-2.0, a test/CI
   dependency never linked into the binary — same posture as `pyte`). Run it on the host
   and through `rein run --nono`, diff the two, classify against rein's emitted profile
   (`deny_credentials`, `allow_domain`, inject vs CDN), and emit a golden report wired into
   a `tests/interactive/` journey. Drift = red = re-review. This is how we keep trusting
   nono across version bumps.

Land the prober against **current srt** on `main` first (sibling of issue #136) so it
exists before the pivot — it's substrate-agnostic by design.

---

## 4. P2 — minimize + fuzz the proxy

With nono owning the sandbox and CDN going direct, whole arms of the proxy are dead — but
**deleting them is P3, not P2.** srt has no `upstream_bypass`, so under the still-default
srt path CDN hosts *do* reach rein and still need `classPassthrough`; deleting it at P2
would break the default. P2 *fuzzes* (srt-safe); the deletions land in the atomic P3
cutover.

- **Delete `classPassthrough` (at P3)** — the CDN-relay arm and its `classifyHost` case
  for `objects/codeload/raw.githubusercontent.com`: under nono, CDN hosts never reach rein
  (they're in `upstream_bypass`). **Keep the `CDNHosts` list** in `hosts.go` — the profile
  generator needs it to populate `upstream_bypass`. Easy to over-delete: the list lives on,
  the relay arm dies. **Update the `hosts.go:24` comment** ("must NOT appear in
  `mitmProxy.domains`") when CDNHosts is repurposed — under nono, CDN hosts go in *both*
  `allow_domain` and `upstream_bypass`.
- **Request-body size gate:** with nono's 16 MiB body cap gone, rein becomes the only
  body-size gate. The streaming relay must not accumulate the body unbounded; add a sane
  upper limit (low-severity self-DoS surface) — confirm this when minimizing.
- Keep the hand-rolled streaming relay (SNI==Host, no token on the response, HTTP/1.1
  relay, ContentLength/TransferEncoding copy, no-redirect-follow) and the receive-pack
  **declare tap**. These are rein's security-critical differentiators; no library fits
  (goproxy would need a gut-and-audit, go-mitmproxy buffers bodies, gomitmproxy is GPL).
- **Fuzz (issue #136):** `go test -fuzz` against `ParseReceivePackCommands`
  (receivepack.go) and the host/tier classifier (`classifyHost` + the GraphQL read/write
  tier resolution in gate.go). Seed from the existing `receivepack_test.go`/`gate_test.go`
  cases. This is the surface an adversarial in-sandbox agent actually touches — higher
  value than more containment probing.

Land #136 fuzzing on `main` unconditionally (no new dependency, a security win either way).

---

## 5. Removing srt

**When:** srt stays fully working behind the default until **P3 — one atomic cutover PR**
that (i) flips the default `rein run` from srt to nono and (ii) deletes srt in the same
commit. **Gated on** all of:
- dogfood-proven nono (a human ran the real agent through the nono stack and pushed real
  code);
- a green prober including the new assertions (§3e): F2 direct-github blocked,
  approval-channel isolation (tty + tmux socket + send-keys), loopback-only-nono-proxy;
- **UDP policy resolved** — either a fail-closed setting is in place, or Tom has explicitly
  accepted open UDP (§3d). Cutover must not slip through with UDP unresolved.
- green re-pointed journeys.

**Do not delete srt before nono is dogfood-proven.** Rollback = revert the one PR — which
only works because P1a/P2 were additive (§3a/§4).

**Extract shared substrate first (do at/before P1a, not P3).** `Check`/`Status`
(preflight.go), the CA-env constants (`caEnvVars`), `ResolveExtraAllowedDomains`
(domains.go), and the `agentenv` helpers move into neutral packages so `internal/nono`
never imports `internal/srt` during P0–P2. A small early "extract shared substrate" PR
avoids the import-untangling churn.

**What gets deleted (P3):**

| Target | Verdict | Note |
|---|---|---|
| `internal/srt/*.go` (~3972 LOC incl. tests) | **delete all** | config, env, cabundle, domains, preflight, selftest, socket_*, githard, allowread |
| `cmd/rein/run_sandboxed.go` + `_test.go` | **delete** | replaced by `run_nono.go` |
| `cmd/rein/sandbox_home.go` + tests | **assess** | if the $HOME/overlay construction is srt-mount-specific → delete; nono uses `deny_credentials`, not mount overlays. Likely delete; audit for reusable env-scrub helpers first |
| `cmd/rein/sandbox_claude_home.go` + tests | **P1 replace, not assess** | #94's property (agent gets a rein-owned `CLAUDE_CONFIG_DIR`; real `~/.claude` unreadable) is user-facing security AND the real agent needs a *writable* config dir to run at all. Under nono there's no overlay: either `~/.claude` is denied and claude breaks, or it isn't and #94 regresses. Specify the nono replacement — per-run `CLAUDE_CONFIG_DIR` + Landlock deny on `~/.claude` — as **P1 work**. The `realagent_write`/`claude_resume` journeys depend on it |
| `internal/proxy/placement.go` (`CheckPlacement`/`pathWithin`, unix-socket `Listen` body) | **delete at P3** | additive until then (§3a); `ResolveAbs`/`cleanAbs` are MOVED, not deleted |
| doctor srt rows | **replace** (P2/P3) | → nono rows (§2.3) |
| `main.go` srt dispatch + preflight | **replace** | → nono dispatch; flip default |

**Env vars — per-var verdict (these are srt's filesystem model):**

| Env var | Verdict |
|---|---|
| `REIN_SANDBOX_ALLOW_READ` / `REIN_SANDBOX_SHOW_HOME` (allowread.go) | **delete** — srt deny-read/allow-back mount semantics; nono uses `deny_credentials` + Landlock allow-read, a different surface. If an escape-hatch is still wanted, add a NEW var under nono semantics, don't carry the srt one |
| `REIN_SANDBOX_ALLOW_UNHARDENED_GIT` + `githard.go` | **assess → likely delete** — this hardening guarded srt's `.git` **bind-mount** threat (a writable bound `.git` with hooks). Under nono there's no bind-mount. Confirm the nono threat (can the agent write `.git/hooks` in the working tree, and does that matter under nono?) before deleting; if the threat survives, port the check, else delete |
| `REIN_ALLOW_DOMAINS` (domains.go `ResolveExtraAllowedDomains`) | **keep** — operator egress opt-in is substrate-agnostic; move the resolver into `internal/nono` (feeds `allow_domain` + `ExtraDomains`, never inject) |
| `REIN_DISABLE_CLAUDE_MCP`, `REIN_REPO_WORKTREES`, `REIN_EPHEMERAL_CLONE_DIR`, `REIN_UPSTREAM_INTENT_FILE`, agent-contract | **keep** — not srt-specific; move helpers into a neutral package (`internal/agentenv` or `internal/nono`) |

**srt-coupled seams to cut (every one):**

- Go imports of `internal/srt`: `cmd/rein/{run_sandboxed,sandbox_home,doctor,main}.go` +
  tests `run_sandboxed_test.go`, `sandbox_home_e2e_test.go`. Each removed or repointed to
  `internal/nono`.
- `runbroker.Config.SocketPath`/`ForbiddenDirs` (§3a) — cut, → `ListenAddr`.
- `proxy.Listen` unix-socket / `CheckPlacement` callers.
- doctor `sandboxDoctorChecks` srt.Preflight wiring (`doctor.go:455-500`).
- Banner/preflight helpers in `run_sandboxed.go` that print srt paths
  (`preflightSrtPath`, `printPreflightAndOK`) — reimplement for nono or drop.
- Tests: `gitidentity_srtexclude_test.go` (name references srt), the srt-containment
  interactive tests (§6).

**End state (srt gone):** `internal/nono` (installer, profile, doctor, selftest) +
`internal/proxy` (streaming inject relay + declare tap + loopback-TCP front, no placement,
no classPassthrough) + unchanged broker/keystore/runscope. `rein run` defaults to nono;
`--direct` unchanged. ~3972 LOC of srt plus the dead proxy arms gone; the security-critical
code rein still owns is the small, fuzzed relay.

---

## 6. Testing

- **Re-point the broker journeys srt→nono and regenerate the goldens.** The user-visible
  path is unchanged (declare/approve/push/scope/git-identity/gh-writes), so the journeys
  in `tests/interactive/journeys/` (write_ceremony, gh_write, scope_expansion, multi_repo,
  push_upstream, git_author, expansion_404, sandbox_gh_read_staleness, realagent_write,
  credential_boundary, claude_resume) run against the nono stack; regenerate the golden
  transcripts via `tests/interactive/run-journeys.sh`. Green = behavior preserved.
- **Drop the srt-containment tests; the prober replaces them.** The `sandbox_filesystem`
  journey and any srt-mount-specific assertions → the sandbox-probe golden report (§3e)
  against nono.
- **New nono-stack push journey:** a real `git push` through `nono run` → nono's proxy →
  rein → real github (throwaway repo, hard-constraint #1), **including a >16 MiB chunked
  push** (the case that proves the cap is gone). Golden-transcript backed.
- **Unit tests:** `internal/nono` installer (digest match/mismatch/missing-pin,
  atomic-place, unsupported platform — inject `HTTPGet`); profile generator (invariants:
  CDN in bypass not inject; inject/declare not in bypass; extra domains never injected).
- **#136 fuzzing:** `ParseReceivePackCommands` + classifier (§4).

`tests/interactive/CLAUDE.md` rules apply: default-keep golden, `SBX|` view-split,
`rein init` setup (not `source ./dev-env`). The new push journey and the re-pointed goldens
are the human-reviewable deliverables — green tests alone are not enough (CLAUDE.md).

---

## 7. Build / branch plan

Long-lived **`nono` branch**, kept in sync with `main`. `main` keeps shipping srt until
P3. Each phase is one (or a few) PR(s) into `nono`.

**Land on `main` NOW, unconditionally (no new runtime dependency, wins either way):**
- #136 fuzz the receive-pack parser + classifier.
- The prober adopted against **current srt** (substrate-agnostic).

**PRs into `nono`:**

| PR | Phase | Depends on | Parallel? |
|---|---|---|---|
| schema dump — nono profile schema + one working profile | P0.0 | — | **yes**; **gates the profile generator** |
| extract shared substrate (`Check`/`Status`, `caEnvVars`, domains resolver, agentenv) | P0 | — | **yes**; before doctor + P1a |
| `internal/nono` installer + pins (tarball + binary digests) | P0 | — | **yes** (standalone) |
| `internal/nono` profile generator | P0 | P0.0 schema + proxy host lists | after P0.0 |
| doctor nono health rows | P0 | installer + substrate extract | after installer |
| proxy re-front: **add** loopback TCP front (additive; srt front kept) | P1a | substrate extract | **yes** (proxy-local; parallel with P0) |
| `run_nono.go` + `--nono` dispatch | P1 | P0 + P1a | serial (integrates) |
| CA-env + host-routing + git proxyAuthMethod | P1b,c | run_nono | with run_nono |
| UDP policy locate/decide | P1d | — | **yes** (research); profile generator gains the UDP-deny field only after this |
| claude config-dir nono replacement (#94) | P1 | run_nono | with run_nono |
| prober retarget to nono (launch gate + dispatch + harness) | P1e | run_nono | after run_nono |
| proxy **fuzz** (classPassthrough deletion deferred to P3) | P2 | — | **yes** (parallel; srt-safe) |
| **P3 cutover:** flip default + delete srt + all deferred deletions | P3 | ALL above + dogfood + green prober + UDP resolved | atomic, serial, LGTM-gated |

**Flag:** `rein run` defaults to srt through P0–P2; `--nono` opts into the new stack. P3
flips the default and deletes srt in one commit. `--direct` is untouched throughout.

**Concurrency:** P0.0 gates the profile generator; the substrate extraction gates doctor +
P1a. After those, P0 (installer, profile), P1a (additive proxy front), P1d (UDP research),
and P2 (fuzz) run in parallel — different files, no ordering. `run_nono.go` integration
(P1) serializes them; the prober and cutover follow. **No srt-breaking deletion lands
before P3** (§3a/§4 additive), which is what keeps "rollback = revert one PR" true.

Each phase **stops-and-surfaces** at its gate. P1d (UDP) and P3 (cutover) are explicit Tom
decisions.

---

## 8. Risks carried from the findings

- **Approval-channel isolation — was the TOP risk; confirmed then resolved (2026-07-18).**
  Confirmed a real hole: the sandboxed agent connected to the tmux socket and drove
  `send-keys` to self-approve (`filesystem.deny` does *not* block socket `connect()`;
  `/dev/tty` was already blocked). Resolved by `linux.af_unix_mediation: "pathname"`
  (default `off`), tested to block the agent's tmux connect while rein keeps the popup
  host-side. Downgraded to a P1 config + unix-socket-allowlist task (§3e). Still needs the
  prober assertion and the minimal-allowlist determination.
- **UDP exfil (§3d):** open by default, no nono config fixes it. Explicit Tom decision
  (accept vs block); a data-confidentiality regression from srt's empty namespace until
  closed. Must be resolved at the P3 gate, not slipped through.
- **F2 anonymous direct-github (§3c):** confirmed blocked — a proxy-bypassing direct
  connect to an inject host is refused by seccomp. Keep the prober check as a regression
  guard.
- **declare virtual-host tunneling (§3a):** that nono tunnels an unresolvable host by its
  CONNECT name is asserted, never spiked. If it breaks, the write gate stalls.
- **claude config-dir under nono (§5):** no overlay under nono; #94 needs an explicit
  per-run `CLAUDE_CONFIG_DIR` + `~/.claude` deny, or the agent can't run / #94 regresses.
- **macOS / Seatbelt:** entirely untested. nono uses a different sandbox on macOS. This
  doc is **Linux-scoped**; a macOS cutover is a separate open gate needing its own spike
  (containment parity, CA-env behavior) *before* flipping the default there. Don't assume
  Linux results transfer.
- **nono is pre-1.0 and moves fast** (0.68.0; profile fields shift — `deny_domain`,
  `platform_overrides` landed in 0.68.0). Mitigations: `PinnedVersion` + vendored digest +
  re-running the prober's golden report on every bump. We trade one moving dependency (srt)
  for another.
- **CA trust untested beyond git:** proven for git only; a non-git tool (node/python)
  reading rein's CA under Landlock is a P1 confirm-item (§3b), including whether the CA file
  needs an explicit read grant.
- **Loopback isolation not yet exercised end-to-end in the full stack.** The
  arbitrary-loopback-port block that lets rein's listener run without a password is proven
  in isolation (§3a); the prober must assert it in the composed profile so a regression can
  never silently open rein's port to in-sandbox processes.
- **Full composed profile never run.** The end-to-end proof used a no-auth rein hop, open
  UDP, no declare host, and an untightened profile — the *proven* composition is not the
  *production* one. Make "**full production profile, all knobs on** (nono→rein tunnel + UDP
  policy + declare host + `GIT_CONFIG_GLOBAL` + `deny_credentials` + `af_unix_mediation`),
  real `git push` + real `gh` write" the explicit **P1 exit gate**, so the scattered deltas
  converge in one run.
- **nono is a new trust root.** nono TLS-terminates nothing rein cares about (rein does the
  github TLS), but it is now the sandbox and the tunnel. "Stronger sandbox than srt" is
  asserted, not measured — the prober is how we substantiate and monitor it.
