# `rein init` GitHub App Manifest Flow — Design (Phase 0.5 CP4)

**Status:** design for CP5 implementation. Companion to
`docs/rein-manifest-flow-research.md` (the empirical deep-dive); this
doc is the build spec.

**Scope:** how `rein init` creates the primary + audit GitHub Apps the
first time it runs. Two browser-open events, in one invocation. Out of
scope: token-mint UX (Phase 0 done), broker daemon (Phase 1), biometric
keystore (Phase 1/2), multi-machine `--import` (Stage 2).

## Goals

1. New user → both Apps registered + private keys on disk + repo install
   URLs printed, in <10 minutes of human time. Browser handshake is the
   only manual step.
2. Fail loud and recoverable. Any partial state on disk is detectable on
   re-run; no silent half-success.
3. Bridge to Phase 0's env-var path: after manifest flow, the existing
   `REIN_APP_*` env vars become advanced overrides; the default is to
   read `~/.config/rein/primary.pem` + `state.json`.

## Flow (end-to-end)

When `rein init` runs and detects no `~/.config/rein/state.json` (or
finds one with `phase != "audit_done"`), it executes:

```
[1/2] primary App
  → bind ephemeral 127.0.0.1:0 callback listener
  → generate 256-bit state nonce
  → render local HTML form that auto-POSTs the primary manifest JSON to
    GitHub (manifest schema below)
  → xdg-open / open the local URL; also print it as fallback
  → block on callback (10-min context timeout)
  → on callback: validate `state` (constant-time compare); 400 on mismatch
  → POST /app-manifests/{code}/conversions, get App config + PEM
  → verify response `owner.login` matches user expectation (if --user/--org)
  → atomic-write PEM to ~/.config/rein/primary.pem (0600), fsync before rename
  → atomic-write state.json with phase=primary_done + slug/id/fingerprint
  → close listener
[2/2] audit App  (skip if --skip-audit)
  → repeat with audit manifest, separate listener, fresh state nonce
  → atomic-write audit.pem + state.json phase=audit_done
Print:
  - both slugs prominently ("save these to your password manager")
  - install deep-links: https://github.com/apps/<slug>/installations/new
Hand off to CP1's local scaffolding (shim install, symlink, alias).
```

`rein init --resume` re-reads `state.json` and jumps to the next step.
A successful `--resume` is equivalent to a fresh start where step 1 was
already done.

## Manifest schemas

**Primary** — bridges Phase 0's existing capability surface (validated
working). `metadata:read` is implicit but listed for explicitness.

```json
{
  "name": "rein-primary-<random10>",
  "description": "rein credential broker — primary identity (mints scoped tokens for AI coding agents)",
  "url": "https://github.com/TomHennen/rein",
  "redirect_url": "http://127.0.0.1:<port>/callback",
  "public": false,
  "default_permissions": {
    "contents":      "write",
    "issues":        "write",
    "pull_requests": "write",
    "metadata":      "read"
  },
  "default_events": []
}
```

**Audit** — separately-identified posting App per design §4.2.4. Strict
minimum permissions; the architectural property (research §7.3) is
that comment deletion is restricted to the creating App.

```json
{
  "name": "rein-audit-<random10>",
  "description": "rein credential broker — audit identity (posts audit comments the agent cannot prune)",
  "url": "https://github.com/TomHennen/rein",
  "redirect_url": "http://127.0.0.1:<port>/callback",
  "public": false,
  "default_permissions": {
    "issues":   "write",
    "metadata": "read"
  },
  "default_events": []
}
```

`request_oauth_on_install: false` (default; do not enable). `public:
false` so the App is private to the creator account.

`<random10>` is `hex(rand.Read(5))` — 40 bits of entropy. GitHub
enforces global uniqueness across all App names; 24 bits across two
Apps × thousands of users hits ~10% birthday-bound collision somewhere
in the corpus. The 5-byte (10-hex-char) form costs nothing and makes
collisions vanishingly unlikely. The collision failure mode is benign
(GitHub returns an error at the UI step; user re-runs) but
operationally annoying.

## Callback server lifecycle

- Bind: `net.Listen("tcp", "127.0.0.1:0")` — kernel-assigned ephemeral
  port. Loopback only per RFC 8252 §7.3 (literal IP, not `localhost`).
