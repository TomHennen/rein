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
   approving adds this repo to the scope ceiling
   (all writes to it then flow without further prompts).

To approve, type the issue number (41) and press enter.
To deny, press Ctrl-C or type anything else.
> 41
  [approved for this run]
Also save tomh/wrangle-utils to the session for future runs? [y/N]
> _
```

(Decided — Tom, 2026-07-11: the run-only-vs-persist choice lives **in the
prompt** as a second question, so nobody has to go run a command elsewhere.
The number stays the sole approval token — the [y/N] only chooses persistence
*after* approval has already succeeded, so a stray "y" can never approve
anything by itself. Default is N = run-only, keeping the standing ceiling a
deliberate act.)

On approve with `N` (or enter):

```
rein: scope for this run is now tomh/wrangle, tomh/wrangle-utils (+#41 confirmed)
rein: (not saved; future runs will ask again — or run:
      rein session add-repo tomh/wrangle-utils)
```

On approve with `y`:

```
rein: scope is now tomh/wrangle, tomh/wrangle-utils (+#41 confirmed)
rein: saved to ~/.config/rein/dev-session.yaml — future runs include
      tomh/wrangle-utils without asking. To undo:
      rein session remove-repo tomh/wrangle-utils
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

### 1.3 Approve semantics — DECIDED: in-prompt choice (Tom, 2026-07-11)

Mechanics under the hood are the former "Variant 1" either way: the approval
appends a `ConfirmedIssue` to the run's approval record — which *already
carries a `Repo` field* (#35 proposal §5). An expansion is literally a
`ConfirmedIssue` whose `Repo ∉ sess.Repos`; the run's effective ceiling
becomes `sess.Repos ∪ {approved expansion repos}`, and write mints scope to
that union. If the human answers `y` to the persistence question, rein
*additionally* appends the repo to `dev-session.yaml` at that moment (same
validated write path as `session add-repo`).

Why the [y/N] default is N (run-only): a persist-by-default would make every
in-run "yes" silently widen the *standing* ceiling — a one-off "check that
other repo's README" would permanently grow what every future run can mint
against. Defaulting to run-only keeps the yaml a deliberate artifact while the
in-prompt `y` still saves the multi-repo project from re-prompting every run.
(Rejected earlier: a second approval token like `41!` meaning "and persist" —
the non-replayable ceremony stays single-token, one meaning; the separate
[y/N] keeps approval and persistence as two distinct acts.)

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

## 3. Flow 3 — init-time repo: DECIDED single-repo + autodetect (Tom, 2026-07-11)

**Decided: `init` stays single-repo, and the default is autodetected from the
directory you run it in.** Tom's reasoning: with in-prompt scope expansion
(§1), a repeatable `--repo` at init is unnecessary — the second repo joins the
session the first time the agent actually needs it, one keystroke at the
prompt. (The earlier draft recommended a repeatable `--repo`; dropped.)

The interactive question below is `rein init`'s (asked when `--repo` is
absent, onboarding-ux-design.md §3 step 5, amended). The `[tomh/wrangle]`
default is **autodetected from the cwd's git remote** (`origin` URL →
owner/name; falls back to no default outside a repo or on a non-github
remote):

```
Which repo should the agent work on? [tomh/wrangle (detected from this dir)]:
  (add more later with `rein session add-repo`, or approve the agent's
   expansion requests as it encounters work)
```

**Autodetection also applies to `rein run`:** launched with no session file
(or a session that doesn't include the cwd's repo), rein detects the repo from
the cwd's remote and offers it — instead of the current cold "no session"
error. Exact `run`-side flow to be mocked with the #35 implementation (it
interacts with declare); the decision of record is: **detect the repo the user
is standing in and make it the default everywhere a repo must be named.**

Validation is `add-repo`'s either way: same-owner rule, install-coverage probe
with the step-7 install-on-repo offer.

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

## 5. Decisions (Tom, 2026-07-11 — was "open questions")

1. **Run-only vs persist:** neither picked in advance — **ask in the prompt**
   (§1.2/§1.3 updated): approve with the number, then `Also save to the
   session? [y/N]`, default run-only. No separate command needed.
