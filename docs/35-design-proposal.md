# Issue #35 design — agent-declared, human-confirmed issue scoping (declaration-first)

**Status:** restructured 2026-07-11 per Tom's PR review; **awaits Tom's final
sign-off** before becoming design-of-record. No production code written.

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

Scope: **sandboxed mode only** (the Phase 1 spine — in-process per run, no
daemon; PLAN-1 notes 2026-07-05). Direct mode is unchanged this checkpoint
(§7).

---

## 1. The model

The agent inside the sandbox has no GitHub write path until it **declares**
which issue its work is for and a human **confirms** it:

1. Any pre-declaration write attempt is denied locally by the proxy with an
   instructive error naming the exact next step: `rein declare <n>` (§2).
   GitHub is never contacted; nothing is minted.
2. The agent runs `rein declare 73`. rein fetches issue #73 from the
   session repo (title, state, canonical URL), and prompts the human on
   /dev/tty (or tmux popup): title + state + home repo displayed, approve by
   typing the displayed number (§3–§4).
3. On approval, #73 joins the run's **confirmed-issue set** (run-scoped,
   decision B). All write channels now flow for the rest of the run. The
   first write-capable token is minted only after this point.
4. Every subsequent `git push` is still **verified**: refs must match
   `agent/{{issue}}/{{nonce}}` (design.md:521) and resolve to a confirmed
   issue — deny on mismatch with an agent-visible rejection (decision C;
   the ref-level half of design.md:590's push-time verification) (§5).
5. Declaring a second issue mid-run is the design's scope-expansion prompt
   (design.md:251–263, 304): same ceremony, appends to the set.

The issue is attribution, not capability (decision D): tokens stay
repo-scoped to the session set (#10; design.md:538/555); issue numbers appear
in prompts, approval records, and audit lines — never in mint parameters.

---

## 2. One gate, every write channel

**Tom's question, answered up front: this applies to ALL writes — git push
and gh/API alike — because every GitHub-bound byte from the sandbox crosses
the same proxy** (`internal/proxy` serveInject; there is no other route —
srt gives the sandbox no direct egress, and sandboxed mode has no rein-gh
shim: gh runs in-sandbox with the stub `GH_TOKEN` and its REST + GraphQL
traffic is classified and injected at the proxy like everything else,
run_sandboxed.go stubGHToken / phase1-design §5.1).

**Before approval (empty confirmed set), per channel:**

| Channel | Pre-declaration write attempt gets |
|---|---|
| `git push` (smart-HTTP handshake, `GET info/refs?service=git-receive-pack`) | Synthesized local advertisement carrying a pkt-line `ERR` — git prints `fatal: remote error: rein: writes are locked until you declare your issue. Run: rein declare <n> …` and exits cleanly (§5.3). Zero upload, zero mint, GitHub never contacted. |
| `gh` REST writes (POST/PATCH/PUT/DELETE via `api.github.com`) | Local `403` with JSON body `{"message": "rein: no issue declared for this run. Run: rein declare <n> …"}` — gh surfaces the message field to the agent. |
| `gh`/raw GraphQL mutations (`POST /graphql`, body-classified) | Same local 403 + JSON message (gh prints the HTTP error + message; a raw client sees the body directly). |
| Raw `curl`/any client to the inject hosts | Same local 403 body, read directly. |
| Reads (all channels) | Unaffected — read tier flows as today (TM-G8 untouched). |

Coverage confirmation: git smart-HTTP, gh-REST, gh-GraphQL, and
direct-REST/curl all terminate at the one serveInject chokepoint, so the
instructive deny needs exactly two implementations (ERR pkt for the git
advertisement; 403 JSON for everything else). **Nothing needs separate work**
in sandboxed mode. (Direct mode's rein-gh shim keeps its legacy static-issue
prompt, unchanged this checkpoint — §7.)

**After approval:** all write channels inherit the run's confirmed binding —
one ceremony per scope, no second prompt per channel or per write
(decision B; design.md:283 run-scoping).

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

## 3. The declare channel

The sandbox's only path to rein is the per-run proxy socket (phase1-design
§5.3), so the declaration rides it as a **virtual host**:

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
  deferred; single-repo is the dogfood path.)
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

