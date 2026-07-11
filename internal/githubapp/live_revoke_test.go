package githubapp_test

// Live proof that RevokeToken actually KILLS the token server-side (issue #67).
//
// This is the load-bearing check behind the whole exit-revoke feature: the
// exit path only ever observed a 204 from DELETE /installation/token, which
// proves the endpoint accepted the call but NOT that the credential is dead.
// If revoke did not really work, every "revoked N write token(s)" line rein
// prints would be a lie. So: mint a real write token, USE it (expect 200),
// revoke it, then USE IT AGAIN and require GitHub to reject it (401).
//
// GATED behind REIN_LIVE=1 (t.Skip otherwise) so `go test ./...` stays offline.
//
//	source ./dev-env && REIN_LIVE=1 go test ./internal/githubapp -run Live -v

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

// getRepoAs performs an authenticated GET /repos/<slug> with the token and
// returns the status code. The throwaway is PRIVATE, so an anonymous or dead
// credential yields 401/404 and only a live installation token yields 200.
func getRepoAs(t *testing.T, token, slug string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+slug, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /repos/%s: %v", slug, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestLiveRevokeTokenKillsTheToken(t *testing.T) {
	if os.Getenv("REIN_LIVE") != "1" {
		t.Skip("live test; set REIN_LIVE=1 (and source ./dev-env) to run against real github.com")
	}
	slug := os.Getenv("REIN_TEST_REPO_A")
	if slug == "" {
		t.Fatal("REIN_TEST_REPO_A unset; source ./dev-env")
	}
	_, bare, ok := strings.Cut(slug, "/")
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

	token, expiresAt, err := client.MintWriteToken(ctx)
	if err != nil {
		t.Fatalf("mint write token: %v", err)
	}
	t.Logf("minted write token (len=%d) expiring %s (%s from now)",
		len(token), expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second))

	// 1. BEFORE revoke: the token must actually work. If this isn't 200, the
	//    rest of the test proves nothing (a dead-on-arrival token would
	//    "pass" the after-check trivially).
	if got := getRepoAs(t, token, slug); got != http.StatusOK {
		t.Fatalf("pre-revoke GET /repos/%s = %d, want 200 — the freshly minted token does not authenticate, so this test cannot prove anything about revoke", slug, got)
	}
	t.Logf("PRE-REVOKE : GET /repos/%s = 200 (token is live)", slug)

	// 2. Revoke it.
	if err := client.RevokeToken(ctx, token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	t.Logf("REVOKE     : DELETE /installation/token = 204 (accepted)")

	// 3. AFTER revoke: the SAME token must now be rejected. This — not the
	//    204 — is what proves the credential is actually dead.
	//
	//    Revocation is EVENTUALLY CONSISTENT: measured live (issue #67), the
	//    token kept returning 200 for ~2-5s after the 204 before flipping to
	//    401. So poll rather than sampling once — a single immediate check
	//    reads the stale-accept window and looks like "revoke is broken."
	//    (This is also why rein cannot treat revoke as a synchronous kill: it
	//    shrinks the exposure window from ~1h to ~seconds, it does not close
	//    it instantly.)
	deadline := time.Now().Add(60 * time.Second)
	var got int
	for {
		got = getRepoAs(t, token, slug)
		if got == http.StatusUnauthorized || got == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("POST-REVOKE GET /repos/%s = %d after 60s — the revoked token STILL AUTHENTICATES. Exit-revoke does not actually kill tokens; every 'revoked N write token(s)' rein prints is false.", slug, got)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("POST-REVOKE: GET /repos/%s = %d (token is DEAD)", slug, got)

	// 4. Revoking an ALREADY-revoked token is the exact condition issue #67's
	//    duplicate ledger entries hit. GitHub answers 404. That is "already
	//    gone" == the desired end state, so RevokeToken must map it to success,
	//    not to a scary warning.
	if err := client.RevokeToken(ctx, token); err != nil {
		t.Fatalf("second revoke of an already-revoked token returned %v; want nil (404 == already gone == success)", err)
	}
	t.Logf("RE-REVOKE  : DELETE /installation/token on the dead token = success (404 mapped to already-revoked)")
}
