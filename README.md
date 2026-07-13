# rein

> [!WARNING]
> rein is an **experimental proof of concept**. The design, interfaces, and
> security guarantees are still settling, and the code has **not had an
> independent security review** — don't point it at anything you can't afford to
> lose, and use **throwaway repos only** for now. Kicking the tires, filing
> issues, and **external security reviews are very welcome**. Keep your existing
> protections in place.

**A local credential broker for AI coding agents — let a coding agent work on
GitHub without ever handing it your credentials.**

To let an agent push a branch or open a PR today, you give it a credential: a
`gh auth login`, a PAT in its environment, your SSH key. That credential is as
broad as *you* are, it lives as long as you let it, and anything that can read
the agent's memory or environment can take it. So you supervise every step, or
you accept that risk.

rein removes the credential from the agent entirely:

- **No standing credential to manage or leak** — no `gh auth login`, no PATs to
  mint, rotate, or revoke. rein brokers human-gated, short-lived tokens from
  **your own GitHub App**; they never sit in the agent's environment, files, or
  memory.
- **A blast radius you chose** — make your App key the only GitHub credential on
  the box, and even a full sandbox escape is capped at the scope you picked, never
  your whole account.
- **Yours, not ours** — the App is yours, the key stays on your machine, the
  tokens are minted locally. No rein service, no shared secret, nothing the rein
  authors can see, hold, or revoke.
- **Less babysitting** — because the agent is sandboxed (no credentials, egress
  limited to GitHub), running it with permissions off
  (`claude --dangerously-skip-permissions`) is **safer, though not safe** — it may
  still have other credentials you left reachable, and it can still do damage
  inside the scope you granted it. rein moves the approval that matters most —
  writing to GitHub — to a single per-issue gate.

### Where the credential lives

```mermaid
flowchart LR
  subgraph machine["your machine"]
    direction TB
    subgraph sandbox["sandbox"]
      agent["the agent<br/>no token · no keys · no login"]
    end
    proxy["proxy<br/>injects the token<br/>at the network layer"]
    broker["broker<br/>holds your App's private key"]
  end
  subgraph github["GitHub"]
    app["your GitHub App<br/>scoped to the repos &<br/>permissions you chose"]
    repos["your repos"]
  end
  broker == "signs with the key,<br/>mints a scoped token" ==> app
  app -. "short-lived token" .-> broker
  agent -- "git / gh" --> proxy
  broker -. "injects the token" .-> proxy
  proxy -- "already-scoped write" --> repos
```

The key lives *outside* the sandbox and never crosses into it. What crosses the
boundary is an already-scoped, short-lived token, added at the network layer — so
a sandbox escape finds no token to steal, only the traffic it was already allowed
to make.

**Today that means Linux, a terminal, and `tmux` for the approval popup** (macOS
is a separate track, not yet done). For the full design and threat model, see
[`docs/design.md`](docs/design.md).

## Prerequisites

**Core:**
- **Go** — the version in [`go.mod`](go.mod) (currently 1.26+).
- A **GitHub account** that can create GitHub Apps (any personal account can).
- One or more **throwaway repositories** to point the agent at. Do not use a real
  repo yet. Clone them over **HTTPS** — rein brokers `https://github.com` remotes.
  An **SSH remote will not work** from inside the sandbox: rein has nothing to
  inject into it, there's no egress for it, and the ssh-agent socket is blocked.
