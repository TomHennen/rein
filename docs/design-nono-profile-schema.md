# nono 0.68.0 profile schema — verified (P0.0)

**Status:** DONE. Gates the profile generator (`internal/nono/profile.go`).
Supersedes the PROVISIONAL struct in `design-nono-pivot.md` §2.2 — read the
**§2.2 corrections** section below before coding `Build`.

**Scope:** Linux, nono **0.68.0** (`~/.local/bin/nono`), source commit `23d93fc`
(vendored at the spike scratchpad). Everything here is verified against the real
binary and/or the real source, not inferred from the spike.

## Method + evidence levels

Three verification levels are used throughout; the task grades on the distinction:

- **schema-verified** — present in `nono profile schema` output and/or accepted /
  rejected by `nono profile validate`.
- **source-verified** — read from nono's Rust source (`crates/nono-cli/src/profile/mod.rs`,
  `crates/nono-proxy/src/{config,external,server}.rs`), commit `23d93fc`.
- **launch-verified** — a composed profile using the field actually launched via
  `nono run --profile <file> -- echo ok` (exit 0). **Caveat:** `echo ok` proves the
  field *shape is accepted, the profile validates, and nono applies the sandbox*. It
  does **not** exercise the upstream_proxy dial, CA trust under TLS, or af_unix
  mediation under load — those remain P1 gates, not claims of this doc.

## Headline corrections (design §2.2 is wrong on all four structural points)

1. **The profile is NESTED, not flat.** §2.2's struct put `allow_domain`,
   `upstream_proxy`, `upstream_bypass`, `deny_credentials`, `env` at the top level.
   Real top-level keys are `network`, `filesystem`, `linux`, `environment`, `groups`,
   `meta`, … The rein-relevant fields live under `network.*`, `filesystem.*`,
   `linux.*`, `environment.*`, and `groups.include`.
