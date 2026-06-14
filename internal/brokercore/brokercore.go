// Package brokercore is the protocol-independent credential decision core.
//
// It answers one question — "for this github.com request, what credential
// should we hand back?" — applying the scope ceiling, the human write-
// approval, and the read/write token mint, and ALWAYS returning a non-empty
// credential for a github.com get (the TM-G8 invariant, design §5.3).
//
// The core deliberately does NOT classify read-vs-write or speak any wire
// protocol. The CALLER classifies (the rein-git shim signal, the rein-gh
// table, or — in Phase 1 sandboxed mode — the proxy inspecting HTTP
// method/path) and passes the result in Request.WriteIntent. Three adapters
// sit on top of the same core:
//
//   - the git credential-helper (internal/broker, direct mode),
//   - the rein-gh shim (direct mode),
//   - the daemon's injecting proxy (sandboxed mode, Phase 1 CP2+).
//
// This is the seam design.md §4–§5 calls for: the mint/scope/approval brains
// are shared; only delivery and tier-detection differ per adapter.
package brokercore

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
)

// Placeholder credentials returned (never empty) when a github.com get is
// refused or its mint fails. TM-G8: git must receive a credential, not an
// empty block, or downstream agents run `gh auth setup-git` and displace the
// broker (validated finding, design §12.1). The values are stable — adapters
// and tests match on them.
const (
	PlaceholderRefused    = "rein-placeholder-out-of-scope"
	PlaceholderMintFailed = "rein-placeholder-mint-failed"
	CredentialUsername    = "x-access-token"
)

// MintFunc mints a fresh installation token (read or write — whichever this
// MintFunc is). Returned as a function so callers/tests stub trivially.
type MintFunc func(ctx context.Context) (token string, expiresAt time.Time, err error)

// ReadCache is the read-token cache abstraction. Direct mode backs it with a
// file (internal/tokencache); the Phase 1 daemon backs it with memory. Nil
// disables caching (every read mints fresh).
type ReadCache interface {
	// Get returns a cached read token still valid beyond skew, or ok=false.
	Get(skew time.Duration) (token string, ok bool)
	// Put stores a freshly minted read token.
	Put(token string, expiresAt time.Time)
}

// Credential is what an adapter hands back to its client. Username/Password
// for the git credential-helper; the proxy uses Password as the bearer/basic
// secret. Never empty on the github.com path.
type Credential struct {
	Username string
	Password string
}

// Request is one normalized credential request. The caller has already
// resolved the host, the target repo, and the read/write classification.
type Request struct {
	// Repo is the requested "owner/name" (derived from the credential-helper
	// path or the proxy URL). Empty when the caller couldn't determine it
	// (e.g. git without useHttpPath); EmptyPathScope then governs.
	Repo string
	// WriteIntent is the caller's read/write classification. The core does
	// NOT second-guess it.
	WriteIntent bool
}

// Core holds the decision dependencies. Build one per adapter (or once, in
// the daemon) and call Serve per request. Logger is required; everything
// else is optional and degrades safely.
type Core struct {
	MintRead    MintFunc
	MintWrite   MintFunc
	MintTimeout time.Duration

	ReadCache     ReadCache
	ReadCacheSkew time.Duration

	// InScope reports whether a repo is within the session's ceiling. Nil
	// disables scope enforcement (every github.com request allowed).
	InScope func(repo string) bool
	// EmptyPathScope governs an empty Request.Repo: "refuse" or "" / "allow".
	EmptyPathScope string

	// ConfirmWrite is the human-in-the-loop hook, called before minting a
	// write token. Returns true to proceed. Nil = no confirmation.
	ConfirmWrite func(repo string) bool
	// RecordWrite, if set, is called with each successfully-minted write
	// token (issue #20 ledger). Best-effort; panics recovered.
	RecordWrite func(token string, expiresAt time.Time)

	Logger *log.Logger
	// Diag receives a short user-facing line when a mint fails (git surfaces
	// helper stderr). Nil discards.
	Diag io.Writer
}

