# nono git-push spike — findings + real-box runbook

**Status:** IN PROGRESS. Local transport measurement DONE (2026-07-18) in an
ephemeral container; real-box validation (real github.com, nono's suggested
profiles, Landlock containment, approval flow) STAGED below, not yet run.

**Why this spike exists.** We are evaluating whether `rein` should stop being
its own sandbox+proxy and instead become the **per-issue, human-approved GitHub
App credential issuer that plugs into `nono`** (via nono's `cmd://`
credential-capture hook), ceding sandbox + injection to nono. Every research
pass favored that split EXCEPT one open gate: can nono's proxy actually carry an
arbitrary **`git push`** (the capability rein's CP1 spike existed to prove for
its own MITM)? This spike answers the transport half of that gate.

## Environment the local run happened in (scoping caveats)

x86_64, kernel 6.18.5, **`CONFIG_SECURITY_LANDLOCK` NOT set**, egress restricted
to package registries + one scoped GitHub repo, **no rein App key present**.
Consequences:
- nono's *sandbox* (Landlock) could not be exercised here — only its **userspace
  proxy** (`nono proxy`), which runs fine without Landlock. So this spike tested
  **transport + injection only**, never containment.
- Target was a **local `git-http-backend`** (git smart-HTTP framing is identical
  to GitHub's), not real github.com. Faithful for transport; still worth
  confirming against real GitHub (runbook below).
- nono installed from crates.io: `cargo install nono-cli` (v0.68.0) after
  `rustup update stable` (needs rustc >= 1.95).

## What was MEASURED (not inferred)

Setup: `nono proxy --profile spike-profile.json --pass … --proxy-ca-cert <EC
P-256 CA>`, a custom_credential service (upstream = local HTTPS git server,
`credential_key: env://SPIKE_TOKEN`, `credential_format: "Basic {}"`), git
pointed at nono as its HTTPS proxy and trusting nono's intercept CA. Note nono's
TLS stack (`ring`) REQUIRES an **EC P-256** CA key (an RSA CA → "WrongAlgorithm"
exit), and nono needs `SSL_CERT_FILE` pointing at a bundle that trusts the
upstream, or it 502s.

| Push through nono (TLS-intercept + inject) | Framing | Result |
|---|---|---|
| small (KB) | Content-Length | **lands**; `Authorization: Basic <x-access-token>` injected onto the `git-receive-pack` POST |
| 10 MiB | **chunked** (`http.postBuffer=1024`) | **HANGS** (timeout, no error logged) |
| 20 MiB | **chunked** | **HANGS** |
| 10 MiB | Content-Length (`http.postBuffer=50MiB`) | **lands** |
| 20 MiB | Content-Length (`http.postBuffer=50MiB`) | **lands** — *over* any 16 MiB "cap" |

Controls: the SAME 10/20 MiB chunked pushes land **directly** (no nono), so the
rig is sound and nono is the differentiator.

### Conclusion (transport)

- nono's proxy **DOES inject credentials into git** (onto the receive-pack POST) —
  the injection mechanism works.
- nono's proxy **stalls on `Transfer-Encoding: chunked` request bodies** — it is
  **chunked encoding, not size** (20 MiB Content-Length sailed through; 10 MiB
  chunked hung). Most likely a missing `Expect: 100-continue` / streaming
  request-body relay step — the exact relay hygiene rein's CP1 recipe already
  implements. The failure is a **silent deadlock** (no nono log line), which is
  worse than a clean 413.
- **git sends every push above `http.postBuffer` (1 MiB default) as chunked.** So
  out of the box, essentially **every real `git push` > 1 MiB hangs through
  nono's injecting proxy.** Only sub-1 MiB (Content-Length) pushes work.

## Real-box confirmation (2026-07-18, aarch64 + Landlock, real nono 0.68.0)

Re-ran on Tom's dev VM (aarch64, `CONFIG_SECURITY_LANDLOCK=y`, nono installed
from `nono.sh/install.sh`, still against a local `git-http-backend`). Result is
the same AND sharper — it exposes a second limit the container masked:

