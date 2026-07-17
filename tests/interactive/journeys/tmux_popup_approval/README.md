# tmux_popup_approval — the DEFAULT approval surface inside `$TMUX` (#37) (COVERED)

rein's write-approval prompt does NOT default to the inline `/dev/tty` prompt when
`$TMUX` is set — it defaults to a `tmux popup -E "rein approval grant --run-id <id>"`
(`internal/ui/grant`). Every other journey runs OUTSIDE tmux (or forces
`REIN_APPROVAL=tty`), so this default surface was untested end to end. This journey
drives the REAL popup on the same #35 loop as the write ceremony.

**The REAL configuration** (`reinharness.tmux_pane_session`):

- a **dedicated tmux server** (`tmux -L <unique>`), never the operator's own; killed
  on teardown;
- a **real pane** — the command is TYPED INTO the pane's shell, so `$TMUX`/`$TMUX_PANE`
  are INHERITED from tmux (nothing synthesized), and rein's output and the popup
  overlay SHARE ONE TERMINAL as on a developer's box;
- an **attached pexpect client** the popup renders on: a popup is a client-owned
  OVERLAY, not an addressable pane — it never appears in `list-panes`, `send-keys`
  can't reach it, and **`capture-pane` cannot SEE it**. The only way to read/answer it
  is the client's own pty, and that client must be DRAINED continuously or the popup
  never lands and rein degrades to the inline prompt;
- that client's pty run through **pyte** (`RenderedScreen`, #100): the popup REDRAWS,
  so its Form A is read off the RENDERED SCREEN (geometry slice), not reconstructed
  from ANSI bytes that interleave the live pane underneath.

**Golden = the pane's `pipe-pane` byte stream** (append-only, complete). A
`capture-pane` shot shows only the visible screen, and under a TUI's alternate screen
has no scrollback, so it can never be the transcript.

Positive proof of the surface: the golden shows `$ rein declare <n>` going straight to
`confirmed` with the Form A block **ABSENT** (it rendered in the popup — contrast the
write-ceremony golden's inline block), ZERO Form A prompts in the pane's stream, and
`helper.log` records `grant: launching tmux popup …` then `grant: issue #<n> CONFIRMED
via tmux popup`. And what only the REAL pane makes assertable: while Form A is up it is
on `client_screen()` and ABSENT from `pane_text()` (which still shows the live `SBX| $
rein declare <n>` the popup is blocking on — it overlays, it does not print); after the
popup closes the pane REPAINTS and the run carries on (`wait_stable`).

**SKIPs with exit 3** if `tmux` or `pyte` is missing — the surface is undriveable
without either, and a skip must never look like a pass.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke, **3** = SKIP if `tmux`/`pyte` absent.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.tmux_popup_approval.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.tmux_popup_approval.journey   # regenerate the RAW golden
```
