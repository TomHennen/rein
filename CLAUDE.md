# rein

A local credential broker for AI coding agents on a developer's laptop. Issues short-lived, narrowly-scoped GitHub credentials per issue, so agents never hold long-lived tokens.

## Read first

- `docs/design.md` — full design. §0 TL;DR is enough for routine work; read specific sections as needed.
- `PLAN.md` — current phase and checkpoint sequence.

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

## CI/CD

- `.github/workflows/build-and-publish.yml` calls wrangle's Go reusable workflow for source checks, release, and SLSA provenance.
- `.goreleaser.yml` is the adopter-owned release config wrangle wraps.
- Don't modify either of these files casually. They're load-bearing for supply-chain hygiene.

## When something surprises you

Note it in PLAN.md under "notes / blockers / design corrections needed" and surface it to the human. Don't silently work around design mismatches.