- A browser for the one-time App creation. On a headless/SSH box, see
  [Headless setup](#headless--remote-machines).

**The sandbox stack (Linux)** — required for the default `rein run`. `rein
doctor` checks every one of these and tells you exactly what's missing:
- **`srt`** — pinned to `@anthropic-ai/sandbox-runtime@0.0.63` (other versions
  may move the injection hook; rein re-verifies on bump). Needs Node 20+:
  `npm install -g @anthropic-ai/sandbox-runtime@0.0.63`
- **`bubblewrap`, `ripgrep`, `socat`** —
  `sudo apt-get install -y bubblewrap ripgrep socat` (or your distro's
  equivalent).
- **Ubuntu 24.04+ only:** an AppArmor profile granting `userns` to `bwrap`, or the
  sandbox won't start. Check with:

  ```bash
  bwrap --unshare-user --uid 0 --bind / / -- true   # if this errors, you need the profile
  ```

  Create `/etc/apparmor.d/bwrap` (this grants `userns` to `bwrap` alone — don't
  disable the sysctl system-wide, that weakens the whole box):

  ```
  abi <abi/4.0>,
  include <tunables/global>

  profile bwrap /usr/bin/bwrap flags=(unconfined) {
    userns,
    include if exists <local/bwrap>
  }
  ```

  Then `sudo apparmor_parser -r /etc/apparmor.d/bwrap` and re-run the check.
- **Healthy NTP** — App token mints fail with a misleading `401 Bad credentials`
  when the clock drifts more than ~60s.

## Quick start

```bash
git clone https://github.com/TomHennen/rein.git
cd rein
go build -o bin/ ./...
# install the sandbox stack (above), then:
./bin/rein init
./bin/rein doctor   # every check should be [ok] — including the sandbox: rows
```

`rein init` walks you through the whole setup and is idempotent — safe to re-run.
It:

1. **Creates your GitHub App(s)** — a **primary** App that mints your tokens, and
   an **audit** App (`--skip-audit` to skip; audit comments are coming soon). A
   browser opens to GitHub's "Create GitHub App" page with the permissions
   pre-filled (see [Token scopes](#what-your-app-and-its-tokens-can-do)); you click
   **Create**, then **Install** on your throwaway repo(s).

   **The browser has to reach rein on `127.0.0.1`** — that's how the App's key gets
   back to you. Working on a remote box over SSH? Set up the port-forward
   *first*: [Headless / remote machines](#headless--remote-machines).
2. **Stores the keys** in `~/.config/rein/` (mode `0600`). You never copy a key by
   hand.
3. **Wires up your shell** — the git/`gh` shims and `rein` on your `PATH`. It also
   offers to alias `claude` to `rein run -- claude` (off by default).
4. **Scaffolds your session** — the [scope
   ceiling](#the-session-sets-the-scope-ceiling). `rein run` won't start without
   one.

Then **install the App on the repos you want**, using the deep-links rein prints.
Run `rein init --help` for the full flag set.

## Daily use

Open a new shell and run your agent:

```bash
rein run -- claude     # or just `claude`, if you installed the alias
```

That's it. The agent works read-only until it needs to write; then it runs `rein
declare <issue>`, **you** get a confirmation prompt on your terminal, and from
there its pushes land. Everything below explains what's happening underneath.

## How rein works

### The write ceremony

```mermaid
sequenceDiagram
  participant A as Agent (sandbox)
  participant You
  participant R as rein broker
  participant App as your GitHub App
  participant G as GitHub
  A->>You: rein declare (its issue)
  You->>R: approve (on your terminal)
  R->>App: App key → mint a short-lived, repo-scoped write token
  App-->>R: scoped token
  R-->>A: inject at the proxy — agent never sees it
  A->>G: push — proxy checks the branch is the declared issue's, lands
```

1. The agent needs to write, so it runs `rein declare <issue-number>`. Every
   blocked write tells it to.
2. rein fetches that issue and shows you its **title, state, and home repo** on
   your terminal (a `tmux` popup by default). You confirm by typing the displayed
   number. The agent **cannot reach or forge this prompt** — it has no controlling
   terminal.
3. rein mints a short-lived write token from your App key.
4. The proxy injects it into the agent's traffic on the wire — the agent never
   sees it.
5. The push lands.

Confirming an issue covers **that issue** for the rest of the run — the agent can
push to it again without re-prompting you. Declaring a *different* issue prompts
you again.

Write capability is revoked when the run ends, after **30 minutes with no GitHub
traffic**, or **4 hours** in total, whichever comes first. Note the idle clock is
reset by *any* request the agent makes, reads included — so an agent that keeps
working stays approved until the 4-hour cap.

**What the issue actually binds.** GitHub tokens can't be scoped to an issue, so
the token is scoped to your session's **repos**. What the issue binds is the
*push*: an approved run's `git push` can only target
`agent/<issue>/<nonce>` for an issue you confirmed. Any other branch — including
`main` — is refused on the wire.

The token itself can still do more than that. It carries `contents: write` for
your repos, so the agent could also write through GitHub's **API**, which the
branch rule doesn't cover. rein's answer there is to **record, not block**: every
request it relays is written to a log the agent can't reach or edit.

```bash
cat ~/.local/state/rein/audit/sandbox-<run-id>.log
```

Posting that history back to the issue (from an identity the agent can't touch) is
coming soon; for now the log is local. See [Known limits](#known-limits).

### The session sets the scope ceiling

A *session* is the set of repos the agent may touch — the ceiling a token can
never exceed. `rein init` scaffolds it; you hand-edit it to change the repo set:

```yaml
id: my-session
role: implement
repos:
  - your-name/your-throwaway-repo   # the token is scoped to this whole set
```

**Why no issue field?** The issue is bound at *runtime*, not at setup
([#35](https://github.com/TomHennen/rein/issues/35)) — the agent declares it, you
confirm it. A session file with a legacy `issue:` line still loads, but the field
is **ignored** (with a loud warning); remove it. Use `rein session show` to see
the standing ceiling and any live expansions, and `rein session add-repo
<owner/name>` to widen it.

### What your App and its tokens can do

You consent to the App's permissions once, at creation. rein then mints each
token with the **narrowest** set the operation needs, so a stolen token is worth
less than the App itself:

| | contents | issues | pull_requests | metadata |
|---|---|---|---|---|
| **Your primary App** — the ceiling you consent to at creation | write | write | write | read |
| **Read tier** — before declare (`git` fetch, `gh pr view`, …) | read | read | read | read |
| **Write tier** — after you approve (`git push`, `gh pr create`, …) | write | write | write | read |
| **Audit App** — writeback, created but not yet posting | — | write | — | read |

The write token only exists once you approve, and is revoked when the run ends or
[expires](#the-write-ceremony). Both tokens are scoped to your session's repos —
never to your account.

> **Note:** on GitHub, `pull_requests: write` also means review, approve, and
> merge — so an approved run could approve or merge its own PR. Branch protection
> that requires an approval won't stop it
> ([#86](https://github.com/TomHennen/rein/issues/86)).

### What the sandbox actually blocks

`rein run` launches the agent inside Anthropic's
[`sandbox-runtime`](https://github.com/anthropic-experimental/sandbox-runtime)
(`srt`). Inside it:

- **No network egress except GitHub** (through rein's proxy, which injects the
  token on the wire) **and the agent's own API** — `api.anthropic.com` is allowed
  by default so `rein run -- claude` works out of the box. A different agent's API
  needs [allowing explicitly](#allowing-extra-network-egress).
- **Your `$HOME` is hidden.** rein denies your home directory wholesale and allows
  back only what the agent needs to run (its install chain and config, a toolchain
  set). Your credential stores are denied on top of that — `~/.config/gh`,
  `~/.ssh`, `~/.netrc`, git-credentials, `~/.gnupg`, your keyrings, and rein's own
  keys — and the keyring/ssh-agent sockets are blocked outright. A credential
  scanner run inside the sandbox finds none of your real credentials.
- **`.git` is protected** ([#64](https://github.com/TomHennen/rein/issues/64)):
  `hooks/` and `config` are read-only, and `.git` can't be renamed aside and
  rebuilt — otherwise a prompt-injected agent could plant a `pre-commit` hook that
  later runs **as you, on your host**. rein can't protect a submodule or a linked
  worktree this way, so it won't bind one: a mapped worktree fails the launch, and
  if it's your current directory the agent gets a scratch clone instead and your
  tree is untouched.
- **Writes are locked until declare**, and commits are authored `<your name> (via
  rein)` under the App's identity — so a push is attributable to the App, not to
  you personally.
- **No credential in the environment.** The sandbox env is an allowlist, and the
  `GH_TOKEN` the agent sees is a stub. The real token only ever exists on the wire.
If hiding `$HOME` breaks a tool you need, `REIN_SANDBOX_ALLOW_READ` allows
specific paths back read-only (never a credential store — rein rejects those), and
`REIN_SANDBOX_SHOW_HOME` turns the whole `$HOME` deny off.

### Allowing extra network egress

Anything that isn't GitHub or the agent's own API — the npm registry, PyPI, a
remote MCP server — is unreachable until you allow its host. Add hosts to
`allow_domains` in your session yaml, or set `REIN_ALLOW_DOMAINS`
(comma-separated) machine-wide:

```yaml
allow_domains:
  - registry.npmjs.org
  - pypi.org
```

Allowed hosts are egress-only — rein never injects a credential on them; only
GitHub gets a token. Entries are bare hosts (`pypi.org`) or a strict wildcard
(`*.example.com`). **Every host you add is somewhere the agent can send your
data**, so keep the list short; rein warns on wildcards and on large sets.

**MCP servers** follow the same rule: local/stdio servers work out of the box,
remote ones (and the claude.ai connectors, which also need `claude.ai`) connect
only if you allow their hosts. `REIN_DISABLE_CLAUDE_MCP=1` turns the account
connectors off.

### `--direct` mode (fallback, throwaway only)

Where there's no working sandbox, `rein run --direct -- <cmd>` uses a git
credential helper instead. The agent runs **unsandboxed** and can reach your
ambient credentials, so it's weaker by design — rein prints a loud banner, and you
should only use it on throwaways. You still declare and confirm, but rein never
sees the branch being pushed, so an approved direct-mode run can push **any** ref.

## Known limits

rein makes an agent **safer to run, not safe to trust.** Read these before you
point it at anything.

- **Linux only.** macOS is a separate track, not yet done.
- **Throwaway repos only, for now.** The sandbox closes the credential-exfiltration
  gap, but none of this has been dogfooded on a real repo yet.
- **rein only helps if it's the *only* credential on the box.** If you also keep a
  broad `gh` login or a PAT lying around, an escaped agent gets those instead —
  rein just removes your reason to have them.
- **The key is protected by file permissions and the sandbox, not by hardware.**
  Hardware-backed keys are on the roadmap.
- **The sandbox is defense-in-depth, not a hard boundary.** An escape re-exposes
  the weaker `--direct` surface. And it only stops the *agent* — anything else
  running as **you** on the host can still reach your credentials. rein defends
  against a prompt-injected agent, not against malware already running as you.
- **An approved run can approve or merge its own PR**
  ([#86](https://github.com/TomHennen/rein/issues/86)), and can write through the
  API to branches the push rule would block, including `main`
  ([#109](https://github.com/TomHennen/rein/issues/109)). Those are **recorded, not
  blocked** — and until audit writeback ships, that record is a local file, not
  something a PR reviewer will ever see.

## Headless / remote machines

The App-creation step needs a browser that can reach rein's loopback callback. On
a headless or SSH-only box, rein detects this and prints a ready-to-paste `ssh -L`
recipe. For a predictable port, pin it:

```bash
rein init --port 41234                      # on the remote box
ssh -L 41234:127.0.0.1:41234 you@remote     # on your laptop
# then open the printed http://127.0.0.1:41234/ in your laptop browser
```

If port-forwarding is blocked entirely, see the manual fallback in
[`docs/init-manifest-design.md`](docs/init-manifest-design.md) — read its **Safe
handling of the App private key** section before moving a key by hand.

## Troubleshooting

**Start with `rein doctor`** — it checks everything above and tells you what's
wrong. `rein doctor --fix` applies the repairs it can make safely; anything
privileged (apt, npm, AppArmor, NTP) it shows you but never runs.

- **`sandbox: ...` check fails** — install the missing piece from
  [Prerequisites](#prerequisites); on Ubuntu 24.04 the usual culprit is the
  `bwrap` AppArmor profile. `rein run` won't launch until these pass.
- **`app credentials: 401`** — almost always clock skew; check `chronyc tracking`.
- **`claude` doesn't go through rein** — open a new shell, or `source` your rc;
  confirm with `type claude`.
- **`rein: command not found`** — `~/.local/bin` isn't on your `$PATH`.
- **A git op fails, mentioning `rein doctor`** — rein refused rather than letting
  the agent silently re-auth. Usually the App isn't installed on that repo, or the
  repo is outside your session's ceiling.
- **Write prompt never appears** — the agent must run `rein declare <n>` first.
  If it did and no prompt reached you, you may be in `--direct` from a shell with
  no tty; run from a real terminal, or approve from another with `rein approval
  grant --run-id <id>`.
- **Logs** — per-run audit log (token-redacted): `~/.local/state/rein/audit/`;
  direct-mode credential helper: `~/.local/state/rein/helper.log`.

## Running the tests

Three layers, hermetic to live:

```bash
go test ./...                                  # unit — no network, no sandbox, no secrets
go test -race ./...                            # the concurrency-sensitive packages

REIN_SANDBOX_E2E=1 go test ./internal/srt -run E2E   # launches real srt; needs the stack healthy

tests/interactive/run-journeys.sh              # journeys: real pty, real repo, golden transcripts
tests/interactive/run.sh                       # the rest of the interactive suite
```

The **journeys** are the ones that matter: each drives a real user path against a
live throwaway repo and checks the transcript against a committed golden, so any
drift shows up as a failure. Regenerate them in any PR that changes behavior. They
need `rein init` run on the box, the sandbox stack, host `gh` authed, and
`python3` + `pexpect`; see
[`tests/interactive/README.md`](tests/interactive/README.md). The interactive
suite is **never** run by `go test ./...`, so the Go suite stays fast and offline.

## Cleanup

- Delete the Apps you created at <https://github.com/settings/apps> (GitHub has no
  API to delete an App).
- Remove `~/.config/rein/` (keys, CA, state, session) and `~/.local/state/rein/`
  (shims, logs, audit, caches). Per-run proxy sockets live under
  `$XDG_RUNTIME_DIR/rein/` and are removed when the run exits.
- Remove the `~/.local/bin/rein` symlink and the `# BEGIN/END rein` alias block
  from your shell rc (or `~/.config/fish/functions/claude.fish`).
