# Design-conformance audit — rein (Phase 1, CP1–CP4.5)

**Date:** 2026-07-10
**Design of record:** `docs/design.md` (end-state) reconciled against `PLAN-1.md` (CP1–CP4.5) + `docs/phase1-design.md` §8 + the dated PLAN-1 Notes.
**Method:** multi-agent audit — 346 normative claims extracted from 11 design sections, each mapped to code and classified, each verdict adversarially re-verified by an independent agent, then every non-conformant verdict phase-reconciled against the current-checkpoint scope. Read-only against GitHub; throwaway-only; no live/e2e/pexpect suites executed.

## Coverage & caveats (read first)

- **Numbers:** 346 claims → 308 rows carried to verdict; **287 adversarially verified**, 21 remained classifier-only (verifier hit the model session limit twice — the 21 are `P-appr-5.6/6/7` positioning/reuse claims, mostly conformant or in-process-daemon divergences already covered by patterns below). 193 conformant, 115 non-conformant before reconciliation.
- **Stale checkout:** the audit ran against worktree HEAD `eaba53a`, which is **14 commits behind `origin/main` (`5a46e6b`)**. Every finding below was **re-verified against `origin/main`**. Two whole finding classes are *already fixed on main* and are NOT reported as live: **#36** (approval prompt collided with the agent TUI — fixed by `4b05a8f`/PR #37, popup-first under `$TMUX`) and **#35** (agent-declared/human-confirmed issue scoping — decisions recorded in PR #38, init scaffolding in #39/#40/#42).
- **Reconciliation is the point.** Measured against `design.md`'s *end-state*, 115 rows look non-conformant. Most are not drift — they are explicitly deferred later-tracks (`phase1-design.md` §8) or documented v1 simplifications (PLAN-1 Notes). Only the findings in §1–§2 survived phase-reconciliation as genuine, in-phase, live-on-main.

---

## 1. Genuine divergences (in-phase, verified live on `origin/main`)

Ranked by severity. Each is a real gap against what CP1–CP4.5 committed to — not a deferred feature.

### D1 — HIGH — Credential helper returns an *error with empty stdout* on a bad session file (TM-G8 / hard-constraint #2 violation)
- **Where:** `cmd/rein/main.go:257-260` — `sess, _, err := session.LoadOrFallback(...); if err != nil { return err }`, sitting **between** `ResolveApp` (`:234`) and `runCredentialHelperWithConfig` (`:266`). `main.go:145-148` turns that error into `os.Exit(1)` with empty stdout on a github.com `get`.
- **Why it's a bug:** the function's own doc comment (`main.go:278-280`) states *"There must be NO error return between ResolveApp and this call on the github.com path"* — the code violates its own invariant one line above the comment. `session.LoadOrFallback` hard-errors on a malformed/invalid `dev-session.yaml`, a `REIN_SESSION_FILE` pointing at a missing file, or no-session-no-fallback (`internal/session/session.go:238-251`) — i.e. the literal "no session active" state that hard-constraint #2 and design.md:765 say must yield the placeholder.
- **Threat:** this is exactly the TM-G8 self-remediation scenario (design.md:699,762; validated §12.1): a same-uid agent that rewrites/corrupts the agent-writable session file in direct mode gets a *failed* helper (empty credential), re-enabling the `gh auth setup-git` displacement TM-G8 exists to prevent.
- **Verified:** reproduced live by the verifier (valid `REIN_APP_*`, broken YAML at the default session path, piped a github.com `get` → exit 1, empty stdout). Confirmed present on `origin/main`.
- **Also same-class, pre-broker, uncovered:** `openLog` / `config.StateDir` failures (`main.go:223,229`) exit non-zero before the broker too.
- **Test tier:** Go unit — feed `runCredentialHelper` a github.com `get` with a malformed/missing session and assert a non-empty credential block on stdout, exit 0. `cmd/rein/main_test.go` today covers only the `InstallationID==0` mint-placeholder path.
- **Matrix rows:** `D-threats-5.1-TMG8-HELPER-ALWAYS-RETURNS-CRED`, `D-tmg-G8-helper-always-returns`, `D-tmg-G8-placeholder-on-refusal`, `D-nonprotect-7.5-HELPER-NEVER-EMPTY-OR-ERROR` (four independent verifiers converged on this).

