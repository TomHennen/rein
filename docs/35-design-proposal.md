# Issue #35 design proposal — agent-declared + human-confirmed issue scoping

**Status:** PROPOSAL for Tom's review. No production code written. Implements
Tom's decisions A–F (issue #35 comments, 2026-07-08) on the Phase 1 sandboxed
spine (in-process per run, no daemon — PLAN-1 notes 2026-07-05).

**One-sentence model:** the agent declares the issue by pushing to
`agent/{{issue}}/{{nonce}}` (design.md:521); rein extracts the issue from the
receive-pack command list at the proxy, fetches the issue's title + home repo
(decision E, REQUIRED), asks the human for non-replayable confirmation
(design.md:281), records the confirmed issue in the run-scoped approval
(design.md:283, decision B), and thereafter cross-checks every push ref against
the confirmed set, denying on mismatch (decision C; the Shape A push-time
verification of design.md:590, ref-level half).

---

## 0. Where the issue becomes visible — a protocol fact that shapes everything

A git smart-HTTP push is two requests:

1. `GET /{owner}/{repo}.git/info/refs?service=git-receive-pack` — the ref
   advertisement. **No ref names from the client appear here.** GitHub 403s
   this GET without a push-capable token (design.md:595, correction C2), so a
   write-capable token must already be injected on it.
2. `POST /{owner}/{repo}.git/git-receive-pack` — the body opens with a
   pkt-line **command section** (`<old-oid> <new-oid> <refname>\0<caps>`, one
   line per ref, terminated by a flush-pkt `0000`), followed by the packfile.
   **This is the only place the agent's declared refs — and therefore the
   declared issue — exist on the wire.**

Today's code classifies the advertisement GET as write tier
(`internal/classify`; phase1-design.md §5.1) and fires the approval prompt
there, before any refs exist. The declared-issue model therefore requires
**moving the human approval from the advertisement GET to the receive-pack
POST**, and giving the advertisement GET a write-capable token *without* a
prompt.

**Why that is safe (and why it must be flagged anyway):** the advertisement
GET cannot mutate anything at GitHub — it only lists refs the agent can
already learn via the read path. The security invariant "no write reaches
GitHub without human approval" is preserved because the only mutating request
(the POST) stays gated. Two invariants *do* weaken, and both must be owned
honestly (**security judgment call SJ-1**, §7):

