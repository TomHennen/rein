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

// MintGhSessionToken returns an installation token shaped for the `gh`
// CLI session used in Phase 0 (CP3.5): contents:read + issues:write +
// pull_requests:write + metadata:read. It deliberately omits contents:write
// because git push already routes through the credential helper's
// dedicated write-tier mint — granting it here too would over-broaden
// what `gh api` and similar low-level subcommands could do.
//
// Known limitation of the narrower grant: `gh pr merge`, `gh release
// create`, and `gh repo edit` need contents:write and will 403 with this
// token. Phase 0 agents should perform merges via local git push instead.
// Phase 1's sandbox+proxy will discriminate per-HTTP-call at the network
// boundary and remove this restriction.
func (c *Client) MintGhSessionToken(ctx context.Context) (token string, expiresAt time.Time, err error) {
	return c.mint(ctx, &githubauth.InstallationPermissions{
		Contents:     githubauth.Ptr("read"),
		Issues:       githubauth.Ptr("write"),
		PullRequests: githubauth.Ptr("write"),
		Metadata:     githubauth.Ptr("read"),
	})
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
