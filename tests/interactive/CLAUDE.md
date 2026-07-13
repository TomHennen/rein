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

## The golden-transcript rule: RAW file, normalize on COMPARE (PR #78)

A journey's job is to make the flow **reviewable**, not to make prose claims about
it. The model, decided on PR #78:

- **The golden FILE is RAW.** `REIN_UPDATE_GOLDEN=1` writes exactly what the run
  produced — real issue number, real repo, real title, real branch nonce, real
  object counts. NO `<ISSUE>`/`<N>` placeholders in the checked-in file. Reviewers
  see reality, not a redacted sketch. (`reinharness.build_raw_transcript` builds
  it: the only things stripped are mechanical — ANSI escapes, the sub-100%
  progress redraw ticks a terminal never shows as lines, and blank-line runs.
  Every terminal `done.` line with its real counts stays.)
- **Determinism lives in the COMPARATOR, not the file.** A fresh run is compared
  to the golden by normalizing **both** sides first
  (`reinharness.compare_golden` → `normalize_for_compare`), then diffing. So a run
  with a different issue / nonce / object count still matches, while a genuinely
  new or changed line trips drift. The normalization rules are GENERIC regexes in
  `reinharness._NORMALIZE_RULES` (issue# → `<ISSUE>`, nonce → `<NONCE>`, counts →
  `<N>`, size/rate → `<SIZE>`/`<RATE>`, tmp/proxy/run-id → `<TMP>`/`<PROXY_SOCK>`/
  `run-<RUNID>`) — generic because the committed golden's baked-in value differs
  from a fresh run's, so both must map to the same placeholder with no knowledge
  of either specific value. Extend that list, never a per-journey whitelist.
  - The one progress rule (`is_progress_tick`) drops sub-100% redraw ticks but
    keeps every `done.`/error line. Everything else is kept, so a brand-new rein
    line — especially a security-relevant one — survives into the normalized diff.
  - Repo and title are NOT normalized: they are stable by construction (the same
    throwaway; a hard-coded issue title), so they match verbatim across runs.
- **On drift**, the runner/journey prints the **normalized** diff (the meaningful
  change, not per-run noise) and drops the **raw** fresh transcript to a scratch
  path so you can eyeball reality, then `REIN_UPDATE_GOLDEN=1` to adopt it.
- **`REIN_SHOW_NORMALIZED=1`** (or `run-journeys.sh --normalized`) prints the
  normalized form on demand — the lens the comparator looks through, for spotting
  anything unexpected. Normalization is the lens you look THROUGH, not what you store.

