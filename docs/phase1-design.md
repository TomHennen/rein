# Phase 1 design — credentials the agent can't read

**Status:** DRAFT for review (2026-06-08). Opens Phase 1.

This document is self-contained: it states the problem and requirements
first, then the design. It references the integration spike
(`docs/phase1-srt-spike-findings.md`) for evidence but does not assume you
have read it.

## 1. The problem

Today rein hands the agent a real GitHub token for each operation — as a
git credential helper, and via a `gh` wrapper. The token is short-lived
and scoped to one repo, but it is a **real, usable token that the agent's
own process can read**: from its environment, from rein's on-disk token
cache, or from its own memory.

The per-issue scoping and the human-approval-for-writes that Phase 0/0.5
built limit what a leaked token can do. They do not stop an agent that
simply reads the token and uses it directly, or copies it somewhere we
never see. We assume a prompt-injected agent may be adversarial, so "the
agent can read its own credential" is the gap that matters. (This is
issue #7.)

## 2. Requirements

What must be true, stated independently of how we build it.

**Functional**
- The agent can clone, fetch, and push to repos in its session's scope,
  and use `gh`, with no token handling by the developer.
- Access is scoped per issue/session (as today).
- Write operations require human confirmation (as today).

**Security**
- **The agent's process can never read a usable GitHub credential** — not
  in its environment, not in a file, not in its own memory. This is the
  headline requirement (the #7 gap). Note it is a property of the agent's
  reach, not a particular mechanism.
- The agent also cannot reach the developer's **ambient** credentials —
  the developer's own `gh` login, SSH keys, `~/.netrc`, stored git
  credentials. Hiding rein's token is pointless if the agent can grab the
  human's instead.
- Write authorization stays human-confirmed and non-replayable (a
  prompt-injected agent can't pre-answer the prompt).
- **Fail closed:** if we cannot satisfy the above, refuse the operation —
  never silently fall back to handing the agent a token.

**Operational**
- Latency on the git path stays tolerable; the agent hits it constantly.
- Linux and macOS.
- If the protection mechanism is unavailable, degrade **loudly and
  safely** — warn, and never silently give the agent a token on a real
  repo.

**Non-goals for this design** (separate later tracks, §8): commit signing,
posting audit comments back to issues, the full five-role permission
catalog, single-use / HEAD-pinned write tokens.

## 3. Approach

Run the agent inside a **sandbox with no direct network access**. Every
request it makes to GitHub goes through a **local proxy that rein
controls**. The agent sends ordinary git/`gh` requests carrying *no*
credentials; rein's proxy adds the credential on the wire, at the last hop,
just before the request leaves for GitHub. The agent only ever sees its own
un-authenticated requests — the token exists only inside rein.

This satisfies the headline requirement directly: the credential is added
inside rein's process, *outside* the sandbox, so nothing the agent can read
ever contains it. The sandbox is the *mechanism*; the property we are after
is "the agent never holds a readable credential."

We will call this the **sandboxed mode**. The credential-helper path that
Phase 0/0.5 already ship becomes the **direct mode** — retained as a
clearly-marked fallback (for environments without a sandbox), never the
default once sandboxed mode works.

> Naming: earlier design docs call these "Shape A" (sandboxed) and "Shape
> B" (direct). That labeling tested poorly in review; this doc uses the
> plain terms. A repo-wide rename (design.md, code comments) is tracked
> separately so it doesn't bloat this design — see the follow-up issue.

The sandbox itself is Anthropic's `sandbox-runtime` (`srt`) — the same
sandbox Claude Code already ships with — so we compose with an existing,
maintained tool rather than building one.

## 4. How it works

Three pieces: a resident broker, an injecting proxy, and the sandbox.

**The broker daemon.** A long-running local rein process owned by your user
account. It holds the GitHub App credentials, the session table, the
token-minting + scope-ceiling + approval logic (all carried over from
today's code), and an audit log. Tokens live in its memory and are never
written to disk.

**The injecting proxy** (part of the daemon). The sandbox routes the
agent's GitHub-bound traffic to it. For each request the proxy decides
which token applies (read vs. write tier, in-scope or not, approved or
needs a prompt), adds the credential, and forwards the request to GitHub.
The agent's side of the connection never carries a credential.

**The sandbox.** `srt` runs the agent with its network namespace removed,
so the agent's only route out is the proxy. `rein run` launches it with a
generated, per-run configuration: where to send traffic, which files to
hide (below), and a certificate to trust (so the proxy can read and re-sign
the agent's HTTPS in order to add the credential).

When `git push` traverses the proxy, the proxy is also the natural place to
add the stronger write protections in §8 (single-use, branch-pinned
tokens) later — it sees the actual push, which the direct mode never could.

### 4.1 Hiding the developer's own credentials (filesystem)

Sandboxing the *network* is not enough. By default the sandbox can still
**read** the developer's home directory — so the agent could read the
developer's own `gh` login or SSH keys and use (or copy) those instead of
rein's brokered token. The spike confirmed this concretely: `gh` running
inside the sandbox silently picked up the host's stored login and
authenticated with it.

So the sandbox configuration must **deny read** to the ambient credential
stores: `~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.ssh`, and
rein's own key material. The safe default is to deny the home directory
broadly and re-allow only what the agent genuinely needs (its working
tree), so a credential store we didn't think of doesn't leak by default.

### 4.2 `srt` specifics (implementation detail; evidence in the spike)

You don't need this subsection to follow the design; it records *why* the
mechanism looks the way it does. All of it was validated empirically in
`docs/phase1-srt-spike-findings.md`.

- `srt` exposes exactly one way to let an external process modify the
  agent's requests: it forwards matched-domain traffic to a unix socket
  that rein owns. rein terminates TLS at that socket with its own
  certificate, adds the credential to the decrypted request, and
  re-encrypts to GitHub. (`srt`'s other hooks can only allow/deny a
  request, not modify it.)
- rein's certificate must be trusted *inside* the sandbox, delivered via
  environment variables (e.g. `GIT_SSL_CAINFO`, `SSL_CERT_FILE`); `srt`
  does not do this automatically on this path.
- `gh` will not send a request unless it believes it is logged in, so the
  sandbox sets a harmless stub `GH_TOKEN`; the proxy overwrites it with the
  real credential.
- On Ubuntu 24.04+, `srt` needs an AppArmor profile granting user
  namespaces to `bwrap`, or it won't start. rein's setup must detect this
  and guide the fix (or sandboxed mode silently fails to launch).

## 5. Key design decisions & open questions

The items most worth review before implementation.

### 5.1 Telling reads from writes is a real classifier, not a free signal

The proxy must know whether a request is a read (serve a cached read token)
or a write (mint a write token and require approval). This is better than
the direct mode's process-tree guessing, but it is **not** simply "POST =
write":
- **git:** the write signal is the `git-receive-pack` service, not the HTTP
  method — `git fetch` also POSTs (to `git-upload-pack`). Key on the
  service/path.
- **GitHub REST API:** the method works (GET/HEAD read; POST/PATCH/PUT/
  DELETE write).
- **GitHub GraphQL API:** always `POST /graphql`; query-vs-mutation lives
  only in the request body, and `gh` uses GraphQL heavily. The proxy can
  inspect the body (it has the plaintext), but that is a classifier to
  build and test. This is also where the direct mode's `gh`-classifier
  (issue #9) must move, now that the `gh` PATH-shim is gone.
- **Fail closed:** anything unclassifiable is treated as a write (prompt),
  not silently served.

### 5.2 One proxy socket per run = the session's identity

A connection arriving at the proxy carries no run/session id of its own.
Phase 0.5 supports concurrent `rein run`s with independent scope and
approval, so the daemon must know which session an incoming request belongs
to. Resolution: **each `rein run` gets its own proxy socket path**, baked
into that run's sandbox configuration; the daemon maps socket → session.

### 5.3 The proxy socket is itself a capability

Because the socket *is* the identity, anything that can connect to it gets
authenticated GitHub access at that session's scope — **even without ever
seeing the token value**. The token is hidden (good — requirement met), but
the *capability* to use it is reachable to whatever can open the socket.
This is bounded, not eliminated, by: the per-run socket (§5.2), `0700`
permissions, and run-lifetime teardown. The sandbox is what keeps non-agent
processes off it. On a shared-user machine, a process *outside* the sandbox
running as the same user could still reach it — the residual of #7 that the
sandbox, not the broker, addresses. Worth stating plainly rather than
implying the token-hiding closes #7 completely.

### 5.4 The proxy's certificate authority

To read and re-sign the agent's HTTPS, rein needs a certificate authority
the sandbox trusts. rein generates one locally; its **private key is key
material and is stored/read through `internal/keystore`** (the same
uid+mode-checked path as App keys — CLAUDE.md hard-constraint #6), never on
the direct mode's plaintext path. Scope the risk: the proxy only intercepts
`github.com` / `api.github.com`, and the certificate is trusted only inside
the sandbox process, not the host trust store. So a compromise of this key
means "can intercept the agent's GitHub traffic," not "can intercept the
host's."

## 6. What we reuse vs. build new

The security win is entirely in *how the credential is delivered and
stored*. The minting, scoping, and approval logic already exist and carry
over unchanged.

| Concern | Today (direct mode) | Phase 1 (sandboxed mode) |
|---|---|---|
| Mint read/write tokens | `githubapp.Client` | **reused** in the daemon |
| Read vs. write signal | git PATH-shim + proc-tree guess | request inspected at the proxy (§5.1) |
| Scope ceiling | `sess.Contains` | **reused** |
| Human approval | run-scoped approval flow | **reused**, daemon-dispatched |
| Token delivery | handed to git/`gh` (agent can read it) | **added at the proxy** (agent never sees it) |
| Token storage | on-disk cache + write-token ledger | **in daemon memory** (no disk) |
| Audit | `helper.log` | hash-chained audit log |

## 7. Risks & limits

- **The sandbox is defense-in-depth, not a hard boundary.** `srt` had two
  sandbox-escape fixes in the last six months. A sandbox escape re-exposes
  the direct mode's surface. We treat it as one layer, not a guarantee.
- **`git push` through the proxy is unproven.** Everything tested so far is
  the read path; a push streams a request body, which the proxy's
  read-modify-forward loop has not yet handled. The plan validates this
  first (PLAN-1 CP1) before building the daemon around it.
- **`srt` API churn:** the request-routing hooks this relies on are not in
  `srt`'s public docs (typed but unexampled). Pin the version; re-verify on
  upgrade.
- **macOS parity:** `srt` uses a different backend there (`sandbox-exec`);
  to be validated separately.
- **Latency:** terminating and re-encrypting TLS per request adds overhead
  on the git hot path; measure it.

## 8. Out of scope for this design (later tracks)

These layer on the sandboxed-mode spine once it holds, each tracked
separately: single-use + branch-pinned write tokens (now reachable because
the proxy sees the push); broker-as-CA commit signing; audit-comment
writeback via the audit App; the five-role permission catalog; a status-app
/ OS-notification approval channel; Claude Code hooks as a complementary
guard (#21).
