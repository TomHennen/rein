# claude_resume — the #94 claude sandbox trust model, PROVEN with a REAL claude (COVERED)

#94 flipped rein's `~/.claude` policy from an allowlist-of-denials (fail-OPEN: a new
subdir a claude update shipped leaked until noticed) to **default-deny**: the host
`~/.claude` / `~/.claude.json` are fully denied in-sandbox, and claude is repointed at
a rein-owned **persistent** overlay via `CLAUDE_CONFIG_DIR`. rein seeds only
`.credentials.json` (fresh, host-side, every launch) and authors **no** settings.json —
claude keeps its own permission prompts (defense-in-depth on top of the boundary; rein
does not launch `--dangerously-skip-permissions`). The overlay is bound read-WRITE and
**persists across runs**.

A live `claude` (headless `-p`/`-c`, so **no tmux/pyte**) proves three claims:

- **(a) authenticated in-sandbox** — `claude -p` answers a prompt, which it can only do
  because rein seeded its OAuth creds into the overlay (host `~/.claude` is denied);
- **(b) resume across two rein sessions** — run 1 (`claude -p`) stores a magic word;
  run 2 (`claude -c`, a **separate** `rein run`) recalls it from the persistent overlay;
- **(c) the host's real `~/.claude` stays hidden** — a deterministic bash probe in the
  same sandbox sees an EMPTY `~/.claude` (`history.jsonl` / `~/.claude.json` unreadable)
  while the overlay holds the seeded creds. The credential-boundary property still holds.

## Shape (a live LLM gets its own, per CLAUDE.md)

A real LLM's prose is never golden material. So:

1. **Invariants in code** — the regression oracle: run 2 recalled the magic word
   (resume), the probe saw the host tree empty + history unreadable (hiding) + the
   overlay creds seeded (auth). A break is exit **2**.
2. **The compared golden** (`golden.txt`) — deterministic content only, two shapes by
   agent kind: the two REAL-claude steps keep rein's launch surface **verbatim** through
   its `rein: running:` echo (`split_at_agent_launch`, so the claude-specific
   `--append-system-prompt` contract line is compared, not dropped by a `rein: `-prefix
   grep) then rein's own lines; the **deterministic bash probe** keeps its full
   `SBX|`-tagged transcript, like `sandbox_filesystem`. claude's own `-p`/`-c` stdout is
   excluded, so a completely different claude session still compares clean. The magic
   word is a FIXED phrase so run 1's `rein: running:` echo stays stable.
3. **The session** (`session.txt`) — **SHOWN, never compared** (the `realagent_write`
   convention: it lands as `session.txt`, not `golden.txt`, so nothing diffs it).
   It captures claude's **actual replies** — run 1's `ok` and run 2's recalled token
   `quokka-overlay-persists-1994` — the visible resume evidence a reviewer wants to
   read, which `golden.txt` deliberately excludes because a live model's exact wording
   would flake a byte-diff. Regenerated alongside `golden.txt` under `REIN_UPDATE_GOLDEN=1`.

**Golden contract.** Exit **0** = the three claims held AND the normalized transcript
matches; **1** = drift; **2** = a claim broke; **3** = SKIPPED (`claude` absent, or no
host claude login `~/.claude/.credentials.json` to seed — nothing to exercise, and a
skip must never look like a pass).

**QUOTA:** launches TWO real `claude` invocations and spends real API tokens (prompts
are one line each). **Self-contained:** clones the throwaway repo for the writable
checkout and removes it; pins its own repo-only session. It writes to rein's OWN
persistent overlay (`~/.config/rein-sandbox-home/.claude`) — that store is meant to
survive, so it is deliberately NOT cleaned. Touches only the throwaway repo
(hard-constraint #1); repo resolved the rein-init way (`resolve_throwaway_repo`).

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.claude_resume.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.claude_resume.journey   # regenerate the RAW golden
REIN_SHOW_NORMALIZED=1 python3 -m tests.interactive.journeys.claude_resume.journey # also print the compare lens
```