| Push through nono (intercept + inject) | Framing | Container | Real box |
|---|---|---|---|
| small (KB) | Content-Length | lands ✓ inj | **lands ✓ inj** |
| 20 MiB | chunked | HANGS | **HANGS** (silent) |
| 20 MiB | Content-Length | lands | **HTTP 413** (Request Entity Too Large) |

So real nono has **two** failure modes, not one: chunked request bodies **hang**,
*and* Content-Length bodies **> 16 MiB get a hard 413** (`MAX_REQUEST_BODY`). The
only pushes that survive the injecting proxy are **Content-Length AND < 16 MiB**.
Because git chunks every push > `http.postBuffer` (1 MiB default), a working push
requires forcing `http.postBuffer` high enough to send Content-Length *and*
staying under 16 MiB. **Any push ≥ 16 MiB is impossible through the injecting
path.** The 413/hang occur in nono's request handling before bytes reach the
upstream, so this is **upstream-agnostic** — real github.com will behave the same.
This kills the "container artifact" doubt: the finding holds on real hardware,
real kernel, real nono. Injection itself works throughout (verified `Basic
x-access-token` on the receive-pack POST).

**Consequence for option (d):** the `postBuffer` stopgap is bounded to pushes
**< 16 MiB**; it cannot carry a larger push at all.

## Correction / retraction (intellectual honesty)

An earlier turn claimed "nono routes git push through `gh`." **Retracted — the
text does not support it.** Verbatim, nono's README: git "only gets the repo,
trusted Git config files, and the Git object store" (no credential in that
example); and there is **NO** mention of `git push`, pushing code, or GitHub
writes via `gh` (the only `gh` example is read-only `issue list` / `issue view`).
So nono documents **no** git-push write path and **no** gh-write path — an
*absence*, which I wrongly narrativized into a routing decision. The transport
conclusion above rests solely on the measurement, not on any claim about nono's
intent.

## The four options to make git push work under nono

- **(a) nono fixes chunked / `Expect: 100-continue` handling upstream** — the
  proper fix (stream request bodies; answer `100 Continue`). Tractable, nono
  moves fast, but it's PR-and-wait on someone else's project. We have a clean
  repro to file.
- **(b) rein keeps a minimal git relay for the write path** — rein's CP1/CP2 MITM
  already implements the full recipe; route only `git push` through it, cede
  everything else to nono. The part we hoped to delete is the part nono can't
  replace → simplification is PARTIAL but real.
- **(c) route writes through `gh` / the Git Data API** (create blobs→tree→
  commit→ref) — Content-Length JSON, injects fine. This is a **proposal, NOT
  nono's documented answer.** Clean for issue/PR writes; awkward-to-unworkable for
  real code pushes.
- **(d) force Content-Length via large `http.postBuffer`** — proven stopgap
  (10/20 MiB landed), but buffers the whole pack in RAM and must exceed every
  push. Demo/dogfood only.

**Recommendation:** hedge with **(a) + (b)** — file the upstream chunked/`Expect`
issue AND keep rein's git relay as the fallback, so `git push` works regardless
of if/when nono fixes it.

## NOT YET VERIFIED — needs a real box (this is the runbook)

Prereqs: Linux with `CONFIG_SECURITY_LANDLOCK=y` (kernel >= 5.13; verify
`grep LANDLOCK /boot/config-$(uname -r)`), unrestricted egress to github.com +
registries, `gh` logged in (or a rein App key), and cargo/nono.
**Hard-constraint #1: throwaway repos ONLY.**

1. **Install + verify sandbox.** `curl -fsSL https://nono.sh/install.sh | sh` (or
   `cargo install nono-cli`). `nono setup --check-only` must report Landlock
   supported (unlike the spike box).
2. **Use nono's OWN suggested profiles** (the user's ask — test nono's blessed
   config, not our hand-rolled one). `nono setup --profiles`, `nono search`,
   `nono list`; identify the profile/pack that wires GitHub creds + git (likely a
   `keyring://gh` credential per the README). `nono pull <pack>` if from registry.
