# Bootstrap — host-side setup before handing to the agent

Run these on your host (Mac or Linux VM) before opening the repo in VS Code. ~15-20 minutes total if the §12 validation artifacts are still around, longer if not.

## 1. Create the GitHub repo (~2 min)

On GitHub: New repository → name `rein` (or whatever you prefer) → Private → Add a README → Create.

Clone it:

```bash
cd ~/dev   # or wherever you keep projects
git clone https://github.com/TomHennen/rein.git
cd rein
```

## 2. Drop in the bootstrap files (~2 min)

Unzip the bootstrap bundle into the repo root. You should now have:

```
rein/
├── .devcontainer/
│   ├── devcontainer.json
│   └── Dockerfile
├── .github/
│   └── workflows/
│       └── build-and-publish.yml
├── docs/
│   └── design.md
├── .goreleaser.yml
├── BOOTSTRAP.md      (this file)
├── CLAUDE.md
└── PLAN.md
```

## 3. Resolve the wrangle SHA pin (~1 min)

The CI workflow references wrangle's Go reusable workflow at a pinned SHA. The bundle ships with a placeholder; resolve it now:

```bash
# Get the current main HEAD SHA on wrangle (where the Go build type was merged)
WRANGLE_SHA=$(gh api repos/TomHennen/wrangle/commits/main --jq '.sha')
echo "wrangle main HEAD: $WRANGLE_SHA"

# Replace the placeholder in build-and-publish.yml
sed -i.bak "s/__WRANGLE_GO_MERGE_SHA__/$WRANGLE_SHA/" .github/workflows/build-and-publish.yml
rm .github/workflows/build-and-publish.yml.bak

# Verify
grep "uses: TomHennen/wrangle" .github/workflows/build-and-publish.yml
```

The line should now read `uses: TomHennen/wrangle/.github/workflows/build_and_publish_go.yml@<40-char-sha>`.

## 4. Commit the initial scaffold (~1 min)

```bash
git add .
git commit -m "Initial scaffold: devcontainer, design doc, CLAUDE.md, PLAN.md, wrangle Go CI"
git push
```

The CI run should start. The first checks (`gofmt`, `go vet`, `go test`, `govulncheck`) will fail because there's no Go code yet — that's expected. Once the agent gets through Checkpoint 1, the next push should pass `gofmt`/`vet`/`test` (govulncheck has nothing to scan until there are dependencies, which is fine).

## 5. Set up the secrets directory (~5 min)

The App private key and config live outside the repo — never committed, mounted into the container at runtime.

```bash
mkdir -p ~/.config/rein-dev-secrets
chmod 700 ~/.config/rein-dev-secrets
```

If you still have the validation App's private key from the §12 work:

```bash
cp ~/agentcreds-validation/app.pem ~/.config/rein-dev-secrets/app.pem
chmod 600 ~/.config/rein-dev-secrets/app.pem
```

If not, regenerate it: GitHub → Settings → Developer settings → GitHub Apps → your validation App → scroll to "Private keys" → Generate a private key → move the downloaded `.pem` to `~/.config/rein-dev-secrets/app.pem` and `chmod 600`.

Verify:

```bash
ls -la ~/.config/rein-dev-secrets/app.pem
# Should show -rw------- (600 permissions)
head -1 ~/.config/rein-dev-secrets/app.pem
# Should show: -----BEGIN RSA PRIVATE KEY-----  (or similar)
```

## 6. Set environment variables (~3 min)

Add to your `~/.zshrc` (or shell rc file). Values come from your existing `inputs.txt` if you have it, or from the GitHub App settings page.

```bash
# rein development — GitHub App config
export REIN_APP_CLIENT_ID="Iv23li..."          # "Client ID" on the App page
export REIN_APP_ID="1234567"                   # "App ID" on the App page
export REIN_APP_INSTALLATION_ID="98765432"     # See below
export REIN_TEST_REPO_A="TomHennen/agentcreds-validation-a"
export REIN_TEST_REPO_B="TomHennen/agentcreds-validation-b"
export REIN_GITHUB_USERNAME="TomHennen"

# Anthropic API key for Claude Code inside the container
export ANTHROPIC_API_KEY="sk-ant-..."
```

To get `REIN_APP_INSTALLATION_ID`:

```bash
gh api /users/TomHennen/installation --jq '.id'
```

Reload your shell:

```bash
source ~/.zshrc
```

Verify all four are set:

```bash
echo "App ID: $REIN_APP_ID"
echo "Installation: $REIN_APP_INSTALLATION_ID"
echo "Repo A: $REIN_TEST_REPO_A"
echo "API key prefix: ${ANTHROPIC_API_KEY:0:10}"
```

