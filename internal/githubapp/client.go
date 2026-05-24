// Package githubapp wraps jferrl/go-githubauth with the surface rein needs:
// minting installation tokens scoped to a single repository with a fixed
// permission set.
//
// In Phase 0 this is just enough to back the credential helper. Caching,
// two-tier read/write splits, and JIT write minting come in later checkpoints.
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
		Permissions: &githubauth.InstallationPermissions{
			Contents: githubauth.Ptr("read"),
			Metadata: githubauth.Ptr("read"),
		},
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
