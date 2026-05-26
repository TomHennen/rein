# rein

A local credential broker for AI coding agents on a developer's laptop. Issues short-lived, narrowly-scoped GitHub credentials per issue, so agents never hold long-lived tokens.

## Read first

- `docs/design.md` — full design. §0 TL;DR is enough for routine work; read specific sections as needed.
- `PLAN-0.5.md` — current phase plan (CP1-CP7). Replaces `PLAN.md` (Phase 0 record, historical).
- `phase0_findings.md` — what Phase 0 actually built + the 7 design corrections to the original PLAN.md + the 4 Shape B limits observed empirically. Read before doing Phase 0.5 design work; don't re-derive.

## Hard constraints

1. **Throwaway repos only** until Phase 1. Never touch a real repo.
2. **Credential helper must always return a credential.** Never empty, never error. See design §5.3 TM-G8.
3. **Fail closed.** Surface errors to the user; don't silently degrade.
4. **License compliance on all imports.** Check before adding a dependency.
5. **Stop and ask on security-sensitive decisions.** The whole project is about security.

## Libraries (don't reinvent these)

- Proxy: `github.com/elazarl/goproxy` (BSD)
- GitHub App tokens: `github.com/jferrl/go-githubauth` (MIT)
- Key storage: `github.com/99designs/keyring` (MIT) — uses Secret Service backend (libsecret/D-Bus) on Linux when available, file backend otherwise
- Hardware keys (Phase 1+): `github.com/facebookincubator/sks` (Apache 2.0). Not used in Phase 0.
- CLI: `github.com/spf13/cobra`

## Dev environment

- Development happens directly in this Linux VM. There is no devcontainer.
- Source `./dev-env` at the start of each work session to load the `REIN_*` environment variables.
- The GitHub App private key is at `$REIN_APP_PRIVATE_KEY_PATH` (`~/.config/rein-credentials/app.pem`).
- Secure Enclave is not available on Linux. Phase 0 uses the `99designs/keyring` file backend. Phase 1's hardware-backed work would require TPM2 (if this VM has one) or shift to a Mac host.
- srt sandbox is out of scope for Phase 0.
- run `gofmt -w .` before all commits.

## CI/CD

- `.github/workflows/build-and-publish.yml` calls wrangle's Go reusable workflow for source checks, release, and SLSA provenance.
- `.goreleaser.yml` is the adopter-owned release config wrangle wraps.
- Don't modify either of these files casually. They're load-bearing for supply-chain hygiene.

## Working style

- Call `advisor()` before substantive work and before declaring a checkpoint done. The advice is high-value; skipping costs more than running it.
- Spawn a reviewer subagent (Agent tool, `claude` type) after each checkpoint's implementation, before surfacing to the human. Brief tightly; ask for real findings only.
- Use `TaskCreate` / `TaskUpdate` to track checkpoint progress; mark `in_progress` when starting, `completed` when done.
- Stop-and-surface at every checkpoint per the current PLAN's discipline section. Don't proceed past a gate without human verification.
- File GitHub issues for deferred items via `gh issue create`. Don't bury followups in commit messages alone — they get lost.
- **Fix smells now, don't defer.** When you notice a fixable code smell during work — formatting inconsistency, hand-counted spacing, a missing escape-hatch env var, a cleanup the reviewer flagged as a nit — fix it in the same pass. Don't ask "fix or defer?"; don't surface it as a deferred item; don't leave it for someone else. Only surface for decision when the fix changes behavior in a way the human should weigh in on (security, API shape, user-visible UX). Pure source-cleanliness fixes are decisions you should already be making. Bias: prefer fixing slightly more than feels necessary over asking.
- No emojis in files unless explicitly requested.
- For interactive tests (anything needing `/dev/tty`, browser, or tmux popup), write a manual script the human runs in their real terminal. Phase 0 used `/tmp/cp*-manual-test.sh`; same pattern.

## When something surprises you

Note it in the current PLAN's "notes / blockers / design corrections needed" section and surface it to the human. Don't silently work around design mismatches.
