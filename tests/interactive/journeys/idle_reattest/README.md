# idle_reattest — idle expiry re-attests writes in place, no hard TTL (#190)

Declare → confirm → verified push lands → the run goes **idle** (no GitHub traffic
at all) past `REIN_IDLE_TIMEOUT` → the write approval is withdrawn **in place**: the
next push is refused with the ordinary "writes are locked until you declare" ERR,
but the run itself keeps going (reads still work, the agent is not killed, the
proxy stays up) → the agent declares the SAME issue again → the human confirms a
**second** Form A → the push lands. One real `rein run`, one sandboxed script, two
Form-A prompts.

- **What it proves that `write_ceremony` doesn't.** `write_ceremony` proves the
  ceremony holds for a run's whole lifetime; this journey proves the **mid-run
  clock** (#190): there is no wall-clock cap that ends the session, and the ONLY
  thing idle takes away is the write approval — not the run, not reads, not the
  agent process. It also proves the write path actually WORKS again after
  re-declaring: an earlier version of this fix left the proxy's in-memory write
  token memoized across the idle trip, so the re-confirmed push 401'd (see "Found
  during authoring" below) — a golden alone would not have caught that, only the
  live push-lands assertion did.
- **Golden contract.** Exit **0** = the flow held AND the normalized fresh run
  matches `golden.txt`; **1** = drift (normalized diff printed, raw fresh dropped to
  a scratch path; `REIN_UPDATE_GOLDEN=1` adopts); **2** = the flow itself broke (a
  phase rc / prompt count / branch / audit-log invariant was wrong). The golden is
  RAW (real repo/issue/nonces); determinism lives in the comparator. The banner's
  printed idle bound (`15s`, pinned by `REIN_IDLE_TIMEOUT`) is the CONFIGURED value,
  not elapsed time, so it needs no extra `_NORMALIZE_RULES` entry.
- **Timing.** `REIN_IDLE_TIMEOUT=15s` is a HOST-side launch-env knob
  (`cmd/rein/reattest.go`'s `envIdleTimeout`; srt's env allowlist never carries it
  into the sandbox, so the agent can neither read nor set it). The script idles for
  25s with **zero** GitHub traffic — `sleep`, not even wrapped in a git/gh call —
  which comfortably crosses the 15s bound.
- **Host-side invariants (independent of the golden, exit 2 on break):** both
  branches land on GitHub; the idle-lock banner (`rein: idle for 15s — writes
  LOCKED.`) and the write-token revoke line (`rein: revoked <N> of <N> write
  token(s) on re-attestation`) both print DURING the idle window (phases
  3→4), proving the lock and the revoke fired AT EXPIRY, not just at the run's
  real exit; the post-idle push is refused with the exact locked-writes ERR;
  **phase 6's push (after the second re-declare) must LAND**; exactly two Form A
  prompts fired; this run's own audit log (`state-dir/audit/sandbox-<runID>.log`,
  located by the run id in rein's own launch banner) records
  `decision=expired-idle-withdrawn` between the first and the second
  `decision=confirmed-issue`.
- **Self-contained.** Creates its own throwaway issue via `gh`, deletes both
  branches and closes the issue in a `finally`.

**A gotcha in the invariant code itself, worth knowing if you touch this journey:**
rein echoes the WHOLE in-sandbox script source verbatim under `rein: running:`
before executing it, so every `@PHASE..` sentinel LITERAL appears **twice** in the
captured text — once in that echo, once for real. The journey locates the phase
boundaries with `str.rfind` (the last/real occurrence), not `find` (the first
occurrence, in the echo) — using `find` here silently computes the idle-window
ordering checks from the wrong (echoed, pre-idle-lock) copy of the text and fails
them for a reason that has nothing to do with rein's actual behavior. The
`@PHASEn_RC=$?` markers are immune (the echo has a literal `$?`, not digits, so the
regex that reads them only matches the evaluated, real line).

**Found during authoring, not a lingering issue:** the first version of this
journey (before `internal/proxy/session.go`'s `mintWrite` was fixed to drop its
memoized write token on every idle trip) reproduced a real 401 on phase 6 — the
proxy kept injecting the token minted before the idle lock, already revoked by
`idleReattest`, instead of minting a fresh one after the re-declare. Fixed on this
branch; the journey and its golden now reflect the fixed behavior.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.idle_reattest.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.idle_reattest.journey   # regenerate the RAW golden
```
