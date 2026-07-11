# rein

A local credential broker for AI coding agents on a developer's laptop. Issues short-lived, narrowly-scoped GitHub credentials per issue, so agents never hold long-lived tokens.

## Read first

- `docs/design.md` — full design. §0 TL;DR is enough for routine work; read specific sections as needed.
- `PLAN-1.md` — current phase plan (Phase 1: sandboxed mode, CP1-CP6). Design of record: `docs/phase1-design.md` + `docs/phase1-srt-spike-findings.md`.
- `HANDOFF.md` — bring up Phase 1 on a fresh `git clone` (env prereqs + `rein init` for your own App) and the live resume pointer. Read when picking up on another machine.
- `PLAN-0.5.md` and `PLAN.md` — Phase 0.5 / Phase 0 records, historical.
- `phase0_findings.md` — what Phase 0 actually built + the 7 design corrections to the original PLAN.md + the 4 Shape B limits observed empirically. Read before doing Phase 0.5 design work; don't re-derive.

## Hard constraints

1. **Throwaway repos only** until Phase 1. Never touch a real repo.
2. **Credential helper must always return a credential.** Never empty, never error. See design §5.3 TM-G8.
3. **Fail closed.** Surface errors to the user; don't silently degrade.
4. **License compliance on all imports.** Check before adding a dependency.
5. **Stop and ask on security-sensitive decisions.** The whole project is about security.
6. **All private-key reads MUST go through `internal/keystore.Keystore` (Get/Fingerprint).** Never `os.ReadFile` a PEM directly — the keystore enforces uid + mode `0o077` checks on read and is the swap point for Phase 1's daemon-backed and Phase 1/2 biometric backends.

## Libraries (don't reinvent these)

- Proxy: **hand-rolled** in `internal/proxy` (the CP1 relay recipe), NOT
  goproxy. Do not "helpfully" swap goproxy back in: srt hands rein an
  opaque byte tunnel to a unix socket (`mitmProxy.socketPath`) that rein
  must TLS-terminate, inject into, and relay itself, and the injection
  invariants (SNI==Host, per-host-class inject, no token on the response
  path, HTTP/1.1-only relay, ContentLength/TransferEncoding copy,
  no-redirect-follow) need direct control of the request/response loop —
  goproxy's shape doesn't fit either the socket hook or those invariants.
- GitHub App tokens: `github.com/jferrl/go-githubauth` (MIT)
- Key storage: hand-rolled `internal/keystore` file backend (PEMs under ConfigDir with uid + mode `0o077` + O_NOFOLLOW checks). `github.com/99designs/keyring` (MIT) was planned but never adopted — it is NOT in go.mod; `internal/keystore` is the swap point for future backends (hard-constraint #6).
- Hardware keys (Phase 1+): `github.com/facebookincubator/sks` (Apache 2.0). Not used in Phase 0.
- CLI: `github.com/spf13/cobra`

## Dev environment

- Development happens directly in this Linux VM. There is no devcontainer.
- Source `./dev-env` at the start of each work session to load the `REIN_*` environment variables.
- The GitHub App private key is at `$REIN_APP_PRIVATE_KEY_PATH` (`~/.config/rein-credentials/app.pem`).
- Secure Enclave is not available on Linux. Phase 0/1 use the `internal/keystore` file backend. Phase 1's hardware-backed work would require TPM2 (if this VM has one) or shift to a Mac host.
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
- **You can drive the tty yourself — the write path does NOT need a human present.** The pexpect suite in `tests/interactive/` gives `rein` a real pty, so it IS the human stand-in: it answers the Form A prompt exactly as a developer would. An agent can and **should** self-verify the whole write ceremony (declare → confirm → push lands) with nobody at the keyboard — don't park verification for the human. This does not weaken the model: the *sandboxed* agent has no tty at all and still cannot self-approve; what pexpect drives is the host-side prompt. A **manual script** is only for what pexpect genuinely cannot drive: a **real browser** — i.e. the GitHub App *creation* (manifest) flow. Nothing else.
- **Every behavior-changing PR moves a JOURNEY, and journeys ship a golden transcript — not just green tests.** A journey walks a major user path live and its deliverable is a checked-in, human-reviewable golden transcript (drift = red = re-review). Full authoring rules — golden-transcript rule, journey-vs-plain-test distinction, the `SBX|` view-split, expect→act→expect, shared helpers, and the `rein init` (not `source ./dev-env`) setup — live in **`tests/interactive/CLAUDE.md`**; the catalogue table is in `tests/interactive/README.md`. Why this matters: green suites hid a skipped install-coverage check (#68, one path untested) and would have hidden the #35 `.git`-remote push break (the fake ignored the repo field) — a live journey catches what a unit test's single path doesn't.

## When something surprises you

Note it in the current PLAN's "notes / blockers / design corrections needed" section and surface it to the human. Don't silently work around design mismatches.
