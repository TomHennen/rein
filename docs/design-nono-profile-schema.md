# nono 0.68.0 profile schema — verified

**Status:** Authoritative field reference for the profile generator
(`internal/nono/profile.go`). **Scope:** Linux, nono **0.68.0**
(`~/.local/bin/nono`), source commit `23d93fc`. Everything here is checked against the
real binary and/or the real source, not guessed from the spike.

## How each fact was checked

Three confidence levels are used throughout, and the distinction matters:

- **schema-verified** — appears in `nono profile schema` output and/or is accepted or
  rejected by `nono profile validate`.
- **source-verified** — read from nono's Rust source
  (`crates/nono-cli/src/profile/mod.rs`, `crates/nono-proxy/src/{config,external,server}.rs`),
  commit `23d93fc`.
- **launch-verified** — a profile using the field actually ran via
  `nono run --profile <file> -- echo ok` (exit 0). **Caveat:** `echo ok` proves the
  field *shape is accepted, the profile validates, and nono applies the sandbox*. It
  does **not** exercise the dial to rein's upstream proxy, CA trust under real TLS, or
  AF_UNIX mediation under load — those are runtime concerns proven elsewhere, not claims
  of this doc.

## The shape

- **The profile is nested**, not flat. Top-level keys are `network`, `filesystem`,
  `linux`, `environment`, `groups`, `meta`, … The rein-relevant fields live under
  `network.*`, `filesystem.*`, `linux.*`, `environment.*`, and `groups.include`.
- **`network.upstream_proxy` is a bare `host:port` string** (alias
  `network.external_proxy`) — not a URL, no scheme. There is **no way to give rein's
  listener a per-session password:** supplying an object with an `auth` field is rejected
  at validate, and the internal auth path is unimplemented (see below).
- **`deny_credentials` is a policy GROUP, not a path list.** Turn it on with
  `groups.include: ["deny_credentials"]` (a fixed, `required` group that blocks known
  credential locations). You can't add arbitrary paths to it.
- **Arbitrary env injection works via `environment.set_vars`** (not a top-level `env`) —
  but nono reserves and overrides the proxy vars (see the env section).

## Field-by-field table (verified)

| rein need | Real JSON path | Type / shape | Level | Notes / gotchas |
|---|---|---|---|---|
| allow github | `network.allow_domain` | array of **string OR** `{domain, endpoints[]}` | schema + launch | Plain string = CONNECT tunnel (no layer-7 filtering). Object form turns on TLS-intercept + endpoint filtering. rein uses plain strings (rein is the upstream that terminates). Aliases: `proxy_allow`, `allow_proxy`. |
| upstream proxy → rein | `network.upstream_proxy` | **string `host:port`** | schema + source | Alias: `network.external_proxy` (legacy). **Bare host:port — NOT a URL.** Source dials it with `TcpStream::connect(addr)`, so `"http://127.0.0.1:PORT"` won't dial — use `"127.0.0.1:PORT"`. (Not dial-tested here; `echo ok` doesn't reach upstream.) |
| CDN → direct | `network.upstream_bypass` | array of string (exact host or `*.` wildcard) | schema + launch | Alias: `external_proxy_bypass`. Hosts here skip the upstream proxy and go **direct** (never reach rein). Requires `upstream_proxy` set, or validate errors. |
| per-session proxy secret | *(none)* | — | schema + source | **No field carries it. Unimplemented in 0.68.0.** See dedicated section. |
| hide credentials | `groups.include: ["deny_credentials"]` | policy-group name in the `groups.include` array | schema + source | Fixed group, `required`, cross-platform. Blocks keys/tokens/cloud creds. Companion groups: `deny_shell_history`, `deny_shell_configs`, `deny_keychains_linux`, `deny_browser_data_linux`. `nono profile groups` lists all 30. |
| arbitrary env inject | `environment.set_vars` | `object{string: string}` | schema + launch | "Injected after env filtering, before credential injection." **`PATH` and `NONO_*` are reserved; nono also OVERRIDES `HTTP(S)_PROXY`/`NO_PROXY`** (see env section). Values expand `$HOME`, `$WORKDIR`, `$TMPDIR`, `$XDG_*`, `$NONO_CONFIG`, `$NONO_PACKAGES`. Everything else passes through verbatim (proved with an arbitrary `REIN_TOTALLY_ARBITRARY_XYZ` var). |
| AF_UNIX mediation | `linux.af_unix_mediation` | enum **`"off"` \| `"pathname"`** | schema + launch | Default `off`. `"pathname"` = deny-by-default seccomp supervisor for pathname AF_UNIX connect/bind; sockets must be granted back via `filesystem.unix_socket*`. This is the approval-channel-isolation control (design §3e). |
| unix-socket allowlist | `filesystem.unix_socket` (+ `_bind`, `_dir`, `_dir_bind`, `_subtree`, `_subtree_bind`) | array of string (paths) | source + launch | **SCHEMA DRIFT — `nono profile schema` OMITS these fields**, but the profile accepts them (the guide documents them; validate + launch confirmed with `["/run/user/1000/bus"]`). `unix_socket` = connect-only, single path; `_bind` = connect+bind; `_dir`/`_subtree` = non-recursive / recursive dir grants; `_bind` variants add bind. Start empty; grant only what a real agent needs, **never** the tmux/approval socket. |
| block all network | `network.block` | boolean (default false) | schema + source | **Do NOT set true** — `block: true` is incompatible with proxy/domain-filter mode (design §3d). All-or-nothing; there is no fine-grained UDP control (the §3d residual stands). |
| CA cert readable | `filesystem.read_file: [<ca.pem>]` | array of string (file paths) | schema + launch | The filesystem is **default-deny**; the CA PEM needs an explicit read grant (`read_file` for one file, or `read` for its dir). Launch showed the CA file listed as a capability. Keep it in its own dir, separate from anything secret. |

