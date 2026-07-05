package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

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

// AppSlug mints an App JWT and calls GET {apiBase}/app, returning the App's
// slug (e.g. "my-app"). The slug is the stable, URL-safe App identifier used to
// derive the bot login "<slug>[bot]" for the git-committer noreply email (CP4).
//
// This is the ONLY way to learn the slug on the env-var config path (state.json
// carries it on the manifest-flow path, but REIN_APP_* env config does not), so
// it is the uniform resolver. JWT auth is required and accepted here (unlike the
// public /users endpoint, which rejects the JWT — see BotUserID).
func (c *AppClient) AppSlug(ctx context.Context) (string, error) {
	keyPEM, err := c.ks.Get(c.roleName)
	if err != nil {
		return "", fmt.Errorf("read private key from keystore[%s]: %w", c.roleName, err)
	}
	appSrc, err := githubauth.NewApplicationTokenSource(c.clientID, keyPEM)
	if err != nil {
		return "", fmt.Errorf("build app token source: %w", err)
	}
	tok, err := appSrc.Token()
	if err != nil {
		return "", fmt.Errorf("mint app jwt: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/app", nil)
	if err != nil {
		return "", fmt.Errorf("build /app request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /app: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read /app response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		excerpt := string(body)
		if len(excerpt) > 512 {
			excerpt = excerpt[:512] + "...(truncated)"
		}
		return "", fmt.Errorf("GET /app returned HTTP %d: %s", resp.StatusCode, excerpt)
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse /app response: %w", err)
	}
	if out.Slug == "" {
		return "", fmt.Errorf("GET /app returned an empty slug")
	}
	return out.Slug, nil
}

// BotUserID returns the numeric user id for a bot login like "myapp[bot]" via
// GET {apiBase}/users/<login>. This endpoint is PUBLIC and must be called
// UNAUTHENTICATED: sending the App JWT here returns 401 (live-verified) because
// a JWT authenticates only App-scoped endpoints, not the user API. The bracket
// characters in the login are percent-escaped.
//
// apiBase defaults to DefaultAPIBase when empty. Standalone (not an AppClient
// method) because it needs no key material or App identity — only the login.
func BotUserID(ctx context.Context, apiBase, botLogin string) (int64, error) {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	endpoint := apiBase + "/users/" + url.PathEscape(botLogin)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("build /users request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET /users/%s: %w", botLogin, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read /users response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET /users/%s returned HTTP %d", botLogin, resp.StatusCode)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("parse /users response: %w", err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("GET /users/%s returned id 0", botLogin)
	}
	return out.ID, nil
}
