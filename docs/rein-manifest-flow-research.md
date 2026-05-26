# Implementing the GitHub App Manifest Flow in `rein` — Research Report

**Audience:** input to `docs/init-manifest-design.md` (~2-page design doc) for code review and implementation.
**Scope:** Go CLI, single binary, Linux + macOS, solo developers running on their own laptop. Two GitHub Apps created in one `rein init` invocation — a primary App (mints scoped installation tokens with `contents:write`, `issues:write`, `pull_requests:write`, `metadata:read`) and an audit App (separate identity, `issues:write` only, used by the broker to post audit comments the agent can't prune).
**Priority:** empirical findings about what GitHub actually does > restating docs; real code excerpts > prose.

---

## TL;DR

- The GitHub App Manifest flow is fully usable from a single-binary Go CLI. Fire a templated form POST to `https://github.com/settings/apps/new` (or `…/organizations/{org}/settings/apps/new`), receive `?code=&state=` on a loopback HTTP listener bound to `127.0.0.1:0`, and exchange the code at `POST /app-manifests/{code}/conversions`. GitHub returns the App ID, **PKCS#1** PEM private key, webhook secret, and client credentials in one shot. The temporary code expires after one hour and is single-use.
- **Two-App-in-one-flow is feasible but must be staged**: sequential — render setup-page-1, wait for callback-1, persist atomically, then render setup-page-2 with a different manifest name/permissions and a different state token. **No public Go library wraps the full manifest flow.** Implement directly against `google/go-github/v88`'s `AppsService.CompleteAppManifest` (BSD-3-Clause) or roll a 20-line `net/http` POST.
- **Borrow Atlantis's `ExchangeCode` pattern, not their UX** — Atlantis (Apache-2.0) is the canonical Go reference for the conversion step but its setup template assumes a long-running server. For a local CLI the right model is closer to `atproto`'s loopback OAuth CLI: ephemeral 127.0.0.1 listener, browser launch, channel-based handoff.
- **carabiner-dev has no directly reusable manifest-flow code** despite the suggestive name `burnafter`. Its value to `rein` is scaffolding patterns (`command`, `ghrfs`) and prior art on Apache-2.0 single-binary CLIs that touch GitHub.
- **Decided v1 storage policy:** plain PEM at `~/.config/rein/{primary,audit}.pem` (0600 in 0700 dir) behind a pluggable `Keystore` interface. **Phase 1/2 followup:** add `--require-biometric` flag using LAContext + age-encrypted file on macOS — gives per-mint Touch ID without requiring code signing or app-bundle distribution.
- **Decided v1 multi-machine policy:** one App pair per user; each new machine generates its own PEM via the GitHub UI ("Generate a private key" button) and imports via `rein init --import <slug>`. Per-machine revocation, shared installation list, audit attribution via key fingerprint.

---

## 1. GitHub App Manifest Flow — Full Spec and Undocumented Quirks

### 1.1 The three-step protocol

GitHub's docs describe the flow as a three-step handshake that must complete inside one hour (the `code` TTL):

> "You must complete this step of the GitHub App Manifest flow within one hour."
> — https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest

The wire-level behavior is:

1. **Browser POST to GitHub.** The client renders an HTML form whose `action` is `https://github.com/settings/apps/new?state=<NONCE>` (user account) or `https://github.com/organizations/<ORG>/settings/apps/new?state=<NONCE>` (org account). The form has one input `manifest` containing a JSON-stringified manifest. `method="post"`. The user reviews the App name on GitHub's page, then clicks "Create GitHub App for <account>".
2. **GitHub 302s to the manifest's `redirect_url`.** The URL receives two query params: `code` (40-char hex temporary code) and, if supplied, `state` (echoed verbatim).
3. **CLI exchanges the code.** A `POST https://api.github.com/app-manifests/{code}/conversions` with header `Accept: application/vnd.github+json` returns the complete App config plus a freshly minted PEM private key and webhook secret. **No `Authorization` header is required for this call** — and sending one against api.github.com (rather than the GHE `api/v3/` path) yields a 406/401 indirectly via a redirect to `/login`. Confirmed empirically:

> "After removing the 'Authorization' header entirely it started working. So there is an error in the documentation as it says that an 'Authorization' header should be present when actually it shouldn't be present."
> — https://github.com/orgs/community/discussions/52733

### 1.2 The `state` parameter — TTL and semantics

GitHub treats `state` as opaque — it is not bound to a session, not stored server-side, and not validated for entropy. Its TTL is effectively the same as the user's interaction window (capped by the same one-hour ceiling that bounds the `code`). There have been long-standing bug reports about `state` being dropped through certain redirect paths; the `select_target` rewrite issue confirms GitHub recently fixed a 302 that ate the parameter:

> "Looks like Github have implemented a 302 redirect from /new to /select_target but failed to pass through the search params. […] We have a fix in testing right now, about to be deployed."
> — https://github.com/orgs/community/discussions/61291

**Implication for `rein`:** generate `state` with `crypto/rand` (≥128 bits), store it in process memory for the lifetime of the callback listener, compare with `subtle.ConstantTimeCompare`, and reject anything else with HTTP 400. Don't try to round-trip business data in `state`; persist it in a server-memory map keyed by listener.

### 1.3 The `code` parameter — TTL and reuse

The official one-hour TTL is documented. Empirically, the conversion endpoint is single-use: posting the same `code` twice returns 404 (the resource is consumed once a key has been minted). The endpoint is also explicitly rate-limited per the GitHub Docs page above ("This endpoint is rate limited") although no public quota number is published. For a solo-developer CLI flow this rate limit is not a realistic concern.

### 1.4 What happens on Cancel / browser close

GitHub does **not fire a callback** if the user clicks "Cancel" or closes the tab on the App creation page. The CLI sees nothing — no GET, no error. Your listener must therefore implement a timeout (suggest 10 minutes) and surface a clear "we never heard back; please retry `rein init`" message. There is no server-side cleanup on the GitHub side because nothing was created.

### 1.5 The conversion-response schema

Atlantis (Apache-2.0) defines an internal subset of the response that captures the fields a CLI actually cares about (`server/events/vcs/github_client.go`):

```go
type GithubAppTemporarySecrets struct {
    ID            int64  // the app id
    Key           string // PEM-encoded private key
    Name          string // app name
    WebhookSecret string
    URL           string // https://github.com/apps/<slug>
}
```

The actual JSON GitHub returns is richer. Per Probot's TypeScript wrapper and the go-github `AppConfig` struct, the full conversion-response shape is:

```jsonc
{
  "id": 12345,
  "slug": "rein-primary-abc123",
  "node_id": "MDEyOk9yZ2FuaXphdGlvbjQzNTM=",
  "owner": { /* full simple-user/org payload */ },
  "name": "rein primary abc123",
  "description": "...",
  "external_url": "https://example.com",
  "html_url": "https://github.com/apps/rein-primary-abc123",
  "created_at": "...",
  "updated_at": "...",
  "permissions": { "contents": "write", "issues": "write", ... },
  "events": [ "..." ],
  "client_id": "Iv23li...",
  "client_secret": "secret",
  "webhook_secret": "shhh",
  "pem": "-----BEGIN RSA PRIVATE KEY-----\n..."
}
```

Probot extracts exactly these fields after the exchange call (`src/manifest-creation.ts`, MIT):

```ts
const { id, client_id, client_secret, webhook_secret, pem } = response.data;
```

### 1.6 PEM format — PKCS#1, not PKCS#8

This is the undocumented quirk. GitHub mints the App private key as PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----` header), not PKCS#8 (`-----BEGIN PRIVATE KEY-----`). Every consumer that goes through `golang-jwt`'s `ParseRSAPrivateKeyFromPEM` accepts both, falling back from PKCS#1 to PKCS#8:

```go
if parsedKey, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
    if parsedKey, err = x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
        return nil, err
    }
}
```

`jferrl/go-githubauth` (MIT) delegates to the same underlying parser, so **`rein` does not need to convert formats**. Persist the bytes exactly as received.

### 1.7 User vs. organization-owned Apps

`/settings/apps/new` (user) and `/organizations/{ORG}/settings/apps/new` (org) behave identically at the API level — same POST shape, same redirect handshake, same conversion endpoint. The differences are entirely UI:

- The org path renders an org-specific "Create GitHub App for <ORG>" button.
- If the org has "Restrict app creation to organization owners" enabled and the user is not an owner, GitHub will show the form but reject the final create with a 403 inline. The CLI cannot detect this without a heartbeat.
- "GitHub App Manifests are not available for enterprise-owned GitHub Apps."

For `rein`'s solo-developer target, default to the user-account path. Offer `--org <name>` as an opt-in.

---

## 2. Production Implementations to Reference

### 2.1 Probot (Node.js, MIT) — the canonical reference

`src/manifest-creation.ts` is short and crisp:

```ts
public getManifest(options: { pkg: PackageJson; baseUrl: string; readFileSync?: ... }): string {
  const generatedManifest = JSON.stringify({
    description: manifest.description || pkg.description,
    hook_attributes: { url: process.env.WEBHOOK_PROXY_URL || `${baseUrl}/` },
    name: process.env.PROJECT_DOMAIN || manifest.name || pkg.name,
    public: manifest.public || true,
    redirect_url: `${baseUrl}/probot/setup`,
    url: manifest.url || pkg.homepage || pkg.repository,
    version: "v1",
    ...manifest,
  });
  return generatedManifest;
}