**Prove determinism before you commit a golden:** run the journey twice (each
creates its OWN issue, so issue# + nonce differ) and confirm the second reports
`[golden OK]`. If it drifts, a per-run-varying token slipped through un-normalized
— add a rule to `_NORMALIZE_RULES`, don't loosen the compare. `test_golden_shape.py`
additionally asserts every journey has a golden and that `normalize_for_compare`
is IDEMPOTENT on it (a well-formed, fixpoint comparator) — a cheap, stack-free CI
catch. It no longer flags real values: raw goldens are supposed to show reality.

## Two ways to read a pty: RENDERED SCREEN vs RAW LINE TRANSCRIPT (#100)

A pty is not one kind of thing, and reading it the wrong way is the bug class #100
retired. Pick by asking **does this surface REDRAW?**

| the surface | how to assert | why |
|---|---|---|
| **REDRAWS** — a TUI (`claude`), a **tmux popup** | the **rendered screen** (`reinharness.RenderedScreen`, a pyte terminal emulator) | it PAINTS CELLS: homes the cursor, overwrites regions, repaints per keystroke. The byte stream is a *history of paint operations*, not a picture. |
| **LINE-ORIENTED** — git, rein's banner/prompts, the `SBX|`-tagged agent script | the **raw transcript** (`build_raw_transcript`), as before | a line, once printed, is final. Scrollback IS the artifact. |

**Every journey golden stays on the raw-transcript path.** A rendered screen is a
point-in-time FRAME (cols x rows of cells) — content that scrolled off is GONE — so
routing `write_ceremony` / `gh_write` / `sandbox_filesystem` / `onboarding` through
a screen would silently truncate the golden to its last N lines. Screen-rendering
applies ONLY to the redrawing surfaces, and (in the popup journey) only to the
*popup's own* Form A block, which is then FOLDED into the raw transcript as `POPUP| `
lines. Don't "helpfully" render a line-oriented golden.

What the emulator buys you, concretely: **the "is the frame complete?" question
becomes a screen state instead of a timer.** The popup driver used to match a
substring in the byte stream and then *drain to quiescence* (read until N ms passed
with no new bytes) and hope the rest of the paint had landed. Now `drive_popup`
waits for a condition — Form A painted through its trailing `>` prompt line
(`popup_forma_complete`), the last thing rein writes before it BLOCKS on input, so
the frame provably cannot change. Same for `ReinRun.read_until_ready` /
`send_and_collect` (the real-agent TUI): they pump the pty into ONE persistent
screen and match what is ON it. Redraws, cursor moves, and read-chunk boundaries
stop being our problem, because resolving them is the emulator's whole job. This
also *shrinks* the hand-modelling: pyte let us DELETE the CUP-splitting /
box-art-stripping / last-write-per-row extractor (`_CUP`, `_BOX_DRAWING`,
`_TMUX_MISC_ESC`, `_clean_popup_segment`, `extract_popup_forma`) outright.

The API (all lazy — see the skip rule below):

- `RenderedScreen(cols, rows)` — `feed(text)`, `display()` (the grid), `text()`
  (grid joined, trailing blanks trimmed), `contains(needle)`.
- `screen_for_child(child)` — a screen sized to that pty (`getwinsize()`), so pyte
  wraps exactly where the real terminal does. **Never hardcode 80x24.**
- `render_stream(text, cols, rows)` — render an already-captured stream (e.g. a
  pexpect `logfile_read` StringIO's value).
- `wait_for_screen(child, pattern, timeout, screen=…)` — THE primitive: pump the
  child's bytes into the screen and return once `pattern` (a regex, or a
  `screen -> bool` predicate for a richer screen state) appears **on the render**.
- `popup_forma_from_screen(screen, answer)` / `popup_forma_complete(screen)` — the
  popup box's content, extracted by GEOMETRY (find the border rows, slice the
  columns inside them), not by parsing escape codes.

### A REAL agent's TUI is neither: collapse it (`journey_realagent_write`, #101)

A deterministic bash "agent" is line-oriented, so its output belongs in the golden
verbatim. A **real LLM's TUI is not**: it redraws AND its content genuinely varies
run to run (prose, tool order, spinners, token counts, promo banners). There is no
token-level normalization for that — it is noise of the same kind
`build_raw_transcript` already drops (progress ticks). So the one journey that drives
a real `claude` **collapses that region at BUILD time** (`reinharness.collapse_agent_tui`),
and the golden keeps what actually matters and is stable: **rein's own host output
verbatim** (banner, injected contract, exit token accounting — a new rein line still
trips drift), the popup's Form A (`POPUP| `), and a ground-truth `MILESTONE| ` block
read from `helper.log` + the GitHub API. This is the ONE sanctioned exception to
"never drop output", and it is kept honest two ways:

- **Two anchors that MUST both be found** — rein's `running:` echo (start) and its exit
  token accounting (end: `revoked N of N write token(s) on exit`, or the per-token
  `exit-revoke … failed` warning that structurally precedes it). A miss is a CEREMONY
  BREAK (exit 2), never a silently smaller golden.
