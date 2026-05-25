// Package githubapp wraps jferrl/go-githubauth with the surface rein needs:
// minting installation tokens scoped to a single repository, in either a
// read-only or read+write permission shape.
//
// MintReadOnlyToken is used for cached session reads (TTL bounded by what
// GitHub returns, which is 1h in practice). MintWriteToken is used JIT at
// push time — never cached — per design §4.2.5. Both tokens are scoped to
// the same single repository.
package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/jferrl/go-githubauth"
)

// Config holds the inputs needed to mint an installation token. All fields
// are required.
type Config struct {
	// ClientID is the GitHub App's Client ID (e.g. "Iv23li..."). The App ID
	// (numeric) would also work — go-githubauth accepts either — but design
	// §4.2.4 recommends Client ID for new apps.
	ClientID string

	// PrivateKeyPath is the path to the PEM-encoded RSA private key
	// associated with the App.
	PrivateKeyPath string

	// InstallationID is the numeric installation ID this client mints for.
	InstallationID int64

	// RepoName is the repository name (without owner) the minted token will
	// be scoped to. The owner is implicit in the installation.
	RepoName string
}

// Client mints installation tokens for a single repository.
type Client struct {
	cfg Config
}

// NewClient validates cfg and constructs a Client. The private key is read
// and parsed lazily inside MintReadOnlyToken so the constructor never holds
// key material; this matches Phase 1's intent of moving the key into a
// keyring/HSM-backed signer without changing this signature.
func NewClient(cfg Config) (*Client, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("github app: ClientID is required")
	}
	if cfg.PrivateKeyPath == "" {
		return nil, errors.New("github app: PrivateKeyPath is required")
	}
	if cfg.InstallationID == 0 {
		return nil, errors.New("github app: InstallationID is required")
	}
	if cfg.RepoName == "" {
		return nil, errors.New("github app: RepoName is required")
	}
	return &Client{cfg: cfg}, nil
}

// MintReadOnlyToken returns a fresh installation access token scoped to
// the configured repository with permissions {contents:read, metadata:read}.
// It honors ctx for cancellation and timeout.
func (c *Client) MintReadOnlyToken(ctx context.Context) (token string, expiresAt time.Time, err error) {
	return c.mint(ctx, &githubauth.InstallationPermissions{
		Contents: githubauth.Ptr("read"),
		Metadata: githubauth.Ptr("read"),
	})
}

// MintWriteToken returns a fresh installation access token scoped to the
// configured repository with permissions {contents:write, metadata:read}.
// The design specifies a 5-minute TTL on write tokens (§4.2.5); the GitHub
// installation-token API does not currently accept a custom expiration on
// the create endpoint (it always returns 1h), so the effective TTL is
// whatever GitHub returns. Single-use semantics are enforced by the
// broker (never cache write tokens; mint per push).
func (c *Client) MintWriteToken(ctx context.Context) (token string, expiresAt time.Time, err error) {
	return c.mint(ctx, &githubauth.InstallationPermissions{
		Contents: githubauth.Ptr("write"),
		Metadata: githubauth.Ptr("read"),
	})
}

// MintGhSessionToken returns an installation token shaped for the design's
// `implement` role (§4.2.2): contents:write + issues:write +
// pull_requests:write + metadata:read. Used to back the `gh` CLI session.
//
// Permissioning rationale: the token is single-repo-scoped (one repo today,
// session-scoped in CP4+), so the blast radius of contents:write here is
// the same as that of a successful git push to the same repo — they're
// the same logical capability surface. A narrower grant (contents:read
// only) was tried briefly but broke `gh pr merge`, which is necessary for
// any workflow that uses branch-protection rules requiring PR-only merges
// into the protected branch. The defense-in-depth from the narrower grant
// was small in Shape B (an agent that can capture a git write token has
// equivalent capability anyway).
//
// Surface to call out for Shape A reviewers: pull_requests:write also
// confers PR review/approve capability. Branch-protection rules that
// require N approvals from non-authors treat the App's bot identity as
// a valid approver path; a session that legitimately holds this token
// could approve its own PR via gh. CP4+ should pair this with the
// "agent-delegated commits don't count as second signer" property
// proposed in design §4.2.8.
//
// 5-minute TTL on this token (per design §4.2.5) is not enforceable at the
// GitHub API layer — installation tokens always return ~1h. CP3.6's
// shim revokes the previous token whenever it mints a fresh one, which
// approximates the 5m effective window for active sessions.
func (c *Client) MintGhSessionToken(ctx context.Context) (token string, expiresAt time.Time, err error) {
	return c.mint(ctx, &githubauth.InstallationPermissions{
		Contents:     githubauth.Ptr("write"),
		Issues:       githubauth.Ptr("write"),
		PullRequests: githubauth.Ptr("write"),
		Metadata:     githubauth.Ptr("read"),
	})
}

// RevokeToken calls DELETE /installation/token authenticated as the supplied
// installation token, which revokes that exact token server-side. Used to
// tighten the effective write-token TTL — design §4.2.5 asks for ~5min;
// GitHub returns 1h; revoking after the operation is done approximates the
// 5-min target.
//
// This is a Client method only by convention (matches NewClient/Mint*);
// the call doesn't actually need any of Client's config because the token
// itself authenticates the request. The 5s timeout is honored via ctx.
//
// Best-effort: callers should log + ignore the error. A failed revoke is
// not a security problem (token still expires at its native ~1h).
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://api.github.com/installation/token", nil)
	if err != nil {
		return fmt.Errorf("build revoke request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("revoke: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) mint(ctx context.Context, perms *githubauth.InstallationPermissions) (string, time.Time, error) {
	keyPEM, err := os.ReadFile(c.cfg.PrivateKeyPath)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read private key: %w", err)
	}

	appSrc, err := githubauth.NewApplicationTokenSource(c.cfg.ClientID, keyPEM)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build app token source: %w", err)
	}

	opts := &githubauth.InstallationTokenOptions{
		Repositories: []string{c.cfg.RepoName},
		Permissions:  perms,
	}

	instSrc := githubauth.NewInstallationTokenSource(
		c.cfg.InstallationID,
		appSrc,
		githubauth.WithInstallationTokenOptions(opts),
		githubauth.WithContext(ctx),
	)

	tok, err := instSrc.Token()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint installation token: %w", err)
	}
	return tok.AccessToken, tok.Expiry, nil
}