public async createAppFromCode(code: string, probotOptions?: OctokitOptions) {
  const octokit = new ProbotOctokit(probotOptions);
  const response = await octokit.request("POST /app-manifests/:code/conversions", { ..., code });
  const { id, client_id, client_secret, webhook_secret, pem } = response.data;
  this.#updateEnv({ APP_ID: id.toString(), PRIVATE_KEY: `"${pem}"`, ... });
  return response.data.html_url;
}
```

(https://github.com/probot/probot/blob/master/src/manifest-creation.ts)

**Pattern worth borrowing:** the `public || true` and `version: "v1"` defaulting; the destructured five-field extraction.
**Anti-pattern:** writing `PRIVATE_KEY` as a quoted string into `.env` is fragile and inappropriate for a CLI; write the PEM verbatim to a 0600 file (or keychain item) instead.

### 2.2 Atlantis (Go, Apache-2.0) — the only Go reference that fully implements the flow

Atlantis exposes two HTTP handlers in `server/controllers/github_app_controller.go`:

- `GET /github-app/setup` → renders an HTML page whose JS auto-submits the manifest to GitHub.
- `GET /github-app/exchange-code` → receives the redirect, calls `GithubClient.ExchangeCode`, renders the resulting secrets.

The exchange itself is ~15 lines (`server/events/vcs/github_client.go`):

```go
func (g *GithubClient) ExchangeCode(logger logging.SimpleLogging, code string) (*GithubAppTemporarySecrets, error) {
    logger.Debug("Exchanging code for app secrets")
    ctx := context.Background()
    cfg, resp, err := g.client.Apps.CompleteAppManifest(ctx, code)
    if resp != nil {
        logger.Debug("POST /app-manifests/%s/conversions returned: %v", code, resp.StatusCode)
    }
    data := &GithubAppTemporarySecrets{
        ID:            cfg.GetID(),
        Key:           cfg.GetPEM(),
        WebhookSecret: cfg.GetWebhookSecret(),
        Name:          cfg.GetName(),
        URL:           cfg.GetHTMLURL(),
    }
    return data, err
}
```

The router wiring:

```go
s.Router.HandleFunc("/github-app/exchange-code", s.GithubAppController.ExchangeCode).Methods("GET")
s.Router.HandleFunc("/github-app/setup",         s.GithubAppController.New).Methods("GET")
```

License header (verbatim, applies file-by-file):

```go
// Copyright 2017 HootSuite Media Inc.
// Licensed under the Apache License, Version 2.0 (the License);
// Modified hereafter by contributors to runatlantis/atlantis.
```

Reusable under Apache-2.0 if you keep the copyright + NOTICE. The `ExchangeCode` body is small enough that re-implementation is cleaner than vendoring.

**Anti-pattern worth flagging:** Atlantis renders the secrets in an HTML response and asks the operator to copy them into config flags. For `rein` that's wrong — the CLI should write directly to storage and never display the PEM.

### 2.3 carabiner-dev — no direct prior art

Survey of every public repo at https://github.com/carabiner-dev (Apache-2.0 across the board):

| Repo | Description | Reusable? |
|---|---|---|
| `burnafter` | "A more secure way to store credentials for CLI programs" | Description matches `rein`'s threat model, but the repo is early-stage; no manifest/installation-token code at the time of review. Worth tracking. |
| `bnd` | Sigstore bundle signing; consumes Actions OIDC tokens | No GitHub App manifest code. |
| `drop` | Secure-first installer for GitHub releases | Read-side; no broker code. |
| `ghrfs` | `fs.FS` over GitHub releases | Read-only. |
| `command` | Cobra helpers | Useful CLI scaffolding only. |
| `ampel`, `signer`, `policy`, `attestation`, `unpack`, `snappy`, `predicates`, `hasher`, `vexflow`, `revex`, `collector`, `policyctl`, `jsonl`, `termtable` | Supply-chain/attestation tooling | Not relevant to manifest flow. |

**Net finding:** carabiner-dev has no manifest-flow or installation-token-broker code that can be directly reused. The org's value to `rein` is scaffolding (`command`, `ghrfs`) and prior art on Apache-2.0 single-binary CLIs that touch GitHub, not the broker logic itself.

### 2.4 Other Go references

- **`rajbos/create-github-app-from-manifest`** (https://github.com/rajbos/create-github-app-from-manifest) — minimal HTML + Node example. Useful as a sanity-check reference for the manifest schema, not directly reusable.
- **`googlesamples/oauth-apps-for-windows`** — C# OAuth-loopback samples, but the loopback patterns translate. Documents binding to `127.0.0.1:0`, PKCE, and dynamic-port selection.

### 2.5 Library survey — no Go library wraps the manifest flow

`jferrl/go-githubauth` (MIT) and `bradleyfalzon/ghinstallation` (Apache-2.0) only handle the *post-creation* side. `ghinstallation` provides an `http.RoundTripper` via `ghinstallation.NewKeyFromFile(tr, appID, installationID, "key.pem")`; `go-githubauth` provides `oauth2.TokenSource` implementations. `google/go-github` (module path `github.com/google/go-github/v88`, BSD-3-Clause) provides `AppsService.CompleteAppManifest(ctx, code) (*AppConfig, *Response, error)` in `github/apps_manifest.go` — that's the entire conversion-step library surface. **No public Go library wraps the full browser-handshake side.** `rein` must implement it itself; this is a useful negative finding.

---

## 3. Local Callback Server Security

### 3.1 Port selection — the `:0` idiom

```go
ln, err := net.Listen("tcp", "127.0.0.1:0")
if err != nil { return err }
addr := ln.Addr().(*net.TCPAddr)
port := addr.Port // OS-assigned ephemeral port
```

This is the same pattern that AT Protocol's Go OAuth CLI tutorial recommends (https://atproto.com/guides/go-oauth-cli-tutorial).

### 3.2 Bind to `127.0.0.1`, not `0.0.0.0` or `localhost`

RFC 8252 §7.3 is explicit:

> "Clients should listen on the loopback network interface only, in order to avoid interference by other network actors."
> — https://www.rfc-editor.org/rfc/rfc8252.html

And:

> "Specifying a redirect URI with the loopback IP literal rather than localhost avoids inadvertently listening on network interfaces other than the loopback interface. It is also less susceptible to client-side firewalls and misconfigured host name resolution on the user's device."

`localhost` can resolve to an IPv6 address that differs from where you bound, or be redirected by a local hosts file. Use `127.0.0.1` literally.

### 3.3 Server timeouts (Go-specific)

```go
srv := &http.Server{
    Handler:           mux,
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       10 * time.Second,
    WriteTimeout:      10 * time.Second,
    IdleTimeout:       0,             // closed after one request
}
```

Also enforce a parent context deadline (~10 minutes) so the listener auto-shuts if the user never returns from the browser.

### 3.4 `state` validation

Pre-generate with `crypto/rand.Read(buf[:32])`, base64url-encode, compare with `subtle.ConstantTimeCompare`. Reject any other value with 400 and do not call the conversion endpoint.

### 3.5 Local-host attacks — the real threat model

RFC 8252 §8.10:

> "Loopback IP-based redirect URIs may be susceptible to interception by other apps accessing the same loopback interface on some operating systems."

On Linux/macOS with the default kernel `SO_REUSEADDR` semantics, a co-resident process **cannot** bind to the same `(127.0.0.1, port)` tuple while your listener is open — the OS rejects it. The realistic attacks are:

1. **Pre-bind race:** a malicious local process bound to a port before `rein` launches and happens to be there when `rein` requests `:0`. The kernel won't hand `rein` an in-use port, so this is detection (you'll fail to bind), not exploitation.
2. **Port-scan + connect-once-released:** a local process polls `/proc/net/tcp` (Linux) or `lsof` to discover `rein`'s ephemeral port, then either races a request after `rein` has closed (too late — code is single-use) or attempts a TCP connect to `127.0.0.1:<port>` during the listen window. This is the real attack: a same-UID process can connect to your loopback listener and read whatever the browser will GET later. Mitigations:
   - Validate `state` (defeats blind hits)
   - Only respond to a single GET, then `srv.Shutdown()` (limits the window)
   - On Linux, default kernel semantics are already restrictive; macOS defaults the same way
   - On Windows, RFC 8252 §B.3 recommends `SO_EXCLUSIVEADDRUSE` — irrelevant to Linux/macOS targets but worth noting for future ports.

Crucially: same-UID processes on the same machine can read your file descriptors via `/proc/<pid>/fd` regardless, and can ptrace you, so the threat model "another process under my UID is malicious" is **broader than the callback server**. At that point your secrets are already compromised. Don't over-engineer this layer. Validate `state`, single-shot the handler, and move on.

### 3.6 TLS on loopback — nobody bothers

RFC 8252 §8.3:

> "Loopback interface redirect URIs use the 'http' scheme (i.e., without Transport Layer Security (TLS)). This is acceptable for loopback interface redirect URIs as the HTTP request never leaves the device."

`gh auth login`, `atproto`'s Go tutorial, Probot's local dev mode, and Atlantis all use plain HTTP on loopback. Don't bother with TLS.

### 3.7 RFC 8252 applicability

RFC 8252 is about OAuth 2.0, and the manifest flow is *not* OAuth (it's a one-off App-creation handshake). The structural guidance — external browser, loopback redirect with OS-assigned port, validate `state`, no embedded webviews — applies identically. The bits that don't apply: PKCE (no token endpoint here), refresh tokens (irrelevant).

---

## 4. Private Key Handling — Disk Storage Patterns

This section covers the file-on-disk patterns that the v1 keystore will use. Section 5 covers the abstraction over them and the biometric-protected phase 1/2 followup.

### 4.1 File permissions

- PEM file: `0600` (read+write by owner only). `0400` is more restrictive but breaks the common rotation flow where the CLI rewrites the file.
- Parent directory: `0700`. Matches `~/.ssh` conventions; `ssh-keygen` will refuse to use keys whose parent dir is world-readable.

### 4.2 Atomic write pattern

```go
dir := filepath.Dir(target)
tmp, err := os.CreateTemp(dir, ".rein-pem-*")
if err != nil { return err }
defer os.Remove(tmp.Name())
if err := tmp.Chmod(0600); err != nil { return err }
if _, err := tmp.Write(pem); err != nil { return err }
if err := tmp.Sync(); err != nil { return err }      // fsync the data
if err := tmp.Close(); err != nil { return err }
if err := os.Rename(tmp.Name(), target); err != nil { return err }
// Optionally fsync the directory on Linux
```

`os.Rename` is atomic on the same filesystem on both Linux and macOS. This matches the patterns used by `age` and `sops`.

### 4.3 Owner verification on read

```go
fi, err := os.Stat(path)
st := fi.Sys().(*syscall.Stat_t)
if int(st.Uid) != os.Getuid() {
    return fmt.Errorf("refusing to use %s: owned by uid %d, expected %d", path, st.Uid, os.Getuid())
}
if fi.Mode().Perm()&0o077 != 0 {
    return fmt.Errorf("refusing to use %s: permissions %o too permissive", path, fi.Mode().Perm())
}
```

This is the SSH/age model.

### 4.4 Does `jferrl/go-githubauth` care about PKCS#1 vs PKCS#8?

No. It uses `golang-jwt/jwt`'s `ParseRSAPrivateKeyFromPEM` (transitively), which tries PKCS#1 first then falls back to PKCS#8. Pass the PEM bytes from GitHub directly.

### 4.5 Failure mode — App created, write fails

This is the worst case: GitHub minted the App, the conversion call succeeded, but the local write failed. Recovery:

1. **The user owns the App.** Visit `https://github.com/settings/apps/<slug>`. The PEM cannot be retrieved from the UI (GitHub never displays it again), but the user can click "Generate a private key" to mint a fresh one. This is the only recovery path GitHub provides.
2. **`rein init` retry semantics:** detect a partial state by reading `~/.config/rein/state.json` and noting `primary_created_at_github_but_local_write_failed`. On retry, prompt: "We previously created App `<slug>` at GitHub but couldn't save its private key locally. Please visit <URL>, click 'Generate a private key', download the PEM, and run `rein import-pem --app primary <file>`."
3. **The audit App, if not yet created, can be retried fresh.**