### D2 — HIGH — The on-disk Secret Service keyring store is readable in-sandbox
- **Where:** `cmd/rein/run_sandboxed.go` `credentialDenyReadPaths` denies the D-Bus / Secret-Service **socket** and gh/ssh/netrc/git-credential files, but **not** the on-disk keyring database `$XDG_DATA_HOME/keyrings` (default `~/.local/share/keyrings/login.keyring`). srt's read-only root bind therefore leaves it readable inside the sandbox.
- **Why it's a bug:** `phase1-design.md:54-57` lists keyrings absolutely among ambient creds that must be hidden, and the banner literally prints *"keyring/agent sockets are hidden"* (`run_sandboxed.go:686`) — but only the socket is hidden. A developer using git's libsecret credential helper keeps their GitHub token in that DB; a prompt-injected agent can `cat` it and exfiltrate over operator-opened egress, and a passwordless auto-unlock keyring is offline-decryptable.
- **Fix:** add `$XDG_DATA_HOME/keyrings` (resolved, with the `~/.local/share` default) to `credentialDenyReadPaths`, mirroring the existing gh/gpg relocation handling.
- **Test tier:** Go unit (assert the resolved keyrings path is in the deny-read set) + the gated real-srt e2e (seed a keyring file, assert empty in-sandbox).
- **Matrix row:** `P-req-NO-AMBIENT-DEV-CREDS` (classifier said conformant; verifier overturned with the missed store-file path). Confirmed live on `origin/main`.

### D3 — MEDIUM-HIGH — CA bundle fail-*open* when the system trust store is empty/garbage
- **Where:** `internal/srt/cabundle.go` `BuildCABundle` validates that `reinCAPEM` is non-empty but **never validates the system bytes** it reads — `buf.Write(sys)` runs even when `sys` is empty. `SystemCAPath` only `os.Stat`s the candidates.
- **Why it's a bug:** an existing-but-empty or corrupt `/etc/ssl/certs/ca-certificates.crt` (broken container image, botched `update-ca-certificates`) passes, and the emitted bundle contains **only the rein CA**; `run_sandboxed.go` writes it and launches silently. That both breaks every allowed non-GitHub HTTPS destination in-sandbox (the agent's own API endpoint included) and fails *open* on the design's absolute guarantee (`phase1-design.md:202-206`) — contradicting the code's own rationale comment. The existing `containsPEMCertificate` helper is applied to `$SSL_CERT_FILE` but not to `sys`.
- **Fix:** apply `containsPEMCertificate`/non-empty validation to `sys` in `BuildCABundle`; fail closed if the system store has no certs.
- **Test tier:** Go unit (empty & garbage system-bytes → error).
- **Matrix row:** `P-hosts-4.4-SSLCERTFILE-REPLACES-ROOTS`. Confirmed live on `origin/main`.

### D4 — MEDIUM — Multi-repo session: install-404 on a non-first repo → placeholder *inside* the agent
- **Where:** `cmd/rein/run.go:322-323` `resolveAndCacheInstallID` probes only `sess.Repos[0]`; no launch code iterates the rest, and mints are lazy closures scoped to **all** session repos.
- **Why it's a bug:** `session.Validate` explicitly supports same-owner multi-repo sessions (`internal/session/session.go:161-190`). If the installation's selected repos exclude a non-first session repo — the exact 404 condition `GET /repos/{owner}/{repo}/installation` reports — launch **succeeds** and every mint for that repo fails as `rein-placeholder-mint-failed` *inside the agent*: precisely the TM-G8-placeholder-in-agent outcome `design.md:581` says must be a loud launch-time error instead.
- **Fix:** resolve/verify installation coverage for **every** `sess.Repos` entry pre-launch; fail loud on any 404.
- **Test tier:** Go unit (multi-repo session, stub lookup 404s on `Repos[1]` → launch refused).
- **Matrix row:** `D-arch-4.2.4-INSTALL-404-LOUD-ERROR` (single-repo path conforms and is tested). Confirmed live on `origin/main`.

