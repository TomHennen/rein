# rein interactive (pexpect) suite — the journey catalogue

Drives the real `rein` binary through a pseudo-terminal with
[pexpect](https://pexpect.readthedocs.io/), against a **live** throwaway repo and
a real srt sandbox. Two kinds of file live here:

| kind | naming | swept by `run.sh`? | job |
|------|--------|--------------------|-----|
| **assertion tests** | `test_*.py` | yes | fail when behavior regresses |
| **journeys** | `journey_*.py` | no (run deliberately) | **show** a user journey end to end; output is pasteable into a PR |

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

The declare **fetches the real issue** before prompting (#35 decision E), so the
write journeys need a REAL open issue on the throwaway — an invented number 404s
and fails closed, by design.

```sh
source ./dev-env                                       # this box; a fresh machine follows HANDOFF.md instead
gh issue create --repo "$REIN_TEST_REPO_A" \
  --title "itest: scratch issue for the write ceremony" --body "throwaway"

REIN_ITEST_ISSUE=<n> \
REIN_ITEST_TITLE_ISSUE=<n> \
REIN_ITEST_TITLE_WORD=<a-word-in-that-title> \
  tests/interactive/run.sh                             # the whole assertion suite

python3 tests/interactive/journey_write_ceremony.py    # the journey; makes + closes its OWN issue
```

## The journey catalogue

`tests/interactive/` is the living catalogue of rein's user journeys; the set of
them **is** the behavioral spec. A PR either changes an existing journey (update
its demo, paste the output in the PR) or introduces a new one (add it here). See
the working-style rule in `CLAUDE.md`.

| # | journey | status | where |
|---|---------|--------|-------|
| 1 | **First-time setup** — `rein init`: repo prompt, opt-in `claude` alias, sandbox-health soft-block, shim/symlink | **PARTIAL** | `test_init_interactive.py` (8 live specs). The App-*creation* half is **UNDRIVEABLE** (browser/manifest) → `scripts/cp5-manifest-manual-test.sh`. Machine-label prompt: decided, unbuilt (xfail) |
| 2 | **The write ceremony** — agent declares an issue → human confirms on the terminal → verified push to `agent/<n>/<nonce>` lands | **COVERED** | `journey_write_ceremony.py` (the demo) + `test_write_approval.py::DeclareConfirmPush` + `test_confirm_shows_title.py` (the prompt shows the *fetched* title/state/home-repo) |
| 3 | **The pre-declaration lock** — agent pushes before declaring; the proxy denies with a synthesized `fatal: remote error`, no prompt ever fires | **COVERED** | `test_write_approval.py::PreDeclarationLock` + phase 1 of the journey |
| 4 | **The denial path** — human types the wrong number; the declare fails and writes stay locked | **COVERED** | `test_write_approval.py::test_wrong_answer_denies_and_writes_stay_locked` |
| 5 | **Ref cross-check** — after approval, a non-`agent/<n>/<nonce>` ref is still rejected (#35 decision C) | **COVERED** | `test_write_approval.py::nonmatching_ref_rejected_after_approval` + phase 4 of the journey |
| 6 | **Scope expansion** — agent declares a SECOND issue mid-run; the prompt re-fires with the expansion header | **GAP** — the code ships (`ui/prompt` `Expansion`, `internal/declare`), nothing drives it interactively. Next journey to write. *(Note: expansion is per-ISSUE. A second **repo** mid-run is not a journey — the session's `repos:` is a hard ceiling.)* | — |
| 7 | **Real agent in the sandbox** — interactive `claude` under `rein run`, reaching `api.anthropic.com` | **COVERED** | `test_realagent_e2e.py` (live since CP4.5 landed egress) |
| 8 | **Sandbox hiding** — the agent cannot read credential stores, `~/.ssh`, keyrings | **COVERED, not a journey** — enforced by `rein run`'s own startup self-test + Go tests; there is no human in this loop to demo |
| 9 | **Direct mode (`--direct`)** — the same ceremony without srt | **GAP** — `reinharness.spawn_rein_run(direct=True)` exists and is unused. Cheap journey to add |
| 10 | **Misconfig: App not installed on a session repo** | **GAP** — this is issue **#68** (the D4 install-coverage check is skipped entirely on the env-App path). A live journey here would have caught it; the unit tests didn't |
| 11 | **Misconfig: broken / expired session file** | **GAP** | — |

Statuses: **COVERED** (a file drives it), **PARTIAL** (some of it), **GAP** (real
journey, no demo yet), **UNDRIVEABLE** (needs a browser — say so and move on).

## Prerequisites

- **A live throwaway repo + a working App.** `source ./dev-env` sets `REIN_*`,
  including `REIN_TEST_REPO_A` — a **throwaway** repo. Hard-constraint #1: this
  suite touches **only** that repo. The same App setup `rein doctor` validates.
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

### `journey_write_ceremony.py` — the write ceremony, as a NARRATIVE

Not an assertion test: the ceremony's **showcase**, and journey #2 of the
catalogue. One real `rein run`, replayed as the two views whose gap *is* the
security argument — what the **agent** sees in-sandbox (pre-declaration push
denied → `rein declare <n>` → verified push succeeds → non-convention ref
rejected) and what the **human** sees on the tty (the Form A prompt carrying the
fetched title/state/home-repo, then `[approved]`). It then checks GitHub for which
branches actually landed.

**Self-contained:** creates its own throwaway issue via `gh`, deletes both
branches and closes the issue in a `finally`. Reuse an existing issue with
`REIN_DEMO_ISSUE=<n>` (then it is left open). Git's progress meter is elided so
the output works as a doc/screenshot source; `REIN_DEMO_RAW=1` keeps it.

**Out of the `run.sh` sweep** (it's slow — a full sandboxed clone + four network
round-trips). `run.sh` discovers `test_*.py`; journeys are `journey_*.py`:

```sh
python3 tests/interactive/journey_write_ceremony.py
```

## Disposable branches & cleanup

Each write test creates a clearly-timestamped `reintest-<UTC>-<rand>` branch on
the throwaway and deletes it from the host in teardown (via `gh api -X DELETE`).
Cleanup is best-effort: if a delete fails, a few `reintest-*` branches may
linger — safe to delete by hand. The suite currently leaves the throwaway clean.

## Files

- `reinharness.py` — binary build/locate, env loading, the `ReinRun` pexpect
  wrapper (transcript capture, prompt matchers, sentinel parsing), in-sandbox
  script generation, host-side branch verify/delete, and the isolated-HOME init
  helpers.
- `itest_base.py` — `ReinTestCase` (one-time build, env + throwaway repo,
  disposable-branch cleanup) and the unittest/xfail/skip rationale.
- `test_write_approval.py`, `test_init_interactive.py`, `test_realagent_e2e.py`,
  `test_confirm_shows_title.py` (gated on a real issue + a title word; a real
  regression spec for #35's Form A title display — see its docstring).
- `journey_write_ceremony.py` — journey #2, the narrative demo (not swept).
- `recipes/` — per-test setup scripts for the gated tests (e.g.
  `confirm-shows-title.sh`).
- `run.sh` — the gated runner.