2. **There is no `external_proxy{url,auth}` object, and no way to give rein's
   listener a per-session proxy-auth secret.** `network.upstream_proxy` (alias
   `network.external_proxy`) is a **bare `host:port` string**. Supplying an object
   with an `auth` field is **rejected at validate** (*"invalid type: map, expected a
   string"*). Even the internal `ExternalProxyAuth{keyring_account}` struct that
   exists in source is (a) never populated from the profile — the CLI path hardcodes
   `auth: None` — and (b) explicitly **"not yet implemented"**, erroring if set. So
   the answer to "inline secret vs OS keyring" is **neither: upstream-proxy auth does
   not work in 0.68.0.** See the dedicated section below — this removes a
   defense-in-depth layer the design leaned on (§3a).
3. **`deny_credentials` is a policy GROUP, not a path list.** It is included via
   `groups.include: ["deny_credentials"]` (a fixed, `required` policy group that
   blocks known credential locations). You cannot add arbitrary paths to it. §2.2's
   `DenyCredentials []string` and "MUST include the profile path itself" are wrong on
   mechanism (see the profile-secret gotcha below).
4. **Arbitrary env injection works — via `environment.set_vars`, not a top-level
   `env`** — but nono reserves/overrides the proxy vars. See the env section.

## Verified field-by-field table

| rein need | Real path (JSON) | Type / shape | Level | Notes / gotchas |
|---|---|---|---|---|
| allow github | `network.allow_domain` | array of **string OR** `{domain, endpoints[]}` | schema + launch | Plain string = CONNECT tunnel (no L7). Object form triggers TLS-intercept + default-deny endpoint filtering. rein uses plain strings (rein is the upstream that terminates). Aliases: `proxy_allow`, `allow_proxy`. |
| upstream proxy → rein | `network.upstream_proxy` | **string `host:port`** | schema + source | Alias: `network.external_proxy` (legacy). **Bare host:port — NOT a URL.** Source dials it with `TcpStream::connect(addr)` (`external.rs`), so `"http://127.0.0.1:PORT"` (as §2.2 wrote) is wrong for this field — use `"127.0.0.1:PORT"`. (Not dial-tested here; `echo ok` doesn't reach upstream.) |
| CDN → direct | `network.upstream_bypass` | array of string (exact host or `*.` wildcard) | schema + launch | Alias: `external_proxy_bypass`. Hosts here bypass the upstream proxy and go **direct** (never reach rein). Requires `upstream_proxy` to be set or validate errors. |
| per-session proxy secret | *(none)* | — | schema + source | **No profile field carries it. Unimplemented in 0.68.0.** See dedicated section. |
| hide credentials | `groups.include: ["deny_credentials"]` | policy-group name in `groups.include` array | schema + source | Fixed group, `required`, cross-platform. Blocks keys/tokens/cloud creds. Companion groups: `deny_shell_history`, `deny_shell_configs`, `deny_keychains_linux`, `deny_browser_data_linux`. `nono profile groups` lists all 30. |
| arbitrary env inject | `environment.set_vars` | `object{string: string}` | schema + launch | "Injected after env filtering, before credential injection." **`PATH` and `NONO_*` reserved; nono also OVERRIDES `HTTP(S)_PROXY`/`NO_PROXY`** (see env section). Values support `$HOME`,`$WORKDIR`,`$TMPDIR`,`$XDG_*`,`$NONO_CONFIG`,`$NONO_PACKAGES` expansion. Everything else passes through verbatim (proved with an arbitrary `REIN_TOTALLY_ARBITRARY_XYZ` var). |
| af_unix mediation | `linux.af_unix_mediation` | enum **`"off"` \| `"pathname"`** | schema + launch | Default `off`. `"pathname"` = default-deny seccomp supervisor for pathname AF_UNIX connect/bind; sockets must be granted back via `filesystem.unix_socket*`. This is the approval-channel-isolation control (§3e). |
| unix-socket allowlist | `filesystem.unix_socket` (+ `_bind`, `_dir`, `_dir_bind`, `_subtree`, `_subtree_bind`) | array of string (paths) | source + launch | **SCHEMA DRIFT — `nono profile schema` OMITS these fields**, but the profile accepts them (guide documents them; validate + launch confirmed with `["/run/user/1000/bus"]`). `unix_socket` = connect-only single path; `_bind` = connect+bind; `_dir`/`_subtree` = non-recursive / recursive dir grants; `_bind` variants add bind. Start empty; grant only what a real agent needs, **never** the tmux/approval socket. |
| block all network | `network.block` | boolean (default false) | schema + source | **Do NOT set true** — `block:true` is incompatible with proxy/domain-filter mode (design §3d). All-or-nothing; no granular UDP control exists (design §3d residual stands). |
| CA cert readable | `filesystem.read_file: [<ca.pem>]` | array of string (file paths) | schema + launch | Filesystem is **default-deny**; the CA PEM must be explicitly granted read (`read_file` for a single file, or `read` for its dir). Launch showed the CA file listed as a capability. Keep it in its own dir, separate from any secret (design §2.2 path-split still holds — though there is now no profile secret to protect, see below). |

## The proxy-auth question — RESOLVED: neither inline nor keyring; unimplemented

The task asked to settle definitively whether the per-session proxy secret lives in
the profile JSON or in an OS keyring. **Answer: neither — nono 0.68.0 cannot carry
upstream-proxy auth at all.** Evidence:

- **schema-verified:** `network.upstream_proxy` / `network.external_proxy` are
  string-typed. An object with `auth` is rejected by `nono profile validate`
  (*"invalid type: map, expected a string on line … column …"*).
- **source-verified (CLI path):** `proxy_runtime.rs:2416` builds
  `ExternalProxyConfig { address, auth: None, bypass_hosts }` — **`auth` is
  hardcoded `None`.** `UpstreamProxyIntent` (the profile-derived struct) has only
  `address` + `bypass`, no auth field.
- **source-verified (proxy struct):** `ExternalProxyAuth` exists
  (`config.rs:1295`) and uses a **`keyring_account: String`** (keystore-backed, not
  an inline secret) — so *had* it been wireable it would be keyring, not inline. But
  it is not wireable, and…
- **source-verified (all three handlers):** if `auth.is_some()`,
  `external.rs:217`, `server.rs:1519`, and `server.rs:1935` each return
  *"external proxy authentication is configured but not yet implemented; remove the
  auth section … or wait for a future release."*

**Consequence for design §3a / §7 — this is an OPEN GAP, not a resolved item.**
§3a's "the close" (rein's listener requires a per-session secret nono sends as
`Proxy-Authorization` on every CONNECT) **cannot be built on 0.68.0.** Do not delete
the framing; re-open it: the loopback-capability *regression* (any in-sandbox process
that can `connect(127.0.0.1:reinport)` reaches rein's listener) now has **no
nono-config mitigation** — structurally parallel to the UDP residual (§3d). The
secret-hiding sub-concern (profile JSON must be agent-unreadable) is moot only
*because the secret layer is gone*, not because it was solved.

This is **not catastrophic**: §3a itself states proxy-auth is defense-in-depth, not
the security boundary. The primary gates stand — declare + tier classifier +
**downstream** token injection (token value never enters the sandbox) + approval-
channel isolation via `af_unix_mediation`. But P1a must drop the `external_proxy.auth`
plan and either (a) accept the loopback residual + document it (Tom decision, like
UDP), (b) rely on loopback-`connect()` seccomp mediation if the §1 measurement shows
nono mediates it, or (c) bind rein's listener somewhere only nono can reach. The
`--no-auth` loopback relay the spike actually used is the current de-facto answer.

## The two proxy hops — do not conflate (both empirically observed)

Launch of the composed profile with `-- /usr/bin/env` shows nono manages the
**agent→nono** hop itself:

- **agent → nono** (hop 1): nono **injects and overrides** `HTTP_PROXY`,
  `HTTPS_PROXY` (and lowercase) to point at **nono's own** loopback proxy
  (`http://nono:<token>@127.0.0.1:<nono-port>`), sets `NO_PROXY=localhost,127.0.0.1`,
  and exports `NONO_PROXY_TOKEN=<64-hex>` + `NONO_CAP_FILE`. The `nono:<token>` basic
  cred is why git needs **`http.proxyAuthMethod=basic`** (spike finding, preserved).
  `NONO_PROXY_TOKEN` **is agent-visible and that is fine** — it authenticates only to
  nono's own proxy; the GitHub token is injected downstream by rein and never enters
  the sandbox.
- **nono → rein** (hop 2): the `network.upstream_proxy` field. **Bare host:port, NO
  auth** (previous section). This is the opaque CONNECT tunnel rein TLS-terminates.

**Design §3c correction:** §3c says rein's profile `env` must set
`HTTPS_PROXY`/`HTTP_PROXY` (→ nono's proxy) and `NO_PROXY` (CDN bypass). **nono owns
those and overrides any `set_vars` you write for them** — proved: a `set_vars`
`HTTPS_PROXY=http://127.0.0.1:47821` was replaced by nono's own value at launch. So:
rein must NOT set the proxy env vars (nono does), and CDN bypass is achieved by
`network.upstream_bypass` (nono→direct), **not** by the agent's `NO_PROXY` (which nono
pins to `localhost,127.0.0.1`). Non-git tools still get proxied because nono sets
their `HTTPS_PROXY` for them.

## Working, launch-verified profile

Committed at `docs/nono-profile-sample.json` (placeholder paths/ports; the generator
substitutes per run). It **validates** (`nono profile validate` → valid) and
**launches** (`nono run --profile … -- echo ok` → `ok`, exit 0). It composes every
rein knob that 0.68.0 actually supports: allow github + api + declare host, upstream
proxy to a loopback rein listener, CDN in `upstream_bypass`, `deny_credentials` group,
`af_unix_mediation: pathname`, CA-trust env (all four vars) + `http.proxyAuthMethod=basic`
and `http.postBuffer` via `GIT_CONFIG_*` env.

```json
{
  "$schema": "https://nono.sh/schema/profile.json",
  "meta": { "name": "rein-sandbox", "description": "rein credential-broker sandbox profile (P0.0 schema-verified)." },
  "groups": { "include": ["deny_credentials", "deny_shell_history", "deny_shell_configs"] },
  "network": {
    "block": false,
    "allow_domain": ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "declare.rein.internal"],
    "upstream_proxy": "127.0.0.1:47821",
    "upstream_bypass": ["codeload.github.com", "objects.githubusercontent.com"]
  },
  "linux": { "af_unix_mediation": "pathname" },
  "filesystem": { "read_file": ["/home/user/.config/rein/ca/rein-ca.pem"], "unix_socket": [] },
  "environment": {
    "set_vars": {
      "SSL_CERT_FILE": "/home/user/.config/rein/ca/rein-ca.pem",
      "GIT_SSL_CAINFO": "/home/user/.config/rein/ca/rein-ca.pem",
      "NODE_EXTRA_CA_CERTS": "/home/user/.config/rein/ca/rein-ca.pem",
      "CURL_CA_BUNDLE": "/home/user/.config/rein/ca/rein-ca.pem",
      "GIT_CONFIG_COUNT": "2",
      "GIT_CONFIG_KEY_0": "http.proxyAuthMethod", "GIT_CONFIG_VALUE_0": "basic",
      "GIT_CONFIG_KEY_1": "http.postBuffer", "GIT_CONFIG_VALUE_1": "524288000"
    }
  }
}
```

Note: the sample intentionally has **no proxy-auth object** — it can't (validate
rejects it). Note also there is **no `HTTPS_PROXY` in `set_vars`** — nono sets it.

## Corrected §2.2 Go struct

The generator should emit nested structs mirroring nono's real schema. `ExternalProxy`
and `ProxyAuth` are **deleted** (no such object; auth unwireable). `Env`, `DenyCredentials`
move; `af_unix_mediation` + unix-socket allowlist + CA read grant are **added**.

```go
type Profile struct {
    Schema      string            `json:"$schema,omitempty"`
    Meta        *Meta             `json:"meta,omitempty"`
    Groups      Groups            `json:"groups"`      // Include: ["deny_credentials", ...]
    Network     Network           `json:"network"`
    Linux       Linux             `json:"linux"`
    Filesystem  Filesystem        `json:"filesystem"`
    Environment Environment       `json:"environment"`
}

type Meta struct {
    Name        string `json:"name,omitempty"`
    Description string `json:"description,omitempty"`
}

type Groups struct {
    Include []string `json:"include"` // MUST include "deny_credentials" (a policy group, NOT a path list)
    Exclude []string `json:"exclude,omitempty"`
}

type Network struct {
    Block          bool     `json:"block"`                     // false — true is incompatible with proxy mode
    AllowDomain    []string `json:"allow_domain"`              // InjectHosts ∪ CDNHosts ∪ ExtraDomains ∪ DeclareHost
    UpstreamProxy  string   `json:"upstream_proxy,omitempty"`  // "127.0.0.1:PORT" — BARE host:port, NO scheme, NO auth
    UpstreamBypass []string `json:"upstream_bypass,omitempty"` // = CDNHosts (nono→direct); needs UpstreamProxy set
    // NO auth field exists. Do not attempt external_proxy.auth (validate rejects it; unimplemented).
}

type Linux struct {
    AfUnixMediation string `json:"af_unix_mediation,omitempty"` // "pathname" (approval-channel isolation)
}

type Filesystem struct {
    ReadFile   []string `json:"read_file,omitempty"`   // CA PEM path (default-deny fs ⇒ must grant read)
    UnixSocket []string `json:"unix_socket,omitempty"` // allowlist under af_unix_mediation; start EMPTY, never tmux
    // also available (source; schema-omitted): unix_socket_bind, unix_socket_dir[_bind], unix_socket_subtree[_bind]
}

type Environment struct {
    SetVars map[string]string `json:"set_vars,omitempty"` // arbitrary env; PATH/NONO_*/HTTP(S)_PROXY/NO_PROXY are nono-managed — do NOT set those
}
```

`Build` invariants from §2.2 mostly carry, with corrections:
- `AllowDomain = InjectHosts ∪ CDNHosts ∪ ExtraDomains ∪ DeclareHost` — unchanged.
- `UpstreamBypass = CDNHosts` verbatim; `InjectHosts` + `DeclareHost` NOT in bypass — unchanged.
- `ExtraDomains` never injected — unchanged (enforced in proxy, not profile).
- `SetVars` carries the **four CA vars + `GIT_CONFIG_*`**; it must **NOT** carry
  `HTTP(S)_PROXY`/`NO_PROXY` (nono overrides them) — corrected from §2.2/§3c.
- Drop the `DenyReadPaths`/"profile path in deny_credentials" logic: `deny_credentials`
  is a fixed group, `filesystem.deny` is a **Linux no-op** (guide: Landlock has no
  deny-within-allow), and there is no profile secret to hide anyway (auth unimplemented).
  Filesystem is default-deny, so the correct rule is **"grant nothing you don't need
  to,"** not "deny the secret."

## Gotchas for the generator implementer (read these)

1. **Schema drift.** `nono profile schema` **omits** the `filesystem.unix_socket*`
   fields (and possibly others) that the profile actually accepts. Do NOT treat the
   schema dump as complete — cross-check `nono profile guide` + source. The
   `unix_socket*` grant fields are the load-bearing example (they gate the §3e
   approval-channel isolation and are invisible in the schema output).
2. **`upstream_proxy` is bare `host:port`.** No `http://` scheme (source:
   `TcpStream::connect`). A URL will fail to dial.
3. **nono overrides proxy env.** `HTTP(S)_PROXY`, `NO_PROXY`, `PATH`, `NONO_*` in
   `set_vars` are ignored/overridden. Only set app-level vars (CA, `GIT_CONFIG_*`,
   agent config).
4. **`deny_credentials` is a group name**, not a path list. Compose credential
   hiding via `groups.include`.
5. **`filesystem` is default-deny**; the CA PEM needs an explicit `read_file`/`read`
   grant or the agent's tools can't read it. `filesystem.deny` does nothing on Linux.
6. **No proxy-auth.** Anything in the design that requires nono to authenticate to
   rein's listener (`external_proxy.auth`, per-session secret) is unbuildable on
   0.68.0 — see the proxy-auth section; treat as an open §3a/§7 gap.
7. **Aliases exist** (`proxy_allow`/`allow_proxy` for `allow_domain`; `external_proxy`
   for `upstream_proxy`; `external_proxy_bypass` for `upstream_bypass`) — emit the
   canonical names.

## Commands used (reproducible)

```
nono profile schema                                  # full JSON Schema (note: incomplete)
nono profile guide                                   # authoring guide (has the unix_socket + env docs)
nono profile groups                                  # 30 policy groups incl. deny_credentials
nono profile validate docs/nono-profile-sample.json  # -> valid
nono run --profile docs/nono-profile-sample.json -- echo ok      # -> ok (exit 0)
nono run --profile docs/nono-profile-sample.json -- /usr/bin/env # -> shows nono-managed HTTPS_PROXY + NONO_PROXY_TOKEN
```