## 7. Confirm the throwaway repos and App are still intact (~1 min)

```bash
# Confirm repos exist
gh repo view "$REIN_TEST_REPO_A" --json name,isPrivate
gh repo view "$REIN_TEST_REPO_B" --json name,isPrivate

# Confirm App installation is active
gh api /users/TomHennen/installation --jq '{id: .id, app_slug: .app_slug}'
```

If any of these fail (repos deleted, App deleted), recreate per the §12 validation pre-work.

## 8. Install VS Code Dev Containers extension (~1 min)

Skip if already installed:

```bash
code --list-extensions | grep remote-containers
# If empty:
code --install-extension ms-vscode-remote.remote-containers
```

## 9. Open the repo in VS Code and build the container (~5-10 min first time)

```bash
code ~/dev/rein
```

VS Code should prompt: **"Folder contains a Dev Container configuration file. Reopen in Container"** → click **Reopen in Container**.

If the prompt doesn't appear: Cmd+Shift+P → "Dev Containers: Reopen in Container".

First build pulls `golang:1.26-bookworm` (~1 GB), installs gh, jq, golangci-lint, Claude Code, configures the vscode user. Takes 5-10 minutes. Subsequent opens are fast.

## 10. Verify the container environment (~2 min)

Inside the container terminal:

```bash
go version          # 1.26.x
gh --version
golangci-lint --version
claude --version    # if "command not found", see below
ls -la ~/.config/rein-dev-secrets/app.pem
env | grep REIN     # all REIN_* vars set
```

**If `claude` is missing:** the install step failed silently during build (the `|| true` allows the build to succeed). Install inside the container:

```bash
curl -fsSL https://claude.ai/install.sh | bash -s -- --yes
```

## 11. Initialize the Go module (~30 sec)

```bash
cd /workspaces/rein
go mod init github.com/TomHennen/rein
git add go.mod
git commit -m "go mod init"
git push
```

This commit will trigger the CI workflow. Source checks should pass (empty module, nothing to lint or test).

## 12. Hand off to the agent

In the container terminal:

```bash
claude
```

Paste this prompt:

```
Read CLAUDE.md and PLAN.md. Start at Checkpoint 1. Stop after each
checkpoint and surface what you built to me before proceeding to the
next. The design doc at docs/design.md is the authoritative reference;
read sections as needed but you don't need to read it cover to cover.

Work inside this container. Don't modify .devcontainer/, .github/, or
.goreleaser.yml without surfacing the change to me first. Don't touch
anything outside the working directory or the throwaway repos
(agentcreds-validation-a and agentcreds-validation-b). Don't run sudo
without asking.

Surface anything that surprises you. Don't work around design
mismatches silently.

Start by running `env | grep REIN` to confirm the environment is set
up. If anything is missing, stop and ask.
```

## Watching it work

- **Between checkpoints:** the agent will stop and surface what it built. Verify before saying "proceed."
- **CI runs:** every push triggers wrangle's checks. Failures should be informative; failures with cryptic messages are a wrangle bug worth surfacing back to wrangle.
- **`~/.gitconfig` changes:** expected and contained to the container.
- **Notes section in PLAN.md:** the agent appends design corrections and surprises here. Check periodically.

## If something goes wrong

| Symptom | Fix |
|---|---|
| Container build fails | Cmd+Shift+P → "Dev Containers: Rebuild Container" |
| `claude: command not found` | `curl -fsSL https://claude.ai/install.sh \| bash -s -- --yes` inside container |
| `env \| grep REIN` empty inside container | Close VS Code, `source ~/.zshrc` on host, reopen VS Code |
| `app.pem` not visible in container | Check `~/.config/rein-dev-secrets/app.pem` on host has `chmod 600` |
| CI fails on wrangle workflow | Check the SHA pin in `.github/workflows/build-and-publish.yml` resolves; may need a re-pin if wrangle has moved |
| Agent runs `gh auth setup-git` | TM-G8 happening live (empty credential returned) — surface to Tom |

## Cleanup after Phase 0

```bash
# On the host (Mac or Linux VM):
rm -rf ~/.config/rein-dev-secrets/
# Remove the REIN_* and ANTHROPIC_API_KEY lines from ~/.zshrc
source ~/.zshrc

# Delete the throwaway repos via the GitHub UI if you're done with them
# Delete the test App via Settings → Developer settings → GitHub Apps → Delete
```

The `rein` repo itself stays around — it either becomes Phase 1 or gets archived.
