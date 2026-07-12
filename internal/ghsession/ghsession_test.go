package ghsession

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/tokencache"
)

// stubMint returns a MintFunc that hands out the configured token (or err)
// and counts invocations.
func stubMint(token string, err error) (MintFunc, *int) {
	calls := 0
	fn := func(ctx context.Context) (string, time.Time, error) {
		calls++
		if err != nil {
			return "", time.Time{}, err
		}
		return token, time.Now().Add(time.Hour), nil
	}
	return fn, &calls
}

func TestEnsureFresh_CacheHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok.json")
	want := "ghs_cached_and_fresh"
	if err := tokencache.Write(path, tokencache.Entry{Token: want, ExpiresAt: time.Now().Add(45 * time.Minute)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mint, calls := stubMint("should-not-be-used", nil)
	got, _, err := EnsureFresh(path, mint, nil, 5*time.Minute, time.Second, discardLogger())
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got != want {
		t.Errorf("token = %q, want %q", got, want)
	}
	if *calls != 0 {
		t.Errorf("mintFn calls = %d, want 0 (cache hit should not mint)", *calls)
	}
}

func TestEnsureFresh_StaleCacheTriggersMint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok.json")
	if err := tokencache.Write(path, tokencache.Entry{Token: "ghs_stale", ExpiresAt: time.Now().Add(30 * time.Second)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mint, calls := stubMint("ghs_fresh", nil)
	got, _, err := EnsureFresh(path, mint, nil, 5*time.Minute, time.Second, discardLogger())
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got != "ghs_fresh" {
		t.Errorf("token = %q, want %q", got, "ghs_fresh")
	}
	if *calls != 1 {
		t.Errorf("mintFn calls = %d, want 1", *calls)
	}
	// New token should be cached.
	if got, err := tokencache.Read(path); err != nil || got.Token != "ghs_fresh" {
		t.Errorf("cache after mint = (%q, %v), want token ghs_fresh", got.Token, err)
	}
}

func TestEnsureFresh_AbsentCacheTriggersMint(t *testing.T) {
	mint, calls := stubMint("ghs_first", nil)
	got, _, err := EnsureFresh(filepath.Join(t.TempDir(), "absent.json"), mint, nil, 5*time.Minute, time.Second, discardLogger())
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got != "ghs_first" || *calls != 1 {
		t.Errorf("token=%q calls=%d, want ghs_first/1", got, *calls)
	}
}

func TestEnsureFresh_MalformedCacheTriggersMint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mint, calls := stubMint("ghs_fresh_after_corrupt", nil)
	got, _, err := EnsureFresh(path, mint, nil, 5*time.Minute, time.Second, discardLogger())
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got != "ghs_fresh_after_corrupt" || *calls != 1 {
		t.Errorf("token=%q calls=%d, want ghs_fresh_after_corrupt/1", got, *calls)
	}
}

func TestEnsureFresh_MintFailurePropagates(t *testing.T) {
	mint, _ := stubMint("", errors.New("simulated mint failure"))
	_, _, err := EnsureFresh(filepath.Join(t.TempDir(), "absent.json"), mint, nil, 5*time.Minute, time.Second, discardLogger())
	if err == nil {
		t.Fatal("expected error from mint failure")
	}
}

func TestEnsureFresh_RevokeCalledOnStaleCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok.json")
	const prev = "ghs_prev"
	if err := tokencache.Write(path, tokencache.Entry{Token: prev, ExpiresAt: time.Now().Add(30 * time.Second)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mint, _ := stubMint("ghs_fresh", nil)
	var revoked []string
	revoke := func(ctx context.Context, token string) error {
		revoked = append(revoked, token)
		return nil
	}
	if _, _, err := EnsureFresh(path, mint, revoke, 5*time.Minute, time.Second, discardLogger()); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if len(revoked) != 1 || revoked[0] != prev {
		t.Errorf("revoked = %v, want [%q]", revoked, prev)
	}
}

func TestEnsureFresh_RevokeSkippedOnCacheHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok.json")
	if err := tokencache.Write(path, tokencache.Entry{Token: "ghs_fresh_cached", ExpiresAt: time.Now().Add(45 * time.Minute)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mint, _ := stubMint("unused", nil)
	revoked := 0
	revoke := func(ctx context.Context, token string) error {
		revoked++
		return nil
	}
	if _, _, err := EnsureFresh(path, mint, revoke, 5*time.Minute, time.Second, discardLogger()); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if revoked != 0 {
		t.Errorf("revoke calls = %d on cache hit, want 0", revoked)
	}
}

// TestReadCachePathForScope_DistinctPerScope pins the issue #95 fix: two
// different scope ceilings resolve to two DIFFERENT cache files, so a token
// minted under one ceiling can never be served from the other's cache.
// Order-insensitivity and case-folding are inherited from runscope.Key(),
// which produces the scopeKey; the same key ALWAYS yields the same path.
func TestReadCachePathForScope_DistinctPerScope(t *testing.T) {
	dir := "/var/state/rein"
	keyA := "owner/alpha"
	keyAB := "owner/alpha,owner/beta"

	pathA := ReadCachePathForScope(dir, keyA)
	pathAB := ReadCachePathForScope(dir, keyAB)

	if pathA == pathAB {
		t.Fatalf("distinct scopes shared a cache file: A=%q AB=%q", pathA, pathAB)
	}
	if got, want := filepath.Dir(pathA), filepath.Join(dir, "cache"); got != want {
		t.Errorf("cache dir = %q, want %q", got, want)
	}
	// Prefix + suffix shape so ReadCacheGlob (gh-read-token*.json) matches it.
	base := filepath.Base(pathA)
	if !strings.HasPrefix(base, "gh-read-token-") || !strings.HasSuffix(base, ".json") {
		t.Errorf("filename %q does not match the gh-read-token-<tag>.json shape", base)
	}
	// Stable: same key => same path (no per-call randomness).
	if ReadCachePathForScope(dir, keyA) != pathA {
		t.Error("ReadCachePathForScope is not deterministic for a fixed key")
	}
	// The legacy untagged filename and a tagged one both match the glob.
	glob := ReadCacheGlob(dir)
	for _, name := range []string{"gh-read-token.json", filepath.Base(pathA)} {
		ok, err := filepath.Match(glob, filepath.Join(dir, "cache", name))
		if err != nil || !ok {
			t.Errorf("glob %q did not match %q (ok=%v err=%v)", glob, name, ok, err)
		}
	}
}

// TestReadCachePathForScope_CrossScopeIsCacheMiss is the behavioral proof of
// the #95 fix at the ghsession layer: a token written under scope A's path is
// NOT served by an EnsureFresh that reads scope [A,B]'s path — it is a cache
// MISS, forcing a re-mint at the wider ceiling. A stub MintFunc keeps it
// off the network.
func TestReadCachePathForScope_CrossScopeIsCacheMiss(t *testing.T) {
	dir := t.TempDir()
	keyA := "owner/alpha"
	keyAB := "owner/alpha,owner/beta"

	// Simulate the stale NARROW cache an earlier single-repo-A run leaves.
	narrow := tokencache.Entry{Token: "ghs_scoped_to_A_only", ExpiresAt: time.Now().Add(45 * time.Minute)}
	if err := tokencache.Write(ReadCachePathForScope(dir, keyA), narrow); err != nil {
		t.Fatalf("seed narrow cache: %v", err)
	}

	// The [A,B] run must NOT see the narrow token: cache miss => mint fresh.
	mint, calls := stubMint("ghs_scoped_to_A_and_B", nil)
	got, _, err := EnsureFresh(ReadCachePathForScope(dir, keyAB), mint, nil, 5*time.Minute, time.Second, discardLogger())
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got != "ghs_scoped_to_A_and_B" {
		t.Errorf("token = %q, want the freshly minted [A,B] token — the narrow A-only token must NOT be served across scopes", got)
	}
	if *calls != 1 {
		t.Errorf("mintFn calls = %d, want 1 (cross-scope read must MISS and re-mint)", *calls)
	}

	// And the narrow A-scope cache is still intact under its own key (the
	// wider run wrote to a different file, so it can't clobber it).
	if e, err := tokencache.Read(ReadCachePathForScope(dir, keyA)); err != nil || e.Token != narrow.Token {
		t.Errorf("A-scope cache = (%q, %v), want it untouched (%q)", e.Token, err, narrow.Token)
	}
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
