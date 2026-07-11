# Journey-authoring guidance (tests/interactive/)

This directory drives the real `rein` binary through a pty (pexpect) against a
**live** throwaway repo + a real srt sandbox. Two kinds of file live here, and
the difference is a rule, not a vibe:

| kind | naming | swept by `run.sh`? | deliverable |
|------|--------|--------------------|-------------|
| **journey** | `journey_*.py` | no — run deliberately | a checked-in, human-reviewable **golden transcript** |
| **plain test** | `test_*.py` | yes | a pass/fail assertion; no transcript ceremony |

**Journeys = major user paths.** A journey walks a whole path a real person takes
(the write ceremony, first-time `init`, the tmux-popup grant) and its artifact is
a **golden transcript** a human reviews in the PR. **Plain tests = edge cases and
invariants** (a wrong answer denies; a non-convention ref is rejected; a pure
function's output). They protect against regressions but produce no reviewable
narrative. When in doubt: if a human would want to *read what happened*, it's a
journey; if they just want it to stay green, it's a plain test.

## The golden-transcript rule

A journey's job is to make the flow **reviewable**, not to make prose claims about
it. So every journey:

1. Runs the path LIVE (real srt, real GitHub throwaway, real tty via pexpect).
2. Captures the **full terminal transcript** and — this is the model, decided on
   PR #72 — **DEFAULT-KEEPS every line, normalizing only known noise**:
   - **Normalize known volatile tokens**: issue → `<ISSUE>`, branch nonce →
     `<NONCE>`, timestamps → `<TS>`, git object counts/hashes → `<N>`/`<HASH>`,
     tmp paths → `<TMP>`, proxy socket/run id → `<PROXY_SOCK>`/`run-<RUNID>`,
     repo/title → `<REPO>`/`<TITLE>`. These rules live in
     `reinharness._NORMALIZE_RULES` — extend that list, not a per-journey list.
   - **Collapse the git progress meter**: drop the intermediate `%` redraw ticks
     (`reinharness.is_progress_tick`) but KEEP every terminal `done.` line and
     every error/reject line.
   - **Everything else stays VERBATIM.** That is the whole point: if rein starts
     printing a new line — especially a new security-relevant one — it lands in
     the golden diff = drift = re-review. (This replaced an earlier curated
     *whitelist* whose blind spot was exactly a brand-new line; `build_golden_
     transcript` is the default-keep implementation.)
3. Writes it to `golden/<journey>.txt`, **checked in**. That file is the PR's
   deliverable — a reviewer reads the actual flow, not a description of it.
4. On a normal run, ASSERTS the live transcript matches the golden. A drift =
   a **red** journey = "the behavior changed, re-review it." Regenerate
   intentionally with `REIN_UPDATE_GOLDEN=1` (or `run-journeys.sh`, below) and
   commit the new golden.

**Prove the golden is deterministic before you commit it:** run the journey
twice (each run creates its OWN throwaway issue, so the issue numbers differ) and
confirm the second run reports `[golden OK]`. If it drifts, a volatile slipped
through un-normalized — add a rule to `_NORMALIZE_RULES`, don't loosen the
assertion. (`test_golden_shape.py` also greps every committed golden for raw
scratch paths / run ids / timestamps / nonces and fails if one leaked — a cheap,
stack-free CI catch for a normalization-rule regression.)

## Splitting one terminal into two views (the principled way)

The human sees ONE terminal where the sandboxed agent's output and rein's
`/dev/tty` prompt genuinely interleave — that interleaving IS the artifact. Don't
reconstruct the split by guessing at content. Instead, **tag at the source**: the
in-sandbox script prefixes every line it emits with `reinharness.SBX_TAG`
(`SBX| `), piping git's own output through `... | tr '\r' '\n' | ...prefix` so
even progress redraws stay tagged. Then `reinharness.get_views(text) ->
(host, agent)` is a single pass — a line is the agent's iff it **starts with** the
tag (rein's banner echoes the script body, so a *substring* test would mis-file
those host lines; `startswith` is deliberate). Everything else is rein's own host
output.

`get_views` is available when a journey wants the two sides *separately* (e.g. to
assert an invariant about only the agent's output). The golden itself does NOT
split them: `build_golden_transcript` keeps the full interleaved transcript, where
the `SBX| `-tagged agent lines and rein's untagged host prompt already show the
two views inline — which is the more faithful "one terminal" artifact. There is
**no whitelist** and therefore no brand-new-line blind spot: default-keep means a
new line survives into the diff.

## Readable expect → act → expect

Where a step is genuinely interactive (the Form A declare prompt), **interleave**:
expect the prompt, THEN send the answer. The in-sandbox script is one srt child
and can't be puppeted line-by-line, so instead each of its steps emits a tagged
`@PHASE..` sentinel and the test asserts those **in sequence** — the run still
reads top-to-bottom as expect→act→expect even though the child runs once. Don't
"send the whole script, split afterward"; that's the pattern #72 rejected.

## Shared helpers (keep the next journey declarative)

`journey_write_ceremony.py` is the exemplar. The reusable machinery already lives
in `reinharness.py`, so a new journey is mostly wiring:

- `SBX_TAG`, `get_views` — the exact view split (for per-side assertions).
- `build_golden_transcript(text, subs)` — the default-keep golden (normalize
  known noise, drop progress ticks, keep everything else). Use THIS for the
  golden; `normalize_transcript` / `is_progress_tick` are its building blocks.
- `read_golden` / `update_golden` / `compare_golden` — the golden file.
- `create_issue` / `issue_title` / `close_issue` — a throwaway issue to declare.
- `branch_exists` / `delete_branch` — HOST-side verify + cleanup (operator's gh).
- `resolve_throwaway_repo` — the repo, resolved the rein-init way (see below).
- `spawn_rein_run` / `ReinRun` — the pty wrapper, transcript, prompt matchers.

## Running & refreshing goldens: `run-journeys.sh`

`tests/interactive/run-journeys.sh` is the **manual, on-demand** runner (no timer,
no background minting): it runs every `journey_*.py` live under
`REIN_UPDATE_GOLDEN`, then `git diff`s the goldens against what's committed and
reports a PASS / DRIFT summary with a non-zero exit on drift.

- **Before a PR that changes a journey**, run it and **commit the updated golden**
  — that regenerated golden IS the PR's reviewable deliverable.
- On a clean tree with no intended change, the goldens are rewritten byte-for-byte,
  so `git diff` is empty = PASS.
- A DRIFT leaves the regenerated goldens in your working tree; review the diff,
  then commit if intended or `git checkout -- tests/interactive/golden/` to discard.

The `test_*.py` sweep (`run.sh`) additionally runs `test_golden_shape.py`, the
stack-free lint that fails if a journey has no golden or a golden leaked a raw
volatile — a cheap gate that needs no srt/GitHub/tty.

## Setup: the `rein init` world, NOT `source ./dev-env`

The documented path is the one a fresh machine uses: `rein init` configures the
App + a dev-session; a journey resolves its throwaway with
`resolve_throwaway_repo` (`REIN_JOURNEY_REPO` → the configured dev-session →
`REIN_TEST_REPO_A` as a **legacy this-box shortcut**, last). A journey must not
DEPEND on `REIN_TEST_REPO_A` special-casing (#40); it may use a throwaway repo,
it just shouldn't assume that one env var names it. `source ./dev-env` is the dead-
App footgun the HANDOFF banner warns about — mention it only as a labeled local
shortcut.

```sh
# once per machine: rein init sets up the App + dev-session (see HANDOFF.md)
gh issue create --repo <throwaway> --title "..." --body "..."   # declare FETCHES a real issue
python3 tests/interactive/journey_write_ceremony.py             # exit 0 == matches golden
```

(The write journeys create their own throwaway issue, so the manual `gh issue
create` above is only needed for the gated `test_*.py` that take an issue via env.)

## Safety

Hard-constraint #1: a journey touches ONLY its throwaway. It creates its own
disposable branches (`H.unique_branch`) and issue, and cleans both up in a
`finally`. Init journeys additionally confine every write to a throwaway
`HOME`/XDG tempdir and keep `REIN_APP_*` present so init never trips the
~25-minute manifest/browser flow.
