package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadCachedToken(t *testing.T) {
	t.Run("missing file → not ok", func(t *testing.T) {
		_, ok := loadCachedToken(filepath.Join(t.TempDir(), "absent.json"))
		if ok {
			t.Fatal("expected !ok for missing file")
		}
	})
	t.Run("malformed JSON → not ok", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		mustWrite(t, path, "{not-json")
		_, ok := loadCachedToken(path)
		if ok {
			t.Fatal("expected !ok for malformed JSON")
		}
	})
	t.Run("valid round trip", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ok.json")
		want := cachedToken{Token: "ghs_xyz", ExpiresAt: time.Now().Add(45 * time.Minute).Truncate(time.Second)}
		body, _ := json.Marshal(want)
		mustWrite(t, path, string(body))
		got, ok := loadCachedToken(path)
		if !ok {
			t.Fatal("expected ok")
		}
		if got.Token != want.Token {
			t.Errorf("token = %q, want %q", got.Token, want.Token)
		}
	})
}

func TestWriteCachedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "tok.json")
	tok := cachedToken{Token: "ghs_abc", ExpiresAt: time.Now().Add(30 * time.Minute)}
	if err := writeCachedToken(path, tok); err != nil {
		t.Fatalf("writeCachedToken: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	got, ok := loadCachedToken(path)
	if !ok {
		t.Fatal("expected ok after write")
	}
	if got.Token != tok.Token {
		t.Errorf("token round-trip mismatch: got %q", got.Token)
	}
}

// TestEnsureFreshToken_CacheHit verifies that a non-stale cache short-
// circuits the mint path. We can't directly test the mint path without
// a real GitHub client; the e2e test covers it.
func TestEnsureFreshToken_CacheHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gh-token.json")
	want := "ghs_cached_and_fresh"
	writeOrFatal(t, path, cachedToken{Token: want, ExpiresAt: time.Now().Add(45 * time.Minute)})

	logger := discardLogger(t)
	got, err := ensureFreshToken(path, logger)
	if err != nil {
		t.Fatalf("ensureFreshToken: %v", err)
	}
	if got != want {
		t.Errorf("token = %q, want %q", got, want)
	}
}

// TestEnsureFreshToken_StaleCacheTriggersMintAttempt asserts that a
// near-expiry token isn't returned as-is. We can't actually run a mint
// in a unit test (needs the App key + network), but we can verify the
// "skip cache" branch by observing that the function returns an error
// when env vars are unset (because mint requires them).
func TestEnsureFreshToken_StaleCacheTriggersMintAttempt(t *testing.T) {
	// Ensure REIN_* env is unset for this test
	for _, k := range []string{"REIN_APP_CLIENT_ID", "REIN_APP_PRIVATE_KEY_PATH", "REIN_APP_INSTALLATION_ID", "REIN_TEST_REPO_A"} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "gh-token.json")
	writeOrFatal(t, path, cachedToken{Token: "ghs_stale", ExpiresAt: time.Now().Add(30 * time.Second)})

	logger := discardLogger(t)
	_, err := ensureFreshToken(path, logger)
	if err == nil {
		t.Fatal("expected error from missing REIN_* env when cache is stale")
	}
}

func TestEnsureFreshToken_AbsentCacheTriggersMintAttempt(t *testing.T) {
	for _, k := range []string{"REIN_APP_CLIENT_ID", "REIN_APP_PRIVATE_KEY_PATH", "REIN_APP_INSTALLATION_ID", "REIN_TEST_REPO_A"} {
		old := os.Getenv(k)
		os.Unsetenv(k)
		defer os.Setenv(k, old)
	}
	logger := discardLogger(t)
	_, err := ensureFreshToken(filepath.Join(t.TempDir(), "absent.json"), logger)
	if err == nil {
		t.Fatal("expected error from missing REIN_* env when cache is absent")
	}
}

// --- helpers ---

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeOrFatal(t *testing.T, path string, c cachedToken) {
	t.Helper()
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mustWrite(t, path, string(body))
}

func discardLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}
