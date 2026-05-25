# Design Doc: `rein` — An Agent Credential Broker for GitHub Development Workflows

**Status:** Pre-Phase-0 design (v3). Phase 0 is built; design corrections from implementation are tracked in `phase0_findings.md` and summarized in §13 below. Read §13 alongside this doc — several mechanisms here (Shape A proxy assumptions, 5-min write TTL, helper-sees-/git-receive-pack) turned out not to hold; the corrections are non-trivial.
**Date:** May 23, 2026 (Phase 0 close: May 25, 2026 — see `phase0_findings.md`)
**Version:** v3 (incorporates §12 validation findings + comparative analysis vs. Agent Vault, Claude Code Cloud, cloud workload identity, current IETF/Sigstore landscape)
**Companion to:** `wrangle` (supply-chain scanner running on GitHub Actions; provides the release pipeline for `rein` itself)

---

## 0. TL;DR

`rein` is a local daemon that issues short-lived, narrowly-scoped GitHub credentials to AI coding agents (Claude Code, Cursor, Codex CLI, Aider) on a per-issue basis. The agent never sees the credential directly — the credential is injected by Claude Code's existing sandbox proxy on the way to GitHub. The developer's role is to approve scope expansions as the agent encounters them, not to configure sessions up front.

The design is **most defensible composed with Anthropic's `sandbox-runtime` (srt)**, the open-source sandbox that already ships with Claude Code. The sandbox solves credential exfiltration (the agent never reaches the token in its memory). The broker solves scope, attribution, and audit (per-issue ceiling, agent-via-human attribution in commit signatures, audit writeback to the originating issue). Without sandbox composition, the broker is an incremental improvement over PATs; with composition, it is a qualitative shift.

**The mental model is workload identity for AI coding agents.** Cloud-native services figured out a decade ago that long-lived API keys are an anti-pattern, and built workload identity (AWS IAM Roles for Service Accounts, GCP Workload Identity Federation, SPIFFE/SPIRE) to solve it. Agents on a developer's laptop have the same problem; this fills the equivalent gap for that environment.

**The closest prior art is real but not equivalent.** Infisical's Agent Vault (April 2026, Apache 2.0) is the closest architecturally — a credential proxy + vault for AI agents — but it's deployed as a separate-machine service targeting DevOps teams running production agents, not solo developers on a laptop. Anthropic's hosted Claude Code uses an identical pattern in their cloud (repo-level scope, proxy-injected credentials) but ships nothing equivalent for local Claude Code. Several stub-token-substitution proxies (OneCLI, Bromure Agentic Coding, NVIDIA OpenShell) implement the credential-injection layer for AI agents generally. **None of them is GitHub-shaped, issue-anchored, or sandbox-composed in the specific way described here.**

PoC achievable in a weekend against throwaway repos; useful v0 in 2–3 weeks with the v1 properties below.

**Validation status:** §12 exploratory work executed and reported (May 23, 2026). Six of eight assumptions passed cleanly. Two failed soft, with documented design adjustments:
- §12.6 (Fulcio extension OIDs): Public-Sigstore integration is not viable in 2026 without upstream Sigstore work. **v1 ships with the broker as its own CA.** Public-Sigstore-via-OIDC-IdP becomes documented future work.
- §12.7 (Issue internal ID stability): GitHub does not preserve internal issue IDs across transfers. **TM-G6 anchor changed to REST URL 301-redirect chain.**

Other findings woven into the relevant sections.

**v1 mandatory properties:**

1. Sandbox composition path implemented as the primary integration (Shape A), with credential-helper integration (Shape B) as fallback.
2. Audit comments posted by a separate non-agent GitHub identity so a compromised agent cannot prune them.
3. Interactive human confirmation for elevated roles, with non-replayable input.
4. The credential helper must always return a credential — never empty, never error. Empty returns trigger downstream agents to run `gh auth setup-git` and silently displace the broker (validated finding §12.1).
5. Broker signing key in OS keychain at minimum (v0); Secure Enclave on macOS for v1+, contingent on codesigning. Honest characterization of which mode is in use, surfaced in the README.
6. Commit signing via broker-as-CA for agent-delegated commits. Honest README characterization: third-party verification requires trusting the broker's CA via local policy.

**Stop conditions:** abandon the project if any of:
- (a) GitHub ships first-party issue-scoped installation tokens.
- (b) Anthropic ships a built-in local credential broker for Claude Code. **40–60% probability over 12 months.** They already have the architectural pattern proven in Claude Code Cloud; porting it to local Claude Code is real but not enormous work.
- (c) Infisical's Agent Vault pivots to single-binary-on-laptop deployment with GitHub opinionation and issue-anchoring.
- (d) AgentWrit relicenses permissively and adds a GitHub backend with proxy integration.

Move fast on the PoC; be intellectually prepared to walk away.

---

## 1. Problem Statement and Motivation

### 1.1 What developers do today

A developer working with an AI coding agent needs the agent to interact with GitHub: read the issue, clone the repo, branch, commit, push, open a PR, leave a comment, run CI. The agent needs credentials for this.

The de facto pattern in 2026 is one of:

1. **A Personal Access Token (PAT) handed to the agent via environment variable or pasted into a config file.** Dominant across Claude Code, Cursor, Codex CLI, Aider, Continue.dev.
2. **GitHub CLI's OAuth token via `gh auth login`**, which writes a long-lived bearer into `~/.git-credentials`. Slightly better but still user-bound and long-lived.
3. **An MCP server with an embedded PAT** pasted into `mcp.json`.

PATs are the wrong primitive for agents because:

- **Long-lived.** GitHub's default enterprise/org policy maximum is 366 days; fine-grained PATs for personal projects may be created with no expiration at all.
- **Over-scoped.** Devs grant broad permissions "just in case" since narrowing means returning to settings.
- **User-attributed.** Every agent action shows as the user, breaking attribution, breaking review, breaking SLSA Source L3/L4's two-person enforcement.
- **Blast radius.** Leaked PAT = full access for its lifetime; revocation is manual.
- **Indistinguishable.** A compromised agent and the human cannot be told apart in audit logs.

Industry data points (vendor-sponsored, directional only): the CSA/Aembit *"Identity and Access Gaps in the Age of Autonomous AI"* report (n=228, March 2026) found 74% of organizations report their AI agents end up with more access than they need. Arkose Labs' *2026 Agentic AI Security Report* found 97% of enterprise security leaders expect a material agent-driven incident within twelve months.

Real exploitation patterns are emerging in the wild. Aonan Guan's *"Comment and Control"* research (April 15, 2026) demonstrated prompt-injection-driven credential theft against Claude Code Security Review, Gemini CLI Action, and GitHub Copilot Agent — using GitHub comments and PR titles to hijack the agents and exfiltrate the host's `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, and `GITHUB_TOKEN`. The threat model "agent has bearer tokens in memory, prompt injection extracts them" is no longer speculative.

### 1.2 What we want instead

A primitive that gives the agent **exactly what the current task requires, for as long as the current task is running, attributable to the human-via-delegation (not impersonating the human), revocable in one place, and unreachable from inside the agent's reasoning loop**:

- **Issue-anchored.** Sessions are bound to one or more GitHub issues; scope ceiling derived from the issues' repos.
- **Role-shaped.** Devs pick from ~5 opinionated roles (scan, triage, implement, review, release).
- **Two-tier.** A read-mostly session token plus an ephemeral write token minted just-in-time at push.
- **Sandboxed.** Inside Claude Code's sandbox, the token never reaches the agent's reachable memory; the sandbox proxy injects it on the way out. Works for `git`, `gh`, and any other tool that hits `github.com` or `api.github.com`.
- **Auditable in the issue itself.** Audit writeback as comments on the originating issues, posted by a separate identity so a compromised agent cannot prune them.
- **Attributable cryptographically.** Commits signed with a broker-as-CA cert tied to the human-via-delegation identity (see §4.2.6).

### 1.3 Why not just use fine-grained PATs?

The honest answer: you can, and most people don't. The capability has existed since 2022 — when creating a fine-grained PAT, you can pick "Only select repositories" and grant minimum per-permission scopes. The mechanics work.

What stops adoption is friction:

- **Mode confusion.** Two PAT flavors exist (Classic and Fine-grained); most tutorials reference Classic; Fine-grained is the right answer but isn't the default mental model.
- **Permission matrix.** A long list of permissions (Contents, Metadata, Pull requests, Issues, Actions, ...) each with read/write/admin choices. You have to know which ones your tool needs. Most people grant more than necessary.
- **Per-repo selection that doesn't compose.** Tolerable for one repo. Annoying for multiple. Worse: if you later need the token to access a new repo, you edit the token or create a new one.
- **Expiration management.** Default expiration is 30 or 90 days; users either accept the renewal pain or set "no expiration" and lose the main benefit.
- **Per-tool repetition.** Each agent/tool wants its own credential. Multiplied by repos and permissions, configuration becomes a tax.
- **Multi-org limitation.** Each fine-grained token can target only one account or organization. Work-plus-personal users need separate tokens.

The result observed in practice: the path of least resistance is a broad classic PAT, and most users take it.

**So one piece of what `rein` does is make repo-scoping the default by construction** — you can't accidentally get the broad token because the broker only mints what the role and the issue's repo allow. But repo-scoping alone isn't the headline; ghtkn already does that more cleanly than fine-grained PATs.

### 1.4 What this adds beyond fine-grained PATs and ghtkn

| Property | Classic PAT | Fine-grained PAT | ghtkn (GitHub App user tokens) | `rein` |
|---|---|---|---|---|
| Repo-scoped | No (whole user) | Yes if configured | Yes (App-installation level) | Yes (App-installation level) |
| Per-permission minimum | No | Yes if configured | Constrained by App permissions | Yes (role-shaped) |
| Short-lived by default | No | No (up to unlimited) | Yes (8h) | Yes (4h read, ≤5m write JIT) |
| Per-task scope below repo | No | No | No | **Yes (per-issue)** |
| Per-push write windows | No | No | No | **Yes (single-use, HEAD-pinned)** |
| Agent-distinguishable attribution | No | No | No (user-attributed) | **Yes (human-via-delegation)** |
| Audit writeback to issue | No | No | No | **Yes (separate identity)** |
| Composes with sandbox proxy (agent never sees token) | No | No | No | **Yes (with `srt`)** |
| Setup friction | Low | High | Low after App created | Low after `rein init` |

The bolded rows are the actual contribution. Repo-scoping is the floor; ghtkn and fine-grained PATs already get you there. Per-task scoping, per-push write windows, agent-distinguishable attribution, and sandbox composition are what's missing in the ecosystem today.

### 1.5 Cloud workload identity as the mental model

The pattern this design borrows from is workload identity. The cloud-native story since roughly 2016:

- **AWS IAM Roles for Service Accounts / Roles Anywhere.** A workload running in a known environment (EC2, EKS, Lambda) requests credentials from a metadata service; the credentials are short-lived, narrowly-scoped, and continuously rotated.
- **GCP Workload Identity Federation.** Same pattern. A workload's environment (GKE pod, Cloud Run service, Cloud Build job) presents an attestation; a token-exchange service mints scoped GCP credentials.
- **SPIFFE / SPIRE.** The standardization layer. Workloads have stable identities (`spiffe://...`); their environment attests to those identities; an issuer mints short-lived credentials (X.509-SVID or JWT-SVID) downstream services verify.

