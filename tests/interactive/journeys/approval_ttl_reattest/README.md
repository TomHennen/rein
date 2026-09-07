# approval_ttl_reattest — the approval-AGE bound re-attests in place, activity does not extend it (#190)

Declare → confirm → verified push lands → the run stays **BUSY** (`git ls-remote
origin` every ~5s for ~25s — continuous GitHub traffic, never idle) → the write
approval is withdrawn **in place** anyway, because it has simply gotten too old →
the next push is refused with the ordinary "writes are locked until you declare"
ERR, but the run itself keeps going (reads still work, the agent is not killed,
the proxy stays up) → the agent declares the SAME issue again → the human
confirms a **second** Form A → the push lands. One real `rein run`, one
sandboxed script, two Form-A prompts.

- **What it proves that `idle_reattest` doesn't.** `idle_reattest` proves the
  bound that fires on SILENCE (no GitHub traffic at all). This journey proves the
  OTHER bound (#190's approval TTL, default 4h): it is measured from the human's
  most recent confirmation and is **NOT extended by activity** — a busy agent
  cannot hold a granted approval forever by staying busy, only a re-declare
  resets the clock. The busy loop is the whole point: if this journey used a
  quiet window instead, it would not distinguish the age bound from the idle one.
- **Golden contract.** Exit **0** = the flow held AND the normalized fresh run
  matches `golden.txt`; **1** = drift (normalized diff printed, raw fresh dropped
  to a scratch path; `REIN_UPDATE_GOLDEN=1` adopts); **2** = the flow itself
  broke (a phase rc / prompt count / branch / audit-log invariant was wrong).
  The golden is RAW; determinism lives in the comparator. The banner's printed
  TTL (`15s`, pinned by `REIN_APPROVAL_TTL`) is the CONFIGURED value, not
  elapsed time, so it needs no extra `_NORMALIZE_RULES` entry.
- **Timing.** `REIN_APPROVAL_TTL=15s` is a HOST-side launch-env knob
  (`cmd/rein/reattest.go`'s `envApprovalTTL`, same 10s floor and fail-closed
  parse as `REIN_IDLE_TIMEOUT`; srt's env allowlist never carries either into
  the sandbox). `REIN_IDLE_TIMEOUT` is deliberately left at its 30-minute
  default so it can never be what trips inside this run's ~1-minute lifetime —
  only the age bound can fire. The busy window runs `git ls-remote origin`
  five times, 5s apart (~25s total), comfortably crossing the 15s bound while
  keeping GitHub traffic flowing throughout.
- **Host-side invariants (independent of the golden, exit 2 on break):** both
  branches land on GitHub; the approval-age-lock banner (`rein: write approval
  is 15s old — writes LOCKED.` — note the DIFFERENT first line from
  `idle_reattest`'s `rein: idle for <d> — writes LOCKED.`, since the human's
  remedy differs: "this confirmation is old" vs "nothing was happening") and the
  write-token revoke line both print DURING the busy window, proving the lock
  fired on AGE ALONE, not on any pause in activity; the post-lock push is
  refused with the exact locked-writes ERR; exactly two Form A prompts fired;
  this run's own audit log (`state-dir/audit/sandbox-<runID>.log`, located by
  the run id in rein's own launch banner) records
  `decision=expired-approval-withdrawn` between the first and the second
  `decision=confirmed-issue`, and contains **no** `decision=expired-idle*`
  entry at all (the idle bound must never fire on a busy run).
- **Self-contained.** Creates its own throwaway issue via `gh`, deletes both
  branches and closes the issue in a `finally`.

**Two wording quirks worth knowing if you touch this journey (neither is a bug
worth a Go change, just documented so nobody "fixes" the journey to hide them):**
- The `@PHASE..` sentinel-echo gotcha is the same as `idle_reattest`'s: rein
  echoes the whole in-sandbox script source verbatim before executing it, so
  every sentinel literal appears twice. The journey uses `str.rfind`, not
  `find`. See `idle_reattest/README.md` for the full story.
- The write-token revoke line always reads `"...on idle re-attestation"`
  (`cmd/rein/run.go`'s `drainRunWriteTokens`/`revokeWriteTokens`), even on an
  approval-AGE trip — that helper is shared between both bounds and was never
  made reason-aware. The journey matches that literal text on purpose; it is
  not evidence of an idle-bound firing (the audit log's `decision=` field is
  the authoritative source for which bound tripped).

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.approval_ttl_reattest.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.approval_ttl_reattest.journey   # regenerate the RAW golden
```
