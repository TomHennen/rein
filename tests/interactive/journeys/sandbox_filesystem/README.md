# sandbox_filesystem — the sandbox filesystem boundary, from INSIDE (#59/#63/#64) (COVERED)

What a sandboxed agent can and cannot touch, proven from inside the sandbox:

- credential stores + `~/.ssh` + `~/.aws` + rein's OWN app key read as **absent**;
- `$HOME` is **ephemeral** — a write succeeds into tmpfs, then never persists on the
  host;
- the `.git` host-exec escape is **CLOSED** (`mv .git` → "Device or resource busy",
  `.git/hooks` + `.git/config` read-only);
- ordinary edits still `add`/`commit`;
- the injected agent contract is shown **verbatim**.

The "agent" is a deterministic bash script (reproducible, unlike a real claude), so
the interleaving is one stable composite golden. RAW golden, normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

**Beside it:** `test_git_hardening.py` asserts the `.git` escape stays closed (incl.
the `config.worktree` edge) as pass/fail invariants; `test_agent_contract.py` reads
the contract back from a REAL claude (LLM phrasing varies, so NOT golden material).
Both are run by `run-journeys.sh --sandbox`. **Complementary journey:**
`credential_boundary` proves the same hide with an INDEPENDENT scanner.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.sandbox_filesystem.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.sandbox_filesystem.journey   # regenerate the RAW golden
```