Mitigation: write to the target dir's tempfile (same filesystem) and `fsync` *before* calling the conversion endpoint, so almost all I/O failures surface earlier. The narrow remaining window is the rename, which is atomic.

### 4.6 Reference patterns from related tools

- `ssh-keygen` writes private keys as 0600, parent dir 0700, refuses to use a key file with looser perms unless `StrictModes no` is set.
- `age-keygen` writes a single PEM-ish file (`AGE-SECRET-KEY-…`) at 0600, prints the public key to stdout, never re-reads from terminal.
- `sops` stores age/PGP secrets in `~/.config/sops/age/keys.txt` at 0600.
- `gh auth` stores `hosts.yml` at 0600 under `~/.config/gh/`.

`rein` v1 follows the `gh auth` directory layout: `~/.config/rein/primary.pem`, `~/.config/rein/audit.pem`, `~/.config/rein/state.json` (App IDs, slugs, install IDs, key fingerprints, machine label), all 0600 in a 0700 directory.

---

## 5. Key Storage Backends — V1 Default and Phase 1/2 Followup

GitHub's own best-practices doc calls disk storage second-best: *"Consider storing your GitHub App's private key in a key vault…Alternatively, you can store the key as an environment variable. However, this is not as strong as storing the key in a key vault."* The same page says — directly relevant to `rein` — *"if your app is a native client, client-side app, or runs on a user device (as opposed to running on your servers), you must never ship your private key with your app."* (https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/best-practices-for-creating-a-github-app)

