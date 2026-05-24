# Bootstrap — one-time setup on a fresh Linux VM

Run these on the VM where you'll be developing `rein`. ~15–20 minutes total if the §12 validation artifacts (App + throwaway repos) still exist, longer if they need recreating.

The devcontainer is gone (see commit `08f006c`); development happens directly in the VM. Non-secret config lives in the in-repo `dev-env` file; secrets live outside the repo.

## 1. Create the GitHub repo (~2 min)

On GitHub: New repository → name `rein` → Private → Add a README → Create.

```bash
cd ~/dev   # or wherever you keep projects
git clone https://github.com/TomHennen/rein.git
cd rein
```

## 2. Drop in the scaffold (~2 min)

If you're seeding from the bootstrap bundle, the repo root should contain:

```
rein/
├── .github/
│   └── workflows/
│       └── build-and-publish.yml
├── docs/
│   └── design.md
├── .goreleaser.yml
├── BOOTSTRAP.md     (this file)
├── CLAUDE.md
├── PLAN.md
└── dev-env
```

## 3. Resolve the wrangle SHA pin (~1 min, skip if already pinned)

`.github/workflows/build-and-publish.yml` references wrangle's reusable Go workflow at a pinned SHA. If the placeholder `__WRANGLE_GO_MERGE_SHA__` is still present:

```bash
WRANGLE_SHA=$(gh api repos/TomHennen/wrangle/commits/main --jq '.sha')
sed -i "s/__WRANGLE_GO_MERGE_SHA__/$WRANGLE_SHA/" .github/workflows/build-and-publish.yml
grep "uses: TomHennen/wrangle" .github/workflows/build-and-publish.yml
```

## 4. Commit the scaffold (~1 min)

```bash
git add .
git commit -m "Initial scaffold"
git push
```

The first CI run's source checks (`gofmt`, `go vet`, `go test`, `govulncheck`) will fail until `go mod init` happens — that's expected.

## 5. Set up the secrets directory (~5 min)

The App private key lives outside the repo — never committed.

```bash
mkdir -p ~/.config/rein-credentials
chmod 700 ~/.config/rein-credentials
```

If the §12 validation App key is still around:

```bash
cp <wherever-it-was>/app.pem ~/.config/rein-credentials/app.pem
chmod 600 ~/.config/rein-credentials/app.pem
```

Otherwise: GitHub → Settings → Developer settings → GitHub Apps → your validation App → Private keys → Generate a private key → move the `.pem` into `~/.config/rein-credentials/app.pem` with `chmod 600`.

Verify:

```bash
ls -la ~/.config/rein-credentials/app.pem
# -rw------- (600 permissions)
head -1 ~/.config/rein-credentials/app.pem
# -----BEGIN RSA PRIVATE KEY-----
```

## 6. Confirm `dev-env` matches your App (~1 min)

The repo's `dev-env` file holds non-secret App identifiers and test-repo names. Open it and confirm the values match your GitHub App and throwaway repos. If you're reusing the existing App from the validation work, the committed values should be correct.

Source it at the start of each work session:

```bash
source ./dev-env
env | grep ^REIN_   # all five REIN_* vars should be set
```

`ANTHROPIC_API_KEY` is not in `dev-env` (it's a credential). Export it from your shell rc or a separate untracked file.

## 7. Confirm the throwaway repos and App are intact (~1 min)

```bash
source ./dev-env
gh repo view "$REIN_TEST_REPO_A" --json name,isPrivate
gh repo view "$REIN_TEST_REPO_B" --json name,isPrivate
```

If any fail (repos deleted, App deleted), recreate per the §12 validation pre-work in the design doc.

## 8. Verify the toolchain (~2 min)

```bash
go version          # 1.22+ is fine for Phase 0; goreleaser pins its own toolchain
gh --version
gh auth status
claude --version    # if missing: curl -fsSL https://claude.ai/install.sh | bash -s -- --yes
```

## 9. Initialize the Go module (~30 sec)

```bash
cd ~/dev/rein
go mod init github.com/TomHennen/rein
git add go.mod
git commit -m "go mod init"
git push
```

CI source checks should now pass (empty module, nothing to lint or test).

## 10. Hand off to the agent

In the repo:

```bash
source ./dev-env
claude
```

Paste:

```
Read CLAUDE.md and PLAN.md. Start at Checkpoint 1. Stop after each
checkpoint and surface what you built to me before proceeding. The
design doc at docs/design.md is the authoritative reference; read
sections as needed.

Don't modify .github/, .goreleaser.yml, or dev-env without surfacing
the change first. Don't touch anything outside the working directory
or the throwaway repos. Don't run sudo without asking.

Surface anything that surprises you. Don't work around design
mismatches silently.

Start by running `env | grep REIN` to confirm the environment is set
up. If anything is missing, stop and ask.
```

## Watching it work

- **Between checkpoints:** the agent stops and surfaces. Verify before saying "proceed."
- **CI runs:** every push triggers wrangle's checks.
- **`PLAN.md` notes section:** the agent appends design corrections and surprises there. Check periodically.

## If something goes wrong

| Symptom | Fix |
|---|---|
| `env \| grep REIN` empty | `source ./dev-env` |
| `app.pem` not readable | `chmod 600 ~/.config/rein-credentials/app.pem` |
| CI fails on wrangle workflow | Check the SHA pin in `.github/workflows/build-and-publish.yml` |
| Agent runs `gh auth setup-git` | TM-G8 happening live — surface to Tom |

## Cleanup after Phase 0

```bash
rm -rf ~/.config/rein-credentials/
# Delete throwaway repos via the GitHub UI if done with them
# Delete the test App via Settings → Developer settings → GitHub Apps → Delete
```
