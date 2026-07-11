# Session scope UX mocks — multi-repo without hand-editing YAML

**Status:** DRAFT mocks for Tom's review (2026-07-11). Written after the PR #53
discussion surfaced that the only way to get a multi-repo session today is
hand-editing `repos:` in `~/.config/rein/dev-session.yaml`. Mocks + rationale
only — decisions come after Tom reacts. No code.

**Design anchor (design.md:12):** *"The developer's role is to approve scope
expansions as the agent encounters them, not to configure sessions up front."*
Everything below is measured against that line. The mocks compose with the #35
declaration-first model (Addendum, settled 2026-07-11): the agent declares an
issue via `rein declare <n>` on the in-sandbox virtual host, the human confirms
with Form A (number displayed, fetched title + repo always shown), and prompts
never fire inside the push relay.

**The one-sentence proposal:** repo scope grows the same way issue scope does —
the agent hits the wall, gets an instructive deny naming the exact command, the
declare channel carries the request, and the human approves on the same prompt
surface as write approval. `rein session add-repo` exists for the human who
already knows; the YAML stays the standing ceiling but stops being hand-edited.

---

## 1. Flow 1 — runtime scope expansion (the design-anchored path)

### 1.1 What the agent sees at the wall

The agent, mid-run on `tomh/wrangle`, tries to touch `tomh/wrangle-utils`
(clone, fetch, push, or `gh`/REST — all hit the proxy's scope ceiling). The
deny is the teacher, delivered at the moment of need (the TM-G8 pattern; same
shape as the #35 Addendum's "writes are locked" ERR pkt and issue #59's
say-exactly-how-to-fix-it denial requirement).

git path (ERR pkt on the advertisement, synthesized locally — GitHub is never
contacted for an out-of-scope repo):

```
$ git fetch https://github.com/tomh/wrangle-utils.git
fatal: remote error: rein: tomh/wrangle-utils is outside this session's scope
    (session repos: tomh/wrangle).
    To request access, run: rein declare <issue> --repo tomh/wrangle-utils
    The human will be asked to approve on their terminal.
```

gh / REST path (local 403, JSON `message` carries the same instruction — `gh`
prints it):

```
$ gh issue view 41 --repo tomh/wrangle-utils
GraphQL: rein: tomh/wrangle-utils is outside this session's scope (session
repos: tomh/wrangle). To request access, run:
rein declare <issue> --repo tomh/wrangle-utils
```

The agent then declares (same command the #35 flow already defines — `--repo`
is the Addendum's `?repo=` parameter, promoted from multi-repo disambiguator to
the expansion trigger):

```
$ rein declare 41 --repo tomh/wrangle-utils
rein: requesting approval from the human (this may take a moment)...
```

The call blocks until the human answers, exactly like a first-of-run declare.

### 1.2 What the human sees

Same layered channel as write approval (existing approval → /dev/tty → tmux
popup (#37) → stderr instruction to run `rein approval grant --run-id <id>`),
same compact block format as the existing prompt (`internal/ui/prompt`), Form A
token (settled: number displayed; fetched title + state + home repo REQUIRED
per decision E). One new line makes the *expansion* unmistakable — the prompt
must say the ceiling is growing, not just name an issue:

```
=== rein: SCOPE EXPANSION requested ===
   session:   sess_dev_001 (role=implement, repos=[tomh/wrangle])
   agent asks to ADD repo:  tomh/wrangle-utils
   for issue:  #41 "Update SBOM serialization"  [open]
               in tomh/wrangle-utils
   approving adds this repo to the scope ceiling for THE REST OF THIS RUN
   (all writes to it flow without further prompts; not saved to the session).

To approve, type the issue number (41) and press enter.
To deny, press Ctrl-C or type anything else.
>
```

On approve:

```
  [approved]
rein: scope for this run is now tomh/wrangle, tomh/wrangle-utils (+#41 confirmed)
rein: to make tomh/wrangle-utils permanent for future runs:
      rein session add-repo tomh/wrangle-utils
```

Agent side, the blocked declare returns:

```
rein: approved — issue #41 in tomh/wrangle-utils is confirmed for this run.
      Push to branches named agent/41/<nonce>.
```

On deny:

```
  [denied: input did not match the issue number]
```

```
$ rein declare 41 --repo tomh/wrangle-utils
rein: DENIED by the human. tomh/wrangle-utils remains out of scope for this
      run. Continue working within: tomh/wrangle.
```

The run continues at its original scope; nothing is torn down (deny ≠ error,
per the #35 decision table).

### 1.3 Approve semantics — run-only vs persisted (both mocked; Tom picks)

**Variant 1 — run-only (recommended).** The approval appends a
`ConfirmedIssue` to the run's approval record — which *already carries a
`Repo` field* (#35 proposal §5). An expansion is literally a `ConfirmedIssue`
whose `Repo ∉ sess.Repos`; the run's effective ceiling becomes
`sess.Repos ∪ {approved expansion repos}`, and write mints scope to that union
(preserving issue #10's token-scope == ceiling invariant). `dev-session.yaml`
is untouched; the next run re-prompts if the agent needs the repo again. The
completion message prints the `session add-repo` one-liner (above) so
promotion to permanent is one copy-paste — the #59 self-serve-remediation
philosophy.

**Variant 2 — persist on approve.** Same prompt, but approval also appends the
repo to `dev-session.yaml`:

```
  [approved]
rein: scope is now tomh/wrangle, tomh/wrangle-utils
rein: saved to ~/.config/rein/dev-session.yaml — future runs include
      tomh/wrangle-utils without asking. To undo:
      rein session remove-repo tomh/wrangle-utils
```

**Trade-off:** Variant 2 saves one command for the genuinely-multi-repo
project, but it makes every in-run "yes" silently widen the *standing* ceiling
— a one-off "check that other repo's README" permanently grows what every
future run can mint against, and the ceiling only ever ratchets up. Variant 1
keeps the yaml a deliberate artifact (changing it is its own act), keeps the
in-run ceremony consequence-bounded ("this run" is easy to reason about at a
prompt), and costs one printed command when permanence is actually wanted.
Run-only also matches the anchor most literally: the session forms itself per
run; standing config stays minimal. Recommendation: **Variant 1**. (A middle
path — a second token like `41!` meaning "and persist" — is rejected: the
non-replayable ceremony should stay single-token, one meaning.)

### 1.4 The install-coverage wrinkle (#53's 404)

Before prompting the human, rein probes installation coverage for the new repo
(same probe PR #53 runs at launch). On a definitive 404, **don't prompt** — an
un-approvable prompt trains rubber-stamping. Deny the declare with the
deep-link, and echo one notice line to the run terminal (the shared banner
channel), so the human sees it even if the agent buries it:

Agent side:

```
$ rein declare 41 --repo tomh/wrangle-utils
rein: cannot request tomh/wrangle-utils — the GitHub App
      rein-primary-toms-laptop-a1b2 is not installed on it.
      The human must install it first:
      https://github.com/apps/rein-primary-toms-laptop-a1b2/installations/new
      Then run this command again.
```

Run terminal (one line, no prompt):

```
rein: agent asked for tomh/wrangle-utils, but the App isn't installed there.
      Install at https://github.com/apps/rein-primary-toms-laptop-a1b2/installations/new
      — the agent will retry after that.
```

Transient (non-404) probe errors: fail the declare closed with "could not
verify, retry" (matching the #35 title-fetch-fails rule), NOT the launch path's
warn-and-continue — at launch there's a cached install id to fall back on; here
there is nothing to fall back to.

**Security notes (flow 1):**
- **Same-owner is enforced before anything else.** `rein declare 5 --repo
  otherorg/thing` denies immediately: `rein: session is scoped to owner
  tomh (the App installation is single-owner); otherorg/thing cannot be added.
  A separate session/App would be required.` No prompt fires — cross-owner is
  structurally impossible (session.Validate; BareRepoNames minting), not a
  human decision.
- **The approval-record signature stays over the yaml session** (ID/Role/Repos)
  — expansions are *content* of the record, like confirmed issues, so a
  hand-widened yaml mid-run still invalidates the whole record (existing #35
  §3 rule) while an approved expansion doesn't.
- **No conflict with #35's prompts:** an expansion IS a declare — same command,
  same handler, same prompt form, same `PendingIssue` snapshot for the
  popup/grant path. The only delta is the "ADD repo / ceiling grows" banner
  lines when the declared repo is outside `sess.Repos`. Decision B holds: the
  only sub-session prompt is still scope expansion.
- **The prompt states the blast radius** ("all writes to it flow without
  further prompts") because approve-once-per-run means this is true and the
  human should approve knowing it.

---

## 2. Flow 2 — explicit add (the manual path): `rein session`

New human-side subcommand family (runs outside the sandbox; agents never need
it — their path is Flow 1). Proposed names: `rein session show`,
`rein session add-repo`, `rein session remove-repo`.

### 2.1 `rein session add-repo`

```
$ rein session add-repo tomh/wrangle-utils
rein: checking tomh/wrangle-utils...
  owner:    tomh — matches session owner            OK
  install:  rein-primary-toms-laptop-a1b2 covers it  OK
rein: added. Session repos are now:
  - tomh/wrangle
  - tomh/wrangle-utils
Takes effect on the NEXT `rein run`. A live run keeps its launch-time scope —
inside a run, the agent requests expansion with `rein declare`.
```

Validation failures, each self-serve (#59 style):

```
$ rein session add-repo otherorg/thing
rein: cannot add otherorg/thing — session sess_dev_001 is scoped to owner
      tomh, and a session must stay single-owner (the App installation is
      single-owner; a mixed list would mint ambiguous token scopes).

$ rein session add-repo tomh/private-thing
rein: App rein-primary-toms-laptop-a1b2 is not installed on
      tomh/private-thing. Install it at
      https://github.com/apps/rein-primary-toms-laptop-a1b2/installations/new
      then re-run this command. (Nothing was changed.)

$ rein session add-repo wrangle-utils
rein: "wrangle-utils" is not owner/name-shaped. Did you mean
      tomh/wrangle-utils?
```

Duplicate add is a friendly no-op ("already in the session"). A transient
probe error refuses the add (nothing written) with "could not verify; retry" —
`add-repo` is a durable widening, so it gets the strict rule, not launch's
warn-and-continue.

### 2.2 `rein session show`

```
$ rein session show
session: sess_dev_001   (~/.config/rein/dev-session.yaml)
  role:      implement
  repos:     tomh/wrangle          [App installed]
             tomh/wrangle-utils    [App installed]
  issue:     agent-declared at runtime (confirmed at first write; #35 model)
  egress:    +registry.npmjs.org   (allow_domains)
  created:   2026-07-10 14:02 UTC  (no TTL enforced in Phase 1)

live runs:
  run 8f3a2c…  started 12m ago
    confirmed: #73 "Fix SBOM path handling" in tomh/wrangle
    expansions: tomh/wrangle-utils (#41, this run only)
```

Rationale: `show` is the answer to "what can the agent touch right now?" — the
standing ceiling (yaml) and the live-run deltas (approval records, which
`rein approval status` already knows how to read) in one place. Install
coverage shown per repo because it's the #53 failure class made visible before
launch. If the current CP still has a static `issue:` in the yaml, `show`
prints it with the #35 "ignored in sandboxed mode" warning rather than
pretending it gates anything.

**Security notes (flow 2):** `add-repo` widens the standing ceiling with no
non-replayable ceremony — acceptable because it's the *human* acting outside
the sandbox on their own tty (the same trust level as hand-editing the yaml,
which remains possible); the sandboxed agent cannot reach the command
(`session add-repo` never rides the declare host). Mid-run adds don't leak into
live runs: sessions load at launch, and the approval-record signature already
invalidates on a changed repo list — `show`'s "takes effect on the NEXT run"
line makes that behavior explicit instead of surprising.

---

## 3. Flow 3 — init-time multi-repo: recommend YES, quietly

**Recommendation: accept a repeatable `--repo`, keep the interactive question
single-answer.** Naming two repos you already know you work across is stating
a fact about your workspace, not "configuring sessions up front" — the anchor
rejects making the human *predict scope expansions*, and with #35 settled the
issue (the actual unit of work) stays agent-declared either way. What would
violate the anchor is an interactive init that interrogates for a repo *list*
("any more? any more?") — so the flow keeps one question and points at the two
growth paths:

```
$ rein init --repo tomh/wrangle --repo tomh/wrangle-utils
...
  session: tomh/wrangle, tomh/wrangle-utils  -> ~/.config/rein/dev-session.yaml
  checking App installation... covers both   OK
```

Interactive (onboarding-ux-design.md §3 step 5, amended):

```
Which repo should the agent work on? [tomh/wrangle]:
  (add more later with `rein session add-repo`, or approve the agent's
   expansion requests as it encounters work)
```

Validation is `add-repo`'s: same-owner across all `--repo` values (mixed
owners is a hard init error naming the rule), install-coverage probe per repo
with the step-7 install-on-repo offer extended to cover every named repo, not
just the first.

---

## 4. Flow 4 — does the YAML stay the source of truth?

**Yes — for the standing ceiling only, and it becomes tool-maintained rather
than hand-maintained.** Three layers, each owned by one artifact:

- `dev-session.yaml` = the standing ceiling. Written by `init` and
  `session add-repo/remove-repo`; hand-editing stays legal (Validate already
  fails closed on garbage) but stops being the documented path.
- Run approval records = per-run grants (confirmed issues + run-only repo
  expansions). Ephemeral, swept at exit, never touch the yaml.
- Nothing is "generated": no second file, no cache to drift. The #53/#59
  lesson is that hand-editing *discovers* validation too late — the fix is
  commands that validate at write time, not a generated artifact.

---

## 5. Open questions for Tom

1. **Run-only vs persist on expansion approve (§1.3)** — recommendation is
   run-only + printed `session add-repo` hint. Agree, or is re-prompting every
   run on a real two-repo project too much friction for dogfood?
2. **404-at-expansion surfacing (§1.4)** — deny-and-instruct with a one-line
   terminal notice (recommended), or hold the human prompt open with the
   deep-link in it ("install, then approve")? The latter is one fewer
   round-trip but leaves a long-lived prompt that trains impatient approval.
3. **Command naming** — `rein session show|add-repo|remove-repo` vs a flatter
   `rein scope`? `session` matches the design's vocabulary; `show` vs `status`
   also open (`status` may collide with a future daemon-health `rein status`,
   design §2.5, so `show` is proposed).
4. **`remove-repo` now or later?** Mocked in passing; narrowing is
   security-positive and cheap, but it's one more surface for this checkpoint.
5. **`--repo` repeatable at init (§3)** — confirm this doesn't read as
   anchor-drift to you; the alternative is single-repo init + `add-repo` only.
6. **Should `session show` fold in `approval status`'s live-run view (§2.2),
   or stay yaml-only and point at the existing command?** Mocked folded-in
   because "what can the agent touch right now" is the question the #53
   discussion actually raised.
