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

### `test_write_approval.py` — the write-approval loop, LIVE and green

Each test drives a write **initiated from inside the sandbox** and asserts
**both sides** of the loop:

- **HOST:** the `/dev/tty` prompt actually appeared, and the correct
  `[approved]` / `[denied ...]` marker printed.
- **SANDBOX:** the in-sandbox `git push`'s own outcome, captured via an explicit
  `SBX_PUSH<n>_RC=<code>` sentinel — blocks-then-succeeds (RC 0) on approval,
  fails cleanly (git fatal 403, RC 128) on denial, **never a hang**. (`rein
  run`'s own exit code is *not* used: the in-sandbox script runs under `set +e`
  so a denied push can't abort it, which means `rein run` exits 0 either way —
  the sentinel is the ground truth for what the agent experienced.)

Cases:

| test | asserts |
|------|---------|
| `correct_approval_completes_the_in_sandbox_push` | approve → host `[approved]`, in-sandbox push RC 0, exactly 1 prompt, branch appears on the throwaway |
| `wrong_answer_denies_and_push_fails_cleanly` | wrong number → host `[denied]`, push RC≠0, **exactly 1 prompt** (git does not re-prompt on 403), agent sees a coherent `remote: … was not approved` message, no branch |
| `run_scoped_approval_covers_a_second_write` | two in-sandbox pushes in **one** run → **exactly one** prompt (run-scoped record short-circuits the second), both branches land |
| `no_issue_session_blocks_writes_without_prompting` | a session with **no** `issue:` → write denied with **no prompt**, reads still flow (`SBX_CLONE_OK`), the no-issue block message fires |

Prompt counting is whole-transcript (`"write access requested"` occurrences), so
a spurious re-prompt would be caught, not masked.

Each write test **pins its own session** (a temp `dev-session.yaml` selected via
`REIN_SESSION_FILE`, binding the throwaway repo to a fixed issue number) and
derives the typed answer from it — so the suite does **not** depend on the
machine's ambient `~/.config/rein/dev-session.yaml`. That's why no session file
is listed under prerequisites.

### `test_init_interactive.py` — TDD-RED for the interactive `init`

Encodes the **settled** parts of `docs/onboarding-ux-design.md` as executable
specs for a build that does not exist yet. Today `rein init` is fully
non-interactive (reads `REIN_TEST_REPO_A`, hardcodes `issue: 1`), so:

- **Settled specs** → `unittest.expectedFailure` (== pytest xfail): init prompts
  for the backing issue; init honors the answered issue number; headless init
  prints a browser/install link and doesn't hang. These fail **cleanly** today
  (short timeouts, no uncontrolled errors) and will flip to "unexpected success"
  — turning the suite red as a promote-me signal — once the feature ships.
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
- `test_write_approval.py`, `test_init_interactive.py`, `test_realagent_e2e.py`.
- `run.sh` — the gated runner.
