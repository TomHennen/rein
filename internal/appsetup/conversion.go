package appsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DefaultGitHubAPIBase is the production base URL. Tests override via
// ConvertManifestCode's baseURL parameter to point at an httptest.Server.
const DefaultGitHubAPIBase = "https://api.github.com"

// AppConfig is the subset of GitHub's
// POST /app-manifests/{code}/conversions response rein cares about.
// `client_secret` and `webhook_secret` are deliberately omitted —
// they're not present in this struct so they cannot be logged or
// written to state.json by accident.
type AppConfig struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	PEM      string `json:"pem"`
	HTMLURL  string `json:"html_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// ConvertManifestCode POSTs the temporary code to
// {baseURL}/app-manifests/{code}/conversions and returns the parsed
// config. Sends Accept and X-GitHub-Api-Version headers ONLY — NO
// Authorization header (sending one yields a redirect to /login).
//
// baseURL is configurable for tests. Production callers pass
// DefaultGitHubAPIBase.
func ConvertManifestCode(ctx context.Context, baseURL, code string) (AppConfig, error) {
	if code == "" {
		return AppConfig{}, fmt.Errorf("convert: empty code")
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return AppConfig{}, fmt.Errorf("build conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// Intentionally NO Authorization header. The temporary code IS
	// the authentication for this endpoint.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return AppConfig{}, fmt.Errorf("exchange code: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return AppConfig{}, fmt.Errorf("read conversion response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		excerpt := string(body)
		if len(excerpt) > 512 {
			excerpt = excerpt[:512] + "...(truncated)"
		}
		return AppConfig{}, fmt.Errorf("conversion returned HTTP %d: %s", resp.StatusCode, excerpt)
	}
	var cfg AppConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return AppConfig{}, fmt.Errorf("parse conversion response: %w", err)
	}
	if cfg.PEM == "" {
		return AppConfig{}, fmt.Errorf("conversion response missing pem field")
	}
	if cfg.ID == 0 || cfg.Slug == "" || cfg.ClientID == "" {
		return AppConfig{}, fmt.Errorf("conversion response missing required fields (id=%d slug=%q client_id=%q)", cfg.ID, cfg.Slug, cfg.ClientID)
	}
	return cfg, nil
}