- "No write-capable token is ever minted before approval" — a write token now
  exists in rein's (out-of-sandbox) process memory, injected into exactly one
  GET, before the human has said yes. The agent never sees it (injection
  invariants, phase1-design.md §4.1) and it is repo-scoped to the session set
  (#10) — but it is not only in memory: **every write mint is persisted by
  value to the `writes/<run-id>.jsonl` ledger** (RecordWrite →
  AppendWriteToken, run_sandboxed.go:282–285; required for the exit-time
  revoke, issue #20). And the agent can *trigger* this mint on demand with no
  prompt ever firing (`git ls-remote` against receive-pack, `git push
  --dry-run` — both stop at the advertisement). Net: a live ~1h
  contents:write token can land on disk pre-approval, reachable by the
  same-uid host residual that already exists (issue #7 class, phase1-design
  §5.3). Bounded by run-lifetime revoke + Sweep, and *not* reachable from
  inside the sandbox — but phase1-design §5.3's sentence "the first write of a
  run is still approval-gated" must be amended to "the first *mutation* is
  still approval-gated"; the token-mint itself no longer is. Mitigation
  considered and rejected: skipping the ledger append for advert-triggered
  mints would exempt exactly those tokens from exit-revoke — worse.

*Alternative considered and rejected:* keep the prompt on the GET and prompt
again on the POST once refs are known — two prompts per first push is the
UX-hostile per-write model design.md:283 explicitly corrected away from.

---

## 1. (a) Extracting the issue from the push

### 1.1 Parser

New, small, dependency-free parser (proposed `internal/proxy/receivepack.go`
or `internal/gitwire`): reads the pkt-line command section of a
`POST /git-receive-pack` body up to the flush-pkt, capped at 64 KiB (a command
section is a few hundred bytes in practice; the cap is fail-closed — an
oversized or malformed section denies the push, never truncates). It returns:

```go
type RefUpdate struct {
    OldOID, NewOID string // 40/64 hex; NewOID all-zero = delete
    Ref            string // e.g. "refs/heads/agent/73/kx3q"
}
// plus the client capability list from the first command line
```

The proxy then relays the body as `io.MultiReader(parsedPrefix, rest)` — the
packfile is **streamed, never buffered** (a push can be GiBs; the existing
GraphQL 1 MiB buffer pattern in `internal/proxy/proxy.go` serveInject is the
precedent for "peek then relay", but receive-pack must not buffer the tail).

Notes on protocol coverage:
- Push is protocol v0/v1 only (git protocol v2 does not cover push), so the
  command-section format is stable.
- The command section may be preceded by `shallow <oid>` pkt-lines when the
  client is a shallow clone (pack-protocol: `update-requests = *shallow
  ( command-list | push-cert )`) — the parser must accept and skip leading
  shallow lines, or every shallow-clone push denies spuriously.
- GitHub does not advertise `push-cert` (signed push); after any shallow
  lines, if the body doesn't continue with a well-formed command line,
  **fail closed: deny**.
- A delete-only push has no packfile after the flush — the parser must not
  require trailing bytes.
- `Expect: 100-continue` ordering: like the GraphQL arm, the proxy must invite
  the body *before* it can decide. The C2 property ("a refused push is never
  invited to upload its pack") weakens to "a refused push uploads at most its
  command section before rein stops reading and answers" — the pack upload is
  aborted by the deny response + connection close. Bounded, acceptable.

### 1.2 Ref convention (decision C: strict)

Every command's ref must match, anchored:

```
^refs/heads/agent/(0|[1-9][0-9]{0,9})/([A-Za-z0-9][A-Za-z0-9._-]{0,63})$
```

- Issue = the decimal group; **leading zeros rejected** (no `agent/073/x`
  ambiguity), `0` rejected as a non-existent issue number at fetch time.
- Nonce: agent-chosen, format-checked only. The nonce exists for branch
  uniqueness (design.md:521); it is *not* a rein security artifact — the
  security nonce is the per-run `REIN_RUN_ID` (approvals keying), which is
  unrelated and unchanged.
- The issue number is resolved **relative to the push-target repo** (the
  repo in the request path). This single choice is what closes S4
  structurally — see §3.

### 1.3 Decision table per push (evaluated on every receive-pack POST, not just the first)

| Push contents | Behavior |
|---|---|
| All refs match convention, all resolve to one issue N, N already in this run's confirmed set | Relay (run-scoped approval, decision B). Audit line carries issue + refs. |
| All refs match, single issue N, N **not** yet confirmed | First write of run → confirmation ceremony (§2). Later in run → **scope-expansion prompt** (design.md:251–263, 304), same ceremony; on approval, N is appended to the confirmed set. Deny ≠ error: agent gets the rejection below. |
| Any ref does **not** match the convention (`main`, `refs/tags/v1`, feature branches, …) | **Deny the whole push** (atomic — no partial relay), with an agent-visible per-ref rejection telling it the convention (decision C). |
| Refs match but resolve to **two or more distinct issues** in one push | **Deny** with "one issue per push; split your push". Keeps the ceremony unambiguous (one prompt confirms one issue). Multi-issue *runs* are still first-class via sequential pushes (design.md:555). Flagged as OQ-5. |
| Delete of a matching ref (`NewOID` all-zero) | Treated identically: issue extracted from the ref name; flows if confirmed, prompts if not. Agents cleaning up their own `agent/N/x` branches is normal. Deletes of non-matching refs are denied by the convention rule — the agent structurally **cannot delete `main`**. |
| Force-push | Not distinguishable at the proxy (force is client-side; the command is just old/new OIDs) and not treated specially: it can only hit `agent/N/*` branches. HEAD-pinned single-use tokens (design.md:590 full form, §8 later track) are where force-push semantics would attach; out of scope here. |
| Unparseable / oversized command section | Deny, 403, close. Fail closed. |

A pleasant structural consequence, **stated precisely**: on the *git wire
channel*, the default branch can never match the convention, so `git push` to
`main` (or any tag) is impossible in sandboxed mode; merges happen via
`gh pr merge`. This is **not** "the agent can't touch `main` at all": once the
run approval is granted, REST writes flow (decision B), and the REST git-data
API (`PATCH/DELETE /repos/{o}/{r}/git/refs/heads/main`, `PUT /contents/…`,
merge endpoints) can still mutate or delete the default branch with the
token's contents:write. That residual is unchanged from today and is bounded
by the same run approval; the convention closes the *git transport* route
only. So `require_human_confirmation_on_default_branch` (design.md:520) is
addressed for pushes, not retired. Surfaced as **SJ-2** (§7) since it's both
a user-visible behavior change and an easy claim to over-state.

### 1.4 Making the denial agent-visible

A plain local `403` body is largely invisible to the agent: git prints
`error: RPC failed; HTTP 403` and drops the body. Decision C demands a *clear
agent-visible error telling it the convention*. The reliable channel is git's
own **report-status** protocol: respond `200` with
`Content-Type: application/x-git-receive-pack-result` and a synthesized
pkt-line report:

```
unpack ok
ng refs/heads/fix-login rein: refs must match agent/<issue>/<nonce> (e.g. agent/73/kx3q); see `rein doctor`
```

(wrapped in side-band-64k band 1 iff the client requested that capability —
the parser already has the capability list). git then prints
`! [remote rejected] fix-login (rein: refs must match agent/<issue>/<nonce> …)`
— exactly the agent-visible, convention-teaching error decision C asks for.
Same channel is used for "issue #74 not found", "confirmation denied", and
"could not verify issue (network)". Fallback for bodies too malformed to have
yielded a capability list: plain 403 (fail closed; nothing better exists).
The response-path hygiene invariant holds: these synthesized bodies are fixed
strings + ref names the client itself sent, never a token (phase1-design.md §4.1).

**Delivery caveat (reviewer finding):** writing the report and closing while
the client is still mid-pack-upload risks a TCP RST clobbering the response
before git reads it (the same early-response-with-unread-body class proxy.go
already documents at its F1 note) — and then the agent sees nothing, defeating
the point. Mitigation: after writing the `ng` report, **drain the remaining
request body up to a hard cap (proposed 8 MiB / 10 s, whichever first)**
before closing. This slightly widens SJ-3's "at most the command section"
bound to "command section + a capped drain"; the cap keeps it bounded and is
the price of decision C's agent visibility. Denies decided *before* the body
was invited (non-push writes, malformed preamble) keep the zero-upload
property.

### 1.5 Non-push writes (REST POST/PATCH/PUT/DELETE, GraphQL mutations)

Per decision B, non-push writes carry no declaration channel and **inherit the
run's issue binding**. Rule: they flow iff the run's confirmed-issue set is
non-empty (run approval granted). If a non-push write arrives **before any
issue has been confirmed** (e.g. `gh issue comment` before the first push):
**deny, fail closed**, with the proxy's 403 body instructing:
"no issue is bound to this run yet — push a branch named
`agent/<issue>/<nonce>` first to declare and confirm the issue." The common
agent flow (push branch → open PR) is unaffected. Alternatives (repo-level
confirmation binding no issue; deriving the issue from REST paths like
`POST /repos/o/r/issues/73/comments`) are listed as OQ-3 — the deny-first rule
is the smallest fail-closed start and can be relaxed later without breaking
anything.

---

## 2. (b) The confirmation flow

### 2.1 When it fires and what is fetched

Fires on the **first receive-pack POST of the run** (and again only for scope
expansion to a new issue — decision B; design.md:283 run-scoping unchanged).
Before prompting, rein (out-of-sandbox, holding the read token) fetches
`GET /repos/{push-owner}/{push-repo}/issues/{n}` via a **no-redirect-follow**
client, recording:

- **title** and **state** (open/closed), and whether the number is actually a
  PR (the issues API returns PRs too — displayed as such; OQ-4),
- **home repo** = the push-target repo the number resolved in (decision E
  requires displaying both title and repo),
- the **canonical REST URL** — the TM-G6 integrity anchor (design.md:753). A
  301 here means the issue was transferred to another repo: **deny + surface
  loudly** (the scope anchor moved; design.md:557).

The fetch result (a per-issue snapshot: number, title, state, canonical URL,
fetched-at) is written into the run's audit log — the design.md:758 issue
snapshot, now captured at confirm time instead of session-start (there is no
session-start issue anymore; this is the decision-A-consistent reading).

Prompt rendering (extends `internal/ui/prompt.Request` with the fetched
fields; PR #48's gated pexpect test asserts exactly this — title word + home
repo visible on the real tty):

```
=== rein: agent declares work on an issue — first write of this run ===
   pushing to:  tomh/wrangle  (branch agent/<n>/kx3q)
   issue:       "sbom-action v2 breaks when --json-output is set"  [open]
                in tomh/wrangle
   session:     sess_dev_001 (role=implement, repos=[tomh/wrangle])
   approving covers ALL writes for this run (git push, gh, API).

To approve, type the ISSUE NUMBER this work is for: _
To deny, press Ctrl-C or type anything else.
```

(Whether `<n>` is printed in the header is exactly the Form A vs Form C choice
below.) The tmux-popup / other-terminal grant path is preserved: the broker
does the fetch (it has network + tokens) and writes the pending issue context
into `runs/<run-id>.json` before prompting, so the out-of-process
`rein approval grant --run-id X` (internal/ui/grant.Grant) renders the same
fetched title/repo from the snapshot — the popup never fetches.

### 2.2 Confirmation-input forms — pros/cons (Tom's requested section)

Setup common to all forms: title + home repo display is REQUIRED (decision E);
the ref cross-check with deny-on-mismatch (§1.3) exists independently of the
form; the input arrives via /dev/tty which the sandboxed agent cannot reach
(bwrap `--new-session`, verified per launch — run_sandboxed.go CP4 invariants),
so **anti-prompt-injection is a property of the channel plus per-issue
variance, and every form below has it equally** (design.md:281). What differs
is what the typed token *verifies* about the human.

Two attacker-relevant asymmetries drive the analysis:

- **The number is GitHub-assigned and unforgeable** — a prompt-injected agent
  can pick a *different* real issue but cannot make issue #74 *be* #73.
- **The title is attacker-influenceable** — the implement role carries
  `issues: write` (design.md:516), so a compromised agent can retitle a decoy
  issue to mimic the intended one (TM-G7, design.md:755–759: the design
  already refuses to let issue *text* authorize anything for exactly this
  reason). First words collide naturally anyway ("Fix", "Update", "Add").

| Form | Usability | S1 (agent declares wrong-but-real issue #74, human intends #73) | S5 (plausible unrelated issue human recognizes) | Attacker leverage over the token | Implementation |
|---|---|---|---|---|---|
| **A. Type the issue number, number displayed in the prompt** (design.md:245–246 as written; :535 declares the same token for the release role) | Best: 2–3 digits, no i18n, trivial retry. Risk: pure transcription — the careless human copies whatever is shown (the probe's "presence ceremony, not knowledge check" critique of today's prompt carries over). | Caught **only if** the human reads the displayed title/repo and notices. The typing step itself adds nothing over pressing enter. | Not caught (human recognizes it, waves it through). | None over the token itself (number unforgeable); attacker relies wholly on human inattention. | Trivial (compare int). |
| **B. Type the first word of the fetched title** (design.md:281's token; the probe's "strongest form") | Worst: unicode/CJK/emoji/markdown titles make "first word" genuinely ambiguous (`[bug]`, `fix:`, `🐛 crash…`); tokenization + normalization (case, NFC) is a real spec; a bad extraction means the human *cannot* approve (fail-closed but hostile). | Forces eyes onto the title — better attention than A. But it verifies *reading the display*, not *intent*: the human transcribes the **wrong issue's** first word just as fluently. Common-word collisions ("Fix…") mean a habitual answer can even **match accidentally — fails open**. | Not caught. | **Real leverage**: title is agent-editable in-scope (TM-G7). A decoy issue retitled to mirror the intended one defeats exactly what this form claims to check. | Messy (tokenizer + normalization + its edge-case tests). |
| **C. Type the issue number, number NOT pre-displayed** (title + repo shown per decision E; the declared number is revealed only on mismatch/deny and in the audit log) | Good typing ergonomics (digits), but demands the human *knows* the number of the task they assigned — else a ~30s lookup (which itself routes them past the real issue: an independent verification channel). Honest limit: the agent's own TUI output shows the pushed branch name, so a determined rubber-stamper can transcribe from there. | **Strongest**: the human types their *intended* number; rein compares it to the *declared* number mechanically. Habit fails closed — typing "73" out of muscle memory against a declared #74 **denies** and surfaces "you typed 73 but the agent declared 74". Detection no longer depends on vigilance. | Partially: if the human doesn't actually know/care which issue they meant, C degrades to A. No form closes S5 (the probe and design.md concede this; backstop is §8 writeback). | None (number unforgeable; withholding it removes the transcription path). | Trivial (compare int) + a good deny message. |
| **D. Hybrid: number + first word** ("74 sbom-action") | Double friction, inherits B's tokenizer pain. | No better than C: the number half already carries the mechanical check; the word half adds only B's transcription. | Not caught. | Word half inherits B's TM-G7 leverage. | B's cost for ~no gain. |

Additional cross-cutting points:

- **Muscle-memory / rubber-stamping:** prompts are rare (once per run +
  expansions, decision B), which limits habituation for all forms — but
  dogfooding means many runs/day on the *same* issue, so habituation is real
  across runs. A and B habituate to transcription (fail-open); C habituates to
  typing the intended number (fail-closed on a swap). This asymmetry in
  *failure direction* is, to me, the decisive security argument.
- **Scope-expansion prompts are different — and are the residual S1 path:**
  when the agent legitimately discovers related issue #41 mid-run
  (design.md:251–263), the human typically *cannot* know that number in
  advance — the design's own example displays it ("To approve, type \"41\"",
  design.md:262). So expansion prompts must display the number (Form A
  behavior) whatever is chosen for first-of-run: the knowledge check is
  impossible by construction there. **Stated plainly: this reopens S1 at the
  expansion prompt as the attacker's cheapest path** — a compromised agent
  can push its first ref to the *correct* issue (passing Form C), then
  "expand" to the decoy, where the check degrades to transcription and the
  only defense is the human reading the fetched title/repo. The mechanically
  fail-closed check covers the first issue of the run only. No form fixes
  this; the expansion prompt's title/repo display and the §8 writeback
  backstop are the mitigations, and the residuals section carries it.
- **Where first-word remains right:** the new-issue-creation prompt
  (design.md:265–279) uses first-word *because no number exists yet* — the
  design assigns tokens by availability, not strength (:278 vs :535). That
  flow is out of scope here and keeps its token when built.

**Recommendation (Tom decides):** **Form C for the first-of-run confirmation,
Form A for scope-expansion prompts.** Type the issue number; don't pre-display
it on the first prompt; always display fetched title + state + home repo; on
mismatch, deny and print both numbers. Reject Form B: its attention benefit is
real but its token is attacker-influenceable (TM-G7), operationally brittle
(i18n/tokenization), and its habitual-failure direction is open, not closed —
and design.md never assigned first-word to *this* prompt anyway (:535 says
`type_issue_number`). If Tom prefers zero-lookup-friction, Form A everywhere
is the honest second choice — with the note that S1 protection then rests
entirely on the required title display plus the ref cross-check, both of which
exist regardless. (Citation honesty: design.md:535's
`human_confirmation_method: "type_issue_number"` sits under the **release**
role's config, not this prompt; the flow example at :245–246 is the direct
authority for typing the number here. Decision A cites :535 for this flow —
the token is the same, the config line is not.)

### 2.3 What one approval covers (reconciling with decision B)

The run approval record becomes **a set of confirmed issues** for the run.
Writes flow when: (session signature matches) AND (push refs resolve to a
confirmed issue | non-push write and set non-empty). New issue in a push ⇒
expansion prompt, appends to the set. Nothing re-prompts per write; nothing
global outlives the run (`ClearRun` + Sweep unchanged).

---

## 3. (c) Failure modes — all fail-closed

| Failure | Behavior |
|---|---|
| **Title fetch fails** (network, API 5xx, rate limit) | **Deny the write.** Decision E makes display REQUIRED, so no fetch ⇒ no prompt ⇒ no approval. Agent-visible `ng … rein: could not verify issue #74 (network); retry`. Reads keep flowing (TM-G8 untouched — the credential path still always returns a credential; this deny is the proxy's local 403/ng path, same as today's refused-scope, proxy.go PlaceholderRefused arm). |
| **Issue doesn't exist** (404 in push-target repo) | Deny + `ng … rein: issue #74 not found in tomh/wrangle; refs must be agent/<real-issue>/<nonce>`. |
| **Issue in a different repo than the push target (S4)** | **Structurally closed**: the number is resolved *only* against the push-target repo. "Valid in another repo" ⇒ 404 here ⇒ deny. A same-number-different-issue coincidence (N exists in both repos) is caught by the required title+repo display — the human sees the *push-target's* issue #N title, which won't match their intent. |
| **Issue transferred (301 on canonical URL)** | Deny + surface loudly (TM-G6, design.md:753). On subsequent write mints for an already-confirmed issue, re-check the canonical URL of the issue(s) in the triggering push; 301 ⇒ invalidate that issue's confirmation, re-prompt (design.md:753's per-mint check, applied to write mints — read mints don't touch issues). |
| **Second ref for a different issue later in the run** | Not a deny and not a silent allow: a **scope-expansion prompt** (decision B's only sub-session prompt; design.md:251/304). Denying it would break the design's multi-issue sessions (design.md:555); silently allowing would gut the binding. |
| **Concurrent runs (S3)** | Unchanged and preserved: approvals/run-context/ledger files stay keyed by the crypto/rand run id (approvals.go per-run keying); each run's confirmed-issue set is its own file; `grant --run-id X` still reads only run X's snapshot (grant.go Grant). The probe rated S3 *prevented* — this proposal does not touch the mechanism that prevents it. |
| **Mid-run session edit** | Signature (ID/Role/Repos) mismatch still invalidates the whole record, including its issue set — a widened repo list can't ride an old approval. |
| **Prompt channel failures** (no tty, popup declined, timeout, Ctrl-C) | Exactly today's layered behavior (grant.go): deny, helpful stderr, TM-G8 preserved. |

---

## 4. (d) What happens to `sess.Issue`

- **Sandboxed mode: `issue:` stops gating anything.** `buildSandboxApprove`
  (run_sandboxed.go:481–505) loses both arms — the "no issue ⇒ deny all
  writes" arm and the static-issue prompt arm — replaced by the declared-issue
  hook. If a session file still contains `issue:`, print a **loud warning**:
  "`issue:` is ignored in sandboxed mode — the issue is agent-declared via the
  push ref and confirmed at first write." Silent ignoring of a
  security-looking field is not acceptable; a warning is.
- **Direct mode keeps the field, unchanged, this checkpoint.** Shape B's
  credential helper cannot see refs at all (design.md:594, correction C1 —
  the helper is invoked at repo-URL level), so the declared model is
  *unimplementable* there. Direct mode is already the clearly-marked fallback
  (phase1-design.md §3); it keeps the static-issue prompt until it is sunset
  or someone deliberately designs a Shape-B analogue. This split must be
  stated in README/HANDOFF, not discovered.
- **Migration for the "add `issue: N` to test writes today" workflow:** the
  HANDOFF/README recipe becomes "push to a branch named
  `agent/<issue>/<nonce>`; the first push prompts". No file edit needed at
  all — strictly less setup. The launch banner (printSandboxBanner) drops
  `issue=#N` and instead prints the branch convention + "first push will
  prompt on this terminal".
- **Audit-anchor semantics (decision D — settled, restated):** the minted
  token stays repo-scoped to the session set (#10; design.md:538/555); the
  confirmed issue is **attribution, not capability**. Concretely: issue
  numbers appear in the approval record, the prompt, and audit lines; they
  never appear in mint parameters. Nothing about token minting changes.
- **Not proposed:** repurposing `issue:` as an optional pin ("declared issue
  must equal N") was considered — it would close S5 when used — but it
  re-introduces configure-up-front (against decision A) and creates two
  sources of truth. Noted as a possible later opt-in (OQ-6); default is plain
  deprecation.

---

## 5. (e) Approval record, audit log, and §8 writeback compatibility

### Approval record (`internal/approvals`)

```go
type ConfirmedIssue struct {
    Number       int       `json:"number"`
    Repo         string    `json:"repo"`          // push-target = home repo
    Title        string    `json:"title"`         // snapshot at confirm time
    State        string    `json:"state"`
    CanonicalURL string    `json:"canonical_url"` // TM-G6 anchor (design.md:753)
    ConfirmedAt  time.Time `json:"confirmed_at"`
}
type Record struct {
    Signature string           // now covers ID, Role, Repos — Issue REMOVED
    SessionID string
    Issues    []ConfirmedIssue // the run's confirmed set; expansions append
    ApprovedAt, ExpiresAt time.Time // unchanged semantics (sweep heuristic only)
}
```

- `SignatureOf` drops the `issue=` line (approvals.go:155–163). The issue
  moves from *identity of the approval* to *content of the approval* — which
  is what decision A means. Validity = signature match AND (for a push)
  declared issue ∈ `Issues`.
- `RunContext` gains `PendingIssue *ConfirmedIssue` — written by the broker
  (which fetched it) just before prompting, so the out-of-process grant
  (popup/other terminal) displays the same fetched title/repo without doing
  its own fetch. The grant subcommand appends to `Issues` on approval, exactly
  as it writes the record today; per-run file keying (S3) untouched.
- Record updates are the existing atomic write (writeAtomic); one writer at a
  time in practice (prompts are serialized on the tty), but append-via-
  read-modify-write under the run's own file keeps concurrent-run safety
  trivially.

### Audit log (`internal/proxy/audit.go` entry fields)

Add: `Issue int` (0 = none), `Refs []string` (receive-pack pushes), and new
`Decision` values: `refused-ref-convention`, `refused-issue-unverified`
(fetch fail / 404 / 301), `refused-issue-mismatch` (typed ≠ declared),
`approved-issue` (first confirm), `expanded-issue`. The confirm-time issue
snapshot is also logged. Still token-redacted, still plain append-only
(hash-chaining stays out per PLAN-1 2026-07-05 note).

### §8 audit-writeback compatibility (design-compatible, NOT built)

design.md:616–632's writeback posts structured comments *to the bound
issue(s)* via the Audit App. This proposal makes that layerable without
redesign: the record's `ConfirmedIssue` carries the canonical URL (the
correct posting target even across transfers), the audit lines carry
issue + refs + commit-range-visible-at-proxy per event, and expansions are
discrete events. The S2 verdict ("writeback failure undetected") remains
intended-v1 per the probe; nothing here worsens or blocks it.

---

## 6. (f) Component-level implementation plan

| # | Component | Change | ~size |
|---|---|---|---|
| 1 | `internal/proxy/receivepack.go` (new) | pkt-line command-section parser (cap, deletes, caps list); report-status/`ng` response synthesis (side-band aware) | ~250 loc |
| 2 | `internal/refissue` or same pkg (new) | ref → issue extraction (anchored regexp, leading-zero rejection) | ~40 loc |
| 3 | `internal/issuemeta` (new) | `Fetch(ctx, repo, n)`, no-redirect client, 301 detection → `ConfirmedIssue`. **Token note:** sandboxed mode's `MintRead` is `MintReadOnlyToken` = contents:read + metadata:read only (githubapp/client.go:97–105) — **no issues:read, so the fetch would 403 and every confirmation would deny.** Use a `MintGhReadOnlyToken`-shaped mint (client.go:121–130, already includes issues:read) for this fetch, or widen the sandboxed read tier | ~120 loc |
| 4 | `internal/brokercore` | `Request` gains `GitPush *PushInfo` (refs, issue); `ConfirmWrite func(repo string) bool` → `func(ApprovalContext) bool`; new no-confirm path for the receive-pack **advertisement** GET (SJ-1) | ~80 loc |
| 5 | `internal/proxy/proxy.go` | serveInject: receive-pack POST arm (invite body → parse → convention check → fetch → approve → stream-relay via MultiReader; deny via ng) ; advertisement GET arm (write mint, no prompt) | ~200 loc |
| 6 | `internal/classify` | distinguish receive-pack advertisement vs receive-pack POST (both stay write tier; proxy needs the sub-kind) | ~20 loc |
| 7 | `internal/ui/prompt` | `Request` gains fetched title/state/home-repo + declared-number-display flag; compare typed vs *declared* | ~60 loc |
| 8 | `internal/ui/grant` | ObtainApproval takes the pending issue; record set-append; `Grant` renders `PendingIssue` from run context | ~100 loc |
| 9 | `internal/approvals` | `Record.Issues`, `RunContext.PendingIssue`, `Valid(rec, sig, issue)`, signature change | ~80 loc |
| 10 | `cmd/rein/run_sandboxed.go` | buildSandboxApprove rework; banner text; `issue:`-ignored warning | ~60 loc |
| 11 | `internal/session` | comment updates; field retained for direct mode + deprecation warning hook | ~15 loc |
| 11b | `internal/broker`, `cmd/rein-gh` (direct-mode adapters) | mechanical: adapt to the new `ConfirmWrite func(ApprovalContext) bool` signature (adapters wrap their existing repo-string behavior; direct-mode *semantics* unchanged); update the approvals package-doc invalidation list, which still names Issue | ~40 loc |
| 12 | docs | design.md conformance note; HANDOFF/README write-test recipe; PLAN-1 checkpoint entry | — |

**Tests**
- Go units: parser vectors (multi-ref, delete-only, malformed, oversized,
  push-cert-shaped garbage → deny), ref→issue extraction table
  (valid/leading-zero/huge/bad-nonce/`refs/tags`), proxy receive-pack arms
  against the existing fake-upstream harness (proxy_test.go pattern):
  convention-deny produces `ng`, unconfirmed issue prompts stub, confirmed
  issue relays, mismatch denies, fetch-fail denies, 301 denies; approvals
  signature/set logic; prompt rendering with title/state/repo. (~700 loc)
- `live_test.go` (REIN_LIVE-gated): real push to `agent/<n>/<nonce>` on the
  throwaway; real push to a non-matching ref sees `! [remote rejected]`.
- **`tests/interactive/test_confirm_shows_title.py` flips** (PR #48 — merged
  2026-07-11; absent from this worktree only because the branch predates the
  merge): drop `@unittest.expectedFailure`; update its session fixture (it
  pins `issue:` in yaml — under this design the env vars stay but the
  declaration moves to the pushed branch name, so the harness's
  clone-and-push script must push `agent/$REIN_ITEST_TITLE_ISSUE/<nonce>`).
  Optionally add a sibling gated test: wrong-ref push shows the rejected-ref
  message.

**Rough total:** ~1,000 loc production + ~800 loc tests. One checkpoint's
worth, gated live verification per PLAN-1 discipline.

**Out of scope (explicit):** §8 audit writeback; single-use / HEAD-pinned
write tokens + commit inspection (the *other* half of design.md:590);
direct-mode parity; the new-issue-creation flow (design.md:265–279);
REST-path issue derivation; multi-issue single-push; role catalog.

---

## 7. (g) Open questions for Tom + flagged security judgment calls

**Security judgment calls (flagged per CLAUDE.md hard constraint 5):**
- **SJ-1** — the receive-pack **advertisement GET is injected with a
  write-capable token without a prompt** (§0). Required by GitHub's 403 on
  unauthorized advertisement (design.md:595 C2); the mutating POST stays
  gated. Full disclosure: the agent can trigger this mint with no prompt
  (`git ls-remote`/`push --dry-run`), and the minted token is persisted by
  value to the per-run `writes/` ledger for exit-revoke (#20) — so a
  pre-approval write token can sit on disk for the run lifetime, within the
  existing same-uid residual. phase1-design §5.3's "first write of a run is
  still approval-gated" becomes "first *mutation* is still approval-gated".
  Please confirm you accept this.
- **SJ-2** — strict convention means **the git transport can never push to
  `main`/tags**, approved or not (§1.3). NOT a claim that `main` is
  unreachable: post-approval REST git-data writes (refs/contents/merge
  endpoints) retain contents:write on it, unchanged from today. Confirming
  because it's user-visible and easy to over-state.
- **SJ-3** — C2's "never invite an unapproved pack upload" weakens to "at
  most the command section plus a capped drain (8 MiB / 10 s) is read before
  deny" — the drain is what makes the deny report actually reach the agent
  (§1.4). Bounded; confirming.

**Open questions:**
1. **OQ-1 (the one you asked for): confirmation form** — recommendation is
   Form C first-of-run + Form A for expansions (§2.2); Form A everywhere is
   the second choice. Your call.
2. **OQ-2:** non-push write before any confirmed issue: deny-until-declared
   (recommended, §1.5) vs allowing a repo-level confirmation that binds no
   issue (weakens decision D's attribution)?
3. **OQ-3:** later, derive issue bindings from REST paths
   (`/repos/o/r/issues/73/comments`) as an additional declaration channel, or
   keep push-ref as the only channel?
4. **OQ-4:** issue numbers that are PRs: allow with a "[pull request]" label
   in the prompt (recommended — shared number space, plausible workflows) or
   deny?
5. **OQ-5:** one-issue-per-push restriction (recommended for ceremony
   clarity) vs prompting per distinct issue in a single push?
6. **OQ-6:** any appetite for an *optional* `expect_issue:` pin later (closes
   S5 when used, at the cost of re-introducing opt-in up-front config)?
   Default: no.
7. **OQ-7:** on the deny path, is report-status `ng` synthesis (agent-visible,
   ~150 loc) worth it vs plain 403 (near-invisible to the agent, ~0 loc)?
   Recommended: yes — decision C's "clear agent-visible error" isn't
   deliverable otherwise.

**Residuals stated honestly:** S5 stays open in every variant (probe verdict:
inherent ceiling of confirm-by-recognition; backstop is the deferred §8
writeback). S1 is closed mechanically only for the run's *first* issue; the
scope-expansion prompt degrades to attention-checked transcription (§2.2) —
the attacker's cheapest remaining path. Post-approval REST writes can still
touch the default branch (SJ-2). Detection latency for anything the human
waves through remains "until someone reads the audit trail." The run-scoped
approval breadth (phase1-design.md §5.3's same-uid residual) is unchanged by
this proposal.