3. **Real sample repo (throwaway).** `gh repo create <throwaway> --private
   --clone`; commit a small file AND a >1 MiB blob (e.g. `head -c 20971520
   /dev/urandom > big.bin`) so the push chunks by default.
4. **Test A — nono's suggested profile, real git push to real GitHub.**
   `nono run --profile <github-profile> -- git push` (or `nono shell` then push).
   OBSERVE: does the >1 MiB (chunked) push to github.com succeed or hang? This is
   the decisive real-world test — nono's own config, real GitHub. If it hangs,
   the local finding generalizes; if it works, nono's blessed path differs from
   our `custom_credentials` reverse-proxy path and we learn how.
5. **Test B — deterministic local repro.** Run `docs/nono-spike/local-repro.sh`
   on the real box to confirm the chunked hang reproduces here too (isolates
   nono from the container).
6. **Test C — Landlock containment.** Run the containment probe (see
   `docs/containment-probe-harness.md`) or `controlplaneio/sandbox-probe` inside
   `nono run` vs on the host; diff. First real check of nono's *sandbox*.
7. **Test D — `cmd://` + host-side approval.** Wire a `custom_credentials`
   entry with `credential_key: cmd://rein-mint` whose helper prompts on the host
   tty (blocks) before returning a token; confirm the sandboxed child has NO tty
   (`/dev/tty` unopenable) while the host prompt drives the approval. Proves
   rein's write-approval popup survives under nono.
8. Record results back into this doc (fill a "real-box results" section).

## Harness (committed, so it survives the machine switch)

`docs/nono-spike/`:
- `git_http_server.py` — minimal `git-http-backend` server (HTTP or HTTPS),
  logs injected `Authorization` + framing.
- `echo_server.py` — byte-counting POST sink (raw large-body test).
- `spike-profile.json` — the nono profile used locally (custom_credential →
  local HTTPS upstream, `env://` credential, Basic inject).
- `local-repro.sh` — end-to-end local reproduction of the chunked-hang result
  (EC P-256 CA, `SSL_CERT_FILE`, small/chunked/Content-Length push matrix).

## Real-box results (2026-07-18, Tom's dev VM: aarch64, Landlock, real nono 0.68.0)

Three things the container couldn't test are now done on real hardware.

### Real github.com push (not just a local server)
Injected the PAT as `Basic` via a nono `custom_credentials` route intercepting
`github.com`, pushed to the throwaway `TomHennen/agentcreds-validation-a`:
- **small push → LANDED on real github.com** (injection brokers a real push).
- **20 MiB push → FAILED** — `send-pack: unexpected disconnect ... remote hung
  up`. Against real GitHub the chunked upload disconnects (rather than hanging
  forever), but same outcome: it does not land. Confirms the finding on the real
  injection path.

### Containment — the positive that justifies adopting nono
Probe inside `nono run --profile nolabs-ai/claude` (readability only, no content):
every credential read is **blocked by Landlock** — `~/.config/gh/hosts.yml`,
`~/.ssh/id_ed25519`, **`~/.config/rein-credentials/app.pem` (the real App key)**,
`~/.bash_history` all fail with "Permission denied" on open (`deny_credentials`
works). Write to `/etc` denied. **Caveat:** nono's stock Claude profile leaves
**egress wide open** (`example.com` → 200) — no allowlist — so a prompt-injected
agent could exfiltrate anywhere. rein would ship a tightened profile.

### The `cmd://` seam — rein's integration point works, host-side, zero nono changes
A rein-shaped `cmd://` capture helper, triggered by a request through nono:
```
ctx: HOST=github.com PATH=/TomHennen/agentcreds-validation-a/info/refs?service=git-upload-pack METHOD=GET SESSION=ea2cc96221ca29b4
host-side proof (read app.pem which sandbox CANNOT)? YES-HOST-SIDE
approval channel /dev/tty? present
```
So nono invokes rein's helper **host-side** (it can read the App key the
sandboxed agent cannot), hands it **per-request context including the repo path**,
method, and session id, gives it **`/dev/tty`** for a human approval prompt the
agent can't reach, and injects whatever token it returns. Config: top-level
`credential_capture: { mint: { command: [...] } }` + a `custom_credentials` entry
with `credential_key: "cmd://mint"` and a required `env_var`. This is rein's
issuance + per-repo-scope + human-approval trifecta dropping into nono natively.

