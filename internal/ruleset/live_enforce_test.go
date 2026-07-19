package ruleset_test

// LIVE proof of the whole point of this package: with the branch floor active,
// a per-run write token (contents:write, NO administration, NOT on the bypass
// list) can create a ref under refs/heads/agent/** but is REJECTED server-side
// (GH013 / 422) when it tries to touch any other ref. This is the one check
// that validates the empirical assumption the floor rests on — that GitHub
// grants the App no implicit bypass over a ruleset it created.
//
// It runs the full loop: Ensure the floor (needs administration:write) → mint a
// write token → try a blocked ref (expect rejected) → try an agent/** ref
// (expect allowed) → clean up. It SKIPS (not fails) when the App lacks
// administration:write, so it doubles as the go/no-go signal for the wired run
// path and can't go red before the App is re-approved.
//
//	source ./dev-env && REIN_LIVE=1 go test ./internal/ruleset -run LiveFloor -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/ruleset"
)

func TestLiveFloorRejectsNonAgentPush(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. Install the floor with an admin token. Skip if the App isn't granted
	//    administration yet (pre-re-approval) — that's the wired path's
	//    fail-closed state, not a test failure.
	adminTok, _, err := client.MintAdminToken(ctx)
	if err != nil {
		t.Skipf("App lacks administration:write (%v); re-approve it, then this test proves GH013 rejection", err)
	}
	defer client.RevokeToken(ctx, adminTok)
	if err := ruleset.Ensure(ctx, http.DefaultClient, "", adminTok, owner, bare); err != nil {
		t.Fatalf("ensure floor: %v", err)
	}
	t.Logf("branch/tag floor active on %s", slug)

	// 2. Mint a per-run WRITE token — contents:write, no administration.
	writeTok, _, err := client.MintWriteToken(ctx)
	if err != nil {
		t.Fatalf("mint write token: %v", err)
	}
	defer client.RevokeToken(ctx, writeTok)

	// 3. Base sha to point new refs at (the default branch head).
	base := getDefaultBranchSHA(t, ctx, writeTok, owner, bare)
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())

	// 4. A ref OUTSIDE agent/** must be REJECTED server-side.
	blocked := "refs/heads/rein-floor-block-" + nonce
	status, body := createRef(t, ctx, writeTok, owner, bare, blocked, base)
	if status == http.StatusCreated {
		// Should never happen; clean it up so we don't leave junk.
		deleteRef(ctx, writeTok, owner, bare, blocked)
		t.Fatalf("write token CREATED %s (status 201) — the floor did NOT bind the token; GH grants an implicit bypass. Floor is a no-op.", blocked)
	}
	if !strings.Contains(body, "GH013") && !strings.Contains(strings.ToLower(body), "rule") && status != http.StatusUnprocessableEntity {
		t.Fatalf("create %s = %d, body=%s; want a ruleset rejection (422 / GH013)", blocked, status, body)
	}
	t.Logf("BLOCKED: create %s -> %d %s", blocked, status, firstLine(body))

	// 5. A ref UNDER agent/** must be ALLOWED.
	allowed := "refs/heads/agent/floor-test-" + nonce
	status, body = createRef(t, ctx, writeTok, owner, bare, allowed, base)
	if status != http.StatusCreated {
		t.Fatalf("create %s = %d, body=%s; want 201 (agent/** is excluded from the floor)", allowed, status, body)
	}
	t.Logf("ALLOWED: create %s -> 201", allowed)
	deleteRef(ctx, writeTok, owner, bare, allowed) // agent/** deletion is not restricted
}

func getDefaultBranchSHA(t *testing.T, ctx context.Context, token, owner, repo string) string {
	t.Helper()
	repoMeta := ghGET(t, ctx, token, fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo))
	var rm struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(repoMeta, &rm); err != nil || rm.DefaultBranch == "" {
		t.Fatalf("resolve default branch: %v (%s)", err, repoMeta)
	}
	refBody := ghGET(t, ctx, token, fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", owner, repo, rm.DefaultBranch))
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(refBody, &ref); err != nil || ref.Object.SHA == "" {
		t.Fatalf("resolve default branch sha: %v (%s)", err, refBody)
	}
	return ref.Object.SHA
}

func createRef(t *testing.T, ctx context.Context, token, owner, repo, ref, sha string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"ref": ref, "sha": sha})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs", owner, repo), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST git/refs: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func deleteRef(ctx context.Context, token, owner, repo, ref string) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/%s", owner, repo, ref)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func ghGET(t *testing.T, ctx context.Context, token, url string) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, b)
	}
	return b
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
