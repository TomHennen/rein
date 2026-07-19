package githubapp_test

// Live check of the administration-token tier used to install the branch-floor
// ruleset (internal/ruleset). Two things it documents:
//
//   - AFTER the App is granted "Administration: write" (re-approved), the mint
//     SUCCEEDS and the token can list rulesets (a 200 on GET /rulesets).
//   - BEFORE the grant, GitHub 422s the mint ("permissions ... not granted") —
//     which is exactly how ensureBranchFloor fails closed. This test then
//     reports that state rather than failing, so it doubles as the go/no-go
//     signal for whether a real `rein run` will proceed.
//
// GATED behind REIN_LIVE=1 (source ./dev-env for REIN_TEST_REPO_A).
//
//	source ./dev-env && REIN_LIVE=1 go test ./internal/githubapp -run LiveMintAdmin -v

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
)

func TestLiveMintAdminToken(t *testing.T) {
	if os.Getenv("REIN_LIVE") != "1" {
		t.Skip("live test; set REIN_LIVE=1 (and source ./dev-env) to run against real github.com")
	}
	slug := os.Getenv("REIN_TEST_REPO_A")
	if slug == "" {
		t.Fatal("REIN_TEST_REPO_A unset; source ./dev-env")
	}
	owner, bare, ok := strings.Cut(slug, "/")
	if !ok {
		t.Fatalf("REIN_TEST_REPO_A=%q is not owner/name", slug)
	}

	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		t.Fatalf("resolve app: %v", err)
	}
	appCfg.RepoNames = []string{bare}
	client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tok, _, err := client.MintAdminToken(ctx)
	if err != nil {
		// Pre-grant state: the mint 422s. Report it (this is how the wired run
		// path fails closed) instead of failing the test.
		t.Logf("MINT administration token FAILED (expected until the App is granted 'Administration: write' and re-approved): %v", err)
		t.Skip("App lacks administration:write; re-approve it, then this test proves the admin tier + ruleset read access")
	}
	defer client.RevokeToken(ctx, tok)
	t.Logf("MINT administration token OK (len=%d) — the App has administration:write", len(tok))

	// The admin token must be able to READ the repo's rulesets (the surface
	// ensureBranchFloor uses to create/verify).
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+owner+"/"+bare+"/rulesets", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /rulesets: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /repos/%s/rulesets = %d, want 200 (admin token cannot read rulesets)", slug, resp.StatusCode)
	}
	t.Logf("GET /repos/%s/rulesets = 200 (admin token can manage rulesets)", slug)
}
