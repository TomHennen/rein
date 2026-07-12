// Package githubapp wraps jferrl/go-githubauth with the surface rein needs:
// minting installation tokens scoped to the session's repository set, in
// either a read-only or read+write permission shape.
//
// MintReadOnlyToken is used for cached session reads (TTL bounded by what
// GitHub returns, which is 1h in practice). MintWriteToken is used JIT at
// push time — never cached — per design §4.2.5. Both tokens are scoped to
// the same set of repositories (Config.RepoNames = the session's scope
// ceiling; issue #10).
package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jferrl/go-githubauth"

	"github.com/TomHennen/rein/internal/keystore"
)

// Config holds the non-key inputs needed to mint an installation token.
// The PEM is supplied separately via a Keystore — see NewClient.
type Config struct {
	// ClientID is the GitHub App's Client ID (e.g. "Iv23li..."). The App ID
	// (numeric) would also work — go-githubauth accepts either — but design
	// §4.2.4 recommends Client ID for new apps.
	ClientID string

	// InstallationID is the numeric installation ID this client mints for.
	InstallationID int64

	// RepoNames are the repository names (without owner) the minted token is
	// scoped to. The owner is implicit in the installation. This is the FULL
	// session scope ceiling — every repo the session's Contains check accepts
	// (issue #10: previously the mint was scoped to a single repo while the
	// scope check accepted all session repos, so a token minted for one repo
	// was handed to a request the session allowed for another repo, which then
	// 403'd at GitHub). Scoping the token to the whole set closes that gap:
	// token scope == scope ceiling.
	RepoNames []string
}

// Client mints installation tokens scoped to the session's repository set
// (Config.RepoNames).
//
// The private key is fetched from the Keystore lazily inside each Mint*
// call so the Client never holds key material across calls. This is the
// swap point for Phase 1's daemon-backed cache and Phase 1/2's
// biometric-gated backend — neither requires a signature change here.
type Client struct {
	cfg      Config
	ks       keystore.Keystore
	roleName string

	// httpClient is the transport used for direct REST calls (RevokeToken).
	// When nil, client() defaults to http.DefaultClient, keeping the zero
	// value and existing constructors working. Tests set it to point at an
	// httptest.Server transport.
	httpClient *http.Client

	// apiBaseURL, when non-empty, overrides the GitHub API base URL for the
	// MINT path (threaded to githubauth.WithEnterpriseURL) and the REVOKE
	// path. It is a TESTABILITY SEAM ONLY: unexported, settable solely from
	// within this package, and no production constructor ever sets it — when
	// empty (always, in production) both paths are byte-for-byte the previous
	// behavior against https://api.github.com. It exists so a unit test can
	// point the mint at a local httptest server and assert the
	// installation-token request body carries the scope ceiling
	// (Repositories + Permissions) — the invariant a regression dropping
	// `opts` below would silently break (conformance audit #44 §2).
	//
	// Revoke honors it too so the two paths cannot drift apart: a revoke that
	// kept targeting api.github.com while the mint went elsewhere would get a
	// 404 from the wrong host and — since RevokeToken maps 404 to
	// "already gone" — report SUCCESS for a token that is still live.
	apiBaseURL string
}

// apiURL joins path onto the API base (the apiBaseURL test seam when set,
// https://api.github.com otherwise).
func (c *Client) apiURL(path string) string {
	base := c.apiBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	return strings.TrimSuffix(base, "/") + path
}

// client returns the injectable HTTP client, defaulting to
// http.DefaultClient when unset.
func (c *Client) client() *http.Client {
	if c.httpClient == nil {
		return http.DefaultClient
	}
	return c.httpClient
}