The PEM that GitHub mints in `rein init` stays on the developer's machine. The question is where on that machine.

### 5.1 Backend candidates

| Backend | macOS | Linux | Headless OK? | Per-call biometric? | Notes |
|---|---|---|---|---|---|
| Plain PEM file (0600) | ✓ | ✓ | ✓ | ✗ | **V1 default.** §4. |
| LAContext + age file | ✓ | — | ✗ on macOS | ✓ | **Phase 1/2 followup.** §5.4. |
| OS Keychain (legacy) | ✓ | ✓ (Secret Service) | ✗ needs GUI | ✗ (session-unlock only) | What `gh` uses |
| Data Protection Keychain | ✓ | — | ✗ | ✓ | Requires Apple Developer ID + app bundle |
| Kernel keyring (KeyCtl) | — | ✓ | ✓ session-scoped | ✗ | Lost on session end |
| `pass` (GPG files) | ✓ | ✓ | ✓ with `pinentry-tty` | ✗ | Requires GPG setup |
| YubiKey PIV | ✓ | ✓ | depends | ✓ (hardware touch) | Strong but heavyweight |

### 5.2 Pluggable Keystore interface (required from day one)

Even though v1 ships only plain PEM, the design must expose a small interface so phase 1/2 can swap in biometric without restructuring callers:

```go
type Keystore interface {
    Get(name string) ([]byte, error)
    Set(name string, data []byte) error
    Delete(name string) error
    Fingerprint(name string) (string, error)  // see §5.5
}
```

Per-invocation override via `--keystore=<backend>` flag and persistent preference via `~/.config/rein/config.yaml`. Token-minting paths take a `Keystore` interface, never a `*os.File` or `[]byte` directly.

### 5.3 V1 default — plain PEM file

