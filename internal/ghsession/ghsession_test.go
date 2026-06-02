package ghsession

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
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

func TestReadCachePath(t *testing.T) {
	got := ReadCachePath("/var/state/rein")
	want := "/var/state/rein/cache/gh-read-token.json"
	if got != want {
		t.Errorf("ReadCachePath = %q, want %q", got, want)
	}
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
