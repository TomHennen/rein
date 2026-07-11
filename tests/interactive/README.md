# rein interactive (pexpect) test suite

Automates the tty-requiring flows that unit tests can't reach — chiefly the
**write-approval loop** — by driving the real `rein` binary through a
pseudo-terminal with [pexpect](https://pexpect.readthedocs.io/). pexpect stands
in for the human at the keyboard.

## Why a pty is required (and why this is legitimate)

rein's write-approval prompt opens **`/dev/tty` directly** (`internal/ui/prompt`)
and reads one line; it approves iff the trimmed line equals the session's issue
number. It does **not** read stdin. So the only way to drive it is to give
`rein run` a *controlling terminal* — exactly what a pty provides.

This does not weaken the security model. The **sandboxed** agent (srt
`--new-session`) has no tty at all, so it still cannot self-answer. What pexpect
drives is the **host-side** prompt on the terminal where `rein run` was launched
— the same terminal a real developer types into. The sandbox side is exercised
end-to-end (a real `git push` from inside srt) and its outcome is asserted
independently.

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

### `test_init_interactive.py` — TDD-RED for the interactive `init`

Encodes the **settled** parts of `docs/onboarding-ux-design.md` as executable
specs for a build that does not exist yet. Today `rein init` is fully
non-interactive, so:

- **Settled specs** → `unittest.expectedFailure` (== pytest xfail): headless
  init prints a browser/install link and doesn't hang; (the two issue-prompt
  specs that used to live here were REMOVED — decision A/#35 settled that init
  must never ask for an issue). These fail **cleanly** today and will flip to
  "unexpected success" — turning the suite red as a promote-me signal — once
  the feature ships.
- **Open decisions (§8)** → `unittest.skip`: machine-name prompt-vs-default,
  sandbox gating, multi-agent alias, `doctor --fix` scope. Not encoded — Tom
  hasn't decided them.

**Safety:** every init run is confined to a throwaway `HOME` + XDG tempdir and
keeps `REIN_APP_*` present, so it can never mutate the real environment nor trip
the ~25-minute manifest/browser flow. All runs pass
`--no-alias --no-symlink --skip-mint-check`.

### `test_realagent_e2e.py` — the real-agent loop, SKIPPED (blocked on CP4.5)

The aspirational end state: a real `claude -p` agent inside the sandbox whose
push trips the same host prompt. **Skipped** because CP4.5 (sandbox egress) has
not landed — `claude` can't reach `api.anthropic.com` yet. Unskip and fill in
after CP4.5; the harness already has everything the flow needs.

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
  `test_confirm_shows_title.py` (gated + human-run; a real regression spec for
  #35's Form A title display — see its docstring).
- `recipes/` — per-test manual setup scripts for the gated, human-run tests
  (e.g. `confirm-shows-title.sh`).
- `run.sh` — the gated runner.
