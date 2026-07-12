package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/runscope"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/tokencache"
)

// TestIssue95_CrossRunStaleness_Live is the acceptance-criterion E2E for
// issue #95: cross-run staleness of the scope-BLIND gh-read cache.
//
// It drives the REAL broker declare-fetch wiring (declareEnv.fetchIssue ->
// ghReadToken -> ghsession.EnsureFresh -> issuemeta.Fetch) against the live
// GitHub API with real App credentials, and proves BOTH halves in one run,
// differing ONLY in the cache-path keying:
//
//   - PRE-FIX behavior (fixed path gh-read-token.json): a still-fresh token
//     minted by an earlier NARROW single-repo-A run is served to an [A,B]
//     run and 404s on repo B's issue — the bug.
//   - POST-FIX behavior (ReadCachePathForScope keyed by the [A,B] ceiling):
//     the seeded narrow token is a cache MISS, EnsureFresh re-mints at [A,B],
//     and the fetch of B's issue succeeds.
//
// Gated on REIN_LIVE_E2E=1 so `go test ./...` stays hermetic (no network).
// Run: source ./dev-env && REIN_LIVE_E2E=1 go test ./cmd/rein -run TestIssue95 -v
func TestIssue95_CrossRunStaleness_Live(t *testing.T) {
	if os.Getenv("REIN_LIVE_E2E") != "1" {
		t.Skip("live E2E: set REIN_LIVE_E2E=1 (with ./dev-env sourced) to run")
	}
	repoA := os.Getenv("REIN_TEST_REPO_A")
	repoB := os.Getenv("REIN_TEST_REPO_B")
	if repoA == "" || repoB == "" {
		t.Fatal("REIN_TEST_REPO_A and REIN_TEST_REPO_B must be set (source ./dev-env)")
	}
	issueB := 2
	if v := os.Getenv("REIN_TEST_ISSUE_B"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("REIN_TEST_ISSUE_B=%q not an int: %v", v, err)
		}
		issueB = n
	}

	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		t.Fatalf("ResolveApp: %v", err)
	}
	logger := log.New(io.Discard, "", 0)
	stateDir := t.TempDir()
	sess := session.Session{ID: "e2e-issue95", Role: "implement", Repos: []string{repoA, repoB}}

	// The effective ceiling of the SECOND run: [A, B] (no expansions).
	rscope := runscope.New(sess, stateDir, "")
	scopedCfg := func() githubapp.Config {
		c := appCfg
		c.RepoNames = rscope.BareNames()
		return c
	}
	// The [A,B]-scoped mint the run's ghReadToken uses on a cache miss.
	mintAB := func(ctx context.Context) (string, time.Time, error) {
		c, e := githubapp.NewClient(scopedCfg(), ks, config.AppKeystoreRole)
		if e != nil {
			return "", time.Time{}, e
		}
		return c.MintGhReadOnlyToken(ctx)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// (1) Mint the NARROW token an earlier single-repo-A run leaves behind:
	// scoped to A ONLY. This is a real installation token, really narrow.
	aCfg := appCfg
	aCfg.RepoNames = []string{bareRepoName(repoA)}
	ac, err := githubapp.NewClient(aCfg, ks, config.AppKeystoreRole)
	if err != nil {
		t.Fatalf("NewClient (A-only): %v", err)
	}
	mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
	narrowTok, narrowExp, err := ac.MintGhReadOnlyToken(mctx)
	mcancel()
	if err != nil {
		t.Fatalf("mint A-only token: %v", err)
	}
	t.Logf("minted NARROW A-only gh-read token (expires %s)", narrowExp.Format(time.RFC3339))

	// Independent sanity: the narrow A-only token genuinely CANNOT read B.
	if _, ferr := issuemeta.Fetch(ctx, "", narrowTok, repoB, issueB); ferr == nil {
		t.Fatalf("the A-only token unexpectedly read %s#%d — test premise broken", repoB, issueB)
	} else {
		t.Logf("confirmed: A-only token cannot read %s#%d: %v", repoB, issueB, ferr)
	}

	// Seed it at the PRE-FIX fixed path — exactly what the scope-blind cache
	// looked like before the fix.
	oldPath := filepath.Join(stateDir, "cache", "gh-read-token.json")
	if err := tokencache.Write(oldPath, tokencache.Entry{Token: narrowTok, ExpiresAt: narrowExp}); err != nil {
		t.Fatalf("seed old fixed-path cache: %v", err)
	}

	// (2) PRE-FIX: ghReadToken reads the FIXED path -> cache HIT on the stale
	// narrow token -> fetchIssue(B) 404s. This is issue #95 reproduced.
	envPre := declareEnv{
		sess:     sess,
		stateDir: stateDir,
		ghReadToken: func(ctx context.Context) (string, error) {
			tok, _, e := ghsession.EnsureFresh(oldPath, mintAB, nil, 5*time.Minute, mintTimeout, logger)
			return tok, e
		},
		appCfg: appCfg,
		ks:     ks,
		logger: logger,
	}
	if _, preErr := envPre.fetchIssue(ctx, repoB, issueB); preErr == nil {
		t.Fatal("PRE-FIX: expected fetchIssue(B) to fail with the stale narrow cache, but it SUCCEEDED")
	} else {
		t.Logf("PRE-FIX (scope-blind fixed path): fetchIssue(%s#%d) failed as expected: %v", repoB, issueB, preErr)
	}

	// (3) POST-FIX: ghReadToken reads the SCOPE-TAGGED path for the [A,B]
	// ceiling -> the narrow token (at the old path) is a cache MISS ->
	// EnsureFresh re-mints at [A,B] -> fetchIssue(B) succeeds.
	newPath := ghsession.ReadCachePathForScope(stateDir, rscope.Key())
	if newPath == oldPath {
		t.Fatalf("scope-tagged path %q collided with the fixed path — fix not engaged", newPath)
	}
	envPost := declareEnv{
		sess:     sess,
		stateDir: stateDir,
		ghReadToken: func(ctx context.Context) (string, error) {
			tok, _, e := ghsession.EnsureFresh(newPath, mintAB, nil, 5*time.Minute, mintTimeout, logger)
			return tok, e
		},
		appCfg: appCfg,
		ks:     ks,
		logger: logger,
	}
	meta, postErr := envPost.fetchIssue(ctx, repoB, issueB)
	if postErr != nil {
		t.Fatalf("POST-FIX: fetchIssue(%s#%d) failed, want success: %v", repoB, issueB, postErr)
	}
	if meta.Title == "" {
		t.Fatalf("POST-FIX: fetched %s#%d but Title is empty", repoB, issueB)
	}
	t.Logf("POST-FIX (scope-tagged path): fetchIssue(%s#%d) succeeded: #%d %q [%s]",
		repoB, issueB, meta.Number, meta.Title, meta.State)
}
