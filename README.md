# rein

**A local credential broker for AI coding agents.** rein issues short-lived,
narrowly-scoped GitHub tokens to agents (Claude Code, Cursor, Codex, Aider) on
your laptop, so the agent never holds a long-lived credential. You create a
GitHub App once via a guided browser flow; from then on rein mints a fresh,
repo-scoped token for each git operation and asks you to confirm writes. For the
full design and threat model, see [`docs/design.md`](docs/design.md).

> **Phase 0.5 status.** rein works today as a git/`gh` credential helper
> (Shape B). It is an incremental improvement over personal access tokens; the
> qualitative jump (an agent that *cannot* exfiltrate the token) needs the
> Phase 1 sandbox. Use **throwaway repos only** until Phase 1 — see
> [Known limits](#known-limits).

## Prerequisites

- **Go** — the version in [`go.mod`](go.mod) (currently 1.26+).
- A **GitHub account** that can create GitHub Apps (any personal account can).
- One or more **throwaway repositories** to point the agent at. Do not use a
  real repo yet. Clone them over **HTTPS** (`https://github.com/...`) — rein
  only brokers `https://github.com` remotes; an SSH remote bypasses rein
  silently (no token, no scope check, no write prompt).
- **`gh`** (GitHub CLI) — optional, only for manually verifying repos.
- A browser to complete the one-time App creation. On a headless/SSH box, see
  [Headless setup](#headless--remote-machines).

## Install

```bash
git clone https://github.com/TomHennen/rein.git
cd rein
go build -o bin/ ./...
./bin/rein init
```

`rein init` is idempotent — safe to re-run.

## First-time setup

`rein init` walks you through everything. On a fresh machine it will:

1. **Create your GitHub App(s).** A browser opens to GitHub's "Create GitHub
   App" page with the right permissions pre-filled. Click **Create**, then
   **Install** it on your throwaway repo(s). rein creates a **primary** App
   (mints your tokens) and — unless you pass `--skip-audit` — an **audit** App
   (posts audit comments an agent can't delete; unused until Phase 1), each in
   its own browser step. Use `--owner=<your-login>` so rein refuses if you
   accidentally create the App under the wrong account.
2. **Store the keys.** The private keys are written to
   `~/.config/rein/{primary,audit}.pem` (mode `0600`) and the App details to
   `~/.config/rein/state.json`. You never copy a key by hand.
3. **Wire up your shell.** rein installs its git/`gh` shims, puts `rein` on your
   `PATH` (`~/.local/bin/rein`), and adds `alias claude='rein run -- claude'`
   to your shell rc (opt out with `--no-alias`).

After init, **install the App on the repos you want** using the deep-links rein
prints (`https://github.com/apps/<slug>/installations/new`), then create a
session file (next section). **No `REIN_APP_*` environment variables are
needed** — rein reads everything from `state.json` and fetches the installation
id automatically on first use.

### Bind a session (required)

A *session* sets the scope ceiling (which repos the agent may touch) and the
issue used for the write-confirmation prompt. **`rein run` will not start
without one.** Create `~/.config/rein/dev-session.yaml`:

```yaml
id: my-session
role: implement
repos:
  - your-name/your-throwaway-repo
issue: 1            # a real issue number on that repo
```

## Daily use

Open a new shell (so the alias is live) and just run your agent:

```bash
claude
```

The alias routes it through rein. git operations inside the agent get a
fresh, repo-scoped token; the **first write triggers a confirmation prompt**
(in your terminal, or a tmux popup if `$TMUX` is set). To run the agent
*without* rein for one invocation, bypass the alias: `\claude` (bash/zsh) or
`command claude` (fish).

rein scrubs `GH_TOKEN`/`GITHUB_TOKEN` from the wrapped agent's environment so
it can only use rein-brokered credentials.

## Headless / remote machines

The App-creation step needs a browser that can reach rein's loopback callback.
On a headless or SSH-only box, rein detects this and prints a ready-to-paste
`ssh -L` recipe. For a predictable port, pin it:

```bash
# on the remote box:
rein init --port 41234
# on your laptop (the recipe rein prints):
ssh -L 41234:127.0.0.1:41234 you@remote
# then open the printed http://127.0.0.1:41234/ in your laptop browser
```

This keeps the automated, safe key import end-to-end. If port-forwarding is
blocked entirely, see the manual fallback in
[`docs/init-manifest-design.md`](docs/init-manifest-design.md) (and its
**Safe handling of the App private key** section — read it before moving a key
by hand).

## Troubleshooting

**Start with `rein doctor`** — it runs read-only checks (rein on `PATH`, shim
freshness, key readable, App credentials, session, `$TMUX`, caches) and tells
you what's wrong.

- **`claude` doesn't go through rein** — open a new shell, or `source` your rc;
  confirm the alias with `type claude`.
- **`rein: command not found`** (in a fresh shell, via the alias) — `~/.local/bin`
  isn't on your `$PATH`. Add it: `export PATH="$HOME/.local/bin:$PATH"`.
- **A git op fails with `rein doctor` mentioned on stderr** — rein returned a
  non-working placeholder (so it stays in control rather than letting the agent
  silently re-auth). Run `rein doctor`; usually the App isn't installed on that
  repo, or the session repo is outside your scope ceiling.
- **Write prompt never appears** — if you attached to tmux *after* launching,
  `$TMUX` isn't set in the agent; `rein doctor` flags this. Run the agent from
  inside tmux, or approve from another terminal with `rein approval grant`.
- **Logs** — credential helper: `~/.local/state/rein/helper.log`; `gh` shim:
  `~/.local/state/rein/gh-shim.log`.

## Known limits

Phase 0.5 is the Shape B (credential-helper) integration. It is honest about
being weaker than the Phase 1 sandbox — see the **Honest Shape B limits**
section of [`phase0_findings.md`](phase0_findings.md). In short, on a
shared-UID machine a determined agent can still reach credentials rein can't
hide (e.g. `gh`'s stored login); the sandbox is the architectural fix. Until
then: **throwaway repos only.**

## Cleanup

- Delete the Apps you created at <https://github.com/settings/apps> (GitHub has
  no API to delete an App).
- Remove `~/.config/rein/` (keys, state, session) and `~/.local/state/rein/`
  (shims, logs, caches).
- Remove the `~/.local/bin/rein` symlink and the `# BEGIN/END rein` alias block
  from your shell rc (or `~/.config/fish/functions/claude.fish`).