2. **404-at-expansion surfacing:** OPEN pending clarification. The draft's
   "trains impatient approval" phrasing, in plain words: if the prompt stays
   open while the human wanders off to install the App (a browser flow that
   can take minutes), the habit being trained is answering long-lived prompts
   without re-reading them when they finally get back — the prompt's ceremony
   value decays with its age. Recommendation stands: deny-and-instruct, fresh
   prompt after the install. Tom to confirm.
3. **Naming:** settled — `rein session show|add-repo|remove-repo`.
4. **`remove-repo`:** later (not in the first checkpoint).
5. **Init repo flags:** settled — single-repo init with cwd autodetection
   (§3); repeatable `--repo` dropped as unnecessary given in-prompt expansion.
   ("Anchor drift" was jargon for: creeping back toward the up-front-
   configuration model design.md:12 rejects. With expansion-at-encounter in
   place, the concern is moot.)
6. **`session show` live-run view:** folded-in as mocked (orchestrator's call
   on Tom's "IDK" — it answers the question #53 actually raised, and
   unfolding it later is cheap).

---

## 6. Direct mode (`--direct`) — REVISED: unified (Tom, 2026-07-11)

*(This section's first draft said "sandboxed-only in v1"; Tom called that bad
UX and asked whether support was hard. It isn't — it's easier: unsandboxed
`rein declare` needs no proxy relay. The #35 design doc now specifies full
unification; summary here for the scope flows.)*

- **Scope expansion via `declare` (§1): works in BOTH modes.** Direct-mode
  `rein declare 41 --repo o/r` runs as a plain CLI: fetches the title, fires
  the same tty/tmux grant prompt (with the same in-prompt `[y/N]` persist
  question), writes the same approval record; the credential helper then
  mints against the run's confirmed union. Pre-declaration writes get the
  placeholder credential + a stderr hint naming the declare command (the #45
  deny channel — no proxy exists to synthesize richer errors).
- **`rein session show|add-repo` and init/run cwd-autodetection (§2/§3):
  mode-independent**, identical in both modes.
- **Issue binding (#35): unified — `sess.Issue` retires in BOTH modes** (one
  binding model; see the #35 doc §7). Direct-mode caveats, both pre-existing
  Shape-B residuals: no push-ref cross-check (refs invisible outside the
  proxy, C1), and the shared-terminal self-answer residual (#12) applies to
  the declare prompt as to today's write prompt.
- **deny-`$HOME` (#59): sandboxed-only by definition.** Direct mode sees the
  full filesystem — that's what the `--direct` banner warns about — which
  also makes §7's clone-location question moot there (existing checkout
  usable directly).

---

## 7. Expansion × sandbox filesystem — where does the second repo's checkout live? (Tom, 2026-07-11)

The scope-expansion prompt grows the **credential** ceiling; it cannot grow
the **filesystem**: bwrap binds are fixed when the sandbox launches, so no
mid-run approval can make a new path writable. That constraint sorts the
question into two cases:

**In-run (the expansion just approved):** the agent clones the new repo into
an already-writable spot and the clone is *ephemeral*:

- Under #59, denied `$HOME` is an empty **writable** tmpfs — a clone there
  works fine and evaporates at run end. `/tmp` likewise.
- Ephemeral is acceptable *by design*: in rein's model the durable artifact is
  the **push** (`agent/<issue>/<nonce>` lands on GitHub), not the local tree.
  The human `git fetch`es the branch into their own checkout afterward.
- The approve message should therefore tell the agent where to go, e.g.:
  `rein: clone it under $HOME/work/ (writable, discarded at run end)` — and
  steer it AWAY from cloning inside the current workdir, where a nested repo
  risks being committed into repo A's tree.

**The user's existing local checkout of repo B:** deliberately NOT writable
in-run — it is either hidden (under `$HOME`, #59) or read-only (other mounts,
e.g. /mnt/dev). That's a feature: it may hold uncommitted human work, and an
expansion approval shouldn't silently hand the agent a live working tree the
human never staged for agent use. The next-run path: after
`rein session add-repo` (or the in-prompt `y`), a future launch can bind the
existing checkout writable — plumbing already exists (`ExtraAllowWrite`), what's
missing is UX: plausibly a `worktrees:` map in the session yaml
(repo → local path, validated at launch like everything else) or a
`rein run --workdir-for owner/repo=PATH` flag. Deferred to dogfood; tracked in
the follow-up issue.

Related: the #59 "warm `go` builds may hit the module-cache lock" note is the
same missing primitive (a scoped write-hatch) seen from the other side.