- Timeouts: `ReadHeaderTimeout: 5s`, `ReadTimeout: 10s`, `WriteTimeout:
  10s`. Parent context deadline 10 minutes (matches user attention; same
  budget as research §3.3).
- Single-shot: handler writes a 200 response + brief "you can close this
  tab" HTML, signals completion via a Go channel, then `srv.Shutdown()`.
- Cleanup: `defer listener.Close()` + `defer srv.Shutdown(ctx)`; both
  idempotent if already closed.

## State nonce

- `crypto/rand.Read(buf[:32])`, base64-url-encoded → ~43-char string.
- Compared with `subtle.ConstantTimeCompare` on the callback.
- Held in a small server-memory map keyed by listener; never persisted.
- Mismatch → HTTP 400 + log + no conversion call.

The research recommends a server-memory map even when only one nonce is
in flight; we'll follow that so the structure is unchanged when CP5+ Stage 2
adds the import flow (which may want concurrent listeners).

## Error handling matrix

| Error | Surface | Recovery |
|---|---|---|
| User closes browser / hits cancel | 10-min timeout fires | Print "no callback received; re-run `rein init`" |
| `state` mismatch on callback | HTTP 400; CLI keeps listening | Will timeout; user re-runs |
| Conversion endpoint returns non-2xx | Surface the response body to stderr | Re-run; transient 429s self-resolve (research §10.6) |
| `owner.login` mismatch (user picked wrong account) | Refuse to persist; print "you created the App under X but I expected Y; delete it at <URL> and re-run" | Manual delete via GitHub UI (no API for App deletion) + re-run |
| PEM write fails AFTER successful conversion | Surface the path + the worst-case (research §4.5) | Print a UI-only recovery (no vapor commands): "App `<slug>` was created at GitHub but the local save failed. Visit `<URL>/advanced`, click 'Generate a private key', save the file as `~/.config/rein/primary.pem` (mode 0600), then re-run `rein init --resume`." File a tracked issue for a Stage 2 `rein import-pem --app primary <file>` polish that automates the save+chmod+state.json patch in one command. |
| Browser launch (`xdg-open`/`open`) fails or absent | Already printed URL on stdout as fallback | None needed; user copy-pastes |
| Port-bind fails (rare; another process on `:0` — basically impossible) | Surface the bind error | Re-run |
| Phase 1 done but phase 2 fails | Exit non-zero with `--resume` hint | `rein init --resume` |

The PEM-write-after-conversion-success failure is the only one without
a fully-automatic recovery; the manual `import-pem` path is straightforward
and the partial state in `state.json` makes it discoverable.

## Key storage timing

Per research §4.2 — temp-file + fsync + rename, atomically. Specifically:

1. Conversion call succeeds → PEM in memory.
2. `os.CreateTemp(~/.config/rein/, ".primary.pem-*")`.
3. `tmp.Chmod(0o600)`.
4. `tmp.Write(pem)`.
5. **`tmp.Sync()` — fsync the file data before rename.** Closes the
   window where the rename target's bytes are in page cache only.
6. `tmp.Close()`.
7. `os.Rename(tmp, ~/.config/rein/primary.pem)`.
8. **`dir.Sync()` on `~/.config/rein/` (open with `O_RDONLY`,
   `f.Sync()`, close).** On ext4/xfs/APFS, `os.Rename` is atomic but
   the directory entry update can sit in dirent cache; without this
   step a crash between rename and the OS's lazy dirent flush can
   leave the target path absent even though the conversion succeeded —
   exactly the worst-case the error matrix's "PEM write fails after
   successful conversion" row admits. Step 8 closes that window.
9. Update `state.json` atomically with the same 8-step sequence
   (including the trailing dir fsync).

Parent dir `~/.config/rein/` is created 0700 in CP1 already. PEM file is
0600. `state.json` is 0600.

We do NOT store the conversion's `client_secret` or `webhook_secret`
(rein is not OAuth-token-authenticating users and never receives
webhooks). Discarded immediately to limit blast radius.

## Repo install deep-link

After both Apps land, print:

```
Both Apps registered. To make them useful, install each on the repos
you want rein to broker tokens for:
  Primary: https://github.com/apps/rein-primary-<slug>/installations/new
  Audit:   https://github.com/apps/rein-audit-<slug>/installations/new
```