## Source confirmation + repo-scoping (from nono's own code, commit 23d93fc)

- The 413 is **verbatim + intentional**: `reverse.rs:37` `const MAX_REQUEST_BODY:
  usize = 16 * 1024 * 1024;` → `:1539` sends `413` when `content_length >
  MAX_REQUEST_BODY`. A deliberate DoS guard (issue #554 lists "enforce upload
  size limits"), **not a tracked bug**. So fix-option (a) "wait for upstream" is
  weak — it's by design.
- **Chunked request bodies are never decoded** (no `Content-Length` → empty body
  returned); chunked decode exists only for responses. **No `Expect:
  100-continue`** handling anywhere. All three request handlers funnel through
  one buffering `read_request_body` — no streaming request path.
- **Not filed.** No issue/PR/discussion raises git push. Nearest analog: **#1433**
  (OPEN) — intercept path hangs on Codex websocket upgrades (same "intercept
  assumes buffered request→response, hangs on streaming" weakness).
- **Repo-scoping requires interception (mutually exclusive with large push).**
  nono can scope git by repo via path-glob credential routes
  (`route.rs:1624`, `/org/**` matched against `/org/repo.git/git-receive-pack`)
  or `endpoint_rules` — but only on the **intercepted** path, which routes through
  the same 16 MiB/chunked-incapable body reader. Plain host-allowlist tunnels
  CONNECT **without** interception (proxy blind → no repo distinction, no
  injection). So:

  | nono config for github | Repo-scoped? | Large `git push`? |
  |---|---|---|
  | path-glob route / endpoint_rules (intercept) | **yes** | **no** (413/hang) |
  | plain host allowlist (tunnel) | no | **yes** (but no injection — git uses own creds) |

  `git clone`/`fetch` survive either way: the `upload-pack` **request** is small;
  the packfile arrives in the **response**, which nono streams uncapped. Only the
  **push request body** trips the cap.

## Design: how rein fits into nono WITHOUT changing nono

Division of labor, all via nono's existing config surface (zero nono source
changes):

- **nono owns:** the Landlock sandbox (proven: `deny_credentials` hides the App
  key + host creds from the agent), the `cmd://` hook, L7 repo-scoping for the
  REST path, and egress enforcement.
- **rein owns (its differentiators), plugged into nono's seams:**
  - **`cmd://` credential authority** — mints per-issue, per-repo, short-lived App
    installation tokens host-side (it can reach the App key), returns them for
    nono to inject. Native fit.
  - **host-side approval** — the same helper prompts on `/dev/tty` (agent has
    none) before minting a write token. Native fit.
  - **a tightened nono profile** — restricts `allow_domain` (restoring rein's
    strict-egress property the stock profile lacks). Config only.
- **The one hard seam — `git push`.** Because repo-scoped injection and large push
  are mutually exclusive in nono, rein routes per-path, still without changing
  nono:
  - REST/`gh`/api.github.com writes + `git clone`/`fetch` → through nono's
    intercept + rein's `cmd://` injection (works today).
  - **large `git push`** → point the sandboxed git's `http.proxy` at **rein's own
    relay** (a localhost port allow-listed via the profile's `open_port`, with
    direct github git egress denied so git can't bypass it). rein's relay does the
    chunked/streaming inject it already implements (CP1). nono still sandboxes
    everything; the relay is the one rein component nono cannot replace.

Net: rein sheds srt + its own sandbox composition + CA/TLS for the REST path, and
keeps only its git-push relay — a large simplification — while nono provides
containment and rein keeps issuance + per-issue scope + approval. This is the
"rein becomes the credential authority for nono" split, achievable with zero nono
changes and one config-wired rein relay for the push path.
