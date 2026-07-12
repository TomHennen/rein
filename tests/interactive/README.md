# rein interactive (pexpect) suite — the journey catalogue

Drives the real `rein` binary through a pseudo-terminal with
[pexpect](https://pexpect.readthedocs.io/), against a **live** throwaway repo and
a real srt sandbox. Two kinds of file live here:

| kind | naming | swept by `run.sh`? | deliverable |
|------|--------|--------------------|-------------|
| **journeys** (major user paths) | `journey_*.py` | no (run deliberately) | a checked-in, human-reviewable **golden transcript** (`golden/*.txt`) |
| **plain tests** (edge cases + invariants) | `test_*.py` | yes | a pass/fail assertion; no transcript |

**A journey's deliverable is a RAW golden transcript.** It runs the path live and
writes `golden/<journey>.txt` with the REAL values (issue number, repo, title,
nonce, object counts) — reviewers see reality, not placeholders. On re-run it
compares fresh-vs-golden by NORMALIZING BOTH sides first, so a different issue /
nonce / count still matches while a genuinely new or changed line = drift = red =
"re-review this journey". Authoring rules (the raw/normalize-on-compare model, the
`SBX|` view-split, expect→act→expect, shared helpers, `rein init` setup) are in
**`tests/interactive/CLAUDE.md`**.

## The doctrine: pexpect IS the human. No human is required.

**The write path needs a tty — it does not need a person.** The docs used to say
"the write path needs a human tty" and "write a manual script the human runs".
That was **stale**, and it cost us: agents kept parking write-path verification
for Tom. pexpect gives `rein run` a genuine controlling terminal and answers the
Form A prompt exactly as a developer would, so **an agent can self-verify the
entire write ceremony autonomously** — and should.

Why a pty at all: rein's approval prompt opens **`/dev/tty` directly**
(`internal/ui/prompt`) and reads one line; it approves iff the trimmed line equals
the declared issue number. It never reads stdin. Only a controlling terminal can
drive it.

**This does not weaken the security model.** The **sandboxed** agent (srt
`--new-session`) has no tty *at all*, so it still cannot self-answer — that
boundary is exactly what `journey_write_ceremony.py` puts on screen (the agent's
view and the human's view, side by side). What pexpect drives is the **host-side**
prompt, on the same terminal a real developer types into. The sandbox side is
exercised for real (a live `git push` from inside srt) and asserted independently.

The **only** thing pexpect genuinely cannot drive is a **real browser** — i.e.
GitHub App *creation* via the manifest flow. That, and only that, still needs a
human script (`scripts/cp5-manifest-manual-test.sh`).

### The recipe (copy this)

Setup is the **`rein init` world**, not `source ./dev-env` (the dead-App footgun
HANDOFF warns about). `rein init` configures the App + a dev-session; a journey
resolves its throwaway with `resolve_throwaway_repo` (`REIN_JOURNEY_REPO` → the
configured dev-session → `REIN_TEST_REPO_A` only as a **labeled legacy shortcut**,
so journeys don't depend on `REIN_TEST_REPO_A` special-casing — #40).

```sh
# once per machine: rein init (see HANDOFF.md); this box's legacy shortcut is
# `source ./dev-env`, but the documented path is init.
python3 tests/interactive/journey_write_ceremony.py    # the journey; makes + closes its OWN issue; exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_write_ceremony.py   # regenerate the golden intentionally

# the gated test_*.py take an issue via env (they don't self-create one). The
# declare FETCHES the issue, so it must be REAL (an invented number 404s):
gh issue create --repo <throwaway> --title "..." --body "..."
REIN_ITEST_ISSUE=<n> REIN_ITEST_TITLE_ISSUE=<n> REIN_ITEST_TITLE_WORD=<word-in-title> \
  tests/interactive/run.sh                             # the whole assertion suite
```

## The journey catalogue

`tests/interactive/` is the living catalogue of rein's user journeys; the set of
them **is** the behavioral spec. A behavior-changing PR either updates an existing
journey (regenerate its golden, ship it in the PR) or adds a new one (a new row +
a new `golden/*.txt`). See `tests/interactive/CLAUDE.md` for the authoring rules.

| # | journey | status | where + golden |
|---|---------|--------|----------------|
| 1 | **First-time setup** — `rein init`: repo prompt, opt-in `claude` alias, sandbox-health soft-block, shim/symlink | **PARTIAL** | `test_init_interactive.py` (8 live specs). The App-*creation* half is **UNDRIVEABLE** (browser/manifest) → `scripts/cp5-manifest-manual-test.sh`. Machine-label prompt: decided, unbuilt (xfail). No golden yet — a `journey_init.py` is the next one to write |
| 2 | **The write ceremony** — agent declares an issue → human confirms on the terminal → verified push to `agent/<n>/<nonce>` lands | **COVERED** | `journey_write_ceremony.py` → **`golden/write_ceremony.txt`** + `test_write_approval.py::DeclareConfirmPush` + `test_confirm_shows_title.py` (prompt shows the *fetched* title/state/home-repo) |
| 3 | **The pre-declaration lock** — agent pushes before declaring; the proxy denies with a synthesized `fatal: remote error`, no prompt ever fires | **COVERED** | `test_write_approval.py::PreDeclarationLock` + phase 1 of the ceremony golden |
| 4 | **The denial path** — human types the wrong number; the declare fails and writes stay locked | **COVERED** | `test_write_approval.py::test_wrong_answer_denies_and_writes_stay_locked` (edge case — a plain test, no golden) |
| 5 | **Ref cross-check** — after approval, a non-`agent/<n>/<nonce>` ref is still rejected (#35 decision C) | **COVERED** | `test_write_approval.py::nonmatching_ref_rejected_after_approval` + phase 4 of the ceremony golden |
| 6 | **Scope expansion** — agent (scoped to repo A) declares an issue that lives in repo B, OUTSIDE scope (`rein declare <n> --repo B`); the SCOPE EXPANSION prompt fires with its distinct "this ADDS a repo to the scope ceiling" header, the human approves + answers the persist `[y/N]`, and the widened token lets the agent clone + push to repo B | **COVERED** | `journey_scope_expansion.py` → **`golden/scope_expansion.txt`** (approve → run-only → push-to-B, ONE story) + `test_scope_expansion.py` (the DENY leg + the CROSS-OWNER structural rejection, as plain assertions — no golden) |
| 7 | **Real agent in the sandbox** — interactive `claude` under `rein run`, reaching `api.anthropic.com` | **COVERED** | `test_realagent_e2e.py` (live since CP4.5 landed egress) |
| 8 | **tmux-popup grant** — the DEFAULT TUI path (#37): the confirm prompt fires in a tmux popup, not inline; the operator answers there | **GAP** — pexpect can drive it (spawn inside a `tmux` session, assert the popup fires, `send-keys` the answer), gated on `$TMUX`. Its own journey (`journey_tmux_popup.py`), not yet built. The popup is the default for a TUI agent, so it must not stay untested | — |
| 9 | **Sandbox filesystem boundary** — from INSIDE (#59/#63/#64): credential stores + `~/.ssh` + `~/.aws` + rein's own app key read as *absent*; `$HOME` is ephemeral (a write succeeds into tmpfs, then never persists on the host); the `.git` host-exec escape is CLOSED (`mv .git`→"Device or resource busy", `.git/hooks` + `.git/config` read-only); ordinary edits still `add`/`commit`; and the injected agent contract is shown *verbatim* | **COVERED** | `journey_sandbox_filesystem.py` → **`golden/sandbox_filesystem.txt`** (a deterministic bash "agent" — reproducible, unlike real claude) + gated `test_git_hardening.py` (the `.git` escape, incl. the `config.worktree` edge) + `test_agent_contract.py` (real-claude contract read-back — LLM phrasing varies, so NOT golden material). **Complementary evidence:** `journey_credential_boundary.py` → **`golden/credential_boundary.txt`** proves the same hide with an INDEPENDENT third-party scanner (`bagel`) run as a differential — finds 4 planted creds `--direct`, 0 sandboxed — a sweep that catches un-enumerated paths the `cat`-checks can't (the #55 unknown-unknown class). Skips if `bagel` (GPL-3.0, external CLI only) is absent |
| 10 | **Direct mode (`--direct`)** — the same ceremony without srt | **GAP** — `reinharness.spawn_rein_run(direct=True)` exists and is unused. Cheap journey to add | — |
| 11 | **Misconfig: App not installed on a session repo** | **GAP** — this is issue **#68** (the D4 install-coverage check is skipped entirely on the env-App path). A live journey here would have caught it; the unit tests didn't | — |
| 12 | **Misconfig: broken / expired session file** | **GAP** | — |
| 13 | **Init repo autodetection** — `rein init`'s repo prompt DEFAULT is autodetected from the cwd's git `origin` (issue **#69**/#78): from a checkout of the repo the prompt is PRE-FILLED with the detected `owner/name` (Enter accepts); from a NON-git dir the prompt is bare — proving it is cwd-derived, not hardcoded. `rein run` with no session likewise hints the detected repo | **COVERED** | `journey_init_autodetect.py` → **`golden/init_autodetect.txt`** (the two prompt legs) + the run-hint as a plain assertion inside it (the `--direct` warning banner keeps it out of the golden) |
| 14 | **Session commands** — the HUMAN-side `rein session show` / `rein session add-repo <owner/name>` (issue **#69**, mocks §2): `show` prints the standing scope ceiling with per-repo LIVE install-coverage (`[App installed]`) and any live-run deltas (`live runs: none` when none); `add-repo` VALIDATES at write time (same-owner + install-coverage probe) then widens the ceiling, and the next `show` lists the new repo. Also fills the hole that `rein session show` had NO test of any kind | **COVERED** | `journey_session_commands.py` → **`golden/session_commands.txt`** (show → add-repo B → show, ONE story; the CROSS-OWNER reject is a plain assertion beside it, not in the golden) |
| 15 | **404-at-expansion install NOTICE** — the sibling of scope expansion (issue **#69**, mocks §1.4/§5.2): the agent declares an expansion to a repo the App is NOT installed on (same owner, so it passes the cross-owner check and reaches the coverage probe). Nothing is approvable, so NO prompt fires — the human gets an interactive NOTICE (names the repo, "there is no approval to give", install deep-link), and the declare REFUSES with the agent-facing install-then-retry message | **COVERED** | `journey_expansion_404.py` → **`golden/expansion_404.txt`** (the host-tty NOTICE + the SBX-tagged agent refusal, in one interleaved terminal) |

Statuses: **COVERED** (a file drives it), **PARTIAL** (some of it), **GAP** (real
journey, no demo yet), **UNDRIVEABLE** (needs a browser — say so and move on).

## Prerequisites

- **A working App + a throwaway repo.** `rein init` configures the App and a
  dev-session (the documented path); the repo is resolved by
  `resolve_throwaway_repo` (`REIN_JOURNEY_REPO` → the configured dev-session →
  `REIN_TEST_REPO_A` as a **legacy this-box shortcut**). Hard-constraint #1: the
  suite touches **only** that one throwaway. `source ./dev-env` still works on
  this VM but is the dead-App footgun HANDOFF warns about — prefer `rein init`.
- **A healthy sandbox stack:** `srt`, `bwrap`, `socat`, `ripgrep`, and working
  unprivileged user namespaces. (`rein doctor` checks these.)
- **`python3` + `pexpect`** (developed against 4.9.0).
- **Host `gh` authed** as the repo owner — used only for host-side branch
  *verification* and *cleanup* (the operator's own token, never the sandbox).
- **No pytest needed.** The suite uses the stdlib `unittest`. (This VM has no
  pip, and installing pytest would need a privileged `apt`, which we avoid.)

## How to run

```sh
tests/interactive/run.sh                              # whole suite
tests/interactive/run.sh test_write_approval          # one module
tests/interactive/run.sh test_write_approval.WriteApprovalLoop.test_wrong_answer_denies_and_push_fails_cleanly
```

`run.sh` sources `./dev-env`, builds `rein` + shims, and runs `unittest`. It is
**never** run by `go test ./...` (there are no `.go` files here), so the Go
suite stays untouched.

## What's covered

### `test_write_approval.py` — the #35 declare-first write loop, LIVE

Each test drives writes **initiated from inside the sandbox** and asserts
**both sides** of the loop:

- **HOST:** the Form A declaration prompt appeared (or, pre-declaration, did
  NOT), and the correct `[approved]` / `[denied ...]` marker printed.
- **SANDBOX:** the in-sandbox commands' own outcomes via explicit sentinels —
  `SBX_DECLARE1_RC=<code>` for the `rein declare <n>` call and
  `SBX_PUSH<n>_RC=<code>` per push — **never a hang**. (`rein run`'s own exit
  code is *not* used: the in-sandbox script runs under `set +e`.)

Cases:

| test | gate | asserts |
|------|------|---------|
| `push_without_declare_fails_with_remote_error` | always runs | pre-declaration push → `fatal: remote error: rein: writes are locked …` (the synthesized ERR advertisement), **no prompt**, reads still flow, no branch |
| `declare_approve_then_push_succeeds` | `REIN_ITEST_ISSUE` | declare → Form A prompt → type the number → `[approved]` → push to `agent/<n>/<nonce>` RC 0, exactly 1 prompt, branch lands |
| `wrong_answer_denies_declare_and_writes_stay_locked` | `REIN_ITEST_ISSUE` | wrong number → `[denied]`, declare RC≠0, the following push is still locked, no branch |
| `one_declare_covers_a_second_push` | `REIN_ITEST_ISSUE` | one confirmation covers the run: two pushes, **exactly one** prompt, both branches land |
| `nonmatching_ref_rejected_after_approval` | `REIN_ITEST_ISSUE` | a confirmed run pushing a non-`agent/<n>/<nonce>` ref sees `! [remote rejected] … refs must match agent/…` (decision C), no branch |

Prompt counting is whole-transcript (`"agent declares work on an issue"`
occurrences), so a spurious re-prompt would be caught, not masked.

The declare **fetches the real issue** before prompting (decision E), so the
declare-flow tests need `REIN_ITEST_ISSUE` set to a real (open) issue number on
the throwaway repo — create one once and export it. Each test still **pins its
own session** (a temp repo-only `dev-session.yaml` via `REIN_SESSION_FILE`; the
retired `issue:` field is never written).

### `test_init_interactive.py` — the interactive `init`, LIVE (was TDD-red)

Drives real `rein init` runs under a pty. **Updated 2026-07-11:** this file used
to skip four "open decision §8.x" specs; two of those decisions shipped in CP4.6
(PR #42), so they are now REAL tests, verified against the binary:

| §  | decision | status | encoded as |
|----|----------|--------|-----------|
| 8.2 | sandbox gating | **SHIPPED** — SOFT-block: an unhealthy stack warns loudly, init still exits 0; `--require-sandbox` makes it a hard gate | 3 passing tests (incl. a healthy-stack control). The unhealthy stack is induced by running init with `srt` off `PATH` |
| 8.3 | agent alias | **SHIPPED** — OPT-IN, default OFF: `--alias` installs, `--no-alias` wins over it, and a real tty with neither gets `[y/N]` defaulting to N | 5 passing tests, incl. the tty prompt genuinely firing and being answered |
| 8.1 | machine label | **DECIDED, NOT BUILT** — prompt pre-filled with the detected hostname, editable (design §4). Today the App is `rein-<role>-<random10>` and init never asks | `expectedFailure` (TDD-red), *not* a skip — it flips to "unexpected success" when CP4.7 lands |
| 8.4 | `doctor --fix` scope | **GENUINELY OPEN** | still `skip` — we must not encode a decision Tom hasn't made |

Plus one `expectedFailure` for the headless install-link (design §5, unbuilt).

**On a real tty, init asks TWO questions in order** — "Which repo…?" then the
alias `[y/N]`. A test that drives the pty without `--yes` **must** answer the
first or it hangs on it. `--yes` suppresses both (that's the headless/CI path).

**Safety:** every init run is confined to a throwaway `HOME` + XDG tempdir and
keeps `REIN_APP_*` present, so it can never mutate the real environment nor trip
the ~25-minute manifest/browser flow. Runs pass `--no-symlink --skip-mint-check`.
The alias tests deliberately DROP `--no-alias` (it would suppress the very
behavior under test); the isolated `HOME` is what keeps them safe — the rc file
they write is the tempdir's.

### `test_realagent_e2e.py` — the real-agent loop, LIVE

A real `claude` running interactively inside the sandbox under `rein run`. Was
skipped while CP4.5 (sandbox egress) was outstanding; CP4.5 landed, so it runs.

### `journey_write_ceremony.py` — journey #2, with a GOLDEN

The write ceremony's journey. One real `rein run`, split into the two views whose
gap *is* the security argument — the **agent** in-sandbox (pre-declaration push
denied → `rein declare <n>` → verified push succeeds → non-convention ref
rejected) and the **human** on the tty (the Form A prompt carrying the fetched
title/state/home-repo, then `[approved]`). It asserts the ceremony held (rc per
phase, exactly one prompt, the right branch landed), then builds the **raw**
transcript and compares it — **normalizing both sides** — to
**`golden/write_ceremony.txt`**.

- Exit **0** = ceremony held AND the normalized fresh run matches the golden.
- Exit **1** = drift; the normalized diff prints and the raw fresh transcript is
  dropped to a scratch path. `REIN_UPDATE_GOLDEN=1` adopts the new raw golden.
- Exit **2** = the ceremony itself broke (a phase rc/prompt/branch was wrong).

The golden file is **RAW** (real repo/issue/nonce/counts) so a reviewer sees
reality; determinism lives in the comparator, which normalizes both sides. Every
terminal line is kept, so a brand-new `rein:` line trips drift (it caught the
exit-time token-revoke lines a whitelist had dropped). `REIN_SHOW_NORMALIZED=1`
prints the comparison lens. The two views appear inline — the in-sandbox script
runs commands through the `run` helper (`reinharness.sandbox_preamble`), which
echoes each as `SBX| $ <command>` then tags its output, so the transcript reads
like a real terminal (command → output → command) and agent-vs-host is visible
without splitting. The steps run **expect→act→expect** — each emits an
`@PHASE..` sentinel the test waits on in order, and the declare's host prompt is
answered live between them.

**Self-contained:** creates its own throwaway issue via `gh`, deletes both
branches and closes the issue in a `finally`. Reuse an issue with
`REIN_DEMO_ISSUE=<n>` (then it is left open). Repo resolved via
`resolve_throwaway_repo` (rein-init way first; #40).

**Out of the `run.sh` sweep** (slow — a full sandboxed clone + four round-trips).
`run.sh` discovers `test_*.py`; journeys are `journey_*.py`, run on demand:

```sh
python3 tests/interactive/journey_write_ceremony.py    # one journey
tests/interactive/run-journeys.sh                      # ALL journeys: regenerate goldens + report drift
```

### `journey_app_not_installed.py` — MISCONFIG: App not installed on a session repo (#68)

A **journey** (not swept by `run.sh`, which only discovers `test_*.py`): it both
SHOWS and ASSERTS the #68 fix — the D4 install-coverage check that early-returned
on the `REIN_APP_*` env path, so an uncovered session repo used to launch happily
and fail *inside* the agent. Two legs, both a real `rein run --direct` (the
coverage gate runs before the mode split, so `--direct` exercises it without the
sandbox stack):

- **misconfig:** a session naming a fictional `<owner>/definitely-not-installed`
  (a FIXED name — stable-by-construction, so not normalized; does not exist and
  404s identically to "App not installed on repo", so it touches **no real repo**
  — hard-constraint #1 holds). rein must refuse at LAUNCH, exit 1,
  and the refusal must name the repo, name the App (slug), and carry the
  App-specific `.../installations/new` deep-link. The inner command never runs.
- **control:** a normal single-repo session on the throwaway clears the gate and
  the inner command actually runs, exit 0.

Run it: `python3 tests/interactive/journey_app_not_installed.py`. A normalized
golden capture lives at `golden/app_not_installed.txt`.

> **Journey-catalogue note.** PR #72 (branch `e2e-suite-doctrine`) rewrites this
> README into a numbered journey catalogue where this is **row 10, "Misconfig: App
> not installed on a session repo"** — currently a **GAP**. This file is the demo
> that row points to; **row 10 flips GAP → COVERED when #72 and this branch both
> land.** The table itself isn't on this branch, so it's reconciled at merge (see
> the PR body). When #72's golden-transcript helpers land, this journey should
> adopt them (its normalization is deliberately simple and local until then).

### `journey_scope_expansion.py` — journey #6, with a GOLDEN

The scope-expansion journey. One real `rein run` whose session is scoped to **repo
A only**; the in-sandbox agent runs `rein declare <issueB> --repo B` for an issue
that lives in **repo B, OUTSIDE that scope**. That fires the **SCOPE EXPANSION**
prompt on the host tty (the distinct "this ADDS a repo to the scope ceiling"
header, carrying the fetched title/state/home-repo). pexpect approves with the
issue number, then answers the persist `[y/N]` with **N** — the run-only path (a
`y` would mutate the session file; that leg is the plain test). The widened token
then lets the agent clone repo B into its writable `$TMPDIR` (binds are fixed at
launch, so B can't nest in A's working tree — #64) and push `agent/<issueB>/<nonce>`
onto B. It asserts the expansion held (rc per phase, exactly one expansion prompt
and zero plain prompts, the branch landed on B, persist=N left the session file
unchanged), then builds the **raw** transcript and compares it — **normalizing both
sides** — to **`golden/scope_expansion.txt`**.

- Exit **0** = expansion held AND the normalized fresh run matches the golden.
- Exit **1** = drift (normalized diff printed; raw fresh transcript dropped to a
  scratch path; `REIN_UPDATE_GOLDEN=1` adopts the new raw golden).
- Exit **2** = the expansion itself broke (a phase rc / prompt-count / branch /
  persist invariant was wrong).

The **DENY** leg and the **CROSS-OWNER** structural rejection are deliberately NOT
in this golden — they are edge-case invariants with no reviewable narrative, so
they live as plain assertions in `test_scope_expansion.py` (`ScopeExpansionDeny`,
`ScopeExpansionCrossOwner`). The golden stays the single approve→push-to-B story.

**The fixture issue is LONG-LIVED, not per-run.** The golden bakes repo B's issue
number + title RAW and un-normalized, so they must be stable-real. `ensure_fixture_issue`
finds (or reopens, or creates) an OPEN issue titled *"rein journey: scope-expansion
fixture (safe to close)"* on repo B and leaves it open — so `[open]` in the prompt
is stable too. Override with `REIN_ITEST_ISSUE_B=<n>`.

**Self-contained writes:** the only durable side effect is the agent's branch on
repo B, deleted in a `finally`; the fixture issue is left OPEN for reuse. Touches
only the two throwaways (hard-constraint #1): repo A via `resolve_throwaway_repo`
(#40), repo B via `REIN_TEST_REPO_B` (same owner — the App installation is
single-owner).

```sh
python3 tests/interactive/journey_scope_expansion.py   # one journey; exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_scope_expansion.py   # regenerate the golden
```

### `journey_init_autodetect.py` — journey #13, with a GOLDEN (#69/#78)

The cwd-autodetection journey. #78 made `rein init`'s repo-prompt DEFAULT the repo
the human is STANDING IN: `cmd/rein/gitremote.go:detectRepoFromGit` reads the cwd's
git `origin` URL → `owner/name`, and `resolveRepoForSession` offers it as the prompt
default. Two legs, both a real interactive `rein init` under a pty (NO --yes, so the
prompt renders), each confined to a throwaway HOME/XDG tempdir:

- **DETECTED:** init runs from a checkout of the throwaway; the repo prompt is
  PRE-FILLED with the detected `[owner/name]`, the human accepts with Enter, and the
  session is scaffolded for the detected repo.
- **CONTRAST:** init runs from a NON-git dir; there is no `origin`, so the prompt has
  NO default (the bare prompt) — proving the default is cwd-derived, not hardcoded.

A PLAIN ASSERTION rides along (NOT in the golden): `rein run` with no session prints
a hint that names the cwd repo (`gitremote.go:noSessionHint`) — from the checkout it
names `rein init --repo <repo>`, from a NON-git dir it degrades to the generic hint.
Reaching that hint needs `--direct`, whose loud UNSANDBOXED-MODE banner would muddy an
init-focused golden, so it stays an assertion. The "checkout" is a bare `git init` +
`remote add origin` at the throwaway — enough for origin-URL detection, touching no
real repo (hard-constraint #1).

```sh
python3 tests/interactive/journey_init_autodetect.py   # one journey; exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_init_autodetect.py   # regenerate the golden
```

## Disposable branches & cleanup

Each write test creates a clearly-timestamped `reintest-<UTC>-<rand>` branch on
the throwaway and deletes it from the host in teardown (via `gh api -X DELETE`).
Cleanup is best-effort: if a delete fails, a few `reintest-*` branches may
linger — safe to delete by hand. The suite currently leaves the throwaway clean.

## Files

- `CLAUDE.md` — journey-authoring guidance (the generic rules; read before adding one).
- `reinharness.py` — binary build/locate, env loading, the `ReinRun` pexpect
  wrapper (transcript capture, prompt matchers, sentinel parsing), in-sandbox
  script generation, host-side branch verify/delete, isolated-HOME init helpers,
  and the shared **journey** API (`sandbox_preamble`, `SBX_TAG`, `get_views`,
  `build_raw_transcript`,
  `normalize_for_compare`, `compare_golden`, `create_issue`/`close_issue`,
  `resolve_throwaway_repo`).
- `itest_base.py` — `ReinTestCase` (one-time build, env + throwaway repo,
  disposable-branch cleanup) and the unittest/xfail/skip rationale.
- `test_write_approval.py`, `test_init_interactive.py`, `test_realagent_e2e.py`,
  `test_confirm_shows_title.py` (gated on a real issue + a title word; a real
  regression spec for #35's Form A title display — see its docstring).
- `test_golden_shape.py` — stack-free lint: every journey has a golden, and
  `normalize_for_compare` is idempotent on it. Runs in the sweep and standalone.
- `journey_write_ceremony.py` + `golden/write_ceremony.txt` — journey #2 and its
  checked-in RAW golden transcript (not swept by `run.sh`).
- `journey_credential_boundary.py` + `golden/credential_boundary.txt` — row 9's
  complementary scanner-differential: `bagel` run `--direct` (finds 4) vs
  sandboxed (finds 0), as two `run_journey` steps. **Needs the external `bagel`
  CLI** — without it the journey exits **3 = SKIP** (the runner prints "did NOT
  run"; it is never reported as PASS — see CLAUDE.md). Install it with:
  `go install github.com/boostsecurityio/bagel/cmd/bagel@latest`. `bagel` is
  GPL-3.0 and used ONLY as an external CLI — never a go.mod dep.
- `journey_app_not_installed.py` + `golden/app_not_installed.txt` — the
  #68 misconfig journey (row 10; NOT swept; run it deliberately) and its RAW
  golden transcript.
- `demo-transcripts/` — reference captures for the non-journey demos
  (`demo_pat_leak.sh` and the `cmd/rein` Go demos). These are static docs, not
  normalize-on-compare goldens, so they live OUTSIDE `golden/`.
- `journey_scope_expansion.py` + `golden/scope_expansion.txt` — journey #6 (scope
  expansion: declare a repo OUTSIDE scope → approve → push to it) and its RAW
  golden. `test_scope_expansion.py` carries the deny + cross-owner edge cases.
- `journey_init_autodetect.py` + `golden/init_autodetect.txt` — journey #13 (#69/#78:
  `rein init`'s repo-prompt default autodetected from the cwd's git `origin`; the
  bare-prompt contrast + the `rein run` no-session hint ride along as assertions).
- `journey_session_commands.py` + `golden/session_commands.txt` — journey #14 (#69:
  the human-side `rein session show` / `add-repo`; show → add-repo B → show, the
  cross-owner reject as a plain assertion beside the golden). ALSO the first live
  exercise of `rein session show`, which previously had no test at all.
- `journey_expansion_404.py` + `golden/expansion_404.txt` — journey #15 (#69: the
  404-at-expansion install NOTICE; the agent declares an expansion to an uninstalled
  same-owner repo → host-tty NOTICE, no approval, declare refuses).
- `run-journeys.sh` — the on-demand runner: compare each journey to its golden
  (normalized); `REIN_UPDATE_GOLDEN=1` to adopt, `--normalized` to view the lens.
- `recipes/` — per-test setup scripts for the gated tests (e.g.
  `confirm-shows-title.sh`).
- `run.sh` — the gated runner.
