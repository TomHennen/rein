# realagent_write — a REAL claude walks the whole write path (#101 gap 2) (COVERED)

Every other journey's "agent" is a deterministic bash script; `realagent_e2e` runs a
real claude but only asks it `2+2`. Here a **live LLM** gets a one-line task and does
the whole thing itself: reads the injected contract, runs `rein declare <n>` **up
front** (it does NOT need to discover the gate from a locked push — a design
correction to #101), gets approved via the tmux **popup**, writes/commits/pushes
`agent/<n>/<its own suffix>` (discovered host-side via `matching-refs` — a real agent
names its own branch) and opens a **PR**, whose author is asserted to be the
**delegated App bot** (`app/<slug>`, `is_bot=true`), never the developer.

It runs in the REAL configuration — rein AND claude INSIDE a real tmux pane
(`reinharness.tmux_pane_session`) — which matters most here because claude is a
full-screen TUI: only then do the agent's TUI and the approval popup actually SHARE
ONE TERMINAL. So it asserts what only that can show: while Form A is up it is on
`client_screen()` and ABSENT from `pane_text()` (which still shows claude's TUI live
underneath, blocked on its `rein declare` tool call — the popup OVERLAYS, it does not
PRINT), and the TUI REPAINTS once the popup closes (`wait_stable`; no anchor string).

**A live LLM gets its OWN shape — three things, each doing one job** (not forced into
the deterministic journeys' single composite golden):

1. **Invariants in code** — the regression oracle for behavior (branch under
   `agent/<n>/`, exactly one PR, PR author `is_bot=true`, `helper.log` shows popup
   launched + issue CONFIRMED + write-tier mint, zero inline prompts, popup overlaid a
   live TUI, TUI repainted). A break is **exit 2**. Nothing about the LLM's prose is
   asserted.
2. **The COMPARED golden (`golden.txt`)** — rein's OWN output + the popup's Form A,
   **no agent content**. Built with one boundary + one regex
   (`split_at_agent_launch`): rein's launch surface verbatim through its `rein:
   running:` echo — the only compared golden covering the claude-specific
   `--append-system-prompt` contract-injection line — then column-0 `rein: …` lines.
   Because no agent content is in it, a COMPLETELY DIFFERENT claude session still
   compares clean, and every rein-emitted line still trips drift.
3. **The agent's session (`session.txt`)** — rendered `capture-pane -p -J` milestone
   frames (TUI live, TUI under the popup, the repaint, the final screen), committed so
   a human can READ what the agent did, and **NEVER compared** (it is not in a
   `golden.txt`, so nothing diffs it: an LLM's prose is not a regression signal).

claude's folder-trust dialog is dismissed as **plumbing** — never asserted, never in
the narrative. **SKIPs with exit 3** if `claude`, `tmux`, or `pyte` is absent. Spends
real API tokens — run it deliberately. `REIN_UPDATE_GOLDEN=1` regenerates BOTH
artifacts.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke, **3** = SKIP if `claude`/`tmux`/`pyte` absent.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.realagent_write.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.realagent_write.journey   # write BOTH artifacts
```