Use the §4 patterns: 0600 file in 0700 dir, atomic write, owner verification on read. Metadata (App IDs, slugs, install IDs, fingerprints, machine label) goes in `state.json` next to the PEMs, also 0600. **No additional storage abstraction is loaded by default** — the implementation is roughly 80 lines of Go.

### 5.4 Phase 1/2 — LAContext + age-encrypted file (`--require-biometric`)

The macOS-native way to get per-call biometric without app-bundle distribution: use `LAContext.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics)` via cgo to prompt Touch ID, then decrypt an age-encrypted PEM. There is small prior art (`github.com/lox/go-touchid`, MIT) that wraps the LAContext call.

Flow on each token mint:

1. `rein` calls LAContext with prompt text "rein wants to mint a token for <repo>".
2. macOS shows Touch ID prompt; user touches sensor.
3. On success, rein reads `~/.config/rein/primary.pem.age`, decrypts with a key derived from a per-machine secret stored in macOS Keychain (legacy keychain, session-unlocked is fine — biometric is enforced by LAContext, not the keychain).
4. Mints JWT, requests installation token, caches.

Token caching matters here: installation tokens last 1 hour. With an idle-timeout cache (default 15 min, configurable via `--biometric-cache=<duration>`), Touch ID fires at most once per ~15 min, not per agent operation. Matches `sudo`'s `timestamp_timeout` model.

**On Linux**, `--require-biometric` falls back to a passphrase prompt with a clear warning that per-call biometric isn't supported by the Linux desktop stack — `fprintd` + PAM can require fingerprint at session unlock but isn't bound to individual Secret Service items. Document this asymmetry.

**Critically, this requires no Apple Developer ID, no code signing, no app bundle.** LAContext works from unsigned binaries.

### 5.5 Fingerprints, not key contents, for cross-machine identification

GitHub displays a SHA-256 fingerprint of each registered public key in the App settings UI:

> `openssl rsa -in PATH_TO_PEM_FILE -pubout -outform DER | openssl sha256 -binary | openssl base64`

Store this fingerprint in `state.json` (`primary.key_fingerprint`) so `rein status` can show which registered key on GitHub corresponds to this machine's PEM — without reading the PEM. Also the right way to detect "is the local PEM still the one registered at GitHub" on startup. Go equivalent:

```go
import (
    "crypto/sha256"
    "crypto/x509"
    "encoding/base64"
    "encoding/pem"
)

func fingerprintPEM(pemBytes []byte) (string, error) {
    block, _ := pem.Decode(pemBytes)
    key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
    if err != nil { return "", err }
    pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
    if err != nil { return "", err }
    sum := sha256.Sum256(pubDER)
    return base64.StdEncoding.EncodeToString(sum[:]), nil
}
```

The `Keystore.Fingerprint(name)` method computes this on demand.

### 5.6 Future backends — note for context, not for v1