- **The collapse is a FILTER, not a delete.** rein has call sites that write to its own
  host pty *inside* that window (`printExpiryBanner`'s `rein: SESSION EXPIRED` block;
  the non-interactive install-NOTICE surface) — the region is NOT "pure agent TUI". So
  any line in it matching `AGENT_TUI_KEEP_RE` (a column-0 `rein: …` / `=== rein: …`) is
  **preserved**, and only the agent-TUI runs around it collapse to a placeholder. The
  doctrine above ("a brand-new rein line — especially a security-relevant one — survives
  into the normalized diff") therefore holds INSIDE this window too. It costs nothing:
  rein prints nothing there on the happy path, and a real claude never paints flush-left
  with rein's prefix (its output sits behind box art — verified on captured pty streams).

Do not generalize the collapse to a line-oriented agent.

Driving a real agent alongside a second pty has one hard requirement:
**`drain_children`**. A pty's buffer is ~64KB and a TUI repaints constantly, so if you
block on the tmux client (waiting for the popup) without reading the agent's pty, the
agent BLOCKS on write and the run deadlocks. `wait_for_screen(..., drain=[child])` and
`drive_popup(..., drain=[child])` take it; discarded bytes are still captured by
`logfile_read`, so the transcript stays complete.

**`tmux capture-pane` is NOT usable for the popup** — verified empirically, don't
retry it: with a real attached client rendering Form A, `list-panes` reports only
the base pane and capturing every pane finds no Form A anywhere. A tmux popup is a
**client-owned OVERLAY**, not an addressable pane. The attached client's own pty is
the only surface it exists on; run THAT through the emulator.

**pyte is a TEST-ONLY dependency** (`sudo apt install python3-pyte`; LGPLv3,
approved on the test-only basis under hard-constraint #4 — it is never linked into
or shipped with the Go binary). It is imported **lazily**, so `reinharness` imports
fine without it and every line-oriented journey plus `test_golden_shape.py` keeps
working. A journey that needs a rendered screen checks `H.pyte_available()` up front
and **SKIPs with exit 3** (never 0 — see the exit-3 rule below).

## Splitting one terminal into two views (the principled way)

The human sees ONE terminal where the sandboxed agent's output and rein's
`/dev/tty` prompt genuinely interleave — that interleaving IS the artifact. Don't
reconstruct the split by guessing at content. Instead, **tag at the source**: the
in-sandbox script runs commands through the `run` helper in
`reinharness.sandbox_preamble()`, which echoes each command as `SBX| $ <command>`
and then tags every line of its output (piping through `tr '\r' '\n'` so even
git's progress redraws stay tagged). So the transcript reads like a real terminal
session — `$ command` then its output then the next `$ command` — and everything
the agent produced carries `reinharness.SBX_TAG` (`SBX| `). Then
`reinharness.get_views(text) -> (host, agent)` is a single pass — a line is the
agent's iff it **starts with** the tag (rein's banner echoes the script body, so a
*substring* test would mis-file those host lines; `startswith` is deliberate).
Everything else is rein's own host output. Use `sandbox_preamble()` in a new
journey's in-sandbox script so it inherits this exact shape.

`get_views` is available when a journey wants the two sides *separately* (e.g. to
assert an invariant about only the agent's output). The golden itself does NOT
split them: `build_raw_transcript` keeps the full interleaved transcript, where
the `SBX| `-tagged agent lines and rein's untagged host prompt already show the
two views inline — the faithful "one terminal" artifact. There is **no whitelist**
and no brand-new-line blind spot: everything is kept, so a new line survives.

## Use the shared journey runner — it is THE interface for EVERY journey (#82)

`reinharness.run_journey` is the ONE interface for ALL journeys — host-command,
single-run in-sandbox, AND multi-run in-sandbox. There is no `spawn_rein_run`
carve-out any more. You declare only **steps** — each step's `argv` and the
ordered `(expect_pattern, answer)` pairs for its prompts — and the runner captures
the **complete pty session** of everything it drove, returning it as one raw
transcript (`JourneyResult.transcript`). Pass that straight to `compare_golden`.

**A `rein run` (sandbox) launch is just a step.** Its `argv` is the full
`["run", "--", …inner…, <workdir>]`; a per-step override points rein at the
writable checkout, and a per-step `timeout` covers the slow srt launch. Each
`JourneyStep` carries three per-step fields, each WINNING over the journey-level
value of the same name (so one slow sandbox step raises just its own budget):

- `cwd` — the directory rein is spawned in. A sandbox step points it (or
  `REIN_SANDBOX_WORKDIR` via `extra_env`) at the writable tree so rein binds the
  intended checkout. (Same name/semantics as parallel branch #78, which the
  runner converges with.)
- `extra_env` — env overlaid for THIS step only (e.g. `REIN_SESSION_FILE`, or
  `REIN_SANDBOX_WORKDIR` to name the sandbox working tree).
- `timeout` — seconds for the step's spawn + every expect. A sandbox launch needs
  ~120-180s vs the fast host-command default; set it on the sandbox step alone.

The in-sandbox script keeps `sandbox_preamble()`/`run` SBX| tagging — that output
is captured as ordinary session content, so the golden is built from the WHOLE
session (banner + injected contract + every tagged agent line), no slicing.

**Do NOT hand-assemble the golden.** You never call `.text()` and slice, you never
pick which sections land in the golden. A section is in the golden because its
command ran in the captured session — that's the whole point. (This is why an
early onboarding golden silently dropped `rein doctor`: the journey curated its
own capture. `run_journey` removes that footgun — there's no supported path to
omit a section.) Volatiles are handled downstream by **normalize-on-compare**
(add a rule to `_NORMALIZE_RULES`); you **normalize** machine-variable values,
you do **not** drop output. `journey_onboarding.py` is the exemplar.

An **in-sandbox** journey drives its `rein run` as a step too — no carve-out.
`journey_sandbox_filesystem.py` is the exemplar (#63's migration): one
`JourneyStep(argv=["run", "--", "bash", "-c", <script>, <workdir>],
extra_env={"REIN_SANDBOX_WORKDIR": <workdir>, ...}, timeout=180)`, and
`run_journey` captures the complete session — banner, injected contract, and every
`SBX| `-tagged agent line — as `.transcript`. It replaced the old
`spawn_rein_run(...); run.expect...; text=run.text()` pattern. Because the inner
`bash -c` body is large and rein re-echoes it under its own banner, give the step
a concise `label` (e.g. `rein run -- bash -c <sandbox agent script> <workdir>`) so
the golden's boundary line stays readable; the full script still appears once, in
rein's own `rein: running:` echo. `spawn_rein_run`/`ReinRun` stay in the module
(other tests use the wrapper directly), but every JOURNEY goes through
`run_journey`. A multi-run in-sandbox journey is the same, one step per `rein run`
— the `$ rein run` echoes ARE the run boundaries.

## Drive tmux for REAL: rein runs INSIDE the pane (`tmux_pane_session`)

A developer runs `rein run -- <agent>` **inside a tmux pane**, so the agent's
output and rein's approval popup **share one terminal**. The popup journey used to
fake that: rein ran on a separate pexpect pty with a **synthesized `$TMUX`** aimed
at a tmux session whose pane was **EMPTY**. It proved the popup surface — and it
structurally could not see a popup-over-live-content bug, because the popup had
nothing to overlay. `reinharness.tmux_pane_session()` is the real configuration and
is what a tmux journey must use: a dedicated-socket tmux server (never the
operator's), a **real pane** whose shell the command is typed into (`run_in_pane`),
so `$TMUX`/`$TMUX_PANE` are **INHERITED from tmux** — nothing is synthesized, and
the "an empty sockpath could fall back to the operator's real server" hazard of the
synthesis is gone with it.

**Three surfaces. Each answers a question only it can answer — do not mix them up:**

| surface | what it is | use it for |
|---------|-----------|------------|
| `raw_stream()` | the **`pipe-pane`** byte stream: everything the pane's program WROTE, append-only and complete | **the line-oriented golden transcript**, and waiting on flow markers (`until_raw`) |
| `pane_text()` | `capture-pane -p -J` — the **rendered** pane, right now (`-J` joins wrapped lines; add `-e` only if you assert attributes, it makes goldens escape-noisy) | what the pane LOOKS like; the proof a popup is **not** in it |
| `client_screen()` | the attached client's pty, **pyte**-rendered (`RenderedScreen`) | the **only** surface a tmux popup exists on |

- **A tmux popup can never be captured by its own server.** `popup.c` holds a
  standalone `struct screen` registered via `server_client_set_overlay()` — no
  `window_pane`, so it never appears in `list-panes`, has no `#{popup_*}` format,
  and `capture-pane` cannot see it. It renders on, and grabs the keyboard of, the
  **attached client**; the only way to answer it is to write keys to that client's
  pty (`send_client`), never `send-keys`.
- **The golden comes from `pipe-pane`, not `capture-pane`.** `capture-pane` shows
  only the visible screen — and while a TUI holds the **alternate screen**, even
  `capture-pane -S -` gives **no scrollback at all**. The raw stream is the only
  complete, line-oriented record. (That alt-screen limit is *why* the golden is
  sourced from `pipe-pane`; a deterministic bash agent never enters the alternate
  screen, so Stage-1's `pane_text()` assertions are safe — it is the **real-agent**
  journey that would be bitten.) Start `pipe-pane` AFTER `new-session -d` but
  BEFORE anything is typed, or the first bytes are raced away.
- **Read the popup off the RENDER, never the client's raw bytes.** With a live pane
  underneath, the client's byte stream interleaves the pane's own writes, and a row
  the popup paints blank lets stale pane text bleed *inside* the Form A box. On the
  pyte render the overlay is genuinely on top, so `popup_forma_from_screen`'s
  geometry slice is truthful.

**Two rules, both learned the hard way:**

1. **DRAIN THE CLIENT, ALWAYS.** pexpect only yields bytes when you READ from the
   child. If a long wait (a slow sandbox clone) polls the pane without reading the
   client, the client's pty fills, tmux's attach blocks on write, the popup render
   never lands, nobody answers it, `rein approval grant` times out at 60s and rein
   **degrades to the inline prompt** — which looks exactly like a rein bug and is
   not one. So draining is not a discipline: it lives inside the ONE shared poll
   primitive (`TmuxPaneSession.until`) that `until_raw` / `until_pane` /
   `until_client` / `wait_stable` / `drive_popup` all go through, and the same read
   that drains the client also feeds its `RenderedScreen`. Keep the main thread the
   sole reader; do not add a drain thread racing `send()`.
2. **NEVER ASSERT ON A SINGLE `capture-pane` SHOT** — it races the redraw. Retry
   the predicate (`until_pane`, ~50ms poll; fzf's `Tmux#until` shape). For an
   assertion with **no anchor string** ("the pane repainted after the popup
   closed"), wait for **quiescence**: `wait_stable(ms)` returns the render once it
   has stopped changing.

**Pin the geometry** (`-x`/`-y` **plus** `window-size manual` **and** `status off`)
or a client attach resizes the window and reflows every wrapped line in the golden;
set `TERM`/`default-terminal` deliberately (the popup's box-drawing depends on it);
and give the pane's shell a **fixed `PS1` via `--rcfile`** (bash ignores an exported
`PS1` and would otherwise bake `bash-5.2$` — its own version — into the golden).

`journey_tmux_popup_approval.py` is the exemplar. Because it runs for real it can
assert what the empty-pane cheat could not: while Form A is up it is on
`client_screen()` and **absent from `pane_text()`**, which at that moment still
shows the live `SBX| $ rein declare <n>` the popup is blocking on — the popup
**overlays** a live pane rather than printing into it — and after the popup closes
the pane repaints and the run carries on, with no Form A residue on the client.

The synthesized-`$TMUX` shape (`TmuxPopupSession`/`tmux_popup_session`) is still in
the module, but ONLY because `journey_realagent_write.py` has not been flipped yet;
it goes away with that flip. **Do not write a new journey against it.** If a surface
can't be driven for real, SKIP with exit 3 — never fake it.

## Prefer inline literals over constants for EXPECTED values (#82)

In journeys and tests, write the expected string/value **inline at the assertion**
rather than behind a named constant — reviewability first. A reviewer should see
`"Name this machine [demo-box]" in text` and know exactly what's expected without
chasing a `DEMO_HOSTNAME = ...` definition elsewhere. (Inputs you reuse several
times — a repo slug typed into multiple prompts — can still be a local, but the
*expected* literals in assertions should be visible where they're checked.)

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

- `sandbox_preamble()` — the bash `emit`/`run` helpers a journey's in-sandbox
  script prepends; `run <cmd>` echoes `SBX| $ <cmd>` then tags its output.
- `SBX_TAG`, `get_views` — the exact view split (for per-side assertions).
- `build_raw_transcript(text)` — the RAW transcript for the golden file (real
  values; strips only ANSI + progress ticks + blank runs).
- `normalize_for_compare(text)` — the comparison lens (raw → placeholders).
  `normalize_transcript` / `_NORMALIZE_RULES` / `is_progress_tick` are its parts.
- `read_golden` / `update_golden` (writes RAW) / `compare_golden` (normalizes
  BOTH sides, then diffs).
- `create_issue` / `issue_title` / `close_issue` — a throwaway issue to declare.
- `branch_exists` / `delete_branch` — HOST-side verify + cleanup (operator's gh).
- `list_matching_refs` / `list_prs_for_branch` / `pr_author` — HOST-side ground truth
  (a REAL agent names its own branch, so DISCOVER it under `agent/<n>/`; `pr_author`
  is the delegated-bot check).
- `collapse_agent_tui` / `drain_children` — the real-agent pair (see above).
- `resolve_throwaway_repo` — the repo, resolved the rein-init way (see below).
- `spawn_rein_run` / `ReinRun` — the pty wrapper, transcript, prompt matchers.
- `RenderedScreen` / `screen_for_child` / `render_stream` / `wait_for_screen` /
  `popup_forma_from_screen` — the pyte rendered-screen layer for REDRAWING
  surfaces (#100); `pyte_available()` / `PyteMissing` for the exit-3 skip.
- `tmux_pane_session()` / `TmuxPaneSession` — rein INSIDE a real tmux pane (see
  above): `run_in_pane`, the three surfaces (`raw_stream` / `pane_text` /
  `client_screen`), the retry (`until_raw` / `until_pane` / `until_client`) and
  quiescence (`wait_stable`) helpers, and `drive_popup` (answers the popup on the
  attached client, and snapshots what `capture-pane` shows while it is up).
- `run_journey(steps)` / `JourneyStep` / `JourneyResult` — THE journey runner
  (#82), for host-command AND sandbox journeys alike: declare steps (argv + prompt
  answers, plus per-step `cwd`/`extra_env`/`timeout` when a step drives a `rein
  run` sandbox launch), get the COMPLETE captured session back as `.transcript`
  for `compare_golden`. Use this for EVERY new journey; it makes complete capture
  structural.

## Running & refreshing goldens: `run-journeys.sh`

`tests/interactive/run-journeys.sh` is the **manual, on-demand** runner (no timer,
no background minting). By default it runs every `journey_*.py` live and each one
COMPARES its fresh run to the committed RAW golden (normalizing both sides), so a
different issue/nonce/count still passes; it reports PASS / DRIFT / BROKE / SKIP with
a non-zero exit on drift. `REIN_UPDATE_GOLDEN=1 run-journeys.sh` instead ADOPTS —
rewriting each raw golden from a live run (then `git diff` shows what to commit).
`run-journeys.sh --normalized` also prints each journey's normalized transcript.

**A journey that cannot run must exit 3, never 0.** The runner maps `0 -> PASS`, so
a journey that returns 0 because a prerequisite is missing (an external tool not
installed, a gated capability absent) reports **green for a path nothing
exercised** — the #68 footgun, the exact failure this suite exists to prevent. Exit
**3 = SKIPPED**: the runner prints `SKIP … this journey did NOT run` plus a summary
warning, so missing coverage is visible instead of masquerading as coverage. A skip
is not a failure (it does not fail the run), but it must never look like a pass.
`journey_credential_boundary.py` is the exemplar (it needs the external `bagel` CLI).

- Default (compare) mode does NOT rewrite the raw goldens, so a PASS leaves the
  tree clean. DRIFT prints the normalized diff and a scratch path to the raw
  fresh transcript; adopt an intended change with `REIN_UPDATE_GOLDEN=1`.
- **Before a PR that changes a journey**, run with `REIN_UPDATE_GOLDEN=1` and
  **commit the regenerated raw golden** — that raw golden IS the PR's deliverable.

### `--sandbox`: prove the sandbox INVARIANTS still hold (not just the transcripts)

A golden compare answers "does the transcript still read the same?". It does NOT,
on its own, answer "does the sandbox still ENFORCE its boundary?" — a golden could
keep matching while a regression quietly opened a hole (the transcript would only
change if the *observable output* changed). So the runner has a **second,
clearly-separated section**, opt-in via `run-journeys.sh --sandbox`:

- **[A] GOLDEN-DRIFT** — every `journey_*.py` vs its golden (the default section).
- **[B] SANDBOX-HOLDS** — the live sandbox invariants, run only with `--sandbox`.
  It is the on-demand **"prove the sandbox still holds"** entry point and runs four
  suites, PASS/FAIL each, non-zero exit if any (or any journey) fails:
  1. `REIN_SANDBOX_E2E=1 go test ./internal/srt/ -run E2E` — deny-read + home-deny
     + home-write-semantics under real srt.
  2. `REIN_SANDBOX_E2E=1 go test ./cmd/rein/ -run E2E` — a working tree under an
     allow-back stays writable.
  3. `test_git_hardening.py` — the `.git` host-exec escape stays CLOSED (mv→EBUSY,
     hooks/config read-only, `config.worktree` denied).
  4. `test_agent_contract.py` — the injected contract reaches a REAL claude. Gated
     on `claude` being on `PATH`; **SKIPped gracefully** (a clear note, not a
     failure) when it is not, since LLM phrasing is not golden material.

  These assert the same claims the `sandbox_filesystem` journey *narrates*, but as
  pass/fail invariants — so a hole that left the transcript unchanged still trips.
  Flags combine in any order: `run-journeys.sh --sandbox --normalized`.

The `test_*.py` sweep (`run.sh`) additionally runs `test_golden_shape.py`, the
stack-free lint that fails if a journey has no golden or if `normalize_for_compare`
isn't idempotent on it — a cheap gate that needs no srt/GitHub/tty.

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
