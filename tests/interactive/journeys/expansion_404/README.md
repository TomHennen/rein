# expansion_404 — the 404-at-expansion install NOTICE (#69) (COVERED)

The sibling of `scope_expansion` (issue #69, mocks §1.4/§5.2): the agent declares an
expansion to a repo the App is **NOT installed on** (same owner, so it passes the
cross-owner check and reaches the coverage probe). Nothing is approvable, so **NO
prompt fires** — the human gets an interactive NOTICE (names the repo, "there is no
approval to give", an install deep-link), and the declare **REFUSES** with the
agent-facing install-then-retry message.

The golden shows the host-tty NOTICE and the SBX-tagged agent refusal in one
interleaved terminal. RAW golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.expansion_404.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.expansion_404.journey   # regenerate the RAW golden
```
