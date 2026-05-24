// Spike for Phase 0 Checkpoint 1.
//
// Proves the GitHub App integration end-to-end against throwaway repos:
//
//  1. Loads App Client ID and PEM-encoded RSA private key from env.
//  2. Signs an App JWT via jferrl/go-githubauth.
//  3. Mints an installation token scoped to repo A only,
//     with permissions contents:read + metadata:read.
//  4. Calls GET /repos/<A> and GET /repos/<B>, prints the status codes.
//
// Success: A returns 200, B returns 404 (or 403 — either demonstrates
// scoping). Anything else is a finding worth surfacing.
//
// Throwaway repos only. Not part of the final binary; absorbed into
// internal/githubapp/ at the end of Phase 0.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jferrl/go-githubauth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "spike-token: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	clientID := mustEnv("REIN_APP_CLIENT_ID")
	keyPath := mustEnv("REIN_APP_PRIVATE_KEY_PATH")
	installationIDStr := mustEnv("REIN_APP_INSTALLATION_ID")
	repoA := mustEnv("REIN_TEST_REPO_A")
	repoB := mustEnv("REIN_TEST_REPO_B")

	installationID, err := strconv.ParseInt(installationIDStr, 10, 64)
	if err != nil {
		return fmt.Errorf("REIN_APP_INSTALLATION_ID not an int64: %w", err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read private key %s: %w", keyPath, err)
	}

	appSrc, err := githubauth.NewApplicationTokenSource(clientID, keyPEM)
	if err != nil {
		return fmt.Errorf("build app token source: %w", err)
	}

	repoAName, err := repoName(repoA)
	if err != nil {
		return err
	}

	opts := &githubauth.InstallationTokenOptions{
		Repositories: []string{repoAName},
		Permissions: &githubauth.InstallationPermissions{
			Contents: githubauth.Ptr("read"),
			Metadata: githubauth.Ptr("read"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	instSrc := githubauth.NewInstallationTokenSource(
		installationID,
		appSrc,
		githubauth.WithInstallationTokenOptions(opts),
		githubauth.WithContext(ctx),
	)

	tok, err := instSrc.Token()
	if err != nil {
		return fmt.Errorf("mint installation token: %w", err)
	}

	fmt.Printf("minted installation token: expires_at=%s ttl=%s\n",
		tok.Expiry.Format(time.RFC3339),
		time.Until(tok.Expiry).Round(time.Second))
	fmt.Printf("token scoped to: %s with permissions {contents:read, metadata:read}\n", repoA)
	fmt.Println()

	codeA, err := repoStatus(ctx, tok.AccessToken, repoA)
	if err != nil {
		return fmt.Errorf("GET %s: %w", repoA, err)
	}
	codeB, err := repoStatus(ctx, tok.AccessToken, repoB)
	if err != nil {
		return fmt.Errorf("GET %s: %w", repoB, err)
	}

	fmt.Printf("GET /repos/%s -> %d (expect 200, in-scope)\n", repoA, codeA)
	fmt.Printf("GET /repos/%s -> %d (expect 404 or 403, out-of-scope)\n", repoB, codeB)
	fmt.Println()

	pass := codeA == 200 && (codeB == 404 || codeB == 403)
	if pass {
		fmt.Println("RESULT: PASS — scope enforcement verified.")
		return nil
	}
	fmt.Println("RESULT: FAIL — unexpected status codes. Surface to human.")
	return fmt.Errorf("unexpected status codes A=%d B=%d", codeA, codeB)
}

func repoStatus(ctx context.Context, token, repo string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+repo, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func repoName(slug string) (string, error) {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("invalid repo slug %q (want owner/name)", slug)
	}
	return name, nil
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required env var %s; did you source ./dev-env?\n", k)
		os.Exit(2)
	}
	return v
}