## The proxy-auth question — settled: neither inline nor keyring; unimplemented

nono 0.68.0 **can't carry upstream-proxy auth at all**, so there is no per-session proxy
secret to store anywhere. Evidence:

- **schema-verified:** `network.upstream_proxy` / `network.external_proxy` are
  string-typed. An object with `auth` is rejected by `nono profile validate`
  (*"invalid type: map, expected a string …"*).
- **source-verified (CLI path):** `proxy_runtime.rs:2416` builds
  `ExternalProxyConfig { address, auth: None, bypass_hosts }` — **`auth` is hardcoded
  `None`.** The profile-derived struct (`UpstreamProxyIntent`) has only `address` +
  `bypass`.
- **source-verified (proxy struct):** `ExternalProxyAuth` exists (`config.rs:1295`) and
  uses a **`keyring_account`** (keystore-backed, not an inline secret) — so *if* it were
  wireable it would be keyring, not inline. But it isn't wireable.
- **source-verified (all three handlers):** if `auth.is_some()`, `external.rs:217`,
  `server.rs:1519`, and `server.rs:1935` each return *"external proxy authentication is
  configured but not yet implemented …"*.

**What this means for the design (§3a).** rein's listener needs no per-session secret,
because the loopback-capability regression it would have closed is closed a different
way — and it's tested: a nono-sandboxed agent's raw `connect()` to an arbitrary loopback
port is refused; nono lets the agent reach only nono's own proxy on loopback, so the
agent can't reach rein's listener directly (design §3a). §3a always treated proxy-auth as
defense-in-depth, not the security boundary; the primary gates stand — declare + tier
classifier + **downstream** token injection (the token value never enters the sandbox) +
approval-channel isolation via `af_unix_mediation`. There is no secret to hide, so the
"profile JSON must be agent-unreadable" sub-concern is simply gone. The spike used a
no-auth loopback relay, which is the intended production shape.

## The two proxy hops — do not conflate (both observed at launch)

Launching the composed profile with `-- /usr/bin/env` shows nono manages the
**agent→nono** hop itself:

- **agent → nono** (hop 1): nono **injects and overrides** `HTTP_PROXY`, `HTTPS_PROXY`
  (and lowercase) to point at **nono's own** loopback proxy
  (`http://nono:<token>@127.0.0.1:<nono-port>`), sets `NO_PROXY=localhost,127.0.0.1`, and
  exports `NONO_PROXY_TOKEN=<64-hex>` + `NONO_CAP_FILE`. That `nono:<token>` basic
  credential is why git needs **`http.proxyAuthMethod=basic`** (spike finding).
  `NONO_PROXY_TOKEN` **is agent-visible and that is fine** — it authenticates only to
  nono's own proxy; the GitHub token is injected downstream by rein and never enters the
  sandbox.
- **nono → rein** (hop 2): the `network.upstream_proxy` field. **Bare host:port, no
  auth** (previous section). This is the opaque CONNECT tunnel rein TLS-terminates.