CP5 does **not** poll `GET /app/installations` for install completion
(research §8.6 / Stage 2 followup). Keeps CP5 scope tight; user runs
`rein doctor` to verify after installing.

## Two-browser-open UX

Sequential, with clear `[1/2]` / `[2/2]` framing in stdout. Each step:
its own listener, fresh port, fresh state nonce. The user sees the
primary App's GitHub create page, returns to terminal, sees the audit
App's create page open in a new tab.

Anti-patterns avoided (research §6.4): same App name, shared listener,
parallel browser opens.

`--skip-audit` flag opts out of step 2 for users who don't want the
audit App in CP5 (which doesn't yet use it). State semantics:
`--skip-audit` leaves state.json at `phase: primary_done` with `audit:
null`. A subsequent `rein init` (with or without `--resume`) sees
`phase: primary_done` and creates the audit App — the user gets the
audit App later without `--force`. To stay opted-out indefinitely, the
user just keeps passing `--skip-audit` on every run; that's
explicit-opt-out-per-invocation rather than a persistent decision, and
matches the principle that state.json records "what was created," not
"what was deliberately skipped."

## Security considerations

The full RFC 8252 / loopback-attack analysis is in research §3.5; the
short version:

- Same-UID processes can already read the broker's PEM directly. The
  callback-server threat model is bounded by that same UID assumption,
  so we don't over-engineer this layer.
- Mitigations applied: `state` validation (defeats blind hits), single-
  shot handler (limits exposure window), 127.0.0.1 literal (no
  `localhost`), 10-min context deadline, fsync-before-rename for the
  PEM.
- TLS on loopback intentionally not used (research §3.6).
- Owner verification on conversion-response prevents "user clicked
  through to wrong org" footgun (research §10.4); implemented;
  activated when `--owner` is passed (recommended). Without `--owner`
  the manifest flow prints a one-line WARN to stderr so the operator
  is reminded of the missing guard.

## Safe handling of the App private key

The App PEM is the **root secret of the whole system**: with it plus the
App ID and an installation ID, anyone can mint installation tokens for
everything the App is installed on. Protecting it is the point of rein,
so how it reaches the keystore matters as much as how it is stored.

**The automated manifest flow is the safe path.** The conversion
response hands the PEM to rein in memory; rein writes it straight to the
keystore via the atomic 0600 sequence (`Key storage timing` above). It
never lands on disk loosely, never passes through `~/Downloads`, never
enters shell history. Prefer this path wherever a browser can reach the
loopback callback — directly (desktop) or via an SSH tunnel (remote);
see the headless fallback below.

**Calibrating the risk of manual handling.** rein's threat model
already concedes (Security considerations above; issue #7) that any
*same-UID* process can read the broker's PEM directly. So the only thing
manual handling adds worth worrying about is exposure that **crosses the
same-UID boundary**, of which there are exactly two:

1. **Off-machine.** A PEM downloaded to `~/Downloads` can be swept into
   iCloud / Dropbox / Time Machine / corporate backup — the root key
   replicated to a third party. This is qualitatively worse than
   anything in rein's in-machine model.
2. **Other local UID.** A browser download lands at the umask default
   (typically `0644`), readable by other local users; the keystore's
   `0600` destination never was.

The within-UID exposure (a loose file readable by your own processes) is
**not a new worry** — it is already in the conceded model.

**The keystore's uid + mode checks do not mitigate this.** They protect
the *destination*. The exposure is entirely in the *journey* —
download → transit → cleanup — which happens before rein ever sees the
file. The journey is what documentation and any future import command
must cover.

**Why manual handling is acceptable as a *fallback*.** The relevant
comparison is not "manual PEM vs. PAT sprawl" (true, but it only argues
the path should *exist*). It is "manual PEM vs. SSH `-L`," because SSH
`-L` keeps the *automated* import and so never incurs the journey risk.
Manual handling is therefore the **last resort**, reached only when
neither a local browser nor port-forwarding is possible. Layered that
way, the off-machine/other-UID exposure is rarely incurred at all.

**Required handling rules when the manual path is used** (to be
surfaced verbatim in the CP7 onboarding doc):

- Get the key into rein's keystore immediately and delete the
  download (`shred -u` / secure-delete), so no loose copy lingers.
- Do not download into a cloud-synced or backed-up directory; prefer a
  scratch path you control, then remove it.
- For a remote box, stream the key in rather than copying a loose file:
  e.g. `cat app.pem | ssh <host> 'rein import-pem --app primary -'`
  (Stage 2 command) so the PEM never lands loosely on the remote disk.
  Until that command ships, `scp` (encrypted transit) into a `0600`
  path and `shred -u` the source.
- Never paste the key into a chat, an issue, a config file, or any
  command that records to shell history.
- After import, the keystore enforces `0600` + owner-only; a key that
  has been re-permed loose or chown'd is refused on read (fail closed).

## Headless / remote fallback (no localhost-reachable browser)

The loopback callback (the CP5 primary path) needs a browser that can
reach `127.0.0.1:<port>` on the machine running rein. On a headless or
SSH-only box that fails. The manifest flow **cannot** be made
callback-free: GitHub delivers the temporary `code` *only* via the
redirect and never displays it on a GitHub-hosted page (researched and
confirmed against GitHub docs). So the options, in preference order:

1. **SSH local port-forward (preferred for remote).** `ssh -L
   <port>:127.0.0.1:<port> <host>`, open the printed URL on the local
   machine. Keeps the automated, safe PEM import end-to-end. Fragile
   only when the callback port is unknown ahead of time (rein binds
   `:0`) or `AllowTcpForwarding no` is set. A future `--port` flag would
   make the tunnel predictable.

2. **URL-parameter prefill + manual key import (last resort).** GitHub's
   [URL-parameter registration](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-using-url-parameters)
   is a plain GET link to `settings/apps/new?contents=write&...` that
   pre-fills the New-App form with rein's exact permission set. The user
   reviews, clicks Create, downloads the private key, and imports it
   under the handling rules above. No callback, no `code`, no localhost
   — but rein does not receive the PEM automatically, so the journey
   risk above applies. This realizes the "advanced setup" fallback the
   project always intended; the import side overlaps the Stage 2
   `rein import-pem` command (Out of scope, below). **Not built in CP5.**

Anti-pattern (rejected after research): pointing `redirect_url` at an
unreachable loopback and having the user copy the `code` out of the
failed-load address bar. It relies on undefined browser behavior and an
unconfirmed assumption that GitHub does not restrict `redirect_url` to
reachable hosts; no shipping tool does this for App *creation*. Do not
ship it.

## Files written by CP5

| Path | Mode | Content |
|---|---|---|
| `~/.config/rein/primary.pem` | 0600 | Primary App private key (PEM as received) |
| `~/.config/rein/audit.pem` | 0600 | Audit App private key (PEM as received), absent if `--skip-audit` |
| `~/.config/rein/state.json` | 0600 | `{phase, primary:{slug,id,client_id,installation_id?,key_fingerprint,created_at}, audit:{...}}` |

`installation_id` is left empty in CP5 (no install-polling); CP1's
existing session-yaml + env-var path continues to supply it manually
until Stage 2 lands.

## Keystore interface — defined now, file-backed only

Per research §5.2 ("required from day one"): add a tiny interface so
later phases can swap in biometric or HW-key backends without changing
callers. Implemented in CP5 as a thin file-backed type.

```go
package keystore

type Keystore interface {
    Get(name string) ([]byte, error)
    Set(name string, data []byte) error
    Delete(name string) error
    Fingerprint(name string) (string, error)
}
```

Token-minting code paths take `Keystore`, never raw `[]byte` or
`os.File`. Phase 1's daemon swaps in a memory-cached backend; Phase 1/2
biometric swaps in LAContext+age. Zero caller churn.

`Keystore.Get(name)` MUST verify `Stat.Uid == os.Getuid()` and
`mode&0o077 == 0` on read (research §4.3). A PEM that's been re-permed
loose or chown'd by another UID is refused with a clear error —
mirrors the SSH/age model and is the file-backed analogue of the
biometric prompt the Phase 1/2 backend will use.

## Wire-up into the existing init

```
runInit(args):
  parse flags  (existing CP1-CP3 flags + --skip-audit + --resume)
  state, err := readInitState()
  if err == nil && state.Phase == "audit_done" && !forceReinit:
      print "already initialized; use --force to redo"; goto scaffolding
  if needs_manifest_flow(state, env):
      runManifestFlow(state)        // §Flow above; updates state.json
  runLocalScaffolding()             // CP1 work, unchanged
  runShellAlias()                   // CP3 work, unchanged
```

`needs_manifest_flow` is true when:
- `state.json` is missing, OR
- `state.json` phase is `<` `audit_done` and `--resume` was passed (or
  is implicit by env-var absence), OR
- `--force` was passed.

### Env-var bridge — explicit state-transition rules

The Phase 0 dev workflow uses `REIN_APP_*` env vars pointing at a
manually-created App. Phase 0.5 introduces `state.json`. Three
transition cases the implementation must handle deterministically:

| state.json | REIN_APP_* env | Behavior |
|---|---|---|
| absent | set & validates | "Managed externally via env" — skip manifest flow; write a marker state.json (`phase: managed_externally, source: env, primary: {client_id, installation_id}`) so subsequent doctor runs and re-runs are coherent. Print: "Detected Phase 0-style env vars; skipping manifest flow. Re-run with `--force` to create fresh Apps via the manifest flow." |
| absent | absent | Run the manifest flow (the common new-user path). |
| present (`phase != managed_externally`) | set | Env vars OVERRIDE state.json at runtime (matches Phase 0 precedence; minimizes surprise for existing devs). On mismatch (`client_id` or `installation_id` differ from state.json), print a one-line WARN on every invocation: "REIN_APP_CLIENT_ID does not match state.json's primary App; env is taking precedence. Run `rein init --force` to reconcile." Continue. |
| present (`phase == audit_done` or `primary_done`) | absent | Use state.json. This is the post-manifest-flow steady state. |
| present (`phase == managed_externally`) | absent | The env vars that justified the marker are gone. Refuse to proceed silently. Print: "state.json says env-managed but REIN_APP_* are not set. Re-source ./dev-env, OR run `rein init --force` to switch to the manifest-flow path." Exit non-zero. |
| present (any phase) | set, validates, MATCHES | No warn; quiet steady state. |

Rule of thumb: env vars are the authoritative runtime source when
present; state.json is the persistent record of "what was created and
how." Mismatch warns but doesn't break — the operator gets one line
per invocation directing them to `--force` if they meant to converge.

## Open questions

None identified that block CP5 implementation. The keystore-interface
decision and the multi-machine `--import` deferral are explicit
decisions, not open questions. If the reviewer surfaces something new,
escalate before CP5 starts.

## CP5 watch-items (not design changes; explicit smoke-test additions)

- **Stateless installation-token format rollout.** Research §11
  describes GitHub's mid-May–late-June 2026 rollout of `ghs_APPID_JWT`-
  shaped opaque tokens. CP5's implementation correctly treats tokens
  opaquely via `jferrl/go-githubauth`, so the format change should be
  transparent. CP5 acceptance includes one manual smoke-test step:
  end-to-end mint + clone + push against a throwaway repo with a
  fresh-format token observed in the proxy / helper log. If the test
  fails, the issue is in `go-githubauth`, not in `rein`'s manifest
  flow — escalate upstream and pin a working version.

## Out of scope for CP5 (research → followups)

- `rein init --import <slug>` multi-machine flow (research §9.4) →
  Stage 2 followup, file an issue.
- `rein import-pem --app {primary|audit} <file>` to automate the
  disk-write-failure recovery path (currently a manual `cp` + `chmod`
  + re-run) → Stage 2 polish, file an issue.
- `rein init --wait-for-install` polling (research §8.6) → Stage 2.
- `rein machines list / revoke` (research §9.4) → Stage 2.
- `rein status` (research Stage 2 item 8) → Stage 2.
- `--org <name>` flag (research §1.7) → followup; CP5 defaults to
  user-account path.
- LAContext + age biometric backend (research §5.4) → Phase 1/2.
- YubiKey PIV backend (research §5.6) → Phase 1/2.
- Stateless installation-token format (research §11 watch-item) →
  monitor; jferrl/go-githubauth handles tokens opaquely.

File a single tracking issue per item before CP5 lands so they don't
get lost.
