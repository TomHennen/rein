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
2. Builds a transcript of what happened and **normalizes the volatile bits**
   (issue numbers → `<ISSUE>`, branch nonces → `<NONCE>`, timestamps → `<TS>`,
   git object counts/hashes → `<N>`/`<HASH>`, tmp paths → `<TMP>`, repo → `<REPO>`)
   so two runs of an unchanged journey yield an **identical** golden.
3. Writes it to `golden/<journey>.txt`, **checked in**. That file is the PR's
   deliverable — a reviewer reads the actual flow, not a description of it.
4. On a normal run, ASSERTS the live transcript matches the golden. A drift =
   a **red** journey = "the behavior changed, re-review it." Regenerate
   intentionally with `REIN_UPDATE_GOLDEN=1` and commit the new golden.

**Prove the golden is deterministic before you commit it:** run the journey
twice (each run creates its OWN throwaway issue, so the issue numbers differ) and
confirm the second run reports `[golden OK]`. If it drifts, your normalization is
incomplete — fix it, don't loosen the assertion.

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

This tag-split is exact for whole lines, but it is **not** the whole story: it
feeds a **curation** step (whitelist the meaningful lines, drop git object-count
noise, mask pty line-wrap) before the golden. So the reviewable artifact is
"tag-split **plus** curation," not the split alone — don't over-claim "no
heuristics." One consequence to know: because the agent view is whitelisted,
*wording changes* to a kept line drift correctly (good), but a **brand-new** line
rein starts printing is silently absent from the golden (no drift signal). If you
add a journey, keep the whitelist honest.

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

- `SBX_TAG`, `get_views` — the exact view split.
- `normalize_transcript(text, subs)` — volatile → placeholder.
- `read_golden` / `update_golden` / `compare_golden` — the golden file.
- `create_issue` / `issue_title` / `close_issue` — a throwaway issue to declare.
- `branch_exists` / `delete_branch` — HOST-side verify + cleanup (operator's gh).
- `resolve_throwaway_repo` — the repo, resolved the rein-init way (see below).
- `spawn_rein_run` / `ReinRun` — the pty wrapper, transcript, prompt matchers.

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
