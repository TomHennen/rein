# Issue #35 design — agent-declared, human-confirmed issue scoping (declaration-first)

**Status:** design of record, approved by Tom (merged 2026-07-11, PR #56);
**IMPLEMENTED 2026-07-11** on the issue-35 implementation branch (see
PLAN-1.md Notes 2026-07-11 for the build record).

**Settled by Tom (recorded, not revisited):**
- 2026-07-08 — decisions A–F on issue #35: agent-declared at runtime, NOT
  pre-configured (A); approval run-scoped, no per-operation policy (B); issue
  encoded in the branch, strict matching (C); issue is the audit/attribution
  anchor, not the credential boundary — tokens stay repo-scoped (D); fetched
  title + home repo display at confirm time is REQUIRED (E); onboarding drops
  the issue prompt (F).
- 2026-07-11 — **declaration-first direction**: the human prompt fires at an
  explicit agent declaration (`rein declare <n>`), and **no write-capable
  token is minted before approval**. Supersedes the earlier push-ref-first
  shape (Appendix A).
- 2026-07-11 — **Form A confirmation**: the human types the **displayed**
  issue number; fetched title + state + home repo always shown. Rationale:
  per decision D wrong-issue attribution today has mild consequences; the
  load-bearing control is the displayed fetched title. (Form C — number
  withheld as a knowledge check — is noted in §4 as a future option if
  attribution gains teeth via §8 writeback.)
- 2026-07-11 — **one binding model across BOTH modes**: `sess.Issue` is
  retired everywhere and `rein declare` works in direct mode too (Tom's
  pushback on direct mode keeping the static model: two models in two modes,
  a dead config field, worse UX). Supersedes the earlier "direct mode
  unchanged" line here and the session-scope-ux-mocks §6 bullets that
  deferred direct-mode declare and kept static `sess.Issue`.

Scope: **both modes.** Sandboxed mode (the Phase 1 spine — in-process per
run, no daemon; PLAN-1 notes 2026-07-05) carries the full model including
push-ref verification; direct mode carries the same declare + confirm + gate
model minus the ref check (§2's stated deltas) — and is the *easier* half:
no relay exists, so `rein declare` runs as a plain CLI against the same
mode-independent grant/approval-record machinery (§3).

---

## 1. The model

The agent has no GitHub write path — in either mode — until it **declares**
which issue its work is for and a human **confirms** it:

1. Any pre-declaration write attempt is denied with an instructive error
   naming the exact next step: `rein declare <n>` (§2). Sandboxed: the proxy
   answers locally, GitHub never contacted, nothing minted. Direct: the
   helper serves the placeholder credential + the same instruction on
   stderr.
2. The agent runs `rein declare 73`. rein fetches issue #73 from the
   session repo (title, state, canonical URL), and prompts the human on
   /dev/tty (or tmux popup): title + state + home repo displayed, approve by
   typing the displayed number (§3–§4).
3. On approval, #73 joins the run's **confirmed-issue set** (run-scoped,
   decision B). All write channels now flow for the rest of the run. The
   first write-capable token is minted only after this point.
4. Every subsequent `git push` is still **verified** in sandboxed mode:
   refs must match `agent/{{issue}}/{{nonce}}` (design.md:521) and resolve
   to a confirmed issue — deny on mismatch with an agent-visible rejection
   (decision C; the ref-level half of design.md:590's push-time
   verification) (§5). Direct mode cannot see refs (C1) — §2 states that
   delta as a design fact.
5. Declaring a second issue mid-run is the design's scope-expansion prompt
   (design.md:251–263, 304): same ceremony, appends to the set.

The issue is attribution, not capability (decision D): tokens stay
repo-scoped to the session set (#10; design.md:538/555); issue numbers appear
in prompts, approval records, and audit lines — never in mint parameters.

---

## 2. One gate, every write channel

**Tom's question, answered up front: this applies to ALL writes — git push
and gh/API alike, in BOTH modes.** Sandboxed: every GitHub-bound byte
crosses the one proxy (`internal/proxy` serveInject; srt gives the sandbox
no other route, and sandboxed mode has no rein-gh shim — gh runs in-sandbox
with the stub `GH_TOKEN` and its REST + GraphQL traffic is classified and
injected at the proxy like everything else, run_sandboxed.go stubGHToken /
phase1-design §5.1). Direct: both write channels — the git credential
helper and the rein-gh shim — read the same run-scoped confirmed-issue set.

**Sandboxed mode, before approval (empty confirmed set), per channel:**

| Channel | Pre-declaration write attempt gets |
|---|---|
| `git push` (smart-HTTP handshake, `GET info/refs?service=git-receive-pack`) | Synthesized local advertisement carrying a pkt-line `ERR` — git prints `fatal: remote error: rein: writes are locked until you declare your issue. Run: rein declare <n> …` and exits cleanly (§5.3). Zero upload, zero mint, GitHub never contacted. |
| `gh` REST writes (POST/PATCH/PUT/DELETE via `api.github.com`) | Local `403` with JSON body `{"message": "rein: no issue declared for this run. Run: rein declare <n> …"}` — gh surfaces the message field to the agent. |
| `gh`/raw GraphQL mutations (`POST /graphql`, body-classified) | Same local 403 + JSON message (gh prints the HTTP error + message; a raw client sees the body directly). |
| Raw `curl`/any client to the inject hosts | Same local 403 body, read directly. |
| Reads (all channels) | Unaffected — read tier flows as today (TM-G8 untouched). |

Sandboxed coverage confirmation: git smart-HTTP, gh-REST, gh-GraphQL, and
direct-REST/curl all terminate at the one serveInject chokepoint, so the
instructive deny needs exactly two implementations (ERR pkt for the git
advertisement; 403 JSON for everything else). Nothing needs separate work.

**Direct mode, before approval — same gate, different deny channel.** There
is no proxy to synthesize errors; the deny channel is the helper's
placeholder + stderr-diag path (TM-G8, design.md:765–766 — git forwards
helper stderr to the caller; #45 tracks hardening this path's
bad-session-file edge, and it is the channel this design relies on):

| Channel (direct mode) | Pre-declaration write attempt gets |
|---|---|
| `git push` via the credential helper | The `rein-placeholder-out-of-scope` credential (never empty — TM-G8), so GitHub rejects the operation, **plus a stderr hint**: `rein: no issue declared for this run — run: rein declare <n>, approve on your terminal, then retry.` The helper no longer prompts inline (there is no `sess.Issue` to prompt about); the prompt lives at declare time. |
| `gh` writes via the rein-gh shim | Deny with `GH_TOKEN=rein-placeholder-denied` (blocks the hosts.yml fallback, design.md:767) + the same stderr hint. |
| Reads (both channels) | Unaffected, as today. |

Both direct-mode write channels are covered by the same instruction;
**neither needs separate design work** — only the gate switch (§10).

**After approval, both modes:** every write channel consults the run's
confirmed-issue set — the same per-run approval record, keyed by
`REIN_RUN_ID`, which `rein run` already exports to the wrapped child
(run.go:188) — and mints. One ceremony per scope, no second prompt per
channel or per write (decision B; design.md:283 run-scoping).

**Direct-mode deltas, stated as design facts** (pre-existing Shape B
residuals, not introduced by this design):

1. **No push-ref cross-check.** The helper is invoked at repo-URL level and
   never sees refs (correction C1, design.md:594), so neither the
   `agent/{{issue}}/{{nonce}}` convention nor the "git transport can't
   touch `main`" property below is enforceable in direct mode — an approved
   direct-mode run can push any ref, exactly as today. The binding rests on
   declare + confirmation alone, partially compensated by the repo scope
   ceiling and the helper.log audit trail. The ref check (§5) is a
   sandboxed-mode property.
2. **Shared-terminal prompt residual (#12).** In direct mode the declare
   prompt uses the same grant channel as today's write prompt, on a
   terminal the agent shares; the tty asymmetry (agent tool subprocesses
   hit ErrNoTTY) is a useful accident, not an airtight gate
   (design.md:749). #12's nonce-via-tty remains open for direct mode and
   applies to this prompt exactly as it did to the old one.

Two smaller deltas, also inherent to Shape B: pre-declaration write
attempts in direct mode DO reach GitHub (carrying the placeholder, which
GitHub rejects) — "zero GitHub-bound side effects pre-approval" is a
sandboxed-only property; and traffic bypassing the helper/shim (raw curl
with ambient network) was never mediated in direct mode — the issue #7
boundary the sandbox exists to close.

**The honest asymmetry:** the `agent/{{issue}}/{{nonce}}` ref cross-check
(§5) is **push-only** — REST/GraphQL writes carry no ref, so API writes rest
on the declared binding alone. What partially compensates: the repo scope
ceiling still bounds every API write (out-of-scope repos are refused before
any token is served, session.Contains → proxy refused-scope), the token's
permission set bounds what any write can do, and every proxied write gets an
audit row attributed to the run's confirmed issue set (§8). What does not
exist: per-API-call issue verification. That is decision D working as
intended — the issue gates attribution, the role/repo ceiling gates
capability — but it means, concretely, that a confirmed run can
`gh pr merge` or hit `PATCH /repos/{o}/{r}/git/refs/heads/main` within the
token's contents:write. The convention closes the *git transport* route to
the default branch (a push to `main` can never match), not every route.
`require_human_confirmation_on_default_branch` (design.md:520) is addressed
for pushes, not retired.

---

## 3. The declare channel (both modes)

One subcommand, two transports, one prompt/record machinery.

**Direct mode — the simple half.** `rein declare 73` runs as a plain CLI in
the wrapped agent's (or the operator's) shell: it reads `REIN_RUN_ID` from
the environment (run.go:188 exports it to the child tree), fetches the
issue title directly (same-uid process — network + keystore in hand), and
fires the **existing mode-independent grant machinery** itself
(internal/ui/grant: tty → tmux popup → other-terminal instructions),
appending to the same per-run approval record the helper/shim then read.
No relay, no endpoint, nothing new — the declare *is* the grant caller.
Invoked outside any `rein run` (no `REIN_RUN_ID`): fail with the
instruction to launch via `rein run`, mirroring grant's existing
no-run-id behavior.

**Sandboxed mode** cannot run that path in-sandbox (the approval record and
tty live outside; state dir is deny-read), so the declaration rides the
sandbox's only channel to rein — the per-run proxy socket (phase1-design
§5.3) — as a **virtual host**; the broker side then performs the identical
fetch + prompt + record steps out-of-sandbox. Transport selection is by
environment: `REIN_RUN_ID` present ⇒ direct path; else the virtual host.

- rein adds `declare.rein.internal` to the srt matched-domain list (srt
  routes by CONNECT host — no DNS involved). The proxy terminates it with
  its CA (already trusted in-sandbox via `SSL_CERT_FILE`) and handles it as
  a new local-only host class — **never relayed upstream**, responses
  token-free (response-path hygiene, phase1-design §4.1).
- Agent-facing command: **`rein declare 73`** — the rein binary is already
  staged readable in-sandbox at `<runTmp>/rein` (the probe copy,
  run_sandboxed.go step 12); a small subcommand calls
  `https://declare.rein.internal/v1/declare?issue=73` and prints the
  outcome. Plain `curl` to the same URL also works; the subcommand is
  ergonomics, not surface.
- Repo resolution: single-repo sessions use the session repo; multi-repo
  sessions require `?repo=owner/name` (`rein declare 73 --repo o/n`) — an
  ambiguous declare is denied with that instruction. (Multi-repo polish is
  deferred; single-repo is the dogfood path.) `--repo` is also the
  scope-expansion trigger in the session-scope mocks
  (docs/session-scope-ux-mocks.md §1, PR #56 branch); **it works in direct
  mode via the same CLI path** — the declare is a plain grant caller there —
  and the mocks' §7 filesystem question (where does the second repo's
  checkout live?) is **moot in direct mode**: no bwrap binds, the existing
  host checkout is usable.
- The declare call **blocks** while the human decides (same unbounded
  approval-pause discipline the proxy already applies). Approve →
  `200 {"confirmed": 73}`; deny/timeout/Ctrl-C → `403 {"message": …}`.
  Idempotent: declaring an already-confirmed issue returns 200 without
  re-prompting.

**Prompts never fire inside a relayed request.** The proxy decides every
request from state (the confirmed set) and never blocks a relayed push or
API call on a human.

**Agent education, thinnest-first:** (1) the synthesized errors themselves
are the primary teacher, delivered at the moment of need — the pattern
TM-G8's diag already proved on real agents (design.md:766); (2) the launch
banner prints the two-line convention (`rein declare <n>`; push to
`agent/<n>/<nonce>`); (3) optional, when the wrapped argv is `claude`:
`--append-system-prompt` with the same two lines (feasible today for Claude
Code; no general per-agent mechanism — revisit only if dogfood shows agents
ignoring the errors).

**Abuse surface:** the declare channel adds no capability an attempted push
didn't already have (both are an agent-chosen integer that can summon a
prompt); the fetch + Form A ceremony checks it identically, and in sandboxed
mode the agent cannot answer its own prompt (no /dev/tty in-sandbox — bwrap
`--new-session`, re-verified every launch, run_sandboxed.go CP4 invariants;
direct mode's weaker shared-terminal channel is §2 delta 2, unchanged from
today's write prompt).
Prompt-spam is bounded by the human's deny + the 60 s prompt timeout; if
dogfood shows spam, design.md:747's optional rate limit attaches naturally
at the declare handler. Pre-approval, the agent can cause **zero**
GitHub-bound side effects (not even an advertisement) — strictly less
observable behavior than today.

---

## 4. The confirmation prompt (Form A)

On a declare for issue N in repo R, rein (out-of-sandbox, holding network +
tokens) fetches `GET /repos/{R}/issues/{N}` with a **no-redirect-follow**
client, using an issues:read-capable read mint (`MintGhReadOnlyToken` shape,
githubapp/client.go:121–130 — the plain read mint lacks issues:read and
would 403). Recorded: title, state, whether N is actually a PR, and the
**canonical REST URL** — the TM-G6 transfer anchor (design.md:753). The
fetch result is snapshotted into the audit log (the design.md:758 issue
snapshot, captured at confirm time — there is no session-start issue
anymore).

```
=== rein: agent declares work on an issue ===
   issue:    #73 "sbom-action v2 breaks when --json-output is set"  [open]
             in tomh/wrangle
   session:  sess_dev_001 (role=implement, repos=[tomh/wrangle])
   approving covers ALL writes for this run (git push, gh, API).

To approve, type the issue number (73) and press enter.
To deny, press Ctrl-C or type anything else.
```

- **Form A (settled):** the number is displayed; typing it is the
  non-replayable token (design.md:245–246; the answer differs per issue, so
  a prompt-injected agent can't pre-construct a "yes" — design.md:281 — and
  cannot deliver one, lacking the tty). The displayed **fetched title +
  state + home repo** is the load-bearing misattribution control (decision
  E; probe scenarios S1/S4/S5): a wrong-but-plausible issue shows the wrong
  title. Titles are agent-editable in-scope (TM-G7, design.md:755–759), so
  the title informs the human but never authorizes — the number the human
  types is GitHub-assigned and unforgeable.
- Future option (recorded, not built): Form C — withhold the number so
  typing it becomes a knowledge check that fails closed on habit — if
  attribution gains teeth with §8 writeback.
- **Expansion prompts** (second issue mid-run, via a new declare) are the
  same prompt with an "agent wants to ALSO work on…" header
  (design.md:254–263). Honest residual: all Form A prompts are
  attention-checked transcription — S1 protection rests on the human
  reading the displayed title/repo; S5 (real-but-unrelated issue the human
  recognizes) is not closable by any prompt form (probe verdict; backstop
  is §8 writeback).
- **Out-of-process grant preserved:** the broker writes the fetched
  `PendingIssue` into `runs/<run-id>.json` before prompting, so the tmux
  popup / `rein approval grant --run-id X` path (internal/ui/grant.Grant)
  renders the same title/repo from the snapshot — the popup never fetches.

**What one approval covers:** the run's approval record holds a
**set of confirmed issues**. Writes flow when the session signature matches
AND (push refs resolve to a confirmed issue | non-push write and the set is
non-empty). Nothing re-prompts per write; nothing outlives the run
(ClearRun + Sweep unchanged).

---

## 5. Push-ref verification (sandboxed mode)

Declaration triggers the prompt; the push ref remains the **verification
anchor** (decision C): the agent states its issue twice — declare and branch
name — and both must agree with what the human confirmed. This section is a
**sandboxed-mode property**: direct mode's helper never sees refs
(C1, design.md:594; §2 delta 1).

**Scope: pushes only — deliberately, and that is not a gap.** A push is the
one write channel that carries an in-band issue claim (the branch name), so
it is the only place a cross-check is possible; non-push writes carry no ref
and are protected by the declare gate itself (§2, decision B). What this
section adds on top of declare is (a) the "declared right, pushed wrong"
drift check (misattribution probe S1 rec 2 — a confirmed #73 cannot push
work labeled `agent/74/…`), and (b) GitHub-side self-evident attribution:
the branch on the remote names its issue (design.md:521's purpose).

### 5.1 Ref convention (strict)

```
^refs/heads/agent/(0|[1-9][0-9]{0,9})/([A-Za-z0-9][A-Za-z0-9._-]{0,63})$
```

Leading zeros rejected; nonce is agent-chosen, format-checked only (branch
uniqueness per design.md:521 — the security nonce is the unrelated
`REIN_RUN_ID`). The issue number resolves **relative to the push-target
repo**, which closes S4 structurally: a number valid only in another repo
404s at declare time, and a push-target mismatch with the confirmed issue's
repo denies.

### 5.2 Parser

`internal/proxy/receivepack.go` (new): reads the pkt-line command section of
the `POST /git-receive-pack` body — optional leading `shallow <oid>` lines,
then `<old-oid> <new-oid> <refname>\0<caps>` commands, to the flush-pkt —
capped at 64 KiB, fail-closed on malformed/oversized. The packfile tail is
**streamed, never buffered** (relay via `io.MultiReader(prefix, rest)`).
Push is protocol v0/v1 only (v2 has no push), GitHub doesn't advertise
push-cert, delete-only pushes have no packfile — all handled.

### 5.3 Per-request behavior

| Request | Behavior |
|---|---|
| Advertisement GET, **no confirmed issue** | Local synthesized advertisement: `200`, `application/x-git-receive-pack-advertisement`, `# service=` pkt, flush, then `ERR rein: writes are locked until you declare your issue. Run: rein declare <n> (then push to agent/<n>/<nonce>)`. git prints it verbatim as `fatal: remote error: …`, exits cleanly, retries fine after declaring. Zero mint, zero upload. *(CP live-gate item: verify git remote-curl accepts the ERR pkt; fallback plain 403 — fail closed either way.)* |
| Advertisement GET, confirmed set non-empty | Mint write token (post-approval — clean), inject, relay. |
| receive-pack POST, all refs match convention + one confirmed issue | Relay. Audit row carries issue + refs. |
| POST, refs match but issue not confirmed | Deny with report-status: `ng <ref> rein: issue #74 not confirmed for this run; run rein declare 74`. |
| POST, any ref not matching the convention (`main`, tags, feature branches) | Deny whole push (atomic): `ng <ref> rein: refs must match agent/<issue>/<nonce> (e.g. agent/73/kx3q)`. Structural consequence, stated precisely: the **git transport** can never push the default branch or tags; API routes remain (§2 asymmetry). |
| POST, refs resolve to 2+ distinct issues | Deny: "one issue per push; split your push" (default, §9). |
| Delete of a matching ref | Same rules (issue from the ref name); agents cleaning their own `agent/N/x` branches is normal; non-matching deletes are denied — the git transport cannot delete `main`. |
| Force-push | Not distinguishable at the proxy and not special-cased: it can only hit `agent/N/*`. HEAD-pinned single-use tokens (design.md:590 full form) are the §8-track home for force semantics. |
| Unparseable command section | Deny, 403, close. |

### 5.4 Deny delivery (report-status synthesis)

Post-approval denies happen mid-POST, so a plain 403 is near-invisible to
git. The proxy synthesizes a git **report-status** response instead
(`unpack ok` + `ng <ref> <reason>` per ref, side-band-64k-wrapped iff the
client requested it — the capability list is already parsed), which git
prints as `! [remote rejected] <ref> (<reason>)`. To ensure delivery isn't
clobbered by a TCP RST while the client is mid-upload, the proxy drains the
remaining request body up to a hard cap (**8 MiB / 10 s**) before closing.
This drain applies **only** to these rare post-approval mismatch denies —
every pre-approval deny happens at the GET or a bodiless REST call with zero
upload. Synthesized bodies are fixed strings + ref names the client itself
sent — never a token.

---

## 6. Failure modes — all fail-closed

| Failure | Behavior |
|---|---|
| Issue fetch fails at declare (network/5xx/rate limit) | Declare returns 403 "could not verify issue #N; retry". No prompt without a fetched title (decision E). Reads unaffected. |
| Issue doesn't exist (404 in session/target repo) | Declare returns 403 "issue #N not found in o/r". |
| Issue in a different repo (S4) | Structurally closed: N resolves only against the declared/push-target repo (404 ⇒ deny). Same-number coincidence across repos is caught by the displayed title + home repo. |
| Issue transferred (301 on canonical URL) | Deny + surface loudly (TM-G6, design.md:753). On each write-token MINT, re-check the canonical URL of the confirmed issue(s); 301 ⇒ invalidate that confirmation, require re-declare. **Cadence, stated honestly (security review, 2026-07-11):** the write token is memoized for its ~1h lifetime, so "per mint" is roughly HOURLY, **not** per write — a transfer that 301s cleanly after the first mint keeps its binding until that token expires. That is acceptable **only** because of the impact ceiling, not because the check is prompt: per decision D the issue is **attribution, not capability** — the token stays repo-scoped, `session.Contains` gates every write, and the ref cross-check gates every push, so a stale binding smears an audit line and cannot widen access. A non-transfer verification failure (network/5xx) is **keep-and-log**, never a silent revocation of a confirmation the human gave. |
| Push ref for an unconfirmed issue | `ng` rejection naming `rein declare <n>` (§5.3) — the expansion path, not an error state. |
| Concurrent runs (S3) | Preserved unchanged: per-run crypto/rand run-id keying of approvals/run-context/ledger files (internal/approvals); each run's confirmed set is its own file; `grant --run-id X` reads only run X's snapshot. Probe verdict "prevented" — mechanism untouched. |
| Mid-run session file edit | Signature (ID/Role/Repos) mismatch invalidates the whole record including its issue set. |
| Prompt-channel failures (no tty, popup declined, timeout, Ctrl-C) | Today's layered grant behavior exactly: deny + helpful stderr; TM-G8 preserved (the credential path always returns a credential; these denies are the proxy's local answer — or, direct mode, the placeholder — path). |
| `rein declare` outside any run (no `REIN_RUN_ID`, no virtual host) | Fail with the instruction to launch via `rein run` — mirrors grant's existing no-run-id fail-closed path (grant.go obtainInteractiveNoPersist rationale). |
| Agent never declares | Reads work all run; every write attempt re-teaches the instruction (ERR/403 sandboxed; placeholder + stderr direct). Nothing hangs. |

---

## 7. `sess.Issue` retirement — everywhere, one model

- **`issue:` stops gating anything, in both modes** (Tom, 2026-07-11 —
  one binding model; no dead config field). If a session file still
  contains `issue:`, print a **loud warning**: "`issue:` is ignored — the
  issue is agent-declared; run `rein declare <n>`." Silent ignoring of a
  security-looking field is not acceptable.
- **Sandboxed:** `buildSandboxApprove` (run_sandboxed.go:481–505) loses both
  arms (no-issue⇒deny-all and the static-issue prompt), replaced by the
  declare-driven confirmed-set gate.
- **Direct:** the helper and rein-gh shim stop prompting inline entirely —
  their write gate becomes a read of the run's confirmed-issue set
  (approved ⇒ mint; empty ⇒ placeholder + stderr hint, §2). The prompt
  moves wholly to declare time. **UX delta, stated:** today's direct mode
  prompts *mid-push* and the same push proceeds on approval; the new flow
  is fail-with-instruction → declare → approve → retry — one extra retry on
  the first write, identical to sandboxed mode, in exchange for one model
  and no YAML editing. `SignatureOf` dropping `issue=` (§8) means existing
  mid-run approvals are unaffected by the field's presence.
- **Workflow migration (both modes):** the HANDOFF "add `issue: N` to test
  writes" recipe becomes: no file edit at all — run `rein declare N`,
  approve, push to `agent/N/<nonce>` (any ref in direct mode, though the
  convention is still the documented habit). Launch banners drop `issue=#N`
  and print the convention + "writes will prompt at declaration on THIS
  terminal". README/HANDOFF state the two §2 direct-mode deltas rather than
  leaving them to be discovered.

---

## 8. Approval record, audit log, §8-writeback compatibility

```go
type ConfirmedIssue struct {
    Number       int       `json:"number"`
    Repo         string    `json:"repo"`
    Title        string    `json:"title"`         // snapshot at confirm time
    State        string    `json:"state"`
    CanonicalURL string    `json:"canonical_url"` // TM-G6 anchor
    ConfirmedAt  time.Time `json:"confirmed_at"`
}
type Record struct {
    Signature string           // covers ID, Role, Repos — Issue removed
    SessionID string
    Issues    []ConfirmedIssue // the run's confirmed set; declares append
    ApprovedAt, ExpiresAt time.Time // unchanged (sweep heuristic only)
}
```

- `SignatureOf` drops its `issue=` line (approvals.go:155–163): the issue
  moves from *identity* of the approval to *content* — decision A in data
  form. Validity = signature match AND (pushes) declared issue ∈ Issues.
- `RunContext` gains `PendingIssue *ConfirmedIssue`, written by the declare
  handler before prompting (transport for the popup/other-terminal grant).
  Atomic writes as today; per-run keying (S3) untouched; the approvals
  package doc's invalidation list drops Issue.
- **Audit log** (internal/proxy/audit.go): add `Issue int`, `Refs []string`,
  and decisions `refused-undeclared`, `refused-ref-convention`,
  `refused-issue-unconfirmed`, `refused-issue-unverified` (fetch/404/301),
  `declared`, `confirmed-issue`, `expanded-issue`. Confirm-time snapshot
  logged. Still token-redacted, plain append-only (PLAN-1 2026-07-05).
- **§8 audit-writeback (design.md:616–632) stays layerable, not built:**
  `ConfirmedIssue.CanonicalURL` is the correct posting target across
  transfers; audit rows carry issue + refs per event; expansions are
  discrete events. The probe's S2 verdict (writeback failure undetected)
  remains intended-v1.

---

## 9. Defaults (previously open questions — now stated as defaults)

- **PR numbers are valid declarations**, displayed as
  `#N "title" [pull request]` (GitHub shares the number space; agent
  workflows against PRs are plausible). Deny only what the fetch denies.
- **One issue per push.** Multi-issue single pushes deny with "split your
  push"; multi-issue *runs* are first-class via sequential declares + pushes
  (design.md:555).
- **No `expect_issue:` pin.** It would re-introduce configure-up-front
  (against decision A) and a second source of truth. Revisit only if S5
  needs teeth before §8 writeback lands.
- **Report-status `ng` synthesis is in** (§5.4) — decision C's agent-visible
  error is not deliverable otherwise. ERR-pkt advertisement synthesis is in
  for the pre-approval gate.
- **Multi-repo declare ergonomics deferred** (`--repo` flag exists;
  polish later). Single-repo sessions are the dogfood path.

---

## 10. Implementation plan

| # | Component | Change | ~size |
|---|---|---|---|
| 1 | `internal/proxy/receivepack.go` (new) | pkt-line command-section parser (shallow lines, deletes, caps, 64 KiB cap); report-status `ng` synthesis (side-band aware); ERR-pkt advertisement synthesis | ~280 loc |
| 2 | ref→issue extraction (same pkg) | anchored regexp, leading-zero rejection | ~40 loc |
| 3 | `internal/issuemeta` (new) | fetch via issues:read-capable mint (`MintGhReadOnlyToken` shape), no-redirect client, 301 detection → `ConfirmedIssue` | ~120 loc |
| 4 | declare channel | `declare.rein.internal` host class in `internal/proxy/hosts.go` (local-only) + handler (parse, fetch, prompt, record, block-until-answer) + srt domain-list entry | ~120 loc |
| 5 | `rein declare` subcommand | HTTPS call to the virtual host via `SSL_CERT_FILE`; friendly output | ~60 loc |
| 6 | `internal/proxy/proxy.go` | receive-pack POST arm (parse → convention → confirmed-set check → stream-relay or `ng`); pre-approval GET arm (synthesized ERR); pre-approval REST/GraphQL 403 bodies | ~180 loc |
| 7 | `internal/brokercore` | `ConfirmWrite func(repo string) bool` → decision from confirmed-set state; declare-side approval hook | ~60 loc |
| 8 | `internal/ui/prompt` | Request gains title/state/home-repo/PR-flag; Form A compare vs declared number | ~50 loc |
| 9 | `internal/ui/grant` + `internal/approvals` | `Record.Issues` set, `PendingIssue`, `Valid(rec, sig, issue)`, signature change, package-doc update | ~160 loc |
| 10 | `cmd/rein/run_sandboxed.go` | buildSandboxApprove rework, banner text, `issue:`-ignored warning, optional `--append-system-prompt` for claude | ~80 loc |
| 11 | `internal/session` | comments + `issue:`-ignored warning (both modes) | ~25 loc |
| 12 | `internal/broker` (direct helper) | write gate switch: drop the inline ObtainApproval prompt path, read the run's confirmed set; placeholder + `rein declare <n>` stderr hint (the #45/TM-G8 deny channel) | ~70 loc |
| 13 | `cmd/rein-gh` (direct shim) | same gate switch + hint; `GH_TOKEN=rein-placeholder-denied` path unchanged | ~40 loc |
| 14 | `rein declare` direct path | transport selection (`REIN_RUN_ID` ⇒ direct), local fetch + grant-machinery call, no-run-id fail-closed error | ~80 loc |
| 15 | `cmd/rein/run.go` (direct) | banner update (convention + declare hint) | ~20 loc |
| 16 | docs | design.md conformance note; HANDOFF/README recipe (one model, both modes; §2 deltas stated); PLAN-1 entry; session-scope-mocks §6 correction | — |

**Tests:** Go units — parser vectors (multi-ref, shallow-prefixed,
delete-only, malformed, oversized → deny), extraction table, declare handler
(confirm/deny/fetch-fail/404/301/idempotent re-declare), proxy arms against
the fake-upstream harness (pre-approval GET → ERR body; POST convention-deny
→ `ng`; unconfirmed-issue → `ng`; confirmed → relay), approvals set logic,
prompt rendering (~750 loc). Live gate (`REIN_LIVE`): real declared push
succeeds; non-matching ref sees `! [remote rejected]`; pre-declaration push
sees `fatal: remote error: rein: …` (the ERR-pkt acceptance check).
Direct-mode tests: helper/shim gate units (empty set ⇒ placeholder + hint;
confirmed ⇒ mint; TM-G8 never-empty preserved), declare-direct units
(run-id detection, fetch, record append, no-run-id error). Interactive:
`tests/interactive/test_confirm_shows_title.py` (PR #48, merged 2026-07-11)
drops `@unittest.expectedFailure`; its fixture adds
`rein declare $REIN_ITEST_TITLE_ISSUE` before pushing
`agent/$REIN_ITEST_TITLE_ISSUE/<nonce>`. A **direct-mode pexpect leg** is
feasible with a small harness extension — `reinharness.spawn_rein_run`
hardcodes `rein run -- …`; add a mode flag so it can spawn
`rein run --direct -- …` and drive the declare prompt on the same pty —
plus updating `test_write_approval.py`, whose current flow asserts the
retired inline prompt.

**Rough total:** ~1,400 loc production + ~1,050 loc tests (the direct-mode
unification adds ~235 production — rows 12–15 — and ~250 tests over the
sandboxed-only plan). One checkpoint, live-gated per PLAN-1 discipline.

**Out of scope:** §8 audit writeback; single-use/HEAD-pinned tokens + commit
inspection (design.md:590 full form); new-issue-creation flow
(design.md:265–279 — keeps its first-word token when built); role catalog;
rate limiting (design.md:747) unless dogfood demands it.

**Implementation residual (recorded 2026-07-11):** sandboxed mode now
persists an **issues:read** token to the cross-run on-disk cache
(`ghsession.ReadCachePath`) to serve the declare fetch + the TM-G6
re-check — new for sandboxed mode (direct mode always did this). It is
read-tier, and the state dir is deny-read in-sandbox, so the exposure is
the pre-existing same-uid HOST residual (issue #7), not a new sandbox
hole.

**Residuals, stated honestly:** S5 open (inherent ceiling of
confirm-by-recognition; §8 writeback is the backstop). All Form A prompts
are attention-checked transcription — S1 detection rests on the human
reading the displayed title/repo. API writes carry no ref, so they rest on
the declared binding + repo ceiling + audit rows (§2). Post-approval REST
writes can touch the default branch within contents:write. Same-uid host
residual and run-scoped approval breadth (phase1-design §5.3) unchanged.

---

## Appendix A — Alternatives considered

**Push-ref-first (the original proposal shape).** Extract the issue from the
first push's receive-pack command list and prompt inside the relay. Rejected
because the receive-pack **advertisement GET** requires a push-capable token
at GitHub (design.md:595 C2) before any ref is visible, forcing an
**unprompted write-capable mint** ("SJ-1"): agent-triggerable at will
(`git ls-remote`, `push --dry-run`), persisted by value to the per-run
`writes/` ledger for exit-revoke (#20) — a live ~1h contents:write token on
disk before any human approval, and a weakening of phase1-design §5.3's
"first write is approval-gated". Declaration-first eliminates the mint
instead of disclosing it, at ~+100–150 loc, and also moves prompts out of
the relay path. Everything else from that shape (parser, convention, prompt
content, record shapes) survives unchanged in this design.

**Declaration-via-push-ref-on-retry.** Deny the first push with "declare via
your branch name", prompt on the retry. Structurally broken: the ref exists
only in the POST body, and git never POSTs without a successful
advertisement — so the retry's GET re-poses the unprompted-mint problem
exactly (mint, or livelock: rein can never see the branch name it demands).
The only escape — synthesizing a fake ref advertisement so git proceeds to
the POST — was rejected as fragile: fabricated old-OIDs break updates to
existing `agent/N/*` branches, zero-haves bloats every pack to a full pack,
and rein would have to guess a capability set GitHub later accepts. Not a
cheaper stage; a trap.

**Confirmation forms B/C/D** (first-word-of-title; number-withheld knowledge
check; number+word hybrid). Tom settled Form A (header note). Form B was
independently rejected on the merits: the title is agent-editable in-scope
(TM-G7), first words collide ("Fix…" can match the wrong issue — fails
open), and "first word" tokenization of unicode/markdown titles is a spec of
its own. Form C is the recorded future option if attribution gains teeth.

**Keep `sess.Issue` for direct mode** (an earlier revision of this doc, and
session-scope-ux-mocks §6). Rejected by Tom (2026-07-11): two binding models
in two modes, a dead config field kept alive for the fallback mode only, and
worse UX than the declare flow it diverges from — while the unified path is
*less* work than it looked, since `rein declare` in direct mode is just a
CLI invocation of the existing mode-independent grant/approval-record
machinery (no relay, no endpoint). The two real direct-mode limits (no ref
visibility per C1; the shared-terminal #12 residual) exist under the static
model too — keeping it bought nothing.
