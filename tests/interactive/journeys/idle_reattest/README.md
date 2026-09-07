# idle_reattest — idle expiry re-attests writes in place, no hard TTL (#190)

**STATUS: BLOCKED — no golden yet.** The journey is fully written (all six phases,
including the push after the second re-declare) and reproduces a real bug on the
first live run: after the idle lock and the SECOND Form A re-confirmation, phase 6's
push (`agent/<n>/<nonce2>`, again) fails — `fatal: could not read Username for
'https://github.com': No such device or address`, rc=128. The run's own audit log
shows why: the final write-tier request is `decision=inject status=401` — the proxy
injected a STALE write token (the one minted before the idle lock, already revoked by
`idleReattest`'s `d.revoke()`) instead of minting a fresh one after the re-declare.

Root cause, traced to `internal/proxy/session.go`'s `mintWrite`
(`NewSessionCore`/`sessionState`): the write token is memoized in-process
(`st.writeToken`/`st.writeExpiry`) and is only dropped when the scope key changes
(issue #69, a mid-run scope expansion) or the cached expiry is within `skew` of
GitHub's own ~1h token TTL. #190's `idleReattest` revokes the token at GitHub and
clears the ON-DISK ledger/approval record, but never touches this in-memory cache —
so the very next `mintWrite` call still satisfies `st.writeToken != "" &&
time.Until(st.writeExpiry) > skew` and hands back the SAME (already-revoked) token,
which GitHub then 401s. The fix likely needs `idleReattest` (or its caller) to drop
`sessionState.writeToken` too, the same way a scope-key change already does.

**Do not adopt a golden until this is fixed** (see `tests/interactive/README.md`'s
catalogue, status `BLOCKED`); once fixed, re-run with `REIN_UPDATE_GOLDEN=1` twice
(prove determinism) and commit `golden.txt`.

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
  agent process.
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
  LOCKED.`) and the write-token revoke line both print DURING the idle window
  (phases 3→4), proving the lock and the revoke fired AT EXPIRY, not just at the
  run's real exit; the post-idle push is refused with the exact locked-writes ERR;
  exactly two Form A prompts fired; this run's own audit log
  (`state-dir/audit/sandbox-<runID>.log`, located by the run id in rein's own
  launch banner) records `decision=expired-idle` between the first and the second
  `decision=confirmed-issue`.
- **Self-contained.** Creates its own throwaway issue via `gh`, deletes both
  branches and closes the issue in a `finally`.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.idle_reattest.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.idle_reattest.journey   # regenerate the RAW golden
```
