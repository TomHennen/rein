# gh_write — the in-sandbox `gh` / REST + GraphQL write boundary (#91, #101 gap 1) (COVERED)

The `gh` twin of `write_ceremony`'s git-push boundary. The SAME `gh api -X POST
.../issues/<n>/comments` write is:

- **DENIED before the declare** — rein's declare gate, a **local 403** (GitHub never
  contacted);
- **LANDS after declare+approve** — HTTP 201, the body echoed back.

Then, on the same post-declare token: a push to `agent/<n>/<nonce>` and a **`gh pr
create`** — which needs BOTH `pull_requests: write` AND the **GraphQL read** `gh pr
create` performs first (rein's proxy classifies/gates GraphQL separately from REST).
Host-side ground truth confirms the comment, the branch, and the PR really exist at
GitHub.

The regression proof for the #91 contents-only-token bug: before it, every in-sandbox
issue/PR write 403'd "Resource not accessible by integration" while `git push` worked
— falsifying the contract's promise that approving covers ALL writes. RAW golden,
normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.
`REIN_DEMO_ISSUE=<n>` reuses an issue.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.gh_write.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.gh_write.journey   # regenerate the RAW golden
```
