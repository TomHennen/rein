package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jferrl/go-githubauth"

	"github.com/TomHennen/rein/internal/keystore"
)

// DefaultAPIBase is the GitHub REST API root. Overridable per-call (see
// NewAppClient) so tests can point at an httptest.Server.
const DefaultAPIBase = "https://api.github.com"

// ErrAppNotInstalled is returned by RepoInstallationID when GitHub responds
// 404 to GET /repos/{owner}/{repo}/installation, i.e. the App is not
// installed on that repository.
var ErrAppNotInstalled = errors.New("github app: not installed on repository")

// AppClient mints an App-level JWT (no installation id required) and performs
// App-authenticated lookups that do not need an installation token. It exists
// because NewClient hard-rejects InstallationID==0 (client.go) — the exact
// value rein run's eager step is trying to discover.
//
// The private key is read from the Keystore lazily inside each call so the
// AppClient never holds key material across calls (matches Client), and honors
// CLAUDE.md hard constraint #6 (all PEM reads go through the keystore).
type AppClient struct {
	clientID string
	ks       keystore.Keystore
	roleName string
	apiBase  string
}

// NewAppClient validates inputs and constructs an AppClient. clientID is the
// App's Client ID (or numeric App ID — go-githubauth accepts either). ks is
// the keystore the PEM is read from at call time; roleName is the entry name
// (typically "primary"). apiBase defaults to DefaultAPIBase when empty.
func NewAppClient(clientID string, ks keystore.Keystore, roleName, apiBase string) (*AppClient, error) {
	if clientID == "" {
		return nil, errors.New("github app: ClientID is required")
	}
	if ks == nil {
		return nil, errors.New("github app: keystore is required")
	}
	if roleName == "" {
		return nil, errors.New("github app: roleName is required")
	}
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	return &AppClient{clientID: clientID, ks: ks, roleName: roleName, apiBase: apiBase}, nil
}

// RepoInstallationID mints an App JWT and calls
// GET {apiBase}/repos/{owner}/{repo}/installation, returning installation.id.
//
//   - 404            -> ErrAppNotInstalled.
//   - other non-200  -> a descriptive error including status + truncated body.
//   - 200 with id==0 -> an error (defensive; the API marks id as required).
//
// Per the GitHub docs ("Get a repository installation for the authenticated
// app"), this endpoint requires JWT auth and returns the Installation object.
func (c *AppClient) RepoInstallationID(ctx context.Context, owner, repo string) (int64, error) {
	keyPEM, err := c.ks.Get(c.roleName)
	if err != nil {
		return 0, fmt.Errorf("read private key from keystore[%s]: %w", c.roleName, err)
	}

	appSrc, err := githubauth.NewApplicationTokenSource(c.clientID, keyPEM)
	if err != nil {
		return 0, fmt.Errorf("build app token source: %w", err)
	}
	tok, err := appSrc.Token()
	if err != nil {
		return 0, fmt.Errorf("mint app jwt: %w", err)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/installation", c.apiBase, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("build installation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("lookup installation for %s/%s: %w", owner, repo, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read installation response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrAppNotInstalled
	}
	if resp.StatusCode != http.StatusOK {
		excerpt := string(body)
		if len(excerpt) > 512 {
			excerpt = excerpt[:512] + "...(truncated)"
		}
		return 0, fmt.Errorf("installation lookup for %s/%s returned HTTP %d: %s", owner, repo, resp.StatusCode, excerpt)
	}

	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("parse installation response: %w", err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("installation lookup for %s/%s returned id 0", owner, repo)
	}
	return out.ID, nil
}
