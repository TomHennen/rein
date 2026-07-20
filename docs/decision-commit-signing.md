# Decision: commit signing — deferred, sigstore is the marked direction

**Status:** Decided (punt). Captures a signing investigation so it isn't re-derived from scratch.

**TL;DR:** We want signed commits to carry provenance — which issue/repo an agent worked, and
that it *was* an agent. We looked at how to do that and at whether GitHub's "Verified" badge is
reachable for rein's identity model. It mostly isn't, and the one path that reaches it
(`createCommitOnBranch`) rewrites the push path. **We are deferring commit signing.** When we
pick it up, **sigstore/keyless is the marked direction** because it's the only option that stays
true to rein's ephemeral-credential thesis — with the caveat that putting the agent identity
*in the signature* needs our own sigstore infra.

---

## Why we care

A rein-brokered commit ideally attests, tamper-evidently: **which issue/repo** it belongs to and
that **an agent produced it** under rein's policy. The signature already covers the whole commit
payload (tree, author/committer, full message), so anything we bind *into* the commit — a
trailer, the author identity — is cryptographically protected. The open questions were *where*
the provenance should live and *whose* trust root verifies it.

## What a signed commit actually is

Signing adds one header, `gpgsig`, holding the armored signature; it covers everything except that
header. Verified empirically (SSH-signed throwaway commit): flipping one character in a
`Rein-Issue:` trailer invalidates the signature. So a signed trailer is tamper-evident. The
metadata "slots":

| Slot | Bound by sig? | Notes |
|---|---|---|
| Commit **trailers** (`Rein-Issue:` …) | Yes | Human-readable, portable, no schema — only meaningful if the *key* is one rein vouches for |
| GPG **notation** subpackets (`k@domain=v`) | Yes | Real key=value slot *inside* the signature; needs a `gpg.program` shim; lost on re-sign |
| **SSH** signature internals | — | `namespace` hardcoded to `git`, `reserved` empty and unexposed — **no room** |
| Signing **key identity** (SSH comment / GPG UID / X.509 subject) | Yes (it's the signer) | The strongest lever: an ephemeral per-issue key *is* the attestation |

## The GitHub-badge problem

GitHub's "Verified" badge is built around **(key registered to a user account)** or
**(GitHub signed it server-side)**. rein's natural shape — a GitHub **App** minting per-issue
*installation* tokens — fits neither:

- Signing-key registration (`POST /user/ssh_signing_keys`, `/user/gpg_keys`) is a **user**
  endpoint. An installation token is not a user; rein-the-App has no account to hang keys on.
- The committer email must also match a *verified* account email, so a label like
  `agent+issue12@rein.local` gets a nice local identity but zero GitHub trust.
- **sigstore/gitsign** commits show **Unverified** on GitHub — Fulcio's roots aren't in the CA
  set GitHub trusts, and its short-lived certs don't fit GitHub's model. (Confirmed still open.)

### The one native-badge path we rejected

`createCommitOnBranch` (GraphQL) with **installation** credentials makes GitHub sign the commit
server-side and mark it Verified, attributed to the rein bot. But it does **not** sign a branch
you pushed — content is sent **inline as base64 `fileChanges`**, GitHub recomputes the tree and
makes a *fresh* commit (new SHA, local/remote history diverges), and you lose author/committer
control. **Rejected:** too breaking a change to the push path for a badge we don't need.

## Options considered

| Option | GitHub Verified? | Agent+issue provenance | Fits rein's local-agent model? |
|---|---|---|---|
| `createCommitOnBranch` | Yes, native | Trailer only (GH-signed) | **No** — server-side commit formation |
| Client-side ephemeral key | No (no account to register to) | Key identity + trailers, **rein-verified** | Yes |
| Client key → bot user account | Yes, as one bot | Lost (collapses to bot) | Awkward (key churn) |
| Presence-gated **human** key (Secure Enclave / FIDO2 / smartcard) | Yes (human's registered key) | Trailer (+ optional rein countersign) | Yes |
| **sigstore / keyless** | No (already accepted) | Cert identity *if* private infra; else human-OIDC + trailer | Yes |

## Two ways rein can inject/enforce provenance (independent of trust root)

Signing splits into two interception points, and they need each other:

1. **Augment early** — inject the trailers *before* the commit object exists (a
   `prepare-commit-msg` hook or a git-command wrapper; a separate workstream is exploring the
   wrapper). A signer **cannot** rewrite the message — its signature must cover the exact bytes
   git hands it — so augmentation can't happen at sign time.
2. **Enforce late** — if rein is the signer, it sees the final bytes and can **refuse** to sign a
   commit whose trailers are missing/malformed. Injection makes trailers *present*; the sign-time
   gate makes them *unbypassable*.

## Decision

**Defer commit signing.** It's additive, nothing in the current spine depends on it, and it
paints no architectural corner to leave for later.

**Marked direction when we pick it up: sigstore / keyless.** Rationale: keyless (ephemeral cert
per signing, nothing durable on the box) mirrors rein's ephemeral-scoped-credential thesis almost
exactly, and we've already accepted that GitHub's badge is not the target — which dissolves the
App-registration and resident-key problems entirely.

**The caveat that gates it:** "agent+issue in the *signature identity*" is the expensive part.

- **Public Fulcio** only issues certs whose subject comes from a trusted OIDC issuer
  (Google/GitHub/…), so the identity would be the *human's* login and provenance falls back to a
  trailer — not much better than the presence-gated-human-key option already on the table. It's
  also a browser-OIDC round trip per sign, and **public Rekor is world-readable** (leaks signer
  email + commit hashes for private work).
- Putting `agent/issue` in the cert subject needs a **private Fulcio + Rekor** — a real
  operational lift, not a weekend adopt.

So sigstore is the right *shape*, but the property we actually want (agent identity in the
signature) is not free in it. Revisit when signing rises in priority; decide public-vs-private
sigstore then, with that cost in view.

## Not doing

- `createCommitOnBranch` / any server-side commit formation.
- Registering rein-minted keys with GitHub (no App path; bot-account churn rejected).
- Chasing GitHub's "Verified" badge as a requirement.
