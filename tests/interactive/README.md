# rein interactive (pexpect) suite

Drives the real `rein` binary through a pseudo-terminal with
[pexpect](https://pexpect.readthedocs.io/), against a **live** throwaway repo and a
real srt sandbox. This README is **system-level**: the doctrine, prerequisites, and a
thin index. **Each journey's own description lives in its dir** —
`journeys/<name>/README.md` is the single source of truth for that journey.

## Two kinds of file

| kind | naming | swept by `run.sh`? | deliverable |
|------|--------|--------------------|-------------|
| **journeys** (major user paths) | `journeys/<name>/journey.py` | no (run deliberately) | a checked-in, human-reviewable **golden transcript** (`journeys/<name>/golden.txt`) |
| **plain tests** (edge cases + invariants) | `test_*.py` | yes | a pass/fail assertion; no transcript |

**When in doubt:** if a human would want to *read what happened*, it's a journey; if
they just want it to stay green, it's a plain test. Full authoring rules are in
[`CLAUDE.md`](CLAUDE.md).

## The doctrine

**A journey's deliverable is a RAW golden transcript.** It runs the path live and
writes `journeys/<name>/golden.txt` with the REAL values (issue number, repo, title,
nonce, object counts) — reviewers see reality, not placeholders. On re-run it compares
fresh-vs-golden by NORMALIZING BOTH sides first
(`reinharness.compare_golden`/`normalize_for_compare`), so a different issue / nonce /
count still matches while a genuinely new or changed line = drift = red = "re-review
this journey". Determinism lives in the comparator, never in the file; extend the
generic `_NORMALIZE_RULES`, never a per-journey whitelist.

**pexpect IS the human. No human is required.** The write path needs a tty — it does
not need a person. rein's approval prompt opens **`/dev/tty` directly**
(`internal/ui/prompt`) and approves iff the trimmed line equals the declared issue
number; only a controlling terminal can drive it, and pexpect gives it one and answers
Form A exactly as a developer would, so **an agent can self-verify the entire write
ceremony autonomously**. This does not weaken the security model: the **sandboxed**
agent (srt `--new-session`) has no tty *at all* and still cannot self-answer — what
pexpect drives is the **host-side** prompt. The **only** thing pexpect cannot drive is
a **real browser** (GitHub App *creation* via the manifest flow) —
`scripts/cp5-manifest-manual-test.sh`.

**Two ways to read a pty: rendered screen vs raw line transcript (#100).** Pick by
asking *does this surface REDRAW?* A **line-oriented** surface (git, rein's
banner/prompts, the `SBX|`-tagged agent script) is asserted on the **raw transcript** —
a line, once printed, is final. A **redrawing** surface (a `claude` TUI, a **tmux
popup**) is asserted on the **rendered screen** (`reinharness.RenderedScreen`, a pyte
terminal emulator): the byte stream is a *history of paint operations*, not a picture,
and `capture-pane` **cannot see** a client-owned popup at all — only the attached
client's pyte render can. Every journey **golden** stays on the raw-transcript path (a
rendered screen is a point-in-time frame — scrolled-off content is gone); screen
rendering applies ONLY to the redrawing surfaces. `pyte` is TEST-ONLY (see
Prerequisites).

## The journey index

The set of these journeys **is** rein's behavioral spec: a behavior-changing PR either
updates an existing journey (regenerate its golden, ship it in the PR) or adds a new
one (a new dir + a new `golden.txt`). Each row links to the journey's own README.

| journey | proves | status |
|---------|--------|--------|
| [onboarding](journeys/onboarding/) | first-run `rein init` guided flow → `rein doctor` (machine-label, install-link) | COVERED |
| [write_ceremony](journeys/write_ceremony/) | the #35 write ceremony: declare → human confirm → verified push lands | COVERED |
| [direct_mode](journeys/direct_mode/) | the same #35 ceremony UNSANDBOXED (`--direct`) | COVERED |
| [scope_expansion](journeys/scope_expansion/) | declare a repo OUTSIDE scope → approve → push to it | COVERED |
| [multi_repo](journeys/multi_repo/) | REAL cross-repo work across a 2-repo ceiling in ONE run | COVERED |
| [sandbox_gh_read_staleness](journeys/sandbox_gh_read_staleness/) | the #95 regression guard: cross-run gh-read staleness | COVERED |
| [tmux_popup_approval](journeys/tmux_popup_approval/) | the DEFAULT approval surface inside `$TMUX` — a real popup (#37) | COVERED |
| [credential_boundary](journeys/credential_boundary/) | the credential hide, proven by an INDEPENDENT `bagel` scanner | COVERED |
| [app_not_installed](journeys/app_not_installed/) | misconfig: App not installed on a session repo (#68) | COVERED |
| [init_autodetect](journeys/init_autodetect/) | `rein init` repo-prompt default autodetected from cwd `origin` (#69/#78) | COVERED |
| [init_steady_state](journeys/init_steady_state/) | `rein init` re-run resolves the App from `state.json`, no `REIN_APP_*` (#128) | COVERED |
| [init_then_run](journeys/init_then_run/) | `rein init` (real mint, no env) then a real `rein run` clone, direct + sandboxed (#128) | COVERED |
| [session_commands](journeys/session_commands/) | the human-side `rein session show` / `add-repo` (#69) | COVERED |
| [expansion_404](journeys/expansion_404/) | the 404-at-expansion install NOTICE (#69) | COVERED |
| [git_author](journeys/git_author/) | delegated commit author "(via rein)", non-impersonating | COVERED |
| [gh_write](journeys/gh_write/) | the in-sandbox `gh` REST + GraphQL write boundary (#91, #101) | COVERED |
| [realagent_write](journeys/realagent_write/) | a REAL claude walks the whole write path (#101) | COVERED |

Statuses: **COVERED** (a journey drives it), **PARTIAL**, **GAP** (real journey, no
demo yet), **UNDRIVEABLE** (needs a browser). Some journeys **SKIP (exit 3)** when a
prerequisite is absent — `credential_boundary` (bagel), `tmux_popup_approval` (tmux /
pyte), `realagent_write` (claude / tmux / pyte), `init_then_run` (no configured App); a
skip is not a pass. Details in each dir's README.

**nono status (after the P3 srt→nono cutover, then the P3-gaps fixes).** The goldens
were regenerated on the nono default. GREEN on nono: `write_ceremony`, `multi_repo`,
`git_author`, `tmux_popup_approval`, `direct_mode`, `expansion_404`, `app_not_installed`,
`init_autodetect`, `init_steady_state`, `session_commands`, and — since the P3-gaps
branch — `gh_write`, `init_then_run`, `onboarding`:
- `gh_write` — FIXED (gap 2): run_nono now gives gh a per-run writable `GH_CONFIG_DIR`
  overlay (the gh twin of #94's `CLAUDE_CONFIG_DIR`) with a placeholder hosts.yml, so gh
  starts and real writes land (403 before declare, 201 after, `gh pr create` succeeds);
  host `~/.config/gh` stays denied.
- `init_then_run`, `onboarding` — FIXED (gap 1): `rein init` now wires `internal/nono.Install`
  to install + digest-verify the pinned nono at `~/.config/rein/nono/bin/nono`, so a fresh
  `git clone` + `rein init` + `rein run` works. (`onboarding` also had a stale cutover
  assertion, `sandbox: srt present`, updated to `nono present`.)

Still Known-RED on nono — each an UNFINISHED item OUTSIDE the P3-gaps scope (a Tom
decision before merge, NOT a cutover-correctness bug), tracked in the P3 cutover report:
- `push_upstream`, and `scope_expansion` — run_nono never wired the `internal/agentenv`
  contract vars (`REIN_UPSTREAM_INTENT_FILE`, `REIN_EPHEMERAL_CLONE_DIR`, `REIN_REPO_WORKTREES`)
  into the profile `set_vars`. The push lands; the upstream-recording / mid-run-clone
  plumbing does not. (Distinct from the P3-gaps gap 3, which briefs the agent *contract
  text* — these are the machine-readable env vars.)
- `sandbox_gh_read_staleness` — harness artifact: the journey's nono state root under `/tmp`
  overlaps nono's own `/tmp` state grant (`Refusing to grant '/tmp' … overlaps protected
  nono state root`). Adapt the harness (state root off `/tmp`) or note.
ENV-BLOCKED (no live claude TUI in this environment, not a nono defect): `realagent_write`,
`claude_resume`. SKIP as before: `credential_boundary` (bagel).

**SECURITY RESIDUAL surfaced by retiring `test_git_hardening.py` (Tom decision).** srt
ro-bound `<tree>/.git/hooks` + `.git/config` to stop an agent planting a git hook that runs
AS THE DEVELOPER on the host at their next git op (#64). run_nono grants the working tree
AND every mapped checkout fully writable via nono `--allow` — including `.git` — with no
read-only carve-out. Under Landlock there is NO "deny under an allowed parent" (the same
limit that blocked the tmux-socket deny, design §3e), so srt's mechanism cannot be ported
as-is. So the `.git`-hooks host-RCE threat SURVIVES under nono for mapped/real checkouts,
and the containment probe does NOT cover write-confinement. This is an accepted-or-mitigate
DESIGN decision (like the UDP residual §3d), not a cutover bug — but the cutover deleted the
srt hardening + its test against the design's own "confirm before deleting" gate (§5), so it
must be decided before nono runs on non-throwaway checkouts.

**Not yet a journey** (no dir): the interactive `rein init` half is covered by the
plain `test_init_interactive.py` (8 live specs) — App *creation* is **UNDRIVEABLE**
(browser/manifest → `scripts/cp5-manifest-manual-test.sh`), and a full first-run
`journey_init` covering the prompts end to end is still to write (**#127**);
**misconfig: broken / expired session file** is a **GAP** (#12).

## Prerequisites

- **A working App + a throwaway repo.** `rein init` configures the App (recorded in
  `state.json` + the managed keystore) and a dev-session; the repo is resolved by
  `resolve_throwaway_repo` (`REIN_JOURNEY_REPO` → the configured dev-session →
  `REIN_TEST_REPO_A` as a **legacy this-box shortcut**). A journey that MINTS resolves
  its App from `state.json` (real home) or via `reinharness.init_app_env()`
  (isolated-home init journeys); no `REIN_APP_*` env vars are needed and **no
  `dev-env` is sourced** — its old committed copy pinned a dead App that shadowed the
  real one (#126). Hard-constraint #1: the suite touches **only** that throwaway.
- **A healthy sandbox stack:** `srt`, `bwrap`, `socat`, `ripgrep`, and working
  unprivileged user namespaces. (`rein doctor` checks these.)
- **`python3` + `pexpect`** (developed against 4.9.0).
- **`pyte`** (`sudo apt install python3-pyte`) — a TEST-ONLY, in-memory terminal
  emulator, needed only by the surfaces that REDRAW (`realagent_write`,
  `tmux_popup_approval`, `realagent_e2e`). The import is lazy, so nothing in `run.sh`'s
  default sweep needs it. A **journey** that needs it SKIPs with **exit 3**; the
  real-agent tests are selected by `run-journeys.sh --sandbox`, where a missing pyte
  (with `claude` present) is a HARD FAIL. LGPLv3, test-only — never linked into or
  shipped with the Go binary (hard-constraint #4).
- **Host `gh` authed** as the repo owner — used only for host-side branch
  *verification* and *cleanup* (the operator's own token, never the sandbox).
- **No pytest needed.** The suite uses the stdlib `unittest`.

## How to run

Journeys run as **modules from the repo root** (so the `tests.interactive` package
resolves):

```sh
# one journey — exit 0 == matches its golden
python3 -m tests.interactive.journeys.write_ceremony.journey
# regenerate that journey's RAW golden intentionally
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.write_ceremony.journey

# ALL journeys: compare each to its golden + report drift/broke/skip
tests/interactive/run-journeys.sh
REIN_UPDATE_GOLDEN=1 tests/interactive/run-journeys.sh   # adopt every golden from a live run
tests/interactive/run-journeys.sh --sandbox              # also prove the live sandbox INVARIANTS hold
tests/interactive/run-journeys.sh --normalized           # also print each journey's normalized lens

# the plain-test sweep (edge cases + invariants; discovers test_*.py)
tests/interactive/run.sh                                 # whole suite
tests/interactive/run.sh test_write_approval             # one module
```

The gated `test_*.py` take a real (open) issue via env — the declare FETCHES it, so an
invented number 404s:

```sh
gh issue create --repo <throwaway> --title "..." --body "..."
REIN_ITEST_ISSUE=<n> REIN_ITEST_TITLE_ISSUE=<n> REIN_ITEST_TITLE_WORD=<word-in-title> \
  tests/interactive/run.sh
```

`run.sh` is **never** run by `go test ./...` (there are no `.go` files here), so the Go
suite stays untouched. Before a PR that changes a journey, run
`REIN_UPDATE_GOLDEN=1 run-journeys.sh` and **commit the regenerated golden** — that raw
golden IS the PR's deliverable.

## Disposable branches & cleanup

Each write journey/test creates a clearly-named disposable branch on the throwaway and
deletes it from the host in teardown (via `gh api -X DELETE`). Cleanup is best-effort:
if a delete fails, a few branches may linger — safe to delete by hand. The suite leaves
the throwaway clean.

## Files (pointers only — per-journey detail lives in each dir's README)

- [`CLAUDE.md`](CLAUDE.md) — journey-authoring guidance (read before adding one).
- `reinharness.py` — the shared machinery: binary build/locate, env loading, the
  `ReinRun` pexpect wrapper, in-sandbox script generation, host-side branch
  verify/delete, isolated-HOME init helpers, the journey API (`run_journey`,
  `sandbox_preamble`, `build_raw_transcript`, `normalize_for_compare`,
  `compare_golden`, `resolve_throwaway_repo`), `tmux_pane_session` (rein INSIDE a real
  tmux pane + the real popup), the real-agent API (`split_at_agent_launch`,
  `write_agent_session`), and the pyte layer (`RenderedScreen`, `wait_for_screen`; #100).
- `itest_base.py` — `ReinTestCase` (one-time build, env + throwaway repo, cleanup).
- `journeys/<name>/` — the 20 journeys (index above); each holds `journey.py`,
  `golden.txt` (+ `session.txt` for `realagent_write`), and its own `README.md`.
- `test_write_approval.py`, `test_init_interactive.py`, `test_confirm_shows_title.py`,
  `test_scope_expansion.py` — the plain-test invariants beside the journeys.
- RETIRED at the nono cutover (P3): `sandbox_filesystem` journey, `test_git_hardening.py`,
  `test_agent_contract.py`. The srt deny-read/allow-back fs model and the `.git`
  bind-mount host-exec escape they narrated are gone under nono (Landlock default-deny +
  `deny_credentials`, no bind mounts). The nono `TestLiveContainment` probe
  (`run-journeys.sh --sandbox` → [B]) now covers cred-hiding + fs containment as a
  pass/fail invariant. (The agent-contract briefing itself is not yet wired into
  run_nono — see the P3 cutover notes.)
- `test_golden_shape.py` — stack-free lint: every journey has a golden and
  `normalize_for_compare` is idempotent on it. Runs in the sweep and standalone.
- `realagent_e2e.py` — the real-agent sandbox-startup check (NOT a `test_*.py`;
  selected by `run-journeys.sh --sandbox`).
- `seedghread/main.go` — TEST-SUPPORT standalone (NOT a rein subcommand) the #95 guard
  uses to plant a stale, narrow-scoped gh-read token.
- `demo-transcripts/` — static reference captures for the non-journey demos (outside
  the journeys' goldens, so not normalize-on-compare).
- `recipes/` — per-test setup scripts for the gated tests.
- `run.sh` / `run-journeys.sh` — the plain-test sweep / the on-demand journey runner.
