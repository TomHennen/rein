# scope_expansion — declare a repo OUTSIDE scope → approve → push to it (COVERED)

One real `rein run` whose session is scoped to **repo A only**; the in-sandbox agent
runs `rein declare <issueB> --repo B` for an issue that lives in **repo B, OUTSIDE
that scope**. That fires the **SCOPE EXPANSION** prompt on the host tty (the distinct
"this ADDS a repo to the scope ceiling" header, carrying the fetched
title/state/home-repo). pexpect approves with the issue number, then answers the
persist `[y/N]` with **N** — the run-only path (a `y` would mutate the session file;
that leg is the plain test). The widened token then lets the agent clone repo B into
its writable `$TMPDIR` (binds are fixed at launch, so B can't nest in A's working
tree — #64) and push `agent/<issueB>/<nonce>` onto B.

- **Golden contract.** Exit **0** = expansion held AND normalized match; **1** =
  drift; **2** = the expansion broke (a phase rc / prompt-count / branch / persist
  invariant was wrong). Invariants: exactly one expansion prompt, zero plain prompts,
  the branch landed on B, persist=N left the session file unchanged.
- **Long-lived fixture, not per-run.** The golden bakes repo B's issue number + title
  RAW, so they must be stable-real. `ensure_fixture_issue` finds/reopens/creates an
  OPEN issue titled *"rein journey: scope-expansion fixture (safe to close)"* on repo
  B and leaves it open (so `[open]` in the prompt is stable). Override with
  `REIN_ITEST_ISSUE_B=<n>`. Only durable side effect is the agent's branch on B,
  deleted in a `finally`. Touches only the two throwaways (hard-constraint #1).

**Beside it (plain tests, no golden):** `test_scope_expansion.py` —
`ScopeExpansionDeny` (the DENY leg) and `ScopeExpansionCrossOwner` (the cross-owner
structural rejection). They are edge-case invariants with no reviewable narrative,
so they stay out of the golden.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.scope_expansion.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.scope_expansion.journey   # regenerate the RAW golden
```