// Serve runs the decision pipeline for one github.com get and returns a
// non-empty Credential (TM-G8). The caller is responsible for only invoking
// Serve on in-handler hosts (github.com over https); non-github hosts get an
// empty block from the adapter, not from here.
func (c Core) Serve(ctx context.Context, req Request) Credential {
	if !c.inScope(req) {
		return Credential{CredentialUsername, PlaceholderRefused}
	}
	if req.WriteIntent {
		if !c.confirmWrite(req.Repo) {
			return Credential{CredentialUsername, PlaceholderRefused}
		}
		return c.serveWrite(ctx)
	}
	return c.serveRead(ctx)
}

func (c Core) inScope(req Request) bool {
	if c.InScope == nil {
		return true
	}
	if req.Repo == "" {
		if c.EmptyPathScope == "refuse" {
			c.logf("scope check: repo unknown (set credential.useHttpPath=true); EmptyPathScope=refuse; returning placeholder")
			return false
		}
		c.logf("scope check: repo unknown (set credential.useHttpPath=true for strict scope-check); allowing — token's repo scope still enforces server-side")
		return true
	}
	if c.InScope(req.Repo) {
		return true
	}
	c.logf("scope check: REFUSED repo=%q (not in session's scope ceiling); returning placeholder", req.Repo)
	return false
}

// confirmWrite invokes ConfirmWrite (if set). A panic is recovered and
// treated as denial — TM-G8 must not be undermined by a buggy prompter.
func (c Core) confirmWrite(repo string) (approved bool) {
	if c.ConfirmWrite == nil {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			c.logf("ConfirmWrite panicked: %v; denying", r)
			approved = false
		}
	}()
	approved = c.ConfirmWrite(repo)
	if approved {
		c.logf("ConfirmWrite: APPROVED for repo=%q", repo)
	} else {
		c.logf("ConfirmWrite: DENIED for repo=%q; returning placeholder", repo)
	}
	return approved
}

func (c Core) serveWrite(ctx context.Context) Credential {
	ctx, cancel := context.WithTimeout(ctx, c.MintTimeout)
	defer cancel()
	token, expiresAt, err := c.MintWrite(ctx)
	if err != nil {
		c.logf("write mint failed: %v; returning placeholder credential", err)
		c.diag()
		return Credential{CredentialUsername, PlaceholderMintFailed}
	}
	c.logf("write mint succeeded: tier=write expires_at=%s ttl=%s token_len=%d",
		expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second), len(token))
	c.recordWrite(token, expiresAt)
	return Credential{CredentialUsername, token}
}

func (c Core) serveRead(ctx context.Context) Credential {
	if c.ReadCache != nil {
		if tok, ok := c.ReadCache.Get(c.ReadCacheSkew); ok {
			c.logf("read cache hit: token_len=%d", len(tok))
			return Credential{CredentialUsername, tok}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.MintTimeout)
	defer cancel()
	token, expiresAt, err := c.MintRead(ctx)
	if err != nil {
		c.logf("read mint failed: %v; returning placeholder credential", err)
		c.diag()
		return Credential{CredentialUsername, PlaceholderMintFailed}
	}
	c.logf("read mint succeeded: tier=read expires_at=%s ttl=%s token_len=%d",
		expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second), len(token))
	if c.ReadCache != nil {
		c.ReadCache.Put(token, expiresAt)
	}
	return Credential{CredentialUsername, token}
}

// recordWrite invokes the RecordWrite hook (issue #20), panic-recovered — a
// buggy/unwritable ledger must never stop the token reaching the client.
func (c Core) recordWrite(token string, expiresAt time.Time) {
	if c.RecordWrite == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.logf("RecordWrite panicked: %v; ignoring (token still served)", r)
		}
	}()
	c.RecordWrite(token, expiresAt)
}

func (c Core) diag() {
	if c.Diag == nil {
		return
	}
	fmt.Fprintln(c.Diag, "rein: no usable GitHub token (mint failed) — this git operation will fail.")
	fmt.Fprintln(c.Diag, "      Run `rein doctor` to diagnose (e.g. the App isn't installed on this repo, or rein isn't set up).")
}

func (c Core) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
	}
}

// RepoFromPath extracts "owner/repo" from a credential-helper path attribute
// (or a proxy URL path): strips a leading slash, a trailing slash, and a
// ".git" suffix, then takes the first two segments. Returns "" if the path
// isn't owner/repo-shaped.
func RepoFromPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	if len(path) >= 4 && strings.EqualFold(path[len(path)-4:], ".git") {
		path = path[:len(path)-4]
	}
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