**nono owns the proxy env vars.** rein must NOT set `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`
in `set_vars` — nono overrides any value you write (proved: a `set_vars`
`HTTPS_PROXY=http://127.0.0.1:47821` was replaced by nono's own at launch, and `NO_PROXY`
is pinned to `localhost,127.0.0.1`). CDN bypass is done with `network.upstream_bypass`
(nono → direct), not the agent's `NO_PROXY`. Non-git tools still get proxied because nono
sets their `HTTPS_PROXY` for them (design §3c).

## Working profile (validates + launches)

Committed at `docs/nono-profile-sample.json` (placeholder paths/ports; the generator
substitutes per run). It **validates** (`nono profile validate` → valid) and **launches**
(`nono run --profile … -- echo ok` → `ok`, exit 0). It composes every rein knob 0.68.0
supports: github + api + declare host allowed, upstream proxy to a loopback rein
listener, CDN in `upstream_bypass`, the `deny_credentials` group, `af_unix_mediation:
pathname`, CA-trust env (all four vars), and `http.proxyAuthMethod=basic` +
`http.postBuffer` via `GIT_CONFIG_*` env.

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

Note: the sample deliberately has **no proxy-auth object** (validate rejects it) and **no
`HTTPS_PROXY` in `set_vars`** (nono sets it).

## The Go struct

The generator emits nested structs mirroring nono's schema. There is no `ExternalProxy`
or `ProxyAuth` object (no such thing; auth can't be wired). `af_unix_mediation`, the
unix-socket allowlist, and the CA read grant are present.

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

`Build`'s invariants:
- `AllowDomain = InjectHosts ∪ CDNHosts ∪ ExtraDomains ∪ DeclareHost`.
- `UpstreamBypass = CDNHosts` verbatim; `InjectHosts` + `DeclareHost` NOT in bypass.
- `ExtraDomains` never injected (enforced in the proxy, not the profile).
- `SetVars` carries the **four CA vars + `GIT_CONFIG_*`**; it must **NOT** carry
  `HTTP(S)_PROXY`/`NO_PROXY` (nono overrides them).
- No `DenyReadPaths` / "profile path in deny_credentials" logic: `deny_credentials` is a
  fixed group, `filesystem.deny` is a **Linux no-op** (Landlock has no deny-within-allow),
  and there's no profile secret to hide (auth unimplemented). The filesystem is
  default-deny, so the rule is **"grant nothing you don't need,"** not "deny the secret."

## Gotchas for the generator (read these)

1. **Schema drift.** `nono profile schema` **omits** the `filesystem.unix_socket*` fields
   (and possibly others) the profile actually accepts. Do NOT treat the schema dump as
   complete — cross-check `nono profile guide` + source. The `unix_socket*` grant fields
   are the load-bearing example (they gate the §3e approval-channel isolation and don't
   appear in the schema output).
2. **`upstream_proxy` is bare `host:port`.** No `http://` scheme (source:
   `TcpStream::connect`). A URL won't dial.
3. **nono overrides proxy env.** `HTTP(S)_PROXY`, `NO_PROXY`, `PATH`, and `NONO_*` in
   `set_vars` are ignored/overridden. Only set app-level vars (CA, `GIT_CONFIG_*`, agent
   config).
4. **`deny_credentials` is a group name**, not a path list. Compose credential-hiding via
   `groups.include`.
5. **`filesystem` is default-deny**; the CA PEM needs an explicit `read_file`/`read` grant
   or the agent's tools can't read it. `filesystem.deny` does nothing on Linux.
6. **No proxy-auth.** Anything requiring nono to authenticate to rein's listener
   (`external_proxy.auth`, a per-session secret) can't be built on 0.68.0 — nono's
   loopback mediation protects rein's listener instead (§3a).
7. **Aliases exist** (`proxy_allow`/`allow_proxy` for `allow_domain`; `external_proxy` for
   `upstream_proxy`; `external_proxy_bypass` for `upstream_bypass`) — emit the canonical
   names.

## Commands used (reproducible)

```
nono profile schema                                  # full JSON Schema (note: incomplete)
nono profile guide                                   # authoring guide (has the unix_socket + env docs)
nono profile groups                                  # 30 policy groups incl. deny_credentials
nono profile validate docs/nono-profile-sample.json  # -> valid
nono run --profile docs/nono-profile-sample.json -- echo ok       # -> ok (exit 0)
nono run --profile docs/nono-profile-sample.json -- /usr/bin/env  # -> shows nono-managed HTTPS_PROXY + NONO_PROXY_TOKEN
```