- **Legacy OS Keychain (gh-style):** `99designs/keyring` (MIT, extracted from AWS Vault) gives a one-line abstraction over Keychain / Secret Service / KWallet / Pass / KeyCtl. Useful if someone files a feature request for "same UX as `gh`." Per `99designs/aws-vault` PR #1243, this backend cannot do per-call biometric; biometric requires Data Protection Keychain, which requires an app bundle. So the LAContext path in §5.4 is strictly better for single-binary distribution.
- **YubiKey PIV:** `go-piv` (Apache-2.0, https://github.com/go-piv/piv-go). Load the GitHub-minted PEM into PIV slot 9c, set touch policy to `cached` or `always`. JWT signing then happens on-card via `crypto.Signer`. Works on macOS and Linux equally, sidesteps the macOS code-signing question entirely. Add as `--keystore=yubikey-piv` only if there's user demand.
- **Data Protection Keychain + app bundle:** track as v2+ if anyone wants the polished macOS-native UX. Costs: Apple Developer Program ($99/yr), notarization in CI, app-bundle distribution. The aws-vault PR is the template.

---

## 6. Two-App-in-One-Flow UX

### 6.1 Has anyone done this?

No public CLI tool creates two GitHub Apps in one invocation that the research surfaced. Probot, Atlantis, and the misc create-github-app-from-manifest demos all create exactly one App. `rein` is pioneering the UX. The good news: the protocol composes trivially.

### 6.2 Recommended sequencing

```
$ rein init
[1/2] We'll create your PRIMARY App (contents/issues/PRs write, metadata read).
      Listening on http://127.0.0.1:54321/callback
      Opening browser → https://github.com/settings/apps/new?state=<S1>
      ...waiting for GitHub to call back (10 min timeout)
      ✓ Primary App "rein-primary-<hash>" created.
      ✓ Wrote ~/.config/rein/primary.pem (0600).

[2/2] We'll create your AUDIT App (issues:write only).
      Listening on http://127.0.0.1:54322/callback
      Opening browser → https://github.com/settings/apps/new?state=<S2>
      ...waiting for GitHub to call back (10 min timeout)
      ✓ Audit App "rein-audit-<hash>" created.
      ✓ Wrote ~/.config/rein/audit.pem (0600).

Save this for re-bootstrapping on another machine:
  Primary slug: rein-primary-<hash>
  Audit slug:   rein-audit-<hash>

Next: visit https://github.com/apps/rein-primary-<hash>/installations/new
      and https://github.com/apps/rein-audit-<hash>/installations/new
      to install each App on the repositories you want rein to broker tokens for.
```

Two separate ephemeral listeners — different ports, different `state` nonces, fresh manifest JSON each time. Don't try to multiplex one listener.

### 6.3 In-between state

After step 1 succeeds, **persist atomically** (`primary.pem`, `state.json` with `primary.app_id`, `primary.slug`, `primary.key_fingerprint`, `primary.created_at`, `phase: primary_done`). Step 2 only fires after step 1 is durable. If step 2 fails, `rein init` exits non-zero with: "Primary App is created and saved locally. Re-run `rein init --resume` to create the audit App."

### 6.4 Anti-patterns to avoid

- **Same App name** — primary and audit must have distinct, generated names (`rein-primary-<random6>` and `rein-audit-<random6>`). GitHub enforces global uniqueness; conflict surfaces only at the GitHub UI step, late and confusing.
- **Sharing a listener** — port conflicts and state-validation complexity not worth it.
- **Trying to parallelize** — the user can only authenticate one App at a time in a browser tab.
- **Identity confusion** — show clearly in the CLI output and ideally in the manifest's `description` field which App is being created at each step.

---

## 7. Permission Catalog Gotchas

### 7.1 `pull_requests:write` includes review/approval

Confirmed. A GitHub App with `pull_requests:write` can `POST /repos/{owner}/{repo}/pulls/{pull_number}/reviews` with `event: APPROVE`. This is exactly the capability that needs careful brokering through `rein`.

### 7.2 `contents:write` — what it does and doesn't allow

- **Push to branches:** yes (including force-push subject to branch protection).
- **Delete branches:** yes (`DELETE /repos/{}/{}/git/refs/heads/{branch}`).
- **Tag and release management:** yes.
- **Workflow files (`.github/workflows/*.yml`):** **NO.** This is the well-known gotcha. Even with `contents:write`, GitHub blocks an App from creating/updating any commit that touches `.github/workflows/*.yml` unless the App also holds the separate `workflows:write` permission:

> "Error: refusing to allow a GitHub App to create or update workflow"
> — https://github.com/orgs/community/discussions/35410

> "GitHub enforces workflow file update permissions even for tag pushes if the commit contains workflow changes."

**Implication for `rein`:** an AI agent operating through `rein`'s primary App **cannot modify CI workflows**. This is a feature, not a bug, and worth documenting — it's a useful guardrail.

### 7.3 `issues:write` semantics

`issues:write` permits:
- Creating issues
- Commenting on any issue, including ones raised by others
- Closing/reopening issues created by anyone
- Locking/unlocking issues
- Adding labels (must already exist) — labels created by others can be applied freely
- Creating new labels (yes — same permission)
- Editing issue titles/bodies created by others

For the audit App, this means the audit App **can** edit or delete its own comments. The protective property — "the primary App cannot delete the audit App's comments" — comes from the fact that **each comment is owned by the App that created it**. A different App (the primary) cannot delete the audit App's comments via the `issues:write` permission alone; the API returns 403 because issue-comment deletion is restricted to the comment's author or repo admins. **This is the architectural pillar of the two-App design.**

### 7.4 Branch protection and CODEOWNERS

- **Required reviewers:** an App's review can satisfy "X approvals required" if the App has `pull_requests:write` and is added as a permitted reviewer. App reviews count as one approval like a human.
- **CODEOWNERS:** Apps cannot be listed in CODEOWNERS files. Only users and teams. So an App's review does **not** satisfy "require review from Code Owners". This is a hard limit.
- **Atlantis's `IsMergeableMinusApply`** reflects this: "checks review decision (which takes into account CODEOWNERS) and required checks for PR (excluding the atlantis apply check)".

### 7.5 Dependabot interaction

Dependabot operates under its own GitHub-managed App identity and is not influenced by the permissions of user-installed Apps. `rein`'s Apps will see Dependabot PRs through the normal `pull_request` events; they cannot impersonate Dependabot or vice versa.

### 7.6 Auto-included permissions

`metadata:read` is **always implicitly granted** to every GitHub App — mandatory and cannot be unselected. Requesting it explicitly in the manifest is harmless but redundant. `contents:write` does not auto-grant `pull_requests:write`; list each permission.

### 7.7 Recommended manifest permissions

```json
"default_permissions": {
  "contents":       "write",
  "issues":         "write",
  "pull_requests":  "write",
  "metadata":       "read"
}
```

For audit:

```json
"default_permissions": {
  "issues":   "write",
  "metadata": "read"
}
```

(`metadata:read` is implicit but most code paths assume it; include it to be explicit.)

---

## 8. Repo Installation Deep-Link

### 8.1 URL pattern

`https://github.com/apps/<slug>/installations/new` is current as of 2026, confirmed by https://docs.github.com/en/apps/using-github-apps/requesting-a-github-app-from-your-organization-owner.

### 8.2 UI behavior

The install page asks the user to (a) pick an account (personal or any org they own), (b) choose "All repositories" or "Only select repositories", (c) confirm permissions, (d) click "Install" (or "Install & Authorize" if the App has "Request user authorization (OAuth) during installation" enabled).

### 8.3 Pre-selection — no public query param

GitHub does not expose a documented query parameter to pre-select repositories, pre-select the account, or pre-grant a permission scope. The only query param that survives is `state` (and only via the redirect to `setup_url`). For `rein`, "to limit blast radius, install on specific repos" must be a human instruction in the CLI's printed guidance.

### 8.4 The `setup_url` contract

If `setup_url` is set in the manifest *and* "Redirect on update" is enabled, the user is redirected after install to `<setup_url>?installation_id=<N>[&setup_action=install|update][&state=<S>]`. Security note from GitHub:

> "Bad actors can hit this URL with a spoofed installation_id. Therefore, you should not rely on the validity of the installation_id parameter."
> — https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url

For `rein` this is moot — the CLI's installation polling (§8.6) is more robust than chasing the install redirect.

### 8.5 Install vs Install & Authorize

- **Install**: server-to-server only. App gets an installation; you mint installation tokens via JWT. This is what `rein` wants for both Apps.
- **Install & Authorize**: also mints a user access token (OAuth). Useful only if you need to attribute actions to the installing user. `rein`'s broker model **should not** request user authorization — keep "Request user authorization (OAuth) during installation" **off** in the manifest.

### 8.6 Detecting installation completion programmatically

1. **Poll `GET /app/installations`** with App JWT. The new installation appears within seconds. Filter by `account.login == <expected>` and `app_id == <our id>`. This is the right approach for a CLI.
2. **Webhook on `installation.created`** — requires a public URL. Not viable for a local CLI.

Recommend a `rein init --wait-for-install` mode that polls every 3 seconds with exponential backoff up to 10 minutes.

---

## 9. Multi-Machine Setup

### 9.1 The constraint that shapes everything

**There is no GitHub REST API to provision additional private keys for an existing App.** Per https://docs.github.com/en/rest/apps/apps, the only manifest-related endpoint is the one-shot conversion. After that, additional PEMs are minted only via the GitHub UI: *Settings → Developer settings → GitHub Apps → Edit → Private keys → Generate a private key.* There is also no BYO public key support — GitHub mints the keypair and never lets you upload your own. This is an open feature request (https://github.com/orgs/community/discussions/124567) but unimplemented as of mid-2026.

GitHub limit: **max 25 private keys per App** (https://github.blog/changelog/2024-11-08-updated-limits-for-github-app-private-keys-and-scoped-tokens/). Plenty for any solo developer.

### 9.2 The three options

**Option A — One App pair, multiple PEMs (V1 RECOMMENDED DEFAULT)**

Create the App pair once on machine 1. For each additional machine, generate a fresh PEM via GitHub UI and import. Each machine has its own PEM but they all authenticate as the same App identity.

- Scales to ~25 machines per `rein` user; far past any realistic need.
- Per-machine revocation: stolen laptop → revoke only that PEM at GitHub. Desktop keeps working.
- Audit attribution: GitHub logs which key fingerprint signed each JWT.
- `rein init --import` walks user through: pick existing App by slug, opens browser to App settings, prompts user to click "Generate a private key", reads the downloaded `.pem` from a path or stdin.
- Installation unchanged — the App is already installed on the relevant repos; the new PEM just signs as that App.

**Option B — One App pair, shared PEM (simplest, coarsest revocation)**

Create once. Securely transport the PEM to machine 2 via age-encrypted file, password manager, etc.

- Simplest UX.
- Revocation is all-or-nothing.
- No audit attribution; all machines look identical.
- Fine when same human, same trust level across machines, and convenience matters more than per-machine revocation.

**Option C — Separate App pair per machine (strict isolation)**

Run `rein init` from scratch on each machine.

- Strongest isolation.
- `2 × N` Apps clutter the developer-settings page.
- Repo installation must be done per-machine.
- Hits org App-creation restrictions N times.
- Appropriate for mixed trust levels or when per-machine *capabilities* differ.

### 9.3 Decision matrix

| Scenario | Recommended |
|---|---|
| 1 dev, 2-3 personal machines, same trust | **A** |
| Want simplest UX, accept coarse revocation | **B** |
| Mix of personal + work machines | **C** |
| Mix of human + CI machine | **C** (different perms anyway) |
| Headless dev VM proxying through laptop | **B** |

Default `rein` recommendation: **Option A**.

### 9.4 Concrete `rein init --import` UX (Option A)

```
$ rein init --import     # on machine 2
Which App pair do you want to import?
  Primary App slug (e.g. rein-primary-abc123): rein-primary-abc123

Step 1/2: Generate a new private key for the PRIMARY App.
  Opening https://github.com/settings/apps/rein-primary-abc123#private-keys
  Click "Generate a private key". A .pem file will download.
  Drag-and-drop the file here, or paste its path:
  > /Users/tom/Downloads/rein-primary-abc123.2026-05-25.private-key.pem

  ✓ Fingerprint: kP3a…/Q= (matches a registered key at GitHub)
  ✓ Stored at ~/.config/rein/primary.pem (0600).
  ✓ Updated ~/.config/rein/state.json
  ✓ You may now delete the downloaded .pem file.

Step 2/2: …same for AUDIT App…

This machine is registered as "tom-mbp-2026".
Manage with: rein machines list | rein machines revoke <label>
```

`rein machines list` calls `GET /app` (returns the App with its `pem_keys` array of fingerprints) and cross-references each machine's locally-stored fingerprint. It can't tell which fingerprint belongs to which machine without `rein` having seen each one, so `state.json` includes a human-set `machine_label` echoed back at registration.

### 9.5 Bootstrap problem — the slug is the bootstrap token

On a fresh machine with no `~/.config/rein/`, the user needs to know their App slug. **The slug is the only piece of non-secret data they need to re-bootstrap**; the PEM regenerates per-machine. Print the slug in big letters at the end of the very first `rein init` and recommend saving it to the user's password manager.

### 9.6 Disaster recovery

If a user loses all machines with no PEM backup:
1. Log into GitHub from any browser.
2. Settings → Developer settings → GitHub Apps → find `rein-primary-<slug>` and `rein-audit-<slug>`. App ID, slug, install list all recoverable.
3. Generate fresh private key for each.
4. New machine: `rein init --import` for each App.
5. **Delete the old PEMs in the GitHub UI** — that's the revocation step.

Because the slug is in the URL and the App ID is on the settings page, **no `rein`-side state is irrecoverable** except past installation-token mint history (server-side at GitHub anyway).

### 9.7 What does NOT work

- `rsync ~/.config/rein/ to other-machine` works for Option B but defeats Option A's per-machine revocation. Don't document.
- Symlinking the keystore to iCloud/Dropbox — exposes PEM to a cloud service. Strongly anti-recommend.
- Generating PEM on machine 1 and using `rein` to "push" it to machine 2 over SSH — builds a key-distribution subsystem strictly worse than the GitHub UI flow.

### 9.8 Future case: BYO public keys

If GitHub ships https://github.com/orgs/community/discussions/124567, `rein` should add a fourth option: each machine generates its own keypair locally, transmits only the public half to GitHub. Equivalent of per-machine SSH keys, strictly better than today's "GitHub mints, you download" model. Track the discussion; migration would be small (~50 lines to add the public-key upload call).

---

## 10. Edge Cases Worth Designing For

### 10.1 Org disables App creation

If the user picks an org during step 1 of the manifest flow where they lack create-app rights, GitHub's UI rejects the create inline. The CLI sees no callback — same as a Cancel. **Detection:** the 10-minute timeout fires. **Recommended UX:** on timeout, prompt: "Did GitHub show an error like 'You must be an organization owner to create an App'? If so, try `rein init --user` to create the App under your personal account instead, or ask an org owner to run `rein init --org <org>`."

### 10.2 No browser (proxy, headless, SSH session)

`rein` should attempt `xdg-open` (Linux) / `open` (macOS) but always also print the URL on stdout as fallback:

```
Please visit this URL to create your primary App:
  https://github.com/settings/apps/new?state=<S>
(We tried to open your browser automatically; if nothing opened, copy-paste the URL.)
```

Matches `gh auth login` UX. For headless machines with port forwarding (`ssh -L`), document that the user can forward the listener port — though `rein`'s listener picks an ephemeral port, so they'd need to learn it from CLI output.

### 10.3 Partial-state recovery

`~/.config/rein/state.json` with explicit `phase` and `app_metadata` fields. On `rein init`:

- `phase == ""`: clean start.
- `phase == primary_done`: print "Resuming after primary App creation. Skipping to audit App..." and run only step 2.
- `phase == audit_done`: print "Both Apps already configured. Use `rein status` to inspect, or `rein init --force` to recreate."

### 10.4 Wrong account/org picked at GitHub

The CLI sees only `code` and `state` on the callback; the conversion response reveals the actual `owner.login`. After exchanging:

```go
if cfg.GetOwner().GetLogin() != expectedOwner {
    return fmt.Errorf("you created the App under %q but I expected %q; please delete it at %s and rerun",
        cfg.GetOwner().GetLogin(), expectedOwner, cfg.GetHTMLURL()+"/advanced")
}
```

There is no API to delete a GitHub App programmatically (only via UI). Best the CLI can do is detect, refuse to persist, and instruct.

### 10.5 2FA / SSO orgs

The user's browser session handles 2FA/SSO during the GitHub UI step. The CLI is not involved. If the org has SAML SSO enforced and the user's session is not authorized, GitHub renders an "Authorize for SSO" prompt before completing the App create. No special handling needed.

### 10.6 Rate limits on App creation

GitHub publicly notes the conversion endpoint is rate-limited (no number documented). For a CLI used a few times per developer, not a constraint. Surface 403/429 responses clearly: "GitHub rate-limited App creation. Try again in a few minutes."

---

## 11. Staged Recommendations

### Stage 1 — Minimum viable `rein init` (V1)

1. Implement single-App manifest flow against `127.0.0.1:0`, with state validation and 10-minute timeout. Use `google/go-github/v88`'s `AppsService.CompleteAppManifest`.
2. Atomic-write PEM at `~/.config/rein/{primary,audit}.pem` (0600 in 0700 dir), state metadata at `~/.config/rein/state.json`. Compute key fingerprints and store alongside.
3. Two sequential rounds (primary then audit) with explicit phase persistence and `--resume` semantics.
4. Print install URLs `https://github.com/apps/<slug>/installations/new` at the end; **do not automate install**.
5. Print App slugs prominently at end with "save these to your password manager for multi-machine setup."
6. Wrap storage in a `Keystore` interface from day one, even though only the file backend ships. Token-minting code paths take `Keystore`, never `[]byte`.

### Stage 2 — Multi-machine + polish

7. `rein init --import <slug>` flow per §9.4. Opens browser to App settings, reads downloaded PEM from path or stdin, verifies fingerprint against `GET /app`, persists.
8. `rein status` to display App IDs, slugs, install IDs (after polling), key fingerprints, machine label, PEM file ages.
9. `rein machines list` / `rein machines revoke <label>` cross-referencing local fingerprint with `GET /app` `pem_keys` array.
10. `rein init --wait-for-install` polling `GET /app/installations` for both Apps.
11. `rein import-pem --app {primary|audit} <file>` for the §4.5 disk-write-failure recovery path.
12. `--org <name>` flag and detect the "user picked wrong owner" failure mode.

### Phase 1/2 — Biometric (`--require-biometric`)

13. Add `--require-biometric` flag. On macOS: LAContext + age-encrypted file per §5.4. Configurable idle-timeout cache (default 15 min) via `--biometric-cache=<duration>`. Token-mint prompt shows "rein wants to mint a token for <repo>".
14. On Linux: fall back to passphrase prompt with clear warning that per-call biometric isn't supported by the Linux desktop stack.
15. Both backends register as alternative `Keystore` implementations behind the same interface.

### Stage 3 — Hardening (later)

16. Owner-verify PEM files on every read.
17. Use `subtle.ConstantTimeCompare` for `state`.
18. Optional `--keystore=yubikey-piv` backend via `go-piv` for users wanting hardware-token isolation.

**Watch for:** GitHub is currently rolling out a new installation-token format (`ghs_APPID_JWT`, ~520 chars, two dots) — staged rollout mid-May to late-June 2026 per https://github.blog/changelog/2026-04-24-notice-about-upcoming-new-format-for-github-app-installation-tokens/. A per-request override header is documented at https://github.blog/changelog/2026-05-15-github-app-installation-tokens-per-request-override-header/. Confirm `jferrl/go-githubauth` handles the new opaque format; as of May 2026 the package treats installation tokens as opaque strings, so it should be transparent.

---

## 12. Caveats and Open Questions

- **Atlantis's full `github_app_controller.go` source** was not directly fetched in the research session due to upstream fetch failures; handler line numbers and behavior reconstructed from referenced PRs (#2141, #2142), the public `github_client.go`, the Atlantis docs (`runatlantis.io/docs/access-credentials`), and the `pkg.go.dev` package listing. The `ExchangeCode` excerpt is verbatim from public `github_client.go`. If vendoring (rather than re-implementing) Atlantis code, fetch `server/controllers/github_app_controller.go` from a working `git clone` and confirm template-rendering logic before copying.
- **`carabiner-dev/burnafter`** is described as a credential-storage tool but repo contents could not be inspected in the research session. Its tagline matches `rein`'s broker model closely enough to be worth checking via `git clone` once before finalizing the design.
- **PKCS#1 vs PKCS#8** — GitHub currently mints PKCS#1 keys. If GitHub ever changes this (docs are silent), the PKCS#1-or-PKCS#8 fallback in `golang-jwt`/`jferrl/go-githubauth` will absorb the change transparently. Trust the parser; don't hard-code format expectations.
- **The `select_target` redirect bug** for installation state preservation was reported fixed in 2024 but is worth re-testing during `rein` development. For the *create* flow (where `redirect_url` is set in the manifest) this issue does not apply, only for the *install* flow.
- **Rate-limit numbers** for App creation are not published. Treat any 429/403 with `documentation_url` mentioning rate limits as transient.
- **Co-resident process attacks on loopback** are real per RFC 8252 but `rein`'s threat model already assumes the local UID is trusted (otherwise PEM-on-disk is moot regardless). Defenses in §3.5 are sufficient.
- **Stateless installation token rollout is in-flight.** Between mid-May and late-June 2026, GitHub is staging the new `ghs_APPID_JWT` format. Any code in `rein` that hard-codes token length or parses token contents will break; treat tokens as opaque strings.
- **LAContext from unsigned Go binaries** — `lox/go-touchid` and similar small libraries demonstrate this works, but the path is less travelled than the keychain-with-app-bundle path. Worth a small spike during Phase 1/2 implementation to confirm `evaluatePolicy` returns reasonable errors from an unsigned binary on macOS 14+.