**Abuse surface:** the declare endpoint adds no capability an attempted push
didn't already have (both are an agent-chosen integer that can summon a
prompt); the fetch + Form A ceremony checks it identically, and the agent
still cannot answer its own prompt (no /dev/tty in-sandbox — bwrap
`--new-session`, re-verified every launch, run_sandboxed.go CP4 invariants).
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

## 5. Push-ref verification

Declaration triggers the prompt; the push ref remains the **verification
anchor** (decision C): the agent states its issue twice — declare and branch
name — and both must agree with what the human confirmed.

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
| Issue transferred (301 on canonical URL) | Deny + surface loudly (TM-G6, design.md:753). On each write-token mint, re-check the canonical URL of the confirmed issue(s) involved; 301 ⇒ invalidate that confirmation, require re-declare. |
| Push ref for an unconfirmed issue | `ng` rejection naming `rein declare <n>` (§5.3) — the expansion path, not an error state. |
| Concurrent runs (S3) | Preserved unchanged: per-run crypto/rand run-id keying of approvals/run-context/ledger files (internal/approvals); each run's confirmed set is its own file; `grant --run-id X` reads only run X's snapshot. Probe verdict "prevented" — mechanism untouched. |
| Mid-run session file edit | Signature (ID/Role/Repos) mismatch invalidates the whole record including its issue set. |
| Prompt-channel failures (no tty, popup declined, timeout, Ctrl-C) | Today's layered grant behavior exactly: deny + helpful stderr; TM-G8 preserved (the credential path always returns a credential; these denies are the proxy's local answer path). |
| Agent never declares | Reads work all run; every write attempt re-teaches the instruction. Nothing hangs. |

---

## 7. `sess.Issue` migration

- **Sandboxed mode: `issue:` stops gating anything.** `buildSandboxApprove`
  (run_sandboxed.go:481–505) loses both arms (no-issue⇒deny-all and the
  static-issue prompt), replaced by the declare-driven hook. If a session
  file still contains `issue:`, print a **loud warning**: "`issue:` is
  ignored in sandboxed mode — declare with `rein declare <n>`." Silent
  ignoring of a security-looking field is not acceptable.
- **Direct mode unchanged this checkpoint.** Shape B's credential helper
  cannot see refs or host a declare endpoint (design.md:594 C1); direct mode
  is the clearly-marked fallback (phase1-design §3) and keeps the static
  `sess.Issue` prompt (including the rein-gh shim's) until sunset or a
  deliberate Shape-B analogue. Stated in README/HANDOFF, not discovered.
- **Workflow migration:** the HANDOFF "add `issue: N` to test writes" recipe
  becomes: no file edit at all — have the agent (or you) run
  `rein declare N`, approve, push to `agent/N/<nonce>`. The launch banner
  drops `issue=#N` and prints the convention + "writes will prompt at
  declaration on THIS terminal".

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
| 11 | `internal/session`, direct-mode adapters | comments + deprecation warning; adapters keep legacy semantics (mechanical signature adaptation only) | ~50 loc |
| 12 | docs | design.md conformance note; HANDOFF/README recipe; PLAN-1 entry | — |

**Tests:** Go units — parser vectors (multi-ref, shallow-prefixed,
delete-only, malformed, oversized → deny), extraction table, declare handler
(confirm/deny/fetch-fail/404/301/idempotent re-declare), proxy arms against
the fake-upstream harness (pre-approval GET → ERR body; POST convention-deny
→ `ng`; unconfirmed-issue → `ng`; confirmed → relay), approvals set logic,
prompt rendering (~750 loc). Live gate (`REIN_LIVE`): real declared push
succeeds; non-matching ref sees `! [remote rejected]`; pre-declaration push
sees `fatal: remote error: rein: …` (the ERR-pkt acceptance check).
Interactive: `tests/interactive/test_confirm_shows_title.py` (PR #48, merged
2026-07-11) drops `@unittest.expectedFailure`; its fixture adds
`rein declare $REIN_ITEST_TITLE_ISSUE` before pushing
`agent/$REIN_ITEST_TITLE_ISSUE/<nonce>`.

**Rough total:** ~1,150 loc production + ~800 loc tests. One checkpoint,
live-gated per PLAN-1 discipline.

**Out of scope:** §8 audit writeback; single-use/HEAD-pinned tokens + commit
inspection (design.md:590 full form); direct-mode parity; new-issue-creation
flow (design.md:265–279 — keeps its first-word token when built); role
catalog; rate limiting (design.md:747) unless dogfood demands it.

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