### D5 — MEDIUM — Approval records are forgeable by a same-uid host process (unkeyed signature)
- **Where:** `internal/approvals/approvals.go:155-164` `SignatureOf` is a plain **unkeyed** `sha256` over `id/role/repos/issue`; `Valid` is bare equality; `Read` does no provenance/ownership check. `grant.ObtainApproval` Layer 1 skips the `/dev/tty` prompt whenever a valid on-disk record exists.
- **Why it's a (bounded) bug:** a same-uid **non-sandbox** process can read `REIN_RUN_ID` (`/proc/<pid>/environ`) and the runs/session snapshot, recompute the signature, write a forged `approvals/<run-id>.json`, and make its first socket write ride Layer 1 with **no human prompt**. The in-sandbox agent is structurally blocked (records are deny-read + the issue number is hidden), but `run_sandboxed.go:473-476` scopes its no-forge claim to "an in-sandbox process," leaving the same-uid host adversary — which §5.3 names for socket reach — unaddressed.
- **Nuance:** §5.3 partly concedes the same-uid host residual, so this is a hardening gap, not a total break. Related to open issue **#12** (nonce-via-tty).
- **Fix:** key the signature (HMAC with a per-run secret delivered off the agent-readable path), or bind the record to the socket-holding pid.
- **Test tier:** Go unit (forge a record with the recomputed sha256, assert it is rejected once the signature is keyed).
- **Matrix row:** `P-dec-5.3-FIRST-WRITE-STILL-APPROVAL-GATED`. Confirmed live on `origin/main`.

### D6 — MEDIUM — Re-exposure guard is purely lexical (no symlink resolution)
- **Where:** `internal/srt/config.go` `Validate` rejects `denyRead`-under-`allowWrite` overlaps lexically; Build "does not resolve symlinks" (comment `config.go:82-83`), and `run_sandboxed.go:153` uses `filepath.Abs` only (no `EvalSymlinks`, unlike the socket-placement check in `internal/proxy/placement.go:67`).
- **Why it's a bug:** a symlinked `REIN_SANDBOX_WORKDIR` pointing into `$HOME` evades the overlap guard, leaving non-re-exposure of credential stores resting on srt's unverified mount ordering. Secondary: the *designed* widening vector (a read-allowlist, `phase1-design.md:148-151`) does not exist — widening is via write-binds (`REIN_SANDBOX_WORKDIR`/`ExtraAllowWrite`), strictly stronger exposure than the design's read-only widening.
- **Fix:** `EvalSymlinks` the widening paths before the overlap check (reuse the placement.go approach).
- **Test tier:** Go unit (symlinked workdir into a denied path → Validate rejects).
- **Matrix row:** `P-hide-FS-WIDENING-NEVER-REEXPOSES`. Confirmed live on `origin/main`.

---

## 2. Genuine untested invariants (in-phase, code conforms, no honest test)

These are real, in-scope invariants whose **code is correct but has no test that would catch a regression** — the #36 lesson class. Confirmed absent on `origin/main`.

