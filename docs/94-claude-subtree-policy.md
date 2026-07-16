# A coherent sandbox policy for the `~/.claude` subtree (#94)

Status: **IMPLEMENTED** in this PR (#121). This documents the settled end-state,
agreed with Tom and proven live against real claude 2.1.204.

## Problem

rein hides the developer's `~/.claude` work history from the sandboxed agent so a
prompt-injected agent can't read cross-project transcripts and exfiltrate them via
whatever extra egress the operator opened. The OLD mechanism was an
**allowlist-of-denials**, and it was **fail-open**:

- `sandbox_home.go` allowed the WHOLE `~/.claude` (and `~/.claude.json`) back,
  read-only, so the agent could reach its own `.credentials.json`/`settings.json`.
- `credentialDenyReadPaths` (`run_sandboxed.go`) then re-hid a **hardcoded list** of
  known sub-dirs (`history.jsonl, projects, sessions, session-env, todos,
  shell-snapshots`, later `file-history, paste-cache, jobs, tasks, downloads,
  backups`).

Anything Claude Code added one level down that was NOT on that list stayed
**visible** by default — the #55 unknown-unknown: a new subdir ships in a Claude
Code update and silently leaks until someone notices. That inverts hard-constraint
#3 (**fail closed**): the safe default should be *hidden*, with visibility the
explicit exception.

## The settled design (four parts)

### 1. Default-deny the whole `~/.claude` subtree

The host's `~/.claude` and `~/.claude.json` are now **fully denied** in-sandbox. The
allow-backs are gone (`sandbox_home.go` `sandboxAllowReadPaths` no longer appends
`~/.claude`, a rein-env `CLAUDE_CONFIG_DIR`, or `~/.claude.json`), and the hardcoded
sub-deny list in `credentialDenyReadPaths` is **retired** — replaced by a single
whole-tree deny:

```
out = append(out, filepath.Join(home, ".claude"))       // whole tree
out = append(out, filepath.Join(home, ".claude.json"))  // sibling file
if cd := os.Getenv("CLAUDE_CONFIG_DIR"); cd != "" && filepath.IsAbs(cd) {
    out = append(out, cd)  // a relocated host config in rein's OWN launch env
}
```

The wholesale `$HOME` deny (#59) already hides these; listing them as authoritative
denies keeps them hidden even under the `REIN_SANDBOX_SHOW_HOME=1` kill switch
(`DenyReadCredStores` applies regardless of the home deny). Because the agent never
touches the host tree, a new subdir a future claude ships **can no longer leak** —
the class is closed structurally, not by chasing names.

### 2. A single shared, rein-owned, PERSISTENT overlay

The agent still needs a `~/.claude` to run — so rein gives it a rein-owned one:
`~/.config/rein-sandbox-home/.claude` (`config.SandboxClaudeHomeDir`,
`cmd/rein/sandbox_claude_home.go`). It is bound **read-write** via `ExtraAllowWrite`
(mechanism precedent: `run_sandboxed.go` `ExtraAllowWrite`, `config.go` allowWrite)
and **PERSISTS** across runs (NOT torn down, unlike the ephemeral agent scratch at
`run_sandboxed.go` `agentTmp`), so claude sessions **resume**.

**A SINGLE shared dir, not per-repo.** A rein session spans multiple repos, so
there is no clean repo key; a single shared home mirrors host claude's own one
`~/.claude`. The overlay is created `0700` and fails the launch closed if it is a
symlink or not user-owned (it holds the OAuth token).

**Why a sibling of `ConfigDir`, not under it.** `~/.config/rein` (`config.ConfigDir`)
holds the proxy CA key and is denied **wholesale** in-sandbox. An overlay nested
under it would (a) collide with that authoritative deny — `srt.Build`
(`internal/srt/config.go`) fails closed on a widening path at/under a deny — and
(b) need to be agent-**readable**, contradicting the deny. So the overlay is a
sibling: `~/.config/rein-sandbox-home/`, under the `$HOME` deny (re-bound read-write
via `ExtraAllowWrite`, the sanctioned pattern) but outside every credential-store
deny. It is added to the `writables` punch-out set so no allow-back can ro-bind over
it (#63).

### 3. Repoint `CLAUDE_CONFIG_DIR` at the overlay

The injected sandbox env sets `CLAUDE_CONFIG_DIR` to the overlay
(`internal/srt/env.go` `BuildEnv`, `EnvParams.ClaudeConfigDir`; precedent: the
`CLAUDE_CODE_TMPDIR` injection alongside it). With this, claude reads its creds +
settings from, and writes/resumes its session into, the overlay — and never touches
the fully-denied host `~/.claude`.

**Spike ground truth** (real claude 2.1.204, drives the seeding):
- Creds are read **exclusively** from `$CLAUDE_CONFIG_DIR/.credentials.json` — no
  fixed `~/.claude` fallback, and the host auth daemon does NOT serve the token to a
  repointed process. Empty dir → "Not logged in"; seeded copy → authenticated.
- `settings.json` comes from `$CLAUDE_CONFIG_DIR/settings.json`; host user-settings
  are NOT merged when repointed.
- `~/.claude.json` becomes `$CLAUDE_CONFIG_DIR/.claude.json`, regenerated from
  scratch at runtime — no host read needed.

### 4. Seeding strategy

- **Seed only `.credentials.json`** — copied fresh from host
  `~/.claude/.credentials.json` into the overlay, **host-side, before the in-sandbox
  deny is applied, on EVERY launch** (the OAuth token lives ~6h, so freshness matters).
  The read and write both refuse to follow a symlink (`O_NOFOLLOW`) and require user
  ownership — the keystore's security bar, matched even though this is not a PEM. The
  sandbox's rotated creds are **never** copied back to the host.
- **Author a rein-controlled minimal `settings.json`** — NOT a copy of the host's.
  rein reads nothing from claude's settings today, so the overlay's is deliberately
  minimal: `{"skipDangerousModePermissionPrompt": true}`. That one setting is the
  sandbox's explicit permission posture — it suppresses the startup confirmation that
  rein's `--dangerously-skip-permissions` launch would otherwise trigger and that
  would hang a headless/autonomous run. The sandbox IS the security boundary, so
  claude's own permission prompt is redundant here. (Host user-settings are NOT
  merged when repointed anyway.)
- **Do NOT seed `.claude.json`** — the overlay regenerates it; seeding would leak
  host project history.
- **Absent host creds is NOT an error.** rein guards GitHub credentials, not claude
  auth; the run proceeds with an unauthenticated overlay (claude reports "Not logged
  in") rather than failing closed — honest, not a silent degrade of a rein control.

### No redaction (explicitly out of scope)

rein makes **no claim** to sanitize the agent's own session content, and redaction
would break resume. In-rein transcripts are clean-by-construction: the agent never
sees the host's real creds or cross-project history, so what it writes into the
overlay is its own work, not the developer's. This is why persisting is safe here
without a redaction pass.

## Proven live (real claude 2.1.204)

`tests/interactive/journey_claude_resume.py` drives a REAL claude and proves all
three properties, with a checked-in golden (`golden/claude_resume.txt`):

- **(a) authenticated in-sandbox** — `claude -p` answers (creds seeded into the
  overlay; host `~/.claude` denied);
- **(b) resume across two rein sessions** — run 1 (`claude -p`) stores a token, run
  2 (`claude -c`, a SEPARATE `rein run`) recalls it from the persistent overlay;
- **(c) host `~/.claude` stays hidden** — a deterministic bash probe in the same
  sandbox sees an EMPTY `~/.claude` (`history.jsonl` / `~/.claude.json` unreadable)
  while the overlay holds the seeded creds.

## Deferred follow-ups

- **#125 — caches.** Give `~/.cache` / `~/.npm` the same rein-owned persistent
  overlay treatment (perf: today they are a cold, empty tmpfs every run). Regenerable
  → low blast-radius. Scope: perf, not security.
- **#122 — daemon key.** `~/.claude/daemon/control.key` (a mode-0600 key) is now
  fully hidden by the whole-tree deny in-sandbox; #122 tracks the broader daemon-key
  hardening separately.