Common properties:

- Long-lived credentials are replaced with short-lived, dynamically-minted ones.
- The environment attests to the workload's identity.
- A broker handles the credential exchange, never giving the workload the underlying long-term secrets.
- Downstream services verify against a policy that names identities, not keys.

`rein` is the same shape, with the developer's laptop as the attestable environment, the broker as the issuer, and GitHub as the downstream service. The differences worth being honest about:

- **Less authoritative attestation.** Cloud providers attest to a VM/pod/job; a developer laptop is less authoritative. The hardware-backed broker key (Secure Enclave/TPM) is the closest equivalent to a cloud metadata server in this model.
- **Decentralized trust.** Cloud workload identity is centralized — AWS/GCP issues attestations everyone trusts. With `rein`, each user's laptop has its own broker. Verification policy has to deal with multiple issuers.
- **Richer principal.** Cloud workload identity attests "this workload is who it claims to be." `rein` attests "this session is operating on this human's behalf for this purpose, with this scope." More information per attestation; more meaningful in audit.
- **Adversarial workload.** Cloud workload identity assumes the workload doesn't fight back; `rein` has to assume the agent might (prompt injection, exfiltration attempts). The sandbox composition is what addresses this — workload identity in clouds doesn't typically need to defend against the workload itself.

The frame matters because it makes the design's choices feel familiar to anyone who's done cloud infrastructure work, and it makes the trade-offs easier to reason about: what does each cloud workload identity property look like in our environment, and what do we accept as different?

### 1.6 Relationship to Claude Code Cloud

Anthropic's hosted Claude Code uses an architecturally identical pattern in their cloud environment: a sandbox + proxy + credential injection. From the Claude Code on the Web docs: *"Inside the sandbox, the git client authenticates using a custom-built scoped credential. This proxy manages GitHub authentication securely: the git client uses a scoped credential inside the sandbox, which the proxy verifies and translates to your actual GitHub authentication token… Restricts git push operations to the current working branch for safety."*

The relevant differences:

| Property | Claude Code Cloud | `rein` |
|---|---|---|
| Deployment | Anthropic's infrastructure | Developer's laptop |
| Scope unit | Repo (sessions name which repos can be touched) | Issue (sessions anchor on specific issues within a repo) |
| Supported repos | GitHub-only | GitHub-only (this design); other forges out of scope |
| Credentials | OAuth via Anthropic's install flow | GitHub App via Device Flow installed by user |
| Attribution | Commits show as user (via Anthropic's translation) | Commits show as user-via-delegation (with session metadata) |
| Audit surface | Anthropic's session logs | Issue comments on the originating issues |
| Sandbox | Anthropic-managed | `srt` (open source, runs locally) |

These are complementary, not competing. Cloud handles the cloud-hosted case; this handles the local-laptop case. The architectural pattern is shared; the deployment shape and the scope granularity differ. A reasonable end state for someone using both: cloud for async work that benefits from running on Anthropic's infrastructure (long-running tasks, mobile-initiated work), local for synchronous laptop development where the agent should respect issue boundaries and produce delegation-attributed commits.

**This relationship affects the stop conditions.** If Anthropic ships local Claude Code with even repo-level scoping and proxy injection (the Cloud version's pattern), much of `rein`' value evaporates for Claude Code users specifically. The remaining wedge would be: multi-tool support beyond Claude Code, issue-level scoping if Anthropic stays at repo-level, audit-to-issue if Anthropic does audit elsewhere, and SLSA Source Track integration. The probability is real (40–60% over 12 months) and the project's posture should reflect that.

---

## 2. User Experience

This section describes what a developer actually sees. No implementation details.

### 2.1 First-time setup (~5 minutes, once per laptop)

```
$ brew install rein            # or apt / equivalent

$ rein init
Welcome to rein.

This setup will:
  1. Create two GitHub Apps owned by you (one for normal work, one for audit posts)
  2. Generate a signing key for the broker's identity
  3. Configure your sandbox runtime to use rein for GitHub credentials

Step 1/3: GitHub authentication
Open https://github.com/login/device and enter code: WXYZ-1234
✓ Authenticated as @tomh
✓ Created GitHub App 'rein-tomh' (you'll install it on repos you want covered)
✓ Created GitHub App 'rein-tomh-audit' (used only for audit comments)
✓ Uploaded fixed audit-bot avatar for the audit App

Step 2/3: Signing key
✓ Generated signing key (backend: macOS Keychain)
   (Alternative backends are auto-selected on other platforms or environments
    without OS keyring access — e.g., encrypted file inside a devcontainer.
    For hardware-backed Secure Enclave keys with persistence, run
    `rein init --hardware` after installing the codesigned distribution.
    See README.)

Step 3/3: Sandbox runtime integration
✓ Detected sandbox-runtime at /opt/homebrew/bin/srt
✓ Configured srt to use rein for credential injection

Setup complete. Next step:
  Install the apps on a repo you want to use this with:
    https://github.com/apps/rein-tomh/installations/new
    https://github.com/apps/rein-tomh-audit/installations/new
```

The user installs the two Apps on their repos via GitHub's normal App installation flow. This happens once per repo (or once per org, for org-level installation).

The audit App ships with a fixed avatar (a small wrench/robot/shield SVG distributed with `rein`) so the comments it posts are visually distinguishable from the user's own comments in the GitHub UI. Without this, GitHub would fall back to showing the user's own avatar on audit comments, which validation §12.8 found undermines the visual-distinguishability claim. Composite-avatar-with-user-overlay is a polish item for future work.

### 2.2 Daily use

The intended ergonomic model is **ambient**: the broker runs continuously, the agent runs inside the sandbox, scope is negotiated as the agent works, sessions clean themselves up. The human's role is to approve scope changes as they come up, not to configure sessions upfront.

```
$ rein run claude
# (or for Cursor, Codex, Aider — same wrapper)
```

That's it. From here, what the user sees is happening inside the agent's UI.

The agent starts in an **uninitialized session** — it can talk to the user, read local files in the working directory, but cannot reach GitHub yet. When the agent needs to do something on GitHub, a prompt appears in a separate window (terminal, OS notification, or an `rein` status app):

```
⚠️  rein: agent is requesting GitHub access

Agent wants to start a session:
  Role:   implement
  Issue:  tomh/wrangle#73 — "Add support for sbom-action v2"

This will allow the agent to:
  - Read repo contents on tomh/wrangle
  - Write to branches matching agent/73/*
  - Comment on issues and PRs in tomh/wrangle
  - Push only commits it has signed

For 4 hours (or until you close the agent).

To approve, type the issue number: ___
To deny, press Ctrl+C.
```

The user types `73` and presses enter. The session starts. The agent now has scope-ceilinged access; `git push`, `gh pr create`, anything else hitting GitHub works without further prompts inside the session.

If the agent later wants to expand scope — say, it notices a related bug in another repo — another prompt appears:

```
⚠️  rein: agent is requesting scope expansion

Current session: implement on tomh/wrangle#73
Agent wants to also work on:
  tomh/wrangle-utils#41 — "Update SBOM serialization"

Reason given by agent (for audit): "The fix requires changes to both repos."

To approve, type "41": ___
```

If the agent finds a bug that has no issue yet:

```
⚠️  rein: agent is requesting to file a new issue

Agent wants to create:
  Repo:   tomh/wrangle
  Title:  "sbom-action v2 breaks when --json-output is set"
  Body:   "Reproduced on commit a1b2c3..."

The issue will be filed by rein-tomh-audit, with attribution
"created on behalf of @tomh".

To approve, type the proposed title's first word: ___
```

The pattern: the user always confirms with non-replayable input (issue number, first word of title, etc.) so that a prompt-injected agent can't construct a "yes" the user didn't mean.

### 2.3 What the user does NOT have to do

- Pick a session up front. The session forms itself as the agent encounters work.
- Pick a role for each thing. The role is implied by what the agent is doing (the first action requiring write access starts an `implement` session; a session that only reads stays in `scan`).
- End the session. Sessions expire when the agent process exits, after an idle timeout (30 min default), or after a hard TTL (4 hours default). Manual `rein session end` exists but is for the careful user.
- Edit any GitHub PATs. The PAT pattern is replaced entirely.
- Configure git. Once `rein run` wraps the agent, all git operations route through the sandbox proxy.

### 2.4 What the user gives up

- A startup confirmation per session, and one per scope expansion. For agents that work on one issue at a time, this is one confirmation per task. For agents that wander across many issues, it's more.
- The flexibility to grant arbitrary GitHub scopes. The five roles cover most of what GitHub-using agents do, but if you need something exotic (manage org settings, manage webhooks, etc.), you have to extend the roles in config.
- The illusion that the agent is "just running locally." There's now a daemon, a sandbox, and a proxy in the picture. None of this is visible during normal use, but it's real infrastructure.

### 2.5 When something goes wrong

- **Agent's session expires mid-task.** Agent's next git operation triggers a re-auth prompt: *"agent wants to continue session for tomh/wrangle#73 — approve?"*
- **Agent requests a scope it shouldn't have.** Confirmation prompt names the over-scope explicitly: *"agent wants to access tomh/private-stuff, which isn't related to issue #73 — this looks suspicious, are you sure?"*
- **The broker daemon crashes.** Agent's GitHub operations fail. `rein status` shows the daemon down. `rein restart` brings it back; sessions are restored from the broker's persistent state.
- **Sandbox can't start (no `srt` available).** `rein run` falls back to unsandboxed mode with a loud warning: *"Sandbox not available; agent will hold tokens in memory. Use only on throwaway repos."* The fallback exists so the system isn't completely blocked, but is clearly demarcated.

---

## 3. Prior Art

The honest answer to "did someone already build this": **no, but they built most of the pieces.** The novelty is in the composition and the GitHub-shaped ergonomics, not in any single primitive. The landscape has moved considerably in the last few months, particularly with the April 2026 launch of Infisical's Agent Vault, which validates the architectural pattern at a different deployment shape.

### 3.1 Closest architectural match: Agent Vault

**Infisical Agent Vault** (`Infisical/agent-vault`, Apache 2.0 + MIT, launched April 22, 2026; `v0.2.0` as of late April, active development). Self-described as "a credential management proxy and vault for AI agents." Core properties:

- *"Brokered access, not retrieval — Agents route requests through a proxy. There is nothing to leak because agents never have credentials."*
- *"Self-onboarding — Paste an invite prompt into any agent's chat and it connects itself."*
- *"Agent-led access — The agent discovers what it needs at runtime and raises a proposal. You review and approve in your browser with one click."*
- *"Multi-user, multi-vault — Role-based access control with instance-level and vault-level permissions."*

This is the credential-proxy half of what `rein` does, generalized across any HTTP API.

The deployment shape is different in ways that matter. From their docs: *"By design, Agent Vault is meant to be deployed on a separate machine from your AI agents to provide the security guarantee needed so your AI agents cannot directly access the credentials within Agent Vault."* The product targets DevOps teams running agents in production, with a server-on-another-machine deployment model: `agent-vault server -d`, `agent-vault register`, account creation, RBAC, multi-tenancy, Docker deployment as a first-class option. The complexity matches that audience and is operationally heavy for a solo developer on one laptop.

What Agent Vault doesn't address, that `rein` does:

1. **GitHub-specific opinionation.** Agent Vault is service-agnostic. `rein` is GitHub-shaped: GitHub Apps as credential source, issue numbers as scope anchors, audit-to-issue, SLSA Source Track linkage.
2. **Issue-anchored scope.** Their model is vault-scoped (a named collection of credentials). `rein` is task-scoped (a specific GitHub issue).
3. **Two-tier read/write tokens.** Agent Vault is single-tier proxy injection. `rein` mints write tokens just-in-time at push, HEAD-pinned, single-use.
4. **Commit signing with delegation attribution.** Out of scope for Agent Vault.
5. **SLSA Source Track integration.** Out of scope for Agent Vault.
6. **Single-binary laptop deployment.** Agent Vault's separate-machine architecture is the wrong shape for a solo developer.

**Positioning.** `rein` is not a competitor to Agent Vault; it occupies a different point in the deployment-complexity space. For teams that need centralized credential management at scale, Agent Vault is the right answer. For solo developers who want safer GitHub credentials for their AI coding agent, a single binary they install on their laptop is the right answer. The architectural pattern is shared. There's a documented composition path (§6) where Agent Vault could be used as the upstream credential proxy in team deployments while `rein` handles the GitHub-broker logic locally.

### 3.2 Other credential-proxy products

**OneCLI** (Rust, Apache 2.0). Self-hosted secret vault and HTTP proxy. *"You give the agent a placeholder key instead of the real one. When the agent makes an outbound HTTP call through OneCLI's proxy, it matches the request by host and path, verifies access rights, swaps the placeholder for the real credential, and forwards the request to the actual destination."* Same architectural pattern as Agent Vault, single-machine deployment, encrypted vault (AES-256-GCM).

**Bromure Agentic Coding** (free + open source). *"A host-side proxy swaps stub tokens for real bearers at the wire; ssh-agent forwards only its socket from macOS Keychain; sensitive credentials require a click."* Host-side proxy doing stub-token substitution. Targets Claude Code and Codex.

**NVIDIA OpenShell.** Credential placeholder substitution at the network boundary, with TLS-termination-required policy (the proxy needs to see plaintext to do replacement). Documented limitation: *"The proxy does not modify request bodies, cookies, or response content."*

**LiteLLM Agent Platform** (BerriAI). *"Self-hosted platform for running coding agents (Claude Code, Codex, Hermes) in isolated sandboxes with vault proxy."* Kubernetes-based. The pod's environment ships with stub credentials; the vault swaps them at the wire.

**Docker Sandboxes for Claude Code** (announced ~April 2026). MicroVM-based isolation; from their docs: *"The proxy handles the OAuth flow, so credentials aren't stored inside the sandbox."* The credential-proxy pattern applied to Anthropic API auth, not GitHub credentials per se.

These are all variants of "host-side proxy substitutes real credentials for stubs at the wire." The pattern is well-validated; the differentiation between them is mostly deployment shape, target audience, and breadth of API coverage.

### 3.3 Generic agent credential brokers

**CB4A** (`draft-hartman-credential-broker-4-agents-00`, K. Hartman, SANS Institute, March 29, 2026). IETF informational draft naming *Credential Broker for Agents* as a pattern. Architecture: SPIFFE/SPIRE workload identity, Policy Decision Point / Credential Delivery Point separation, DPoP for sender-constrained tokens, three credential proxy models. The draft explicitly identifies **GitHub App Installation Tokens as the appropriate Model B mechanism**. Treat CB4A as the protocol layer; this design is the GitHub-shaped instance. Its threat model (eleven threats) is adopted in §5.

**AgentWrit** (`devonartis/agentwrit`, PolyForm Internal Use 1.0.0). Generic ephemeral-credential broker with delegation chain verification. Architecturally close in spirit. Service-agnostic scope language, no GitHub awareness, no git integration, no issue anchor. PolyForm Internal Use license blocks OSS redistribution.

**Aembit IAM for Agentic AI** (commercial SaaS, GA April 9, 2026). Combines an agent identity with the human user's IDP identity and intercepts MCP tool calls for credential exchange. Sets the bar for what mature commercial looks like.

**AWS Bedrock AgentCore Identity** (GA October 13, 2025). OAuth 2.0 flows for agent inbound/outbound auth; has explicit GitHub OAuth2 provider documentation. AWS-coupled.

**Microsoft Entra Agent ID.** Agents as a class of service principal. Entra/Azure-coupled.

### 3.4 GitHub-shaped credential helpers

**ghtkn** (`suzuki-shunsuke/ghtkn`). Go CLI that creates 8-hour GitHub App **User Access Tokens** via Device Flow, caches them in OS keychain, acts as a git credential helper. Closest open-source single-purpose project. User-attributed (not agent-aware), no two-tier model, no issue anchor, no session, no audit.

**`bdellegrazie/git-credential-github-app`.** Pure git credential helper that mints installation tokens (1h TTL). Single-user, no agent identity, no scoping logic.

### 3.5 Sandbox-side adjacent work

**Anthropic's `sandbox-runtime` (srt)** (`anthropic-experimental/sandbox-runtime`, Apache 2.0, research preview). The primary sandbox integration target. Uses `sandbox-exec` on macOS and `bubblewrap` on Linux with HTTP and SOCKS5 proxies for network mediation. Validation §12.2 confirmed `srt` exposes a usable library API (`SandboxManager`, `FilterRequestCallback`, `network.mitmProxy.socketPath`, `parentProxy`) — no fork or upstream PR required to integrate.

Worth flagging: `srt` has had two confirmed sandbox bypass vulnerabilities in the last six months (CVE-2025-66479 silently fixed November 26, 2025; SOCKS5 hostname check bypass silently fixed April 1, 2026). The sandbox is real defense-in-depth, not a hard boundary. The design treats it that way.

**Anthropic's cloud Claude Code credential proxy.** Described in §1.6. Same architectural pattern, cloud-hosted, repo-scoped rather than issue-scoped.

**Greywall** (Apache 2.0, fork of Tusk AI's Fence, inspired by `sandbox-runtime`). Container-free sandboxing on Linux and macOS with deny-by-default transparent proxy. An alternative if `srt` is not available.

### 3.6 IETF / standards landscape

The agent identity space is incredibly active but fragmented. A non-exhaustive list of in-flight drafts at -00 stage in early-to-mid 2026:

- **CB4A** (`draft-hartman-credential-broker-4-agents-00`) — credential broker pattern, generic.
- **AIP (Prakash)** (`draft-prakash-aip-00`) — Invocation-Bound Capability Tokens, JWT/Biscuit dual-mode.
- **AIP (Cao/NVIDIA)** (`draft-aip-agent-identity-protocol-00`) — two-layer identity + enforcement.
- **AIP (Singla)** (`draft-singla-agent-identity-protocol-00`) — DID-based.
- **AIMS** (Kasselman et al.) — composing WIMSE, SPIFFE, OAuth.
- **WIMSE** (Ni and Liu) — Dual-Identity Credential binding agent identity to owner identity.
- **Agentic JWT** (Goswami) — JWTs with agent-specific claims.
- **SCIM for agents** — provisioning lifecycle.

Six+ competing approaches at -00. No standard will consolidate in the next 12 months, probably not the next 24. The closest in spirit to the user-identity-annotation approach is WIMSE's Dual-Identity Credential binding agent identity to owner identity.

**Sigstore community engagement is not active on agent identity.** No issues, no roadmap items, no blog posts on Sigstore's side. Sigstore is heavily CI-focused; local-developer workload identity is largely absent from the Sigstore conversation. The future-work entry in §11 reflects this honestly.

### 3.7 Supporting standards / tools

**SLSA Source Track.** Currently delegates identity enforcement to the SCS (GitHub's branch protection rules). The verifier checks that protection rules were configured at the time of a commit but does not itself verify the cryptographic identity of committers. There is no first-class mechanism today in SLSA Source Track for *"this branch only accepts commits cryptographically signed by these specific identities."* This design proposes adding one (§4.2.8); it's a conceptually new policy primitive, not a missing field.

**Gitsign** (`sigstore/gitsign`). Keyless commit signing using Sigstore. Designed primarily for GitHub Actions. For local agents, public Sigstore's Fulcio does not currently accept arbitrary delegation claims as cert extensions (validation §12.6). The v1 design uses broker-as-CA rather than public Sigstore.

### 3.8 What doesn't exist in public OSS

No project found that scopes GitHub credentials per issue, composes a credential broker with `sandbox-runtime` for proxy-side injection on a developer laptop, anchors agent session identity on issues, uses issue audit comments as the primary audit surface, ships as a single laptop-focused binary, and combines all of the above with delegation-attributed commit signing.

The composition is novel; each piece is borrowed or adapted.

---

## 4. Architecture

### 4.1 High-level diagram (sandbox-composed)

```
            ┌──────────────────────────────────────────────────────────────────┐
            │                        Developer Laptop                           │
            │                                                                   │
            │  ┌────────────┐      1. rein run claude                     │
            │  │  Claude    │─────────────────────────────┐                    │
            │  │  Code      │                             ▼                    │
            │  │ (host CLI) │      ┌──────────────────────────────────┐        │
            │  └─────┬──────┘      │ rein daemon                │        │
            │        │ 2. start    │  - signing key (keychain / SE)   │        │
            │        │   sandbox   │  - session table                 │        │
            │        ▼             │  - 5 roles, scope ceilings       │        │
            │  ┌──────────────────┐│  - token minter (read + JIT write)│       │
            │  │ srt sandbox      ││  - issue resolver                │        │
            │  │ (bubblewrap/     ││  - separate audit identity       │        │
            │  │  sandbox-exec)   ││  - hash-chained local audit log  │        │
            │  │                  ││  - human approval dispatcher     │        │
            │  │  ┌────────────┐  │└────┬─────────────────────┬───────┘        │
            │  │  │ agent +    │  │     │ 4. token for proxy  │                │
            │  │  │ git, gh,…  │  │     ▼                     │                │
            │  │  └─────┬──────┘  │  ┌────────────────────────┴───┐            │
            │  │        │         │  │ srt HTTP/SOCKS5 proxy      │            │
            │  │        │ HTTPS   │◄─┤  - asks broker for token   │            │
            │  │        │ to gh   │  │  - injects Authorization   │            │
            │  │        ▼         │  │  - never returns to sandbox│            │
            │  │  ┌────────────┐  │  └────────────────┬───────────┘            │
            │  │  │ proxy.lo   │  │                   │                        │
            │  │  │ :8080      │  │                   │ 5. request w/ token    │
            │  │  └────────────┘  │                   │                        │
            │  └──────────────────┘                   │                        │
            │  agent never sees token                 │                        │
            │                                         │                        │
            └─────────────────────────────────────────┼────────────────────────┘
                                                      ▼
                                          ┌──────────────────────┐
                                          │ github.com           │
                                          │  api.github.com      │
                                          │  issue comments      │
                                          └──────────────────────┘
```

### 4.2 Component breakdown

**4.2.1 Broker daemon (`rein`).** A long-running local process. Listens on a Unix domain socket owned by the user's UID. Holds: GitHub App client config; signing key for delegation certs; session table (in-memory + WAL on disk); hash-chained audit log; a separate audit identity (the audit GitHub App from §2.1) so compromised agent tokens cannot prune audit comments; a human-approval dispatcher that surfaces confirmation prompts via terminal or OS notification.

The broker plays three distinct roles worth keeping separate in the mental model:
- **Orchestrator.** Holds session state, runs scope ceiling logic, dispatches human-confirmation prompts, posts audit comments.
- **GitHub App token minter.** Exchanges its App credentials for short-lived, scoped GitHub installation tokens at request time.
- **Delegation issuer.** Signs certs that bind an agent session to the user's identity (the user-via-delegation model in §4.2.6).

These are conceptually separable. The broker is the issuer in roles 2 and 3. The signing key (§4.2.1.1) matters for role 3 specifically — it's what makes delegation certs verifiable as having come from this user's broker.

**4.2.1.1 Signing key custody.** The broker's signing key needs integrity (to prevent forging delegation attestations) and persistence (so the broker's identity survives restarts). Two distinct concerns:

1. **Where the key bytes (or handle) live.** OS-native credential store on each platform, with a portable encrypted-file fallback. Handled by `99designs/keyring` (§6.3), which abstracts across macOS Keychain, Linux Secret Service, Windows Credential Manager, and a portable encrypted-file backend for environments without OS keyring services (devcontainers, headless CI). The fallback is transparent — same code path, `AvailableBackends()` picks the strongest option present.

2. **Where the signing operation happens.** Software-backed (key material decrypted into process memory) vs hardware-backed (key never leaves the Secure Enclave / TPM). For hardware-backed, `facebookincubator/sks` (§6.3) drives the actual SE/TPM2 operations. The keyring stores the key handle, not the key material; signing happens inside the hardware.

Custody options, in order of strength:

| Custody mode | Persistence | Integrity | Distribution requirement |
|---|---|---|---|
| Keyring file backend (portable encrypted file) | Yes | Low–Medium (encrypted at rest with user-provided passphrase) | None |
| Keyring OS-native backend | Yes | Medium (login-keychain / Secret Service / WinCred) | None |
| macOS Secure Enclave, ephemeral | Process-lifetime only | High (key never leaves Enclave) | None |
| macOS Secure Enclave, persistent | Yes | High | **Codesigning + `keychain-access-groups` entitlement** |
| Linux TPM2 via PKCS#11 | Yes | High | `tpm2-pkcs11` installed; depends on hardware |
| Windows Platform Crypto Provider | Yes | High | None for self-signed; production needs codesign |

Validation §12.5 found that on macOS, unsigned Go binaries cannot persist Secure Enclave keys (`errSecMissingEntitlement -34018`). Ephemeral SE keys work (key never leaves Enclave for the process's lifetime), but the broker's identity would rotate on every restart, breaking any policy that pins to the broker's key. Persistent SE keys require codesigning with `keychain-access-groups` entitlement, which means Apple Developer ID + notarization for distribution channel.

**Trajectory:**
- **v0 (PoC):** Keyring abstraction with whatever backend is available. On a host Mac that's macOS Keychain; inside a devcontainer that's the encrypted file backend. No codesigning needed in either case. Persistent across restarts. Same code path either way. Honest characterization: meaningfully better than a raw file on disk; less hardware-isolated than the Enclave path that comes in v1.
- **v1 (release):** Secure Enclave on macOS via `sks` (with the broker distributed as a codesigned and notarized binary). TPM2 on Linux where available. Keyring file backend documented as the fallback for users building from source without codesigning their own binary or running on a host without hardware support.
- **Distribution channel question.** Shipping codesigned macOS binaries requires Apple Developer ID + notarization in the release pipeline. This is a `wrangle` opportunity (see §6.3) — the canonical productized version of "codesign + notarize + SLSA provenance for Go macOS binaries" is missing from the ecosystem.

**4.2.2 Roles and scope ceilings.** Five roles:

```yaml
# ~/.config/rein/roles.yaml
roles:
  scan:
    description: Read-only inspection
    github_permissions: { contents: read, metadata: read, issues: read, pull_requests: read }
    write_allowed: false
    default_read_ttl: 8h

  triage:
    github_permissions: { contents: read, issues: write, pull_requests: write, metadata: read }
    default_read_ttl: 4h
    default_write_ttl: 5m

  implement:
    github_permissions: { contents: write, pull_requests: write, issues: write, metadata: read }
    default_read_ttl: 4h
    default_write_ttl: 5m
    require_signed_commits: true
    require_human_confirmation_on_default_branch: true
    branch_pattern: "agent/{{issue}}/{{nonce}}"
    single_use_write_tokens: true

  review:
    github_permissions: { contents: read, pull_requests: write, metadata: read }
    write_allowed: limited
    default_read_ttl: 2h

  release:
    github_permissions: { contents: write, packages: write, metadata: read }
    default_read_ttl: 1h
    default_write_ttl: 5m
    require_signed_commits: true
    require_human_confirmation: true
    human_confirmation_method: "type_issue_number"
```

Effective scope ceiling is `intersect(role.permissions, github_app_installation.permissions, union(issues' repos))`.

**4.2.3 Session lifecycle (ambient model).**

Sessions are formed and modified during agent operation, not configured up front. Lifecycle:

1. **Agent starts.** `rein run claude` launches the sandbox with the agent inside. No session exists yet. Agent has no GitHub access.
2. **Agent requests GitHub access.** Any operation hitting `github.com` or `api.github.com` from inside the sandbox triggers a broker request: "agent wants to do X on Y."
3. **Broker derives the appropriate role and scope** from the request. If the request implies write to a specific issue's repo, the broker proposes an `implement` session for that issue.
4. **Human confirms.** Broker surfaces a confirmation prompt with non-replayable input (type the issue number / first word of a title / etc.). If approved, the session starts.
5. **Session active.** Subsequent requests within the same scope ceiling proceed without new prompts. The broker mints tokens as needed (read tokens for read operations, JIT write tokens for `git push` and similar).
6. **Scope expansion.** If the agent needs something outside the current ceiling — another issue, another repo — a new confirmation prompt.
7. **Issue creation.** If the agent wants to file a new issue mid-session (e.g., found a bug), the broker prompts the human, creates the issue using the audit App's identity attributed "on behalf of @tomh," and adds the new issue to the session's scope.
8. **Session ends.** Automatically on any of: agent process exit, idle timeout (default 30 min), hard TTL (default 4 hours), explicit `rein session end`. The human does not have to remember to end the session.

Multi-issue sessions are first-class: a single session can be bound to issues 73, 74, 75 (scope ceiling = union of their repos), and audit comments are cross-posted to all three with mutual cross-references.

The session's identity for cryptographic purposes is a SPIFFE-shaped string like `spiffe://laptop.local/role/implement/issues/wrangle#73+74/session/01HXYZ...`. Pattern-matchable for policy purposes (see §4.2.8). The human-readable form is what users see in audit comments and policy. Internal IDs (used for integrity anchors per §5.3 TM-G6) are invisible plumbing.

**4.2.4 GitHub App integration — two Apps, two token types.**

- **Primary App** (`rein-<user>`): the working App, Device Flow enabled, all role permissions, webhook disabled.
- **Audit App** (`rein-<user>-audit`): minimal permissions (`issues: write` only). Used only for posting audit comments and for filing agent-requested new issues. Broker holds installation tokens for this App separately; never exposed to the sandbox proxy.

**Token types in the broker's possession.** Validation §12.4 confirmed the two-step flow:

1. **User-to-server token (`ghu_...`).** Returned by GitHub Device Flow during `rein init`. 8-hour TTL with a 6-month refresh token. Held by the broker as evidence the user has blessed this installation on this laptop. Used by the broker to call the installation-token API (and to refresh as needed). **Not given to agents.**

2. **Installation tokens (`ghs_...`).** Minted by the broker on demand via `POST /app/installations/{id}/access_tokens` with `repository_ids` and `permissions` for per-mint scope shaping. 1-hour max TTL per GitHub. **These are what the proxy injects into agent requests.**

Validation §12.3 confirmed that installation tokens can be scoped to a repository subset and a permission subset, with the API enforcing both. The two-tier read/write design (§4.2.5) is built on this.

**4.2.5 Two-tier token issuance.**

- **Session read token** covers read APIs (cloning, reading issue/PR, listing files). Held in the broker; served by the sandbox proxy on demand for the session TTL.
- **Write token** is minted just-in-time at push time. When `git push` traverses the proxy, the proxy notifies the broker, which (1) inspects the commits being pushed; (2) verifies they're on a branch matching the session's pattern; (3) mints a fresh installation token with minimum push permissions; (4) returns it via proxy injection; (5) marks the token single-use and sets a 5-minute revocation timer.

This addresses TOCTOU concerns (TM-G6) by binding the token to a specific HEAD and limiting it to a single use. The effective write window is "5 minutes or one push, whichever first."

**4.2.6 Commit signing — broker as CA (v1 default).**

This is the design's biggest open question; validation §12.6 resolved it for v1 by ruling out the alternative.

**v1 approach: broker as its own CA.** The broker mints X.509 certs naming agent-delegated session identities. Certs are signed with the broker's signing key (§4.2.1.1). gitsign signs commits with these certs. SLSA Source policy explicitly trusts the broker's CA via the proposed `allowed_signer_identities[]` extension (§4.2.8).

The cert's SAN names the session identity in SPIFFE form: `spiffe://broker-<userid>.rein.local/role/implement/issues/wrangle#73/session/01HXYZ`. The user's GitHub identity is named in a separate cert extension as the human-via-delegation principal. The "Tom committed this via agent delegation in session X for issue 73" semantic is preserved.

**Honest characterization:** third-party verification requires trusting the broker's CA. This is fine for personal projects and small teams where users configure `source-tool` to trust their own brokers' CAs. It is **bounded for adoption beyond that**: an open-source maintainer who wants to verify a PR from someone using `rein` has to trust that user's broker CA — there is no public trust root for agent-delegated commits.

**Why not public Sigstore in v1.** The user-identity-annotation approach (using Fulcio with agent claims as cert extensions) was the leading v2 design until validation §12.6 found that Fulcio's `Extensions` struct (`pkg/certificate/extensions.go:61-143`) is a closed enum of fields with hardcoded OIDs. The `ciprovider` template mechanism cannot add new OIDs; it can only populate existing ones. The hashedrekord Rekor entry type (what gitsign emits) is `hash + sig + pubkey`, no metadata field. Rekor's SearchIndex supports only email/hash/publicKey. There is no path to adding agent annotations to public-Sigstore-issued certs without upstream Sigstore source changes coordinated via the Sigstore TAC. Sigstore community activity on agent identity is currently zero (no issues, no roadmap items as of May 2026).

**The public-Sigstore-via-OIDC-IdP path** — where the broker is an OIDC issuer Fulcio is configured to trust, with session metadata in Fulcio extension fields — remains the right architectural target. It just requires the Sigstore community to engage on agent identity and accept the broker (or a class of brokers) as a recognized issuer. This is documented as future work (§11.6) with honest acknowledgment that the timeline is multi-quarter even with active shepherding.

**What happens if v1 ships and no Sigstore engagement materializes.** Agent-delegated commits remain verifiable only by parties who trust the broker's CA. SLSA Source Track integration works for users who configure their own policy. Adoption is bounded to "people willing to configure trust in their local broker." This is a real constraint and worth being honest about; it is also fine for personal-laptop use, which is the v1 target.

**4.2.7 Audit writeback.** On every meaningful action (session start, scope expansion, write-token mint, push, session end), the broker:

1. Appends to a local hash-chained log.
2. Posts a structured comment on the issue(s) bound to the session **using the Audit App's identity**, not any agent token. Example:

```
🤖 rein audit
session: sess_01HXYZ...
role: implement
event: push
branch: agent/73/abc123
commits: 3 (a1b2c3..f4e5d6)
signed by: tomh (via agent delegation, session 01HXYZ)
audit hash: sha256:8f4e...
```

Because the Audit App's installation token is never given to the agent or its proxy, a compromised agent token cannot edit or delete audit comments. The comments are **tamper-evident** against agent compromise (only the Audit App can edit them) but not tamper-proof against broker compromise. Real prevention against broker compromise requires periodic anchoring to an external append-only log (Rekor, sigstore-transparency, or a separate git repo); recommended for v2.

**Optional v2+ upgrade: in-toto attestations as queryable audit artifacts.** The broker can emit an in-toto attestation per session covering "session sess_xyz, role implement, issues [73], started at T, scope ceiling X, commits Y, audit hash Z." Signed by the broker, logged to Rekor. This gives a canonical structured artifact describing the session, queryable via Rekor's API by anyone, cryptographically tied to the broker's signing identity, and compatible with `wrangle` and other supply-chain tools that already understand in-toto. **This is audit-layer, not auth-layer** — in-toto attestations describe state; they don't authorize action. The authorization decision still happens at the broker. Worth noting explicitly so future readers don't confuse this with the §4.2.6 commit-signing question.

**4.2.8 SLSA `source-tool` linkage.** The current SLSA Source Track delegates identity enforcement to the SCS (GitHub's branch protection). The verifier checks rule configuration, not cryptographic committer identity. There is no first-class mechanism today for "this branch only accepts commits cryptographically signed by these specific identities."

This design proposes adding one as an upstream contribution to `slsa-framework/source-policies`:

```json
{
  "canonical_repo": "https://github.com/tomh/wrangle",
  "protected_branches": [{
    "Name": "main",
    "target_slsa_source_level": "SLSA_SOURCE_LEVEL_3",
    "allowed_signer_identities": [
      { "issuer": "broker-ca:rein:<broker-key-fingerprint>",
        "subject_pattern": "spiffe://broker-<userid>.rein.local/**" },
      { "issuer": "broker-ca:rein:<broker-key-fingerprint>",
        "subject_pattern": "spiffe://broker-<userid>.rein.local/role/implement/**",
        "counts_as_reviewer": false }
    ]
  }]
}
```

The pattern matching handles issue rotation: `spiffe://.../role/implement/**` matches any session for any issue without requiring policy updates per issue. The `counts_as_reviewer: false` flag means agent-delegated commits count toward authorship but **not** toward the second-signer requirement at L3/L4 — a human must still be the second signer.

**This is a conceptually new policy primitive, not a missing field.** The upstream conversation needs to be "how should agent identity work in Source Track at all?" rather than "please accept this field." A SLSA community meeting discussion before any concrete proposal is the right first step.

### 4.3 Protocol design

**4.3.1 Proxy → broker (primary interface):**

```
POST /v1/proxy/token
Headers: X-Proxy-Auth: <ephemeral-per-request-bearer>
Body: { "session_id": "sess_...",
        "request": { "method":"POST","host":"github.com",
                     "path":"/<owner>/<repo>/git-upload-pack" } }
→ 200 { "authorization": "Bearer ghs_...", "expires_at": "..." }
```

The proxy bearer is short-lived (rotated per-request, not per-session) to limit replay if a bearer leaks via process memory.

**4.3.2 Agent ↔ broker (session management, via proxy):**

The agent doesn't talk to the broker directly. Session-management requests from the agent reach the broker via the proxy too: the proxy intercepts the agent's call to `rein.local/session/start`, forwards to the broker, broker pops a human confirmation, returns the result. This keeps the broker socket out of the sandbox.

**4.3.3 Bootstrap.** Described in §2.1.

---

## 5. Threat Model

We adopt CB4A's eleven-threat taxonomy and add eight GitHub-/composition-specific threats.

### 5.1 Threats prevented vs PAT status quo (honestly characterized)

| Threat | PAT status quo | This design **with sandbox** | This design **without sandbox** |
|---|---|---|---|
| Long-lived PAT exfil | Catastrophic, up to 366d/unbounded | **Token never reaches agent memory** | TTL-bounded (hours read, ≤5m write) |
| Over-scoped use | Whole-user-scoped | Scope ceiling = issues' repos | Same |
| Agent commit impersonating user | Yes | Commit signed "user-via-delegation" | Same |
| PR opened bypassing review | Possible if user has bypass | Agent-delegated commits don't count as second signer | Same |
| Forensic "what did the agent do?" | Untangle agent from user activity | Issue comments + delegation-annotated signatures | Same |
| **Exfil during write window** | N/A — PAT already exfilled | **Sandbox prevents** | **Not prevented** |
| **Prompt-injection-driven scope escalation** | N/A | Human confirmation with non-replayable input | Same |
| **Prompt-injection-driven config rewrite** (TM-G8) | Yes, no defense | Sandbox prevents | Limited defense; helper must always return credential |

**Honest characterization:** read tokens with hours-long TTL are not exfil-proof against a determined adversary. A prompt injection that gets the agent to leak a token via tool output requires roughly one model turn. The broker alone buys you faster revocation, smaller scope, and per-session attribution — not exfil prevention. The sandbox is what closes the exfil door.

**Calibration of the overall claim.** The pitch is not "this makes agents safe." It is "this makes agents *accountable and bounded* in a way the PAT pattern doesn't." Compared to the status quo:

1. A boundary that exists at all. The role permissions ceiling is enforced at token-mint time, not just by convention.
2. Per-task scoping. The boundary is the issue's repo and the role's permissions, not the user's entire GitHub presence.
3. Attribution that survives compromise. Audit comments from a separate identity, signed commits with delegation claims.
4. Required human confirmation for scope changes, with non-replayable input.
5. Sandbox-bounded exfiltration when composed with `srt`.

The agent goes from "indistinguishable from human, broad scope, no defined boundary" to "distinguishable in audit, narrow scope, defined boundary with explicit expansion ceremony." That is not a complete defense, but it is meaningfully better than the status quo.

### 5.2 CB4A threats — disposition

- **TM-1 Broker Compromise.** Mitigated by hardware-backed signing key in v1 (Enclave/TPM). v0 keychain is honestly weaker; documented.
- **TM-2 Revocation Propagation Failure.** Short TTLs because revocation isn't reliable.
- **TM-3 Token Theft and Replay.** GitHub doesn't support DPoP on installation tokens. Sandbox composition is the meaningful mitigation.
- **TM-4 Approval Bypass.** N/A for local-only PoC.
- **TM-5 Justification Field Gaming.** Justification is evidence, not authorization. Scope decisions rest on the issue → repo lookup.
- **TM-6 Multi-Agent Scope Composition.** Each session has a distinct identity; broker logs cross-session correlations.
- **TM-7 Audit Log Compromise.** Mitigated by separate Audit App identity.
- **TM-8 Policy Engine Injection.** Issue numbers, bodies, branch names treated as untrusted strings.
- **TM-9 Fail-Open Under Pressure.** Explicit fail-closed default.
- **TM-10 Approver Spoofing.** N/A for v0.
- **TM-11 Broker Bypass.** **Without sandbox: essentially unsolvable in software at this layer. With sandbox: the agent has no network path except via the proxy; bypass closed at network layer.** This is the single largest reason sandbox composition is mandatory.

### 5.3 GitHub-/composition-specific threats

**TM-G1 GitHub App private key custody.** Device Flow path; no private key on disk for the user's installation. The broker holds its own App signing key (separate from the user's auth) per §4.2.1.1.

**TM-G2 Repo-scope leakage via cross-repo submodules.** SLSA Source Track issue; out of scope.

**TM-G3 Forged agent identity in signatures.** In v1 with broker-as-CA, an attacker who compromises the broker's signing key can forge agent-delegation certs. Mitigations: hardware-backed key in v1; honest README characterization that the broker CA is a high-value target; future v2+ public-Sigstore-via-IdP path would shift the trust anchor to Sigstore's CT log monitoring.

**TM-G4 Broker-as-confused-deputy / proxy bearer theft.** The proxy authenticates to the broker with an ephemeral bearer. On a default-config Linux laptop, any user-owned process can read another user-owned process's memory (`ptrace`) or environment (`/proc/<pid>/environ`) and steal the bearer. **This is not a new attack surface introduced by `rein` — it exists for any sandbox+proxy architecture, including `srt` alone.** The broker is actually a marginal improvement over static credentials in the proxy: bearers are per-request rather than per-session, limiting replay value.

Mitigations:
- Mandatory: rotate bearers per-request, not per-session.
- Mandatory: never pass bearer via environment variable; use one-shot pipe at process spawn.
- Recommended in README: `kernel.yama.ptrace_scope=1` on Linux meaningfully reduces this exposure.
- Recommended in README: run agent processes as a separate UID if the user is willing to set that up.

macOS is meaningfully better here: System Integrity Protection blocks `ptrace` by default and `/proc/<pid>/environ` doesn't exist.

**TM-G5 Prompt-injection-driven session escalation.** Indirect prompt injection in a README, issue body, or PR title can convince the agent to request elevated roles or expand scope. Aonan Guan's *"Comment and Control"* research (April 15, 2026) demonstrated this against three production agents. Mitigations:
- Mandatory: all session starts, scope expansions, and issue creations require human confirmation with non-replayable input (type the issue number, type the first word of the title, etc.).
- Optional: per-repo per-day rate-limit on session starts.

**TM-G6 TOCTOU between resolution and use.** Two sub-cases.
- *Between mint and use:* attacker amends commits or changes branches between the JIT mint and the push. Mitigations: single-use tokens, branch HEAD pinned at mint time.
- *Between session start and later use:* issue is transferred to another repo, changing the implied scope ceiling. **Validation §12.7 found that GitHub does not preserve the internal issue ID across transfers** (both `id` and `databaseId` change). The integrity anchor in this design is therefore **the REST URL 301-redirect chain**, not the internal ID. At session start, the broker captures the issue's canonical REST URL. On every token mint, the broker fetches the URL; a 301 redirect indicates the issue has moved, triggering a session re-prompt. Users still see `tomh/wrangle#73`; the redirect-chain check is invisible plumbing.

**TM-G7 Issue content manipulation by the agent.** The agent typically has `issues: write` in `implement` and `triage` roles. A compromised agent could edit the bound issue's title or body to make a malicious action look authorized. Mitigations:
- **The issue is the scope anchor, not the scope authorizer.** Scope ceiling is derived from the issue's *repo* and the *role*, not from what the issue text says. An agent that edits issue #73 to say "drop branch protection on main" does not gain the ability to drop branch protection — that capability is not in any role's GitHub permissions. The role gates authority; the issue gates only "which repo are we anchored to."
- **Scope is read-once for security purposes.** The broker reads the issue at session start to derive the ceiling. Subsequent reads (for context, by the agent) do not affect the ceiling. Scope expansions require a separate human confirmation with non-replayable input.
- **Issue snapshot captured at session start.** Title, body, labels, and the canonical REST URL are recorded in the broker's hash-chained audit log and included inline in the Audit App's first comment on the issue. Post-hoc editing cannot rewrite this snapshot.
- **GitHub's issue edit history is preserved.** A forensic reviewer can see both the original and the edited versions; the broker's snapshot tells them which version the broker saw.
- **Audit comments are written by the Audit App, not the agent.** The agent cannot edit or delete them.

**TM-G8 Agent self-remediation displacing controls.** Validation §12.1 found that when the broker's credential helper returned no credentials, Claude Code reflexively ran `gh auth setup-git`, which silently rewrote `~/.gitconfig` and displaced the broker. The agent was not malicious; it was trying to "fix what looked like a config problem." This is a generic class of failure: when the controls we want to enforce look to the agent like a broken environment, the agent's self-remediation behavior will undo them.

Mitigations:
- **Mandatory: the credential helper must always return a credential.** Never empty, never error. The broker mints a deliberately-narrow credential (read-only, expired, or minimal-scope) if no session is active, rather than letting the agent see a failure.
- **Mandatory under sandbox composition:** the agent's view of `~/.gitconfig` is sandbox-local; rewrites don't escape to the host. The proxy mediates all traffic regardless of what sandboxed git thinks it's doing. This makes the rewrite issue moot for Shape A (sandbox-composed) deployments.
- **Acknowledged limitation for Shape B (credential-helper without sandbox):** a sufficiently determined or compromised agent can run `git config --global` to remove or substitute the helper. This is the same threat surface that exists today with PATs (a compromised agent can do anything the user can do). Shape B is meaningfully worse than Shape A here; production use should always go through Shape A.

### 5.4 What this design does NOT protect against

- Stolen unlocked laptop. Disk encryption and screen lock are the user's responsibility.
- A prompt injection that convinces the **developer** (not the agent) to manually escalate.
- The LLM provider exfiltrating session content. Tokens injected by the proxy are not visible to the agent, so they aren't in conversation logs; but anything the agent sees (issue text, code, command output) is visible to whoever logs the conversation.
- Malicious VS Code extensions or MCP servers running outside the sandbox.
- Supply-chain attacks on the broker's own dependencies — `wrangle`'s job.
- Sandbox escapes. `srt` has had multiple known bypasses (§3.5); the sandbox is defense-in-depth, not a hard boundary. When sandbox holds, broker is redundant for exfil; when sandbox fails, broker scope ceiling is the last line.
- Compromise of the broker's CA in v1. The broker-as-CA model means a compromised broker key can mint arbitrary delegation certs. Hardware-backed key custody (v1) raises the cost; future v2+ public-Sigstore integration would shift the trust anchor.

---

## 6. Build vs Integrate vs Punt

**Recommendation: build `rein` as a standalone laptop-focused binary, leaning on mature OSS libraries for the hard parts.** The proxy integration is where the real security wins are; the helper integration is the fallback for environments without `srt`. Document a composition path with Agent Vault for team deployments.

### 6.1 Why not part of `wrangle`

`wrangle`'s identity is "supply-chain security tool that runs on GitHub Actions and scans for risky patterns." Different audience (devs vs CI), different release cadence, different binary shape. Clean architectural seam: they share data (delegation signer identities → `source-tool` policy → `wrangle`'s scanning) but not code. `wrangle` provides the release pipeline for `rein`; `rein` doesn't live inside `wrangle`.

### 6.2 Why not contribute to an existing project

- **Agent Vault:** different deployment shape (separate-machine server vs single-binary laptop). Their `internal/` packages are "internal" in the Go sense — can't be cleanly imported without forking. A composition story (Agent Vault as upstream proxy for team deployments) is more honest than "this is a layer above Agent Vault." For solo laptop use, building our own minimal proxy is cleaner than fitting Agent Vault into a single-binary shape.
- **AgentWrit:** PolyForm Internal Use license blocks OSS distribution; Python-only SDK doesn't fit a Go supply-chain ecosystem.
- **ghtkn:** User-to-server token model is the wrong shape; would require rewriting the core.
- **`source-tool`:** *Should* receive the policy-schema proposal. That's the natural contribution path, separate from the broker itself.
- **`sandbox-runtime`:** Validation §12.2 confirmed `srt` exposes a usable library API. We use it as a library; no fork needed. Upstream contribution of first-class `injectHeaders` config in `srt`'s upstream config schema is plausible but not required.

### 6.3 Implementation building blocks (libraries to lean on)

The hard parts are well-trodden ground in OSS. The libraries below are mature, appropriately-licensed, and well-suited to what we need. License compliance is non-negotiable; all of these are explicitly compatible with permissive OSS distribution (Apache 2.0, BSD, MIT). Any direct code reuse will be appropriately attributed and the license terms followed.

**MITM HTTPS proxy substrate.**
- **`elazarl/goproxy`** (BSD-licensed, 10+ years mature). Customizable HTTP proxy with CONNECT-style HTTPS hijacking and on-the-fly cert generation. The project's own status: *"This project has been created 10 years ago, and has reached a stage of maturity. It can be safely used in production, and many projects already do that."* This is the proxy substrate.
- **`stripe/goproxy`** is Stripe's fork adding transparent proxy support; worth knowing about but elazarl's upstream is the primary candidate.

What we don't reinvent: cert cache management, CONNECT tunneling edge cases, h2 handling, SNI extraction. All well-handled by goproxy.

**GitHub App token management.**
- **`jferrl/go-githubauth`** (zero external dependencies as of v1.5.0). Implements `oauth2.TokenSource` for GitHub App authentication and installation token generation with intelligent caching. Supports both legacy App IDs and the new Client IDs that Device Flow returns. Production-ready, recently active.
- **`palantir/go-githubapp`** is a heavier framework (full GitHub App lifecycle including webhook handling). More than we need for the broker; useful reference for patterns.

What we still write: the GitHub-shaped opinionation (session state, scope ceilings, role-based permissions, issue resolution, two-tier read/write token policy, human-confirmation prompts). None of these libraries know about issues or roles.

**Hardware-backed keys and key custody.**
- **`99designs/keyring`** (MIT). Cross-platform key/credential storage abstraction. Originally extracted from AWS Vault. Backends: `keychain` (macOS), `secret-service` (Linux D-Bus / libsecret), `wincred` (Windows), `file` (portable encrypted-file fallback for any environment, including devcontainers). `AvailableBackends()` auto-selects the strongest backend present on the current host. Same code path works on a host Mac (uses Keychain) and inside a devcontainer (falls back to encrypted file with a passphrase prompt on first use). Dissolves the devcontainer/host split for key custody without per-environment special-casing.
- **`facebookincubator/sks`** (Apache 2.0, beta). Unified Go interface abstracting Secure Enclave (macOS) and TPM2 (Linux) for hardware-backed signing. P256 ECDSA only — which is exactly what we need (ES256 JWT signing). Used in v1 for hardware-backed key custody. Documents the macOS entitlement requirement (`com.apple.application-identifier`) lining up with the §12.5 finding.
- **`KizzyCode/secureenclave-c`** (BSD/MIT) — thin C API specifically for Secure Enclave, useful as a fallback if `sks` is too high-level.

`99designs/keyring` and `sks` are complementary, not redundant: keyring stores key material or key handles; sks drives the actual hardware signing operations. v0 uses keyring alone (software-backed signing); v1 adds sks for hardware-backed signing while keyring stores the handle.

**Sigstore (deferred to future work).**
- **`sigstore-go`** (Apache 2.0) — the newer, smaller alternative to `cosign` for Go integration. Per the Sigstore blog: *"Cosign was designed first and foremost as a CLI… we began work on a new Go library, sigstore-go, to provide a more minimal and friendly API for integrating Go code with Sigstore."* If/when the future-work Sigstore engagement bears fruit (§11.6), this is what we'd use.
- For v1's broker-as-CA path, no Sigstore client library is needed — we're operating our own CA, not talking to public Fulcio.

**General-purpose Go libraries.**
- Standard library `crypto/x509` for the broker's local CA.
- `golang.org/x/oauth2` for the Device Flow.
- `github.com/spf13/cobra` for CLI structure.

What we still write end-to-end:
1. The GitHub-shaped broker logic (sessions, scope ceilings, roles, two-tier tokens, human prompts).
2. The audit-to-issue writeback and the audit App identity handling.
3. The integration glue between proxy, broker, and GitHub.
4. The session identity primitives (SPIFFE-shaped IDs, delegation cert template).
5. The SLSA Source Track policy schema proposal (paper, not code).

That's a meaningfully smaller surface than starting from zero. The proxy + GitHub App + hardware key trifecta is well-covered by mature OSS. The originality is in the opinionation.

**On reference implementations.** Several products implement similar patterns (Agent Vault, OneCLI, Bromure, NVIDIA OpenShell). We treat these as architectural references for design patterns, not as code we'd directly reuse. Code reuse requires license compatibility; we'd only do it from sources we've checked. Agent Vault's `internal/` packages are explicitly internal in the Go sense and can't be imported even though the repo is Apache 2.0. OneCLI is Rust, language-mismatched. Bromure has no public source repo as of this writing. The libraries above are the actual reuse candidates.

---

## 7. Implementation Roadmap

### 7.1 Phase 0 — PoC (weekend)

**Goal:** demonstrate the credential-helper round-trip with one GitHub App, one role (implement), unsandboxed only.

**Pre-work before starting (~5 minutes if reusing §12 setup; ~15 minutes for fresh):**

If the §12 validation setup is still in place (`rein-validation-a/b` repos, `rein-validation-<suffix>` App with private key still on disk), reuse it. The App's permissions are already a superset of what Phase 0 needs. Just verify:

```
ls ~/rein-validation/app.pem  # should exist
gh api /users/<you>/installation     # confirm App still installed
```

For a fresh setup, create one or two throwaway repos in your GitHub account (private, README-initialized) and a new GitHub App with Contents read/write, Pull requests read/write, Issues read/write, Metadata read-only, Device Flow enabled, webhook disabled. Generate and save the private key locally. Install the App on the repos.

**Development repo can be private.** The `rein` source can live in a private GitHub repo throughout Phase 0/1. Phase 0 doesn't engage the release pipeline (no tags, no signing, no provenance), so repo visibility is irrelevant until Phase 2 (limited community release), at which point you flip to public. Starting private is the right default — it preserves the option to walk away quietly if validation Phase 0/1 surfaces a reason to.

**Implementation scope:**

- Single Go binary; no daemon/helper split.
- No SPIFFE infrastructure; SPIFFE-shaped IDs as strings.
- No commit signing yet; commits unsigned.
- No audit writeback; stderr logs.
- Hardcoded role config.
- Device Flow for App auth.
- TTL hardcoded.
- **Helper always returns a credential (TM-G8). Never empty, never error.**
- **Constraint: only against throwaway repos. README states this in bold.**

**Success criterion:** `rein-poc session start --issue throwaway/test#1 --role implement` followed by `git push` from an unmodified `claude` session succeeds against a minted, scoped, time-limited write token.

Validation §12.1 confirmed Claude Code uses git credential helpers as designed (4 invocations: fetch get/store, push get/store). The architecture for Shape B is viable; Phase 0 should work.

### 7.2 Phase 1 — Personal dogfooding (2–3 weeks)

Real v0:

- Split daemon/proxy/UI.
- **Signing key via `99designs/keyring` abstraction.** Auto-selects the strongest available backend: macOS Keychain on a host Mac, encrypted file backend inside a devcontainer or other environment without OS keyring. Hardware-backed Secure Enclave keys (via `sks`) available on macOS when running a codesigned distribution. Whichever mode is in use is surfaced explicitly in `rein status` output so the user knows.
- Five roles.
- Audit Identity App; audit writeback as issue comments.
- **Broker-as-CA for commit signing** (§4.2.6 v1 default). Bootstrap a local CA at `rein init`; mint delegation certs per session; gitsign signs commits with them.
- **Sandbox composition against `srt`** via the library API confirmed by §12.2. The broker registers as a MITM upstream for github.com / api.github.com.
- Ambient session model with human confirmation prompts.
- Single-use write tokens; HEAD pinning; REST URL 301-redirect chain as TM-G6 anchor.
- Automatic session expiry (idle, hard TTL, agent process exit).
- Audit App ships with a fixed avatar (uploaded during `rein init`).

**Hypothesis:** Tom uses it on `wrangle` for two weeks without reverting to a PAT under deadline pressure.

### 7.3 Phase 2 — Limited community release

(The dev repo should be made public at this stage. Until now it can live private without consequence; community use of a private repo isn't possible.)

- Linux + Windows keychain/TPM backends.
- Codesigning + notarization for macOS distribution (requires Apple Developer ID).
- `source-tool` `allowed_signer_identities[]` upstream discussion.
- Cursor + Codex + Aider tested under their respective sandbox stories.
- Documentation: 5-minute setup; threat model; honest characterization of every defense.
- External audit anchor (Rekor or similar) for tamper-evidence beyond broker compromise.
- Optional: in-toto attestations as queryable audit artifacts (§4.2.7).

### 7.4 Phase 3 — Broader adoption

- Team mode (potentially via Agent Vault composition).
- Approval routing for `release`.
- Full CB4A spec compliance.
- AgentWrit-style sub-agent delegation.
- Begin Sigstore community engagement on the broker-as-OIDC-IdP path (§11.6).

### 7.5 Forbidden in v0

- Running Phase 0 against any real repository.
- Running Phase 0 with `--dangerously-skip-permissions` or equivalent.
- Claiming v1 broker-as-CA signatures are publicly verifiable (they are bounded by broker CA trust).
- Mounting the broker socket inside any sandbox.
- Allowing `release` role without interactive human confirmation.
- Returning an empty/error credential from the helper (TM-G8).

---

## 8. Open Questions

**The public-Sigstore-via-OIDC-IdP path (§11.6) is multi-quarter at best.** Sigstore community is not currently engaged on agent identity; v1 doesn't depend on this.

**Long-running tasks spanning days.** The 4h read TTL is too short. Re-attest interactively at expiry. Annoying.

**Cloud agents.** Cloud workload identity-style deployment of `rein` (e.g., for self-hosted CI runners) is plausible but out of scope for v0/v1. The pattern is the same; the key custody and attestation story differs.

**Multi-device.** Tom runs `rein` on a Brooklyn Mac and a Jefferson Mac. Each broker has its own CA key. SLSA policy uses pattern matching (`spiffe://broker-tomh.*/...`) to unify. Operationally fine; documented in the multi-device setup guide.

**Agent Vault composition for team deployments.** The shape is clear — `rein` does GitHub-specific scoping locally; Agent Vault handles centralized credential vaulting at the team level. Concrete integration design is Phase 3 work.

**SLSA Source L3/L4 two-person review with delegation.** The `counts_as_reviewer: false` flag handles this in the proposed policy, but the conceptual change needs upstream discussion.

**Webhook-driven agents (`@claude` mentions in CI).** Those already run with GitHub Actions OIDC; they don't need this broker. Out of scope.

---

## 9. Recommendations (decision-ready)

1. **Exploratory validation is complete (§12).** Findings documented; design adjusted accordingly. The hard "is the architecture viable" questions are answered: yes for Shape A (sandbox-composed), yes for Shape B (credential-helper with the always-return-a-credential fix), no for the user-identity-annotation Sigstore path in 2026.

2. **Build the Phase 0 PoC this weekend, throwaway repos only.** Success criterion as in §7.1. With validation done, the failure modes are characterized; this should work or fail informatively.

3. **Commit 2–3 weeks to Phase 1 if Phase 0 succeeds.** Sandbox composition via `srt`'s library API, broker-as-CA commit signing, keychain key custody, ambient session model. Threshold to continue: Tom uses `rein` on `wrangle` for two weeks without reverting to a PAT.

4. **Open an upstream discussion thread (not a PR) on `slsa-framework/source-policies`** about how agent identity should work in Source Track. Conceptually new policy primitive; needs SLSA community meeting discussion before any concrete proposal.

5. **For Sigstore: stage the engagement.** Now: drop into Sigstore Slack to ask whether anyone is thinking about agent identity (cheap signal-gathering, no project pitch). After Phase 1: if pursuing Phase 2, take the working PoC to whoever surfaced and ask how to start the broker-as-OIDC-IdP conversation. Don't gate v1 on any of this.

6. **Don't oversell the novelty publicly.** Honest pitch: *"Workload identity for AI coding agents on a developer laptop. The architectural pattern Anthropic uses in Claude Code Cloud and Infisical uses in Agent Vault, applied to GitHub with issue-anchored scope and agent-distinguishable attribution. Open source. Single binary."*

7. **Stop work if any of:**
   - GitHub ships first-party issue-scoped installation tokens.
   - Anthropic ships a built-in local credential broker for Claude Code. **40–60% probability in 12 months.** This is the most likely stop condition.
   - Infisical's Agent Vault pivots to single-binary-on-laptop deployment with GitHub opinionation.
   - AgentWrit relicenses permissively and adds a GitHub backend with proxy integration.

Move fast on the PoC; be intellectually prepared to walk away.

---

## 10. Caveats

- **The 74% and 97% survey figures** come from vendor-sponsored research. Directionally useful, not ground truth.
- **The CB4A draft is informational, not standards-track, at -00.** Compatibility-in-spirit, not strict compliance.
- **`sandbox-runtime` is a research preview** with the explicit disclaimer that APIs may evolve. Two confirmed sandbox bypasses in the last six months (CVE-2025-66479 and the SOCKS5 hostname check bypass) are documented; sandbox is defense-in-depth, not a hard boundary. Plan for breakage.
- **GitHub's installation-token scope today is repo-level, not issue-level.** The broker enforces issue-anchoring above GitHub's scope. A token minted for issue #73 in `wrangle` can technically push to any branch the App is installed on; broker/proxy logic enforces the additional restriction.
- **v1 broker-as-CA commit signatures are bounded by broker CA trust.** Third-party verification requires trusting the user's broker CA via local policy. Public-Sigstore integration is future work without committed timeline.
- **TM-11 (broker bypass) is mitigated only by sandbox composition**, not by the broker alone. Shape B is meaningfully weaker than Shape A.
- **TM-G8 (agent self-remediation) is mitigated by the always-return-a-credential rule for Shape A; partially mitigated for Shape B; not fully solvable in software without sandbox composition.**
- **Fine-grained PAT default lifetime is up to 366 days** at org level; personal can be unlimited.
- **This design does not protect against the LLM provider itself.** Anything the agent sees may be in conversation logs.
- **This design has not been reviewed by an external security expert.** The threat model leans on CB4A's taxonomy (itself an IETF informational draft at -00) and on self-review. Before Phase 2 release, real review paths: Sigstore community Slack; SLSA source-track working group; Trail of Bits paid review; Google's internal supply-chain security group.
- **The SLSA Source Track integration proposes a new policy primitive, not a missing field.** Upstream conversation will be longer and more conceptual than a simple PR review.
- **Anthropic shipping a local credential broker for Claude Code is a real and likely stop condition.** Build with the awareness that 40–60% probability over 12 months means the project may be obviated before it gets traction.
- **Codesigning + notarization for macOS distribution requires Apple Developer ID + notarization in the release pipeline.** Doable; non-trivial; not in scope for v0. Tracked as a follow-up against `wrangle`.

---

## 11. Future Possibilities

These are not v1 features and are not promised. They are things the v1 substrate makes possible that are difficult or impossible with PATs, ghtkn, or any current option. They are listed to show the design choices weren't arbitrary — the same primitives that solve the day-one problem compose into more.

### 11.1 Review agent as a first-class participant

Today a review agent — whether human-triggered (`@claude review this PR`) or autonomous — has the same credential problem as the implement agent. With the substrate in place, a review agent runs with the `review` role (read repo, write PR comments, no push), in a separate sandbox, bound to the same issue as the implement session.

What this enables:
- Cryptographic separation of authorship and review. Implement-agent's commits are signed under one session; review-agent's PR approval under another. SLSA L3/L4 policy can be configured to accept the review-agent's verdict if a human has separately blessed "this review agent counts."
- The review agent reads the implement agent's audit trail. The review becomes "did the agent stay within its declared scope, and does the work match what was authorized?" — not just "do the changes look right."
- The two agents share an issue but not a sandbox or a credential. A compromised implement agent can't taint the review.

Hard to bolt on later if the substrate doesn't already have per-session identity and audit. Straightforward once those exist.

### 11.2 Agent handoff with provable continuity

Session A (implement, by Claude Code) hands work to session B (review, by some other agent). The broker records the handoff in both sessions' audit trails, cryptographically linked. Roughly the AgentWrit delegation pattern but anchored on issues.

### 11.3 Policy-driven scope deny

The broker is a policy decision point. Rules like:
- "No agent session may write to `main` on this repo."
- "No `implement` role on issues labeled `security`."
- "No agent session for issues older than 90 days without re-approval."
- "No agent session for issues authored by users outside the org."

PATs have no equivalent — once you have the PAT, permissions are baked in.

### 11.4 Differential audit of agent vs human activity

Because agent commits carry the delegation extension and audit comments mark agent activity explicitly, you can query: *"of the last 100 commits to `main`, how many were agent-delegated vs human-direct?"* *"Are there commits where the agent's audit comment says it touched files outside the issue's stated scope?"*

Hand-rolled tooling today; structured data with the substrate.

### 11.5 Meaningful `wrangle` integration

- `wrangle`'s CI scans become agent-aware: "this PR contains commits signed by agent session X for issue Y — does the diff match the audit comment's claimed scope?"
- The `source-tool` policy extension (§4.2.8) becomes shared data plane: `wrangle` enforces it in CI; `rein` mints credentials consistent with it locally.

### 11.6 Sigstore integration via broker-as-OIDC-IdP

The architectural target if Sigstore community engagement materializes. The broker becomes an OIDC issuer Fulcio is configured to trust (via the existing CI-provider config mechanism). Tokens it issues carry session metadata as claims; Fulcio's claim-to-extension mapping populates cert extensions; the resulting certs are publicly verifiable against the trusted Sigstore root.

What this would buy: agent-delegated commits become verifiable by anyone who trusts public Sigstore. Open-source maintainers can verify PRs from anyone using `rein` without trusting individual brokers' CAs. SLSA Source Track integration becomes simpler (one trust root for everything, with policy on extensions).

What stands in the way:
- Sigstore community is not currently engaged on agent identity (no issues, no roadmap items as of May 2026).
- Fulcio's CI-provider claim-to-extension mapping can populate existing extension OIDs but cannot add new ones (validation §12.6). Cleanly expressing "agent delegation" semantics may require a new extension OID, which is a coordinated Sigstore TAC PR.
- Multi-quarter timeline even with active shepherding through the Sigstore community.

The honest framing: **outline what integration would look like, don't bet on it, have v1 ship without it.**

A staged engagement plan that avoids the common mistake of pitching standards bodies too early:

- **Now (cheap signal-gathering):** drop into Sigstore community Slack with a low-stakes question. *"Is anyone in the community thinking about agent identity? I'm tracking IETF drafts (CB4A, AIP, WIMSE) but haven't seen Sigstore engagement on this."* No project pitch. The goal is to learn whether there's latent interest, whether a TAC member has been thinking about the question, whether there's existing community context worth knowing. The answer informs everything that comes later.
- **After Phase 1 dogfooding (concrete artifact in hand):** if you decide the project is worth pursuing into Phase 2, return to whoever surfaced from the signal-gathering, with the PoC running locally. Frame as *"here's what I've been running on my laptop; here's what I'd want to make work with public Sigstore; what's the right way to start this conversation?"* A working artifact is a more credible interlocutor than a design doc.
- **Phase 3+ if all of the above goes well:** SIP (Sigstore Improvement Proposal), TAC discussion, eventual Fulcio config + extension OID PR. Multi-quarter timeline even with active shepherding.

A framing that may resonate when the time comes: *"agents are just the first compelling reason to figure out local-developer workload identity in Sigstore."* Sigstore today is heavily CI-focused; local-developer workload identity is largely absent. The agent use case is one specific instance of a broader gap.

### 11.7 In-toto attestations as queryable audit artifacts

Described in §4.2.7. Broker emits an in-toto attestation per session, signed by the broker, logged to Rekor. Compatible with `wrangle` and other supply-chain tools. Audit-layer addition, not auth-layer change.

### 11.8 Composite avatar for the audit App

Polish item. Current v1 ships a fixed image for the audit App's avatar (e.g., a stylized robot/wrench/shield). v2+ could composite the user's existing avatar with a "bot" or "agent" badge overlay — visually relates the audit App to the user (instant recognition: "this is part of my system") while preserving the "automated, not personal" cue.

### 11.9 What's not on this list

- **Agent reputation systems.** Agent identities are per-session and ephemeral; no meaningful reputation primitive at the agent level.
- **Cross-org agent portability.** Org boundaries are real; orgs want their own brokers with their own policies.
- **Insurance / liability primitives.** Speculative; out of qualified scope.
- **Mobile / cloud-hosted agents.** Different deployment shape; out of scope for this design.

---

## 12. Exploratory Work (executed May 23, 2026)

This section preserved as a record of pre-build validation. Findings have been woven into the relevant sections above. Summary:

| Check  | Status            | Confidence | Key finding |
|--------|-------------------|------------|-------------|
| §12.1  | pass              | high       | Claude Code uses credential helpers. **Important secondary:** when the helper returns no credential, claude reflexively runs `gh auth setup-git`, silently displacing the broker. Drove the always-return-a-credential rule (TM-G8). |
| §12.2  | pass              | high       | `srt` exposes a usable library API (`SandboxManager`, `FilterRequestCallback`, `network.mitmProxy.socketPath`). No fork or upstream PR required. |
| §12.3  | pass              | high       | GitHub installation tokens can be scoped to a repo subset and a permission subset via the API. 1-hour TTL. |
| §12.4  | pass              | high       | Device Flow returns `ghu_` (user-to-server) with 8h TTL + 6-month refresh. The broker uses installation-token API for per-mint scope shaping. |
| §12.5  | pass with caveat  | high       | Secure Enclave signs JWTs (ES256) end-to-end. Caveat: unsigned Go binaries can't persist SE keys (errSecMissingEntitlement -34018). Codesigning needed for production distribution. |
| §12.6  | fail              | medium-high | Fulcio's `Extensions` struct is a closed enum. The "user-identity-annotation in public Sigstore" path is not viable in 2026. v1 ships with broker-as-CA; public-Sigstore-via-IdP becomes future work. |
| §12.7  | fail              | high       | GitHub does not preserve internal issue ID across transfers. TM-G6 anchor changed to REST URL 301-redirect chain. |
| §12.8  | review            | medium     | App-posted comments are programmatically distinguishable (`user.type: Bot`, `[bot]` suffix). Visually mediocre without a custom avatar — falls back to user's avatar. Drove the fixed-avatar shipped with `rein` (§2.1). |

Net result: design is buildable. Failures are fail-soft. The §12.6 finding (Sigstore non-viability in 2026) drove the most significant design change — the broker-as-CA inversion. The §12.7 finding required a small TM-G6 substitute. Everything else is woven into the design above.

---

## 13. Phase 0 implementation corrections (added May 25, 2026)

Phase 0 of `rein` is built. The implementation produced seven concrete corrections to the design above; this section lists each correction with a one-line summary and a pointer to the section it amends. The authoritative record is `phase0_findings.md` at the repo root; this section is a navigation aid so a reader of §1-§12 knows which assumptions to update.

| Correction | Affects | Summary |
|---|---|---|
| **C1: Helper can't see smart-protocol endpoint** | §4.2.5 ("Two-tier token issuance") | The credential helper's `path` attribute is the repo URL path (`owner/repo.git` with `useHttpPath=true`), never `/git-upload-pack` or `/git-receive-pack`. Git asks for credentials at the repo level, before deciding fetch vs push. Phase 0's Shape B substitute is a PATH-shim (`cmd/rein-git`) that sets `REIN_GIT_OP=read|write` in env, with a `/proc` ancestor-walk fallback. |
| **C2: Pre-push hook fires too late** | §4.2.5 (intent signal pattern) | The pre-push hook runs AFTER git successfully retrieves remote refs, which already requires a write-capable token. A hook can't be used as the write-intent signal in Shape B. |
| **C3: `credential.useHttpPath=true` is required for strict scope** | §4.2.2 (scope ceilings) | Without it, the helper sees `path=""` and can only fall back to server-side scope enforcement. Phase 0's `rein install-shim` prints the config recommendation; `rein run` sets it per-process. |
| **C4: Per-write prompting is UX-hostile; design §2.2 was always right** | §2.2 (session start ceremony) | PLAN.md CP5's literal "single prompt presented to human for the push" was misread as per-write. Approve-once-per-session with a configurable TTL (Phase 0 default 4h, matching §4.2.2's `default_read_ttl`) is the right pattern and aligns with §2.2. |
| **C5: `/dev/tty` is unavailable inside agent TUIs' subprocesses** | §2.2 (prompt channel), §5.3 TM-G5 | Claude detaches `/dev/tty` from its Bash-tool subprocess (but preserves it for `!` shell escape). Phase 0 builds a layered grant flow: try `/dev/tty` → try tmux popup (if `$TMUX` set) → emit helpful stderr with grant-from-another-terminal instructions. Sandbox composition (Shape A) is the architectural fix; out-of-band status app channel (per §2.2) closes the remaining gap. |
| **C6: GitHub's installation-token mint TTL is fixed at ~1h** | §4.2.5 ("5-minute write TTL") | `POST /app/installations/{id}/access_tokens` does not accept a custom expiration. The 5-minute effective write window is achieved broker-side via `DELETE /installation/token` on the credential-helper's store/erase action (effective TTL = "operation duration + revoke RTT"). |
| **C7: `gh` writes bypass the git credential helper entirely** | §4 (architecture) | `gh` reads `GH_TOKEN` env or `~/.config/gh/hosts.yml`. Without coverage, an agent could TM-G8-displace at the API layer. Phase 0 adds `cmd/rein-gh` (PATH-shim, same pattern as rein-git) which gates `gh` writes through the same `ConfirmWrite` approval flow, with two-tier read-cached / write-JIT minting and revoke-on-gh-exit. On denial, sets `GH_TOKEN=rein-placeholder-denied` to prevent fallback to the user's `hosts.yml`. |

### Shape B limits observed empirically

The design's §5.1 threat table is honest that Shape B is weaker than Shape A. Phase 0 observed specific failure modes:

1. **TM-G5 is bypassable by an agent with shell access.** An agent that reads `dev-session.yaml` knows the issue number; it could run `rein approval grant` itself. Issue #12 (nonce-via-tty) marginally hardens; Shape A sandbox is the architectural fix.
2. **Tokens are reachable from any UID-shared process.** `GH_TOKEN` env, cache files, env-source files — all UID-readable. Phase 1 sandbox closes this.
3. **`/dev/tty` and `$TMUX` propagation are agent-implementation-dependent.** Layered discovery (C5 above) handles known cases.

### Phase 1 questions the corrections raise

- Status-app channel (out-of-band prompt) becomes more important: layer 4 (helpful stderr) is the only reliably-working channel for agent-driven writes in Shape B.
- Token-mint rate-limit handling (transient 401 "Bad credentials" under burst): per-session caching, batched revokes, backoff. Empirical surprise from Phase 0 testing.
- Cross-platform: Phase 0 is Linux-only for the `/proc` fallback in `DetectWrite`. macOS proc-tree is Phase 0.5 (issue #8); Windows is later.

### Reading order from here

For Phase 1 design work: read §1-§12 above as the original design intent, then `phase0_findings.md` for what was actually built and learned, then `PLAN-0.5.md` for the operator-UX work that closes the most visible gaps before Phase 1's architectural work begins. The 15 GitHub issues (#6-#15) capture specific followups by category.
