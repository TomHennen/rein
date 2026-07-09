# CP6 dogfood — test plan

**Goal (design §7.2 hypothesis):** run rein's sandboxed mode for real work and
confirm it holds — you don't revert to a PAT under deadline pressure. Two
phases: a **throwaway shakedown** (mostly automatable), then the **real-repo
gate** (`wrangle` — your conscious call, hard-constraint #1).

**Precondition:** CP4.5 landed (the sandbox can run a real agent — egress to the
agent's API + package hosts, agent's own credential reaches it). Until then
`rein run -- claude` can't start.

**How writes get approved in automated runs — drive the REAL tty prompt.** The
approval prompt opens `/dev/tty` directly (not stdin). To automate the *actual*
human path, wrap `rein run` in a pseudo-terminal and script the interaction with
**`pexpect`** (installed here, 4.9.0): spawn the run under a pty, wait for the
approval prompt, `sendline(str(issue_number))`. This is faithful and does NOT
weaken the security model — pexpect gives the *host-side `rein run`* process a
controlling tty; the *sandboxed agent* still has none (srt `--new-session`), so
it still can't reach the prompt. The harness is the host-side human, exactly
where a real approver sits. (`tmux send-keys`/`capture-pane` is an alternative;
`rein approval grant --run-id <id>` from the host exercises the *secondary*
grant path and is worth one run too.) For *your* interactive runs, a real
terminal (local or SSH) gives the tty and you approve live.

---

## Phase A — throwaway shakedown (I can run most of this)

Throwaway repo, session with a real `issue:`. Each scenario: expected result +
who runs it.

### A1. Real agent runs in the sandbox (the CP4.5 proof) — [me]
`rein run -- claude -p "<small task on the throwaway>"`. Expect: the agent
reaches its API (no egress block), authenticates, does the task. This is the
end-to-end proof that rein can wrap a real agent at all.

### A2. Read path — [me]
Inside `rein run`: `git clone` the throwaway (HTTPS), `git fetch`, `gh api
repos/<throwaway>`, `gh issue list`. Expect: all 200/success via injected read
tokens; no token visible to the agent.

### A3. Write path + approval — [me, via grant side channel]
Inside `rein run`: branch, edit, commit, `git push`; then `gh pr create`, `gh
issue comment`. Expect: first write blocks → approve via `rein approval grant
--run-id <id>` from the host → push/PR/comment succeed; commit authored as
`<name> (via rein)` + bot email; pusher = the App bot (check the repo Activity
feed). Confirm one approval covers the whole run (run-scoped).

### A4. Egress policy — [me]
Inside `rein run`: a project command that hits a NON-GitHub host you allowed
(e.g. `npm install` / `curl` the agent's API). Expect: allowed hosts work
(direct TLS, no injection); a NON-allowed host (e.g. `curl https://example.com`)
is blocked. Confirms the CP4.5 allowlist + the exfil boundary.

### A5. Ambient-credential isolation — [me]
Inside `rein run`: attempt to read the host's `gh` login, `~/.ssh`, a relocated
`XDG_CONFIG_HOME/gh` — all must be empty/hidden; seeded `ANTHROPIC_API_KEY`
(other than the agent's own) and `AWS_*` must be absent. (Re-confirms CP3/CP4
live-gate under a real agent's process tree.)

### A6. Approval non-forgeability — [me]
From inside the sandbox, attempt to self-approve: open `/dev/tty` (ENXIO),
attempt to write the approval file under stateDir (deny-read + not writable).
Both must fail. (Re-confirms the CP4 tty-severing finding under a real run.)

### A7. Expiry — [me, needs a short-timeout build hook or a patient run]
Idle past the idle timeout (or hard TTL), then attempt a write → must fail
closed (token revoked, proxy down), with a loud message. (Currently 30m/4h with
no fast override — either add a test-only override or accept the unit coverage;
decide when running.)

### A8. Failure modes / loud-degrade — [me]
- Break a sandbox prereq (e.g. rename `srt` on PATH) → `rein run` fails closed
  with a `rein doctor` pointer, does NOT run unsandboxed.
- `--direct` on the throwaway → loud reduced-protection banner, then runs.
- Session with no `issue:` → writes denied, reads flow.
- Clock skew (if reproducible) → mint fails with the doctor hint.

### A9. Concurrency — [me]
Two `rein run` sessions at once → isolated scope/approval/tokens (no bleed).

---

## Phase B — the real feel (you, interactive)

Automation proves the mechanisms; only a human proves the *hypothesis*. Over a
real terminal (SSH is fine — gives the tty):

### B1. A real session on the throwaway — [Tom]
`rein run -- claude` interactively; give it a genuine small task (fix a bug, add
a test, open a PR). Experience: the write prompt in your flow, the latency, any
friction. Note anything that would tempt you back to a PAT.

### B2. Sustained use — [Tom]
A few real throwaway sessions over a few days. Watch for: prompt fatigue,
egress surprises (a tool that needs a host you didn't allow), expiry
interrupting a long task, identity/attribution correctness on GitHub.

### B3. THE GATE — first real repo (`wrangle`) — [Tom, explicit decision]
Only after A + B1/B2 are clean. This crosses hard-constraint #1. Not granted by
this plan — your call. Prereqs: durable NTP (#23), the srt pin re-verified.
Then: the §7.2 hypothesis — two weeks on `wrangle`, no PAT fallback.

---

## Exit criteria
- **Phase A:** every scenario green; no credential ever visible to the agent;
  every degrade is loud + fail-closed.
- **Phase B:** you'd reach for `rein run -- claude` by default on the throwaway
  without it slowing you down — the precondition for trusting it on `wrangle`.

## Record results
Append outcomes here (date — scenario — result) as they run.
