# write_ceremony — the #35 write ceremony (COVERED)

Agent declares an issue → human confirms on the terminal → a verified push to
`agent/<n>/<nonce>` lands. One real `rein run`, split into the two views whose gap
**is** the security argument: the **agent** in-sandbox (pre-declaration push denied
→ `rein declare <n>` → verified push succeeds → a non-convention ref rejected) and
the **human** on the tty (the Form A prompt carrying the *fetched*
title/state/home-repo, then `[approved]`).

- **Golden contract.** Exit **0** = ceremony held AND the normalized fresh run
  matches `golden.txt`; **1** = drift (normalized diff printed, raw fresh dropped to
  a scratch path; `REIN_UPDATE_GOLDEN=1` adopts); **2** = the ceremony itself broke
  (a phase rc / prompt count / branch was wrong). The golden is RAW (real
  repo/issue/nonce/counts); determinism lives in the comparator (normalize both
  sides). Every terminal line is kept, so a new `rein:` line trips drift (it caught
  the exit-time token-revoke lines a whitelist had dropped).
- **One terminal, tagged at source.** The in-sandbox script runs commands through
  `sandbox_preamble`'s `run` helper, echoing `SBX| $ <cmd>` then tagging its output,
  so agent-vs-host reads inline. Steps run **expect→act→expect** (each emits an
  `@PHASE..` sentinel; the declare's host prompt is answered live between them).
- **Self-contained.** Creates its own throwaway issue via `gh`, deletes both
  branches and closes the issue in a `finally`. `REIN_DEMO_ISSUE=<n>` reuses one.

**Beside it (plain tests, no golden):** `test_write_approval.py` — the
pre-declaration lock (push before declaring → synthesized `fatal: remote error`, no
prompt), the wrong-answer denial (writes stay locked), one-declare-covers-a-second-push
(exactly one prompt), and the ref cross-check (a non-`agent/<n>/<nonce>` ref rejected
after approval, #35 decision C). `test_confirm_shows_title.py` — the prompt shows the
fetched title/state/home-repo.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.write_ceremony.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.write_ceremony.journey   # regenerate the RAW golden
```