// NewClient validates cfg and constructs a Client. ks is the backend the
// PEM is read from at mint time; roleName is the entry name passed to
// ks.Get (typically "primary"). Both are required.
func NewClient(cfg Config, ks keystore.Keystore, roleName string) (*Client, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("github app: ClientID is required")
	}
	if cfg.InstallationID == 0 {
		return nil, errors.New("github app: InstallationID is required")
	}
	if len(cfg.RepoNames) == 0 {
		return nil, errors.New("github app: RepoNames is required (at least one repo)")
	}
	if ks == nil {
		return nil, errors.New("github app: keystore is required")
	}
	if roleName == "" {
		return nil, errors.New("github app: roleName is required")
	}
	return &Client{cfg: cfg, ks: ks, roleName: roleName}, nil
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

// MintGhReadOnlyToken returns a read-only installation token shaped for
// gh read subcommands (gh repo view, gh issue list, gh pr view, etc.):
// contents:read + issues:read + pull_requests:read + metadata:read.
//
// This is the "long-lived cacheable" tier of the gh two-tier model
// (CP3.7). The cached token sits on disk for up to its full ~1h TTL;
// limiting it to read-only means an exfil during that window grants
// read-only capability, not full implement-role write capability.
// Mirrors the principle behind the git read/write split in CP3.
func (c *Client) MintGhReadOnlyToken(ctx context.Context) (token string, expiresAt time.Time, err error) {
	return c.mint(ctx, &githubauth.InstallationPermissions{
		Contents:     githubauth.Ptr("read"),
		Issues:       githubauth.Ptr("read"),
		PullRequests: githubauth.Ptr("read"),
		Metadata:     githubauth.Ptr("read"),
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
// GitHub API layer — installation tokens always return ~1h. CP3.7 mints
// this tier JIT per gh write call (no caching) and revokes it via
// RevokeToken as soon as gh exits, giving an effective TTL of
// "gh process lifetime + revoke RTT" — typically sub-second, far below
// the 5-min target.
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
// Status handling (issue #67): 204 is "revoked"; 404 is "this token is not a
// live installation token" — i.e. it is ALREADY GONE (revoked earlier, or
// expired). Both mean the token is dead, which is the entire goal, so both are
// success. Only a network error or some OTHER status is an error. Mapping 404
// here rather than at the call site means every caller (exit-revoke, the
// expiry path, the broker's revoke closure) gets the same idempotent contract.
//
// NOTE: revocation is eventually consistent — a revoked token can still
// authenticate for a few seconds after the 204 (measured ~2-5s live). Revoke
// shrinks the exposure window to seconds; it is not an instantaneous kill.
//
// Best-effort: callers should log + ignore the error. A failed revoke is
// not a security problem (token still expires at its native ~1h).
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiURL("/installation/token"), nil)
	if err != nil {
		return fmt.Errorf("build revoke request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil // revoked
	case http.StatusNotFound:
		return nil // already gone (revoked earlier, or expired) — same end state
	default:
		return fmt.Errorf("revoke: unexpected status %d", resp.StatusCode)
	}
}

func (c *Client) mint(ctx context.Context, perms *githubauth.InstallationPermissions) (string, time.Time, error) {
	keyPEM, err := c.ks.Get(c.roleName)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read private key from keystore[%s]: %w", c.roleName, err)
	}

	appSrc, err := githubauth.NewApplicationTokenSource(c.cfg.ClientID, keyPEM)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build app token source: %w", err)
	}

	opts := &githubauth.InstallationTokenOptions{
		Repositories: c.cfg.RepoNames,
		Permissions:  perms,
	}

	instOpts := []githubauth.InstallationTokenSourceOpt{
		githubauth.WithInstallationTokenOptions(opts),
		githubauth.WithContext(ctx),
	}
	if c.apiBaseURL != "" {
		// Test seam only — see the apiBaseURL field doc. Never set in
		// production, so this branch is dead outside unit tests.
		instOpts = append(instOpts, githubauth.WithEnterpriseURL(c.apiBaseURL))
	}

	instSrc := githubauth.NewInstallationTokenSource(
		c.cfg.InstallationID,
		appSrc,
		instOpts...,
	)

	tok, err := instSrc.Token()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint installation token: %w", err)
	}
	return tok.AccessToken, tok.Expiry, nil
}