| Invariant | Where | Why untested | Test tier |
|---|---|---|---|
| Keystore `O_NOFOLLOW` symlink-swap refusal (hard-constraint #6) | `internal/keystore/file.go:59` | No test symlinks a PEM and asserts refusal; `file_test.go` covers only loose-mode/wrong-uid | **Go unit** (symlink a PEM → `Get` fails closed, not `ErrNotExist`) |
| Scope-ceiling enforced at mint (`Repositories`+`Permissions` in the token request) | `internal/githubapp/client.go:209-236` | No fake token endpoint asserts the request body; a regression dropping `opts` passes every test | **Go unit** (httptest `/access_tokens` fake, assert the JSON carries repos+perms) |
| All private-key reads route through `keystore.Get` (hard-constraint #6) | package-wide | No lint/arch check; a future direct `os.ReadFile` of a PEM would compile & pass | **Go unit** (source-scan arch test / `depguard`) |
| ALPN pinned to `http/1.1` | `internal/proxy/proxy.go:248-251` | Every test client pins http/1.1 itself; deleting the pin leaves the suite green | **Go unit** (offer `["h2","http/1.1"]`, assert `NegotiatedProtocol=="http/1.1"`) |
| Audit log lives under the deny-read `stateDir` | `run_sandboxed.go:242-247,572` | Tests pin `stateDir`-in-deny-set but nothing pins that the audit path is *under* it | **Go unit** (assert audit path ∈ a deny-read path) |
| rein-gh denial sets `GH_TOKEN=rein-placeholder-denied` | `cmd/rein-gh/main.go:199-213` | No test asserts the denial-path env | **Go unit** |
| git write-intent `/proc` ancestor-walk fallback | `cmd/rein/proctree_linux.go` | No `proctree_*_test.go`; `TestDetectWrite` injects stubs and bypasses it | **Go unit** (Linux) |
| rein-gh two-tier mint + exit-revoke + approval gate | `cmd/rein-gh/main.go:271-354` | Only the classifier table is tested; the write flow needs a tty | **Go unit** (read/mint routing) + **pexpect** (approval leg) |

---

## 3. Design-doc reconciliation needed (code is fine; the design was never amended)

Not code bugs — the implementation made a deliberate, reasonable choice that the design of record still contradicts. Each should be recorded as a design correction (the PLAN-1 Notes pattern) so the next audit does not re-flag it.

- **CDN hosts bypass the proxy entirely.** Only the 3 credentialed hosts route to rein's socket; `codeload`/`objects`/`raw.githubusercontent.com` get direct srt TLS (to avoid injecting on pre-signed URLs, gap #6). Consequence: 3 of 6 GitHub hosts escape rein's audit/policy plane, and the proxy's `classPassthrough` redirect-classification arm is dead code. `design.md`/`phase1-design.md:186-189` still say CDN redirects "arrive at the proxy as fresh connections." (`P-req-ALL-GH-TRAFFIC-VIA-REIN-PROXY`)
- **Extra-egress makes "proxy is the only route out" literally false.** CP4.5's `allow_domains`/`REIN_ALLOW_DOMAINS` open an operator-widened egress set (recorded in PLAN-1.md:417-505) that `phase1-design.md:112-113` never mentions. (`P-req-PROXY-ONLY-EGRESS-ROUTE`)
- **Env allowlist is a documented superset.** Beyond CA vars + stub `GH_TOKEN` + PATH/locale, `env.go` also passes `HOME`/`TERM` and sets the `GIT_*` authorship vars — deliberate, non-secret, recorded in PLAN-1.md:539-541, but `phase1-design.md:159-160` was never updated. Strict-allowlist *mechanism* intact. (`P-hide-ENV-ALLOWLIST-CONTENTS`)
- **Plaintext write tokens on disk during a run.** The issue-#20 revoke ledger persists raw tokens (0600) so same-uid host malware *can* steal a real replayable token — contradicting §5.3's "no token value to steal" and the §6 table's "daemon memory (no disk)." Deliberate trade for post-SIGKILL revoke; §5.3 never caveats it. (`P-dec-5.3-NO-EXFILTRATABLE-TOKEN-VALUE`)
- **Shape-B read cache can cross sessions.** The direct-mode read-token cache is a global on-disk file with no session id/scope, served to a later session for up to ~1h — undocumented drift from "served for the session TTL." (`D-arch-4.2.5-READ-TOKEN-SESSION-TTL`)

---

## 4. Deferred / documented / tracked (the bulk — not drift)

The remaining ~90 non-conformant rows reconcile cleanly and are **not** findings:

- **Tracked by #35** (agent-declared, human-confirmed issue scoping) — ~27 rows: branch-pattern `agent/{{issue}}/{{nonce}}` extraction, session-start confirmation (fires at first write instead), issue-transfer redirect anchor, issue→repo-derived scope. Decisions recorded on `origin/main` (PR #38); the misattribution consequences are documented in a comment on #35.
- **Documented v1: in-process, not a daemon** (PLAN-1.md:610-625) — ~14 rows: `/v1/proxy/token` endpoint, `X-Proxy-Auth` bearer + per-request rotation + one-shot pipe (TM-G4), UDS-owned-by-uid, session-mgmt-via-proxy. `internal/daemon` is unwired shelf code; these are obviated by the in-process host.
- **Documented v1: plain append-only audit log, not hash-chained** (PLAN-1.md:641-651) — ~8 rows.
- **Deferred §8 later-tracks** — audit-comment writeback via the Audit App (TM-7, forensic-attribution, multi-issue cross-post), single-use + HEAD-pinned write tokens, broker-as-CA commit signing / delegation sigs, the five-role permission catalog.
- **Tracked elsewhere:** clock-skew preflight guidance is **#22** (`P-hosts-4.4-PREFLIGHT-APPARMOR-CLOCK` — AppArmor half conforms); the approval-TUI collision is **#36**, already fixed on `origin/main`.
- **`ghu_`/Device-Flow** never built — `design.md:583` records the manifest-App-JWT divergence itself.
- **README hardening recs** (`ptrace_scope=1`, separate-UID) — minor doc follow-up.

---

## 5. Misattribution subsection (from the focused probe)

A separate adversarial probe pressure-tested whether a prompt-injected/confused agent can get a wrong-but-plausible issue attributed to its writes. Full analysis is a comment on **#35**. Summary: **no gap survives phase-reconciliation as a new bug** — S1/S4/S5 are consequences of the (tracked) #35 static-`sess.Issue` model; S2 (audit writeback fails silently) is the deferred §8 writeback + the documented best-effort/fail-open-on-observability log; S3 (concurrent runs) is genuinely **prevented** by per-run nonce file-keying. Recommendation for when #35's model lands: fetch + display the issue **title/repo** at confirm time (make it required) and extract+cross-check the push ref against the confirmed binding. A gated, human-run pexpect spec for the title-display behavior is staged (`tests/interactive/test_confirm_shows_title.py`, `expectedFailure`).

---

## 6. Recommended actions

1. Fix **D1** (TM-G8 helper error-return) — highest priority; it re-opens the exact threat TM-G8 exists to close, and it's a one-line-class fix (route the session-load failure through the placeholder minter like `ResolveApp` failure already is).
2. Fix **D2** (keyring store deny-read) and **D3** (CA fail-open) — both small, both close real holes.
3. Land the **§2 Go unit tests** (keystore `O_NOFOLLOW`, mint scope-ceiling options first — both hard-constraint-adjacent).
4. Record the **§3** items as design corrections in PLAN-1/phase1-design so they stop reading as drift.
5. Triage **D4/D5/D6** as medium hardening follow-ups.

*Filed as individual issues: **D1 → #45**, **D2 → #46**, **D3 → #47**. D4–D6 and the §3 design-doc reconciliations are tracked in this report pending triage.*

*Tests shipped in the accompanying PR: the keystore `O_NOFOLLOW` symlink-swap Go test (`internal/keystore/nofollow_test.go`) and the gated misattribution pexpect spec (`tests/interactive/test_confirm_shows_title.py`). The mint scope-ceiling test is **specified but not shipped**: it needs a small testability seam (thread `githubauth.WithHTTPClient`/`WithEnterpriseURL` into `client.mint`) so a test can point the installation-token call at an httptest server and assert the request carries `Repositories`+`Permissions`. That touches the security-critical mint path, so it is left for review rather than added unilaterally.*
