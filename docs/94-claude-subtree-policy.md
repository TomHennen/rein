# A coherent sandbox policy for the `~/.claude` subtree (#94)

Status: DECISION DOC for Tom. The min-safe hardening (below) is SHIPPED in this
PR. The end-state (default-deny + overlay + redaction) is PROPOSED, NOT built —
three gates need sign-off (last section) per CLAUDE.md hard-constraint #5.

## Problem

rein hides the developer's `~/.claude` work history from the sandboxed agent so a
prompt-injected agent can't read cross-project transcripts and exfiltrate them via
whatever extra egress the operator opened. But the mechanism is an
**allowlist-of-denials**, and it is **fail-open**:

- `sandbox_home.go:131-145` allows the WHOLE `~/.claude` (and `~/.claude.json`)
  back, read-only, so the agent can reach its own `.credentials.json`/`settings.json`.
- the sub-deny list in `credentialDenyReadPaths` then re-hides a **hardcoded list** of known sub-dirs
  (`history.jsonl, projects, sessions, session-env, todos, shell-snapshots`).

Anything Claude Code adds one level down that is NOT on that list stays **visible**
by default. This is the #55 unknown-unknown: a new subdir (`jobs`, `file-history`,
`paste-cache`, …) ships in a Claude Code update and silently leaks until someone
notices. That inverts hard-constraint #3 (**fail closed**): the safe default should
be *hidden*, with visibility the explicit exception — not the reverse.

### Live leak found (now patched)

A live audit of `~/.claude` on the dev box found these VISIBLE in-sandbox, all the
same secret/history class as the already-hidden dirs:

| subdir | what it holds | class | severity |
|---|---|---|---|
| `file-history` | versioned copies of every file the agent edited, ALL projects | history/secret | **must-fix** |
| `paste-cache` | mode-0600 pasted blobs (keys, snippets) | secret | **must-fix** |
| `jobs` | background-job state | artifact | fix |
| `tasks` | task state | artifact | fix |
| `downloads` | files the agent downloaded | artifact | fix |
| `backups` | `.claude.json.backup.*` — stale copies of the file we deliberately allow-back readable (`sandbox_home.go:136-145`, #62) | history | fix |

**Shipped in this PR:** all six appended to the sub-deny list in
`credentialDenyReadPaths` (+ pinned in `run_sandboxed_test.go`). Pure hardening — no
agent-behavior change (denyRead of a dir = empty writable tmpfs, so any dir Claude
*writes* still works, ephemerally). This closes the known holes but does NOT fix the
fail-open shape — that is the end-state below.

## `~/.claude` subdir inventory (basis for a decidable allowlist)

Classes: **CRED** (must stay readable for the agent to run) · **HIST** (cross-project
history/secrets — must stay hidden) · **RUNTIME-WRITE** (Claude writes it per-run;
under a deny it is an empty writable tmpfs, so hiding it is free) · **CACHE**
(regenerable; hiding = cold start, degraded-but-working) · **CODE** (installed code
the agent may need to read).

| entry | class | in default-deny world |
|---|---|---|
| `.credentials.json` | CRED | **allow (read)** — agent OAuth |
| `settings.json` | CRED/config | **allow (read)** |
| `.claude.json` (sibling file) | config + HIST | allow today (issue #62 tradeoff); ideally sanitized copy |
| `plugins/` | CODE | allow (read) if installed plugins must load; else deny |
| `history.jsonl`, `projects/`, `sessions/`, `shell-snapshots/`, `todos/` | HIST | deny (already) |
| `file-history/`, `paste-cache/`, `jobs/`, `tasks/`, `downloads/`, `backups/` | HIST/artifact | deny (**this PR**) |
| `session-env/` | HIST + RUNTIME-WRITE | deny (already; tmpfs doubles as per-run scratch) |
| `cache/`, `gh-pr-status-cache.json`, `mcp-needs-auth-cache.json`, `stats-cache.json` | CACHE | deny (cold start ok) |
| `daemon/` (holds a mode-0600 `control.key`), `daemon.log`, `.last-cleanup`, `.last-update-result.json` | RUNTIME-WRITE + key | deny under the flip; still readable today (low reachability — fresh /tmp), deferred to it — issue #122 |

Decidability check: under default-deny the **allowlist is tiny and stable** —
`.credentials.json`, `settings.json`, `.claude.json`, and (maybe) `plugins/`.
Everything else is deny-by-default; runtime dirs keep working via the writable
tmpfs. This is what makes the flip safe and low-maintenance.

## Options, ranked (security posture)

### 1. (SHIPPED) Keep the allowlist-of-denials, patch the six leaks
- **Posture:** closes today's known holes. Still **fail-open**: the #55
  unknown-unknown returns with the next Claude Code subdir. Zero behavior risk.
- **Verdict:** correct as an immediate stop-gap (done). Not the resting state.

### 2. Flip to default-deny + explicit allowlist  ← **recommended core**
- **Posture:** **fail-closed** (hard-constraint #3). A new subdir is hidden by
  default; visibility is an explicit, reviewed exception. Best security/maintenance
  ratio. Small, contained change.
- **Cost:** must enumerate the true read-needs of a live Claude (the inventory
  above is the starting allowlist); risk of hiding something Claude genuinely needs
  → caught loudly by the sandbox self-test / a real-claude journey, not silently.

### 3. Per-repo persistent overlay (resume)
- **Posture:** neutral-to-positive ON TOP of #2. Gives durable, resumable history
  in a **rein-owned namespace**, never the host's real `~/.claude`. Safe *only*
  because in-rein transcripts are clean-by-construction (agent never sees real
  creds). Adds cross-session blast-radius surface → granularity matters (gate 3).
- **Cost:** more moving parts (persistent host dir + `CLAUDE_CONFIG_DIR` repoint).

### 4. Redaction-on-write
- **Posture:** defense-in-depth for the `api.anthropic.com`/MCP caveat (data can
  still legitimately flow through the model channel into a transcript). **REQUIRED
  precondition of #3:** persisting transcripts WITHOUT redaction is net-negative —
  strictly worse than today's discard-at-run-end tmpfs, because it turns a transient
  in-memory exposure into a durable on-disk one.
- **Cost:** a redaction pass (bagel-style) on the persist path; false-neg risk.

Ranking rationale: #2 is the coherent fix and should land regardless. #3 is only
worth building *with* #4; #3 without #4 is a regression.

## Recommended end-state and concrete wiring

**Default-deny allowlist (#2) + per-repo overlay (#3) + mandatory redaction (#4).**

### Wiring #2 — default-deny the `~/.claude` subtree
- **Stop allowing the whole dir back.** `sandbox_home.go:131-135` currently appends
  `~/.claude` wholesale. Replace with allow-backs of only the CRED entries
  (`~/.claude/.credentials.json`, `~/.claude/settings.json`, and `plugins/` if
  needed). The parent `~/.claude` stays under the `$HOME` deny → empty tmpfs by
  default; the narrow allow-backs re-expose exactly the read-needs.
- **srt layering makes this work:** allow-back is shallow, deeper/exact deny wins;
  a file allow-back re-binds just that host file. The sub-deny list in
  `credentialDenyReadPaths` then becomes **redundant** (everything is denied by default) and can
  be retired — or kept as belt-and-suspenders.
- **Fail-closed guards it must satisfy** (`internal/srt/config.go:276-293`):
  `srt.Build` errors if a widening (allow-back/allowWrite) path sits **at or under**
  an authoritative deny (`:276-282`), and if an allow-back covers the whole home
  deny (`:287-293`). So the CRED allow-backs must be the specific files, never a
  dir that re-contains a deny. Getting this wrong fails the build LOUDLY — the
  desired direction.
- **`~/.claude.json`** (`:136-145`) stays as-is (issue #62 tradeoff: it carries
  per-project prompt history but Claude re-onboards without it). Revisit under #4.

### Wiring #3 — per-repo persistent overlay
- **Mechanism already exists:** srt `allowWrite` is a persistent rw host bind
  (`ExtraAllowWrite`, `run_sandboxed.go:420`, `config.go:346`) — the working tree
  uses it. A persistent overlay = an `ExtraAllowWrite` of a **rein-owned** dir
  (e.g. `~/.config/rein/sandbox-home/<repo-key>/.claude/`), contrasting the
  **ephemeral** scratch tmpfs at `run_sandboxed.go:236-240`.
- **CAVEAT — no source→dest remap in srt:** the overlay appears at its own host
  path in-sandbox. To make Claude actually use it, repoint via `CLAUDE_CONFIG_DIR`
  in the injected env (`internal/srt/env.go` `BuildEnv`; precedent: the
  `CLAUDE_CODE_TMPDIR` injection at `env.go:303-306`). With `CLAUDE_CONFIG_DIR`
  pointed at the overlay, the whole `~/.claude` question moves to the rein-owned
  path — the host's real `~/.claude` is then fully denied (no allow-back needed),
  and resume reads/writes stay entirely in the rein namespace.
- Same mechanism also fixes cold caches (`~/.cache`, `~/.npm` as persistent
  rein-owned dirs) — out of scope for #94 but noted.

### Wiring #4 — redaction-on-write
- Redact on the **persist path only** (writing the overlay), not on read. A
  bagel-style secret scan scrubs matches before bytes land in the rein-owned
  overlay. This is the gate that makes #3 safe; if it can't be built reliably,
  **do not build #3** — keep the ephemeral tmpfs.

## THREE decisions awaiting Tom's sign-off (do NOT implement)

Per hard-constraint #5 (stop-and-ask on security-sensitive decisions):

1. **Flip to default-deny** for the `~/.claude` subtree (retire the
   allowlist-of-denials in `credentialDenyReadPaths` in favor of default-deny +
   narrow CRED allow-backs at `sandbox_home.go:131-135`). Security-positive but
   changes the sandbox's read surface for a live Claude — needs a real-claude
   journey to prove nothing essential got hidden.
2. **Persist transcripts at all.** Today they are discarded at run end (tmpfs).
   Persisting is only safe WITH redaction-on-write (#4); persisting without it is a
   regression. Decide whether durable resume is wanted, and that #4 is a hard
   precondition.
3. **Overlay granularity** — global vs per-role vs per-repo vs per-session. Sets
   both resume granularity AND the cross-session secret-blast-radius if a transcript
   ever does capture something through the model/MCP channel. Recommendation:
   per-repo, but this is Tom's call.

The min-safe patch in this PR needs none of these — it is pure hardening. The three
gates govern only the end-state.
