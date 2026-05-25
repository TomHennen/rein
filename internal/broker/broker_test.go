package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/tokencache"
)

// stubMinter exposes a configurable token+err MintFunc and a call counter.
type stubMinter struct {
	token string
	err   error
	calls atomic.Int64
}

func (s *stubMinter) Mint(ctx context.Context) (string, time.Time, error) {
	s.calls.Add(1)
	if s.err != nil {
		return "", time.Time{}, s.err
	}
	return s.token, time.Now().Add(time.Hour), nil
}

func (s *stubMinter) Calls() int64 { return s.calls.Load() }

// alwaysWrite/alwaysRead are tiny detector stubs for tests.
func alwaysWrite() bool { return true }
func alwaysRead() bool  { return false }

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// TestRunCredentialHelper exercises the protocol surface with a stubbed
// minter so we can assert on stdout, exit behavior, and — most importantly —
// the TM-G8 invariant: github.com gets always produce a non-empty credential.
func TestRunCredentialHelper(t *testing.T) {
	tests := []struct {
		name              string
		action            string
		stdin             string
		readToken         string
		readErr           error
		wantStdoutHasPwd  bool
		wantPasswordExact string
	}{
		{
			name:              "github.com get with successful read mint",
			action:            "get",
			stdin:             "protocol=https\nhost=github.com\n\n",
			readToken:         "ghs_read_token",
			wantStdoutHasPwd:  true,
			wantPasswordExact: "ghs_read_token",
		},
		{
			name:              "TM-G8: github.com get with read mint failure still returns placeholder",
			action:            "get",
			stdin:             "protocol=https\nhost=github.com\n\n",
			readErr:           errors.New("simulated read mint failure"),
			wantStdoutHasPwd:  true,
			wantPasswordExact: "rein-placeholder-mint-failed",
		},
		{
			name:             "non-github.com host returns empty",
			action:           "get",
			stdin:            "protocol=https\nhost=gitlab.com\n\n",
			readToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:             "ssh protocol returns empty (Bearer token wouldn't help)",
			action:           "get",
			stdin:            "protocol=ssh\nhost=github.com\n\n",
			readToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:              "url= attribute is parsed into protocol/host",
			action:            "get",
			stdin:             "url=https://github.com/owner/repo\n\n",
			readToken:         "ghs_url_form_token",
			wantStdoutHasPwd:  true,
			wantPasswordExact: "ghs_url_form_token",
		},
		{
			name:             "store action is a no-op (no stdout)",
			action:           "store",
			stdin:            "protocol=https\nhost=github.com\nusername=x\npassword=y\n\n",
			readToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:             "erase action is a no-op (no stdout)",
			action:           "erase",
			stdin:            "protocol=https\nhost=github.com\n\n",
			readToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:             "unknown action is a no-op",
			action:           "watusi",
			stdin:            "protocol=https\nhost=github.com\n\n",
			readToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			// TM-G8 hardening: a single malformed stdin line must not
			// prevent the github.com guard from running.
			name:              "TM-G8: malformed line is skipped, github.com guard still runs",
			action:            "get",
			stdin:             "garbage-line-no-equals\nprotocol=https\nhost=github.com\n\n",
			readToken:         "ghs_after_malformed",
			wantStdoutHasPwd:  true,
			wantPasswordExact: "ghs_after_malformed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			read := &stubMinter{token: tc.readToken, err: tc.readErr}
			cfg := Config{
				MintRead:    read.Mint,
				MintTimeout: 5 * time.Second,
				Logger:      discardLogger(),
			}
			err := RunCredentialHelper(tc.action, strings.NewReader(tc.stdin), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper returned error: %v (expected nil for well-formed input)", err)
			}
			got := stdout.String()
			hasPwd := strings.Contains(got, "password=")
			if hasPwd != tc.wantStdoutHasPwd {
				t.Fatalf("stdout pwd presence = %v, want %v; stdout = %q", hasPwd, tc.wantStdoutHasPwd, got)
			}
			if tc.wantPasswordExact != "" {
				wantLine := "password=" + tc.wantPasswordExact
				if !strings.Contains(got, wantLine) {
					t.Fatalf("stdout missing %q; got %q", wantLine, got)
				}
			}
		})
	}
}

// TestDetectWrite confirms the broker routes by the caller-supplied
// detector, that a true result triggers write minting, and that read
// remains the safe default when no detector is provided.
func TestDetectWrite(t *testing.T) {
	helperStdin := "protocol=https\nhost=github.com\n\n"

	t.Run("DetectWrite=true triggers write mint, never touches read cache", func(t *testing.T) {
		dir := t.TempDir()
		cache := filepath.Join(dir, "cache.json")
		writeCacheFile(t, cache, "ghs_cached_read", time.Now().Add(45*time.Minute))

		read := &stubMinter{token: "should-not-be-used"}
		write := &stubMinter{token: "ghs_write_token"}
		cfg := Config{
			MintRead:      read.Mint,
			MintWrite:     write.Mint,
			Logger:        discardLogger(),
			ReadCachePath: cache,
			DetectWrite:   alwaysWrite,
		}

		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_write_token") {
			t.Fatalf("expected write token, got %q", stdout.String())
		}
		if read.Calls() != 0 {
			t.Errorf("read minter calls = %d, want 0", read.Calls())
		}
		if write.Calls() != 1 {
			t.Errorf("write minter calls = %d, want 1", write.Calls())
		}
		// Read cache should be untouched (still holds original cached value).
		body, _ := os.ReadFile(cache)
		var c tokencache.Entry
		_ = json.Unmarshal(body, &c)
		if c.Token != "ghs_cached_read" {
			t.Errorf("write path should not have overwritten read cache; cache.Token = %q", c.Token)
		}
	})

	t.Run("DetectWrite=false uses read path", func(t *testing.T) {
		read := &stubMinter{token: "ghs_read_token"}
		write := &stubMinter{token: "should-not-be-used"}
		cfg := Config{
			MintRead:    read.Mint,
			MintWrite:   write.Mint,
			Logger:      discardLogger(),
			DetectWrite: alwaysRead,
		}

		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_read_token") {
			t.Fatalf("expected read token, got %q", stdout.String())
		}
		if write.Calls() != 0 {
			t.Errorf("write minter calls = %d, want 0", write.Calls())
		}
	})

	t.Run("nil DetectWrite defaults to read (no MintWrite required)", func(t *testing.T) {
		read := &stubMinter{token: "ghs_read_default"}
		cfg := Config{
			MintRead: read.Mint,
			Logger:   discardLogger(),
			// MintWrite intentionally omitted
			// DetectWrite intentionally nil
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_read_default") {
			t.Fatalf("expected read token, got %q", stdout.String())
		}
	})

	t.Run("DetectWrite panic is recovered, falls back to read", func(t *testing.T) {
		read := &stubMinter{token: "ghs_read_after_panic"}
		write := &stubMinter{token: "should-not-be-used"}
		cfg := Config{
			MintRead:    read.Mint,
			MintWrite:   write.Mint,
			Logger:      discardLogger(),
			DetectWrite: func() bool { panic("simulated detector failure") },
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_read_after_panic") {
			t.Fatalf("expected read token, got %q", stdout.String())
		}
		if read.Calls() != 1 {
			t.Errorf("read calls = %d, want 1", read.Calls())
		}
	})

	t.Run("TM-G8: write mint failure still returns placeholder", func(t *testing.T) {
		read := &stubMinter{token: "should-not-be-used"}
		write := &stubMinter{err: errors.New("simulated write mint failure")}
		cfg := Config{
			MintRead:    read.Mint,
			MintWrite:   write.Mint,
			Logger:      discardLogger(),
			DetectWrite: alwaysWrite,
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=rein-placeholder-mint-failed") {
			t.Fatalf("expected TM-G8 placeholder, got %q", stdout.String())
		}
	})
}

// TestReadCache covers the CP3 file-based read-token cache.
func TestReadCache(t *testing.T) {
	helperStdin := "protocol=https\nhost=github.com\n\n"

	t.Run("cache hit serves cached token without minting", func(t *testing.T) {
		dir := t.TempDir()
		cache := filepath.Join(dir, "cache.json")
		writeCacheFile(t, cache, "ghs_cached_token", time.Now().Add(45*time.Minute))

		read := &stubMinter{token: "should-not-be-used-fresh"}
		cfg := Config{
			MintRead:      read.Mint,
			Logger:        discardLogger(),
			ReadCachePath: cache,
		}

		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_cached_token") {
			t.Errorf("expected cached token in stdout, got %q", stdout.String())
		}
		if read.Calls() != 0 {
			t.Errorf("minter should not have been called; calls = %d", read.Calls())
		}
	})

	t.Run("expired cache triggers fresh mint", func(t *testing.T) {
		dir := t.TempDir()
		cache := filepath.Join(dir, "cache.json")
		writeCacheFile(t, cache, "ghs_expired_token", time.Now().Add(-10*time.Second))

		read := &stubMinter{token: "ghs_fresh_token"}
		cfg := Config{
			MintRead:      read.Mint,
			Logger:        discardLogger(),
			ReadCachePath: cache,
		}

		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_fresh_token") {
			t.Errorf("expected fresh token in stdout, got %q", stdout.String())
		}
		if read.Calls() != 1 {
			t.Errorf("minter calls = %d, want 1", read.Calls())
		}
	})

	t.Run("corrupt cache triggers fresh mint", func(t *testing.T) {
		dir := t.TempDir()
		cache := filepath.Join(dir, "cache.json")
		mustWrite(t, cache, "{not-valid-json")

		read := &stubMinter{token: "ghs_fresh_after_corrupt"}
		cfg := Config{
			MintRead:      read.Mint,
			Logger:        discardLogger(),
			ReadCachePath: cache,
		}

		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_fresh_after_corrupt") {
			t.Errorf("expected fresh token in stdout, got %q", stdout.String())
		}
	})

	t.Run("fresh mint is written to cache with 0600", func(t *testing.T) {
		dir := t.TempDir()
		cache := filepath.Join(dir, "cache.json")

		read := &stubMinter{token: "ghs_will_be_cached"}
		cfg := Config{
			MintRead:      read.Mint,
			Logger:        discardLogger(),
			ReadCachePath: cache,
		}

		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(helperStdin), &stdout, cfg); err != nil {
			t.Fatalf("RunCredentialHelper: %v", err)
		}
		body, err := os.ReadFile(cache)
		if err != nil {
			t.Fatalf("cache should exist after mint: %v", err)
		}
		var c tokencache.Entry
		if err := json.Unmarshal(body, &c); err != nil {
			t.Fatalf("cache body not valid JSON: %v", err)
		}
		if c.Token != "ghs_will_be_cached" {
			t.Errorf("cached token = %q, want %q", c.Token, "ghs_will_be_cached")
		}
		info, err := os.Stat(cache)
		if err != nil {
			t.Fatalf("stat cache: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("cache file mode = %o, want 0600", mode)
		}
	})
}

// TestRevokeOnStoreErase covers the CP3.6 hardening: write tokens get
// revoked when git signals it's done with them, tightening the effective
// TTL well below GitHub's 1h floor. Cached read tokens must NOT be
// revoked (they stay reusable for the session).
func TestRevokeOnStoreErase(t *testing.T) {
	helperStdin := func(token string) string {
		return "protocol=https\nhost=github.com\nusername=x-access-token\npassword=" + token + "\n\n"
	}

	stubRevoker := func(t *testing.T) (func(ctx context.Context, tok string) error, *atomic.Int64, *struct {
		mu     sync.Mutex
		tokens []string
	}) {
		var n atomic.Int64
		seen := &struct {
			mu     sync.Mutex
			tokens []string
		}{}
		fn := func(ctx context.Context, tok string) error {
			n.Add(1)
			seen.mu.Lock()
			seen.tokens = append(seen.tokens, tok)
			seen.mu.Unlock()
			return nil
		}
		return fn, &n, seen
	}

	for _, action := range []string{"store", "erase"} {
		t.Run(action+": non-cached token (write) gets revoked", func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "cache.json")
			writeCacheFile(t, cache, "ghs_the_cached_read", time.Now().Add(45*time.Minute))

			revoke, calls, seen := stubRevoker(t)
			cfg := Config{
				MintRead:      (&stubMinter{token: "unused"}).Mint,
				Logger:        discardLogger(),
				ReadCachePath: cache,
				Revoke:        revoke,
			}
			var stdout bytes.Buffer
			err := RunCredentialHelper(action, strings.NewReader(helperStdin("ghs_fresh_write_token")), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("revoke calls = %d, want 1", calls.Load())
			}
			if seen.tokens[0] != "ghs_fresh_write_token" {
				t.Errorf("revoked token = %q, want %q", seen.tokens[0], "ghs_fresh_write_token")
			}
		})

		t.Run(action+": cached read token is NOT revoked", func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "cache.json")
			writeCacheFile(t, cache, "ghs_cached_read", time.Now().Add(45*time.Minute))

			revoke, calls, _ := stubRevoker(t)
			cfg := Config{
				MintRead:      (&stubMinter{token: "unused"}).Mint,
				Logger:        discardLogger(),
				ReadCachePath: cache,
				Revoke:        revoke,
			}
			var stdout bytes.Buffer
			err := RunCredentialHelper(action, strings.NewReader(helperStdin("ghs_cached_read")), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper: %v", err)
			}
			if calls.Load() != 0 {
				t.Errorf("revoke calls = %d, want 0 (cached read must not be revoked)", calls.Load())
			}
		})

		t.Run(action+": empty password attr is a no-op", func(t *testing.T) {
			revoke, calls, _ := stubRevoker(t)
			cfg := Config{
				MintRead: (&stubMinter{token: "unused"}).Mint,
				Logger:   discardLogger(),
				Revoke:   revoke,
			}
			var stdout bytes.Buffer
			err := RunCredentialHelper(action, strings.NewReader("protocol=https\nhost=github.com\n\n"), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper: %v", err)
			}
			if calls.Load() != 0 {
				t.Errorf("revoke calls = %d, want 0", calls.Load())
			}
		})

		t.Run(action+": non-github.com host is a no-op", func(t *testing.T) {
			revoke, calls, _ := stubRevoker(t)
			cfg := Config{
				MintRead: (&stubMinter{token: "unused"}).Mint,
				Logger:   discardLogger(),
				Revoke:   revoke,
			}
			var stdout bytes.Buffer
			err := RunCredentialHelper(action, strings.NewReader("protocol=https\nhost=gitlab.com\nusername=x\npassword=ghs_some_token\n\n"), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper: %v", err)
			}
			if calls.Load() != 0 {
				t.Errorf("revoke calls = %d, want 0 for non-github host", calls.Load())
			}
		})

		t.Run(action+": revoke failure does not propagate", func(t *testing.T) {
			cfg := Config{
				MintRead: (&stubMinter{token: "unused"}).Mint,
				Logger:   discardLogger(),
				Revoke: func(ctx context.Context, tok string) error {
					return errors.New("simulated GitHub 503")
				},
			}
			var stdout bytes.Buffer
			err := RunCredentialHelper(action, strings.NewReader(helperStdin("ghs_some_token")), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper returned error on revoke failure: %v (must be best-effort)", err)
			}
		})

		t.Run(action+": no Revoke configured is a no-op", func(t *testing.T) {
			cfg := Config{
				MintRead: (&stubMinter{token: "unused"}).Mint,
				Logger:   discardLogger(),
				// Revoke intentionally nil
			}
			var stdout bytes.Buffer
			err := RunCredentialHelper(action, strings.NewReader(helperStdin("ghs_some_token")), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper: %v", err)
			}
		})
	}
}

// TestScopeCeiling covers the CP4 InScope predicate: in-scope requests
// proceed normally; out-of-scope requests get the TM-G8 placeholder; the
// empty-path policy chooses between allow (default) and refuse.
func TestScopeCeiling(t *testing.T) {
	makeStdin := func(path string) string {
		s := "protocol=https\nhost=github.com\n"
		if path != "" {
			s += "path=" + path + "\n"
		}
		return s + "\n"
	}

	t.Run("in-scope repo proceeds to read mint", func(t *testing.T) {
		read := &stubMinter{token: "ghs_read"}
		cfg := Config{
			MintRead: read.Mint,
			Logger:   discardLogger(),
			InScope:  func(r string) bool { return r == "owner/in-scope-repo" },
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(makeStdin("owner/in-scope-repo.git")), &stdout, cfg); err != nil {
			t.Fatalf("err: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_read") {
			t.Errorf("expected real token, got %q", stdout.String())
		}
		if read.Calls() != 1 {
			t.Errorf("read mint calls = %d, want 1", read.Calls())
		}
	})

	t.Run("out-of-scope repo refused with TM-G8 placeholder", func(t *testing.T) {
		read := &stubMinter{token: "should-not-be-used"}
		cfg := Config{
			MintRead: read.Mint,
			Logger:   discardLogger(),
			InScope:  func(r string) bool { return r == "owner/in-scope-repo" },
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(makeStdin("owner/other-repo.git")), &stdout, cfg); err != nil {
			t.Fatalf("err: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=rein-placeholder-out-of-scope") {
			t.Errorf("expected out-of-scope placeholder, got %q", stdout.String())
		}
		if read.Calls() != 0 {
			t.Errorf("read mint calls = %d, want 0 (refusal must not call mint)", read.Calls())
		}
	})

	t.Run("empty path with EmptyPathScope=allow (default)", func(t *testing.T) {
		read := &stubMinter{token: "ghs_read"}
		cfg := Config{
			MintRead: read.Mint,
			Logger:   discardLogger(),
			InScope:  func(r string) bool { return false }, // would refuse if checked
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(makeStdin("")), &stdout, cfg); err != nil {
			t.Fatalf("err: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_read") {
			t.Errorf("default allow should have proceeded to mint, got %q", stdout.String())
		}
	})

	t.Run("empty path with EmptyPathScope=refuse", func(t *testing.T) {
		read := &stubMinter{token: "should-not-be-used"}
		cfg := Config{
			MintRead:       read.Mint,
			Logger:         discardLogger(),
			InScope:        func(r string) bool { return true },
			EmptyPathScope: "refuse",
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(makeStdin("")), &stdout, cfg); err != nil {
			t.Fatalf("err: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=rein-placeholder-out-of-scope") {
			t.Errorf("refuse mode should have placeholder'd, got %q", stdout.String())
		}
	})

	t.Run("nil InScope disables scope enforcement (pre-CP4 behavior)", func(t *testing.T) {
		read := &stubMinter{token: "ghs_read"}
		cfg := Config{
			MintRead: read.Mint,
			Logger:   discardLogger(),
			// InScope intentionally nil
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(makeStdin("any/repo.git")), &stdout, cfg); err != nil {
			t.Fatalf("err: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_read") {
			t.Errorf("nil InScope should proceed normally, got %q", stdout.String())
		}
	})

	t.Run("scope refusal does NOT trigger write mint even if DetectWrite=true", func(t *testing.T) {
		read := &stubMinter{token: "should-not-be-used"}
		write := &stubMinter{token: "should-not-be-used-write"}
		cfg := Config{
			MintRead:    read.Mint,
			MintWrite:   write.Mint,
			Logger:      discardLogger(),
			DetectWrite: alwaysWrite,
			InScope:     func(r string) bool { return false },
		}
		var stdout bytes.Buffer
		if err := RunCredentialHelper("get", strings.NewReader(makeStdin("any/repo.git")), &stdout, cfg); err != nil {
			t.Fatalf("err: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=rein-placeholder-out-of-scope") {
			t.Errorf("expected out-of-scope placeholder, got %q", stdout.String())
		}
		if write.Calls() != 0 || read.Calls() != 0 {
			t.Errorf("expected no mints; got read=%d write=%d", read.Calls(), write.Calls())
		}
	})

	t.Run("path normalization: case-insensitive + .git suffix tolerated", func(t *testing.T) {
		read := &stubMinter{token: "ghs_read"}
		seen := []string{}
		cfg := Config{
			MintRead: read.Mint,
			Logger:   discardLogger(),
			InScope: func(r string) bool {
				seen = append(seen, r)
				return r == "TomHennen/agentcreds-validation-a"
			},
		}
		var stdout bytes.Buffer
		// Path contains uppercase + .git; pathToRepo doesn't lowercase
		// (the caller's InScope does). Confirm caller sees the raw form.
		if err := RunCredentialHelper("get", strings.NewReader(makeStdin("TomHennen/agentcreds-validation-a.git")), &stdout, cfg); err != nil {
			t.Fatalf("err: %v", err)
		}
		if !strings.Contains(stdout.String(), "password=ghs_read") {
			t.Errorf("expected real token, got %q", stdout.String())
		}
		if len(seen) != 1 || seen[0] != "TomHennen/agentcreds-validation-a" {
			t.Errorf("InScope received %v, want [TomHennen/agentcreds-validation-a]", seen)
		}
	})
}

// TestParseAttrsURL confirms the url= backfill matches what git sends in the
// modern protocol (gitcredentials(7)).
func TestParseAttrsURL(t *testing.T) {
	in := "url=https://github.com/TomHennen/agentcreds-validation-a.git\n\n"
	attrs, err := parseAttrs(strings.NewReader(in), discardLogger())
	if err != nil {
		t.Fatalf("parseAttrs error: %v", err)
	}
	if attrs["protocol"] != "https" {
		t.Errorf("protocol = %q, want %q", attrs["protocol"], "https")
	}
	if attrs["host"] != "github.com" {
		t.Errorf("host = %q, want %q", attrs["host"], "github.com")
	}
	if attrs["path"] != "TomHennen/agentcreds-validation-a.git" {
		t.Errorf("path = %q, want %q", attrs["path"], "TomHennen/agentcreds-validation-a.git")
	}
}

// TestRunCredentialHelperRequiresConfig confirms missing config returns
// a programming-error, not a silent no-op.
func TestRunCredentialHelperRequiresConfig(t *testing.T) {
	read := &stubMinter{token: "x"}
	t.Run("missing logger", func(t *testing.T) {
		var stdout bytes.Buffer
		err := RunCredentialHelper("get", strings.NewReader(""), &stdout, Config{
			MintRead: read.Mint,
		})
		if err == nil {
			t.Fatal("expected error for missing Logger")
		}
	})
	t.Run("missing read minter", func(t *testing.T) {
		var stdout bytes.Buffer
		err := RunCredentialHelper("get", strings.NewReader(""), &stdout, Config{
			Logger: discardLogger(),
		})
		if err == nil {
			t.Fatal("expected error for missing MintRead")
		}
	})
	t.Run("DetectWrite set without MintWrite", func(t *testing.T) {
		var stdout bytes.Buffer
		err := RunCredentialHelper("get", strings.NewReader(""), &stdout, Config{
			MintRead:    read.Mint,
			Logger:      discardLogger(),
			DetectWrite: alwaysWrite,
		})
		if err == nil {
			t.Fatal("expected error for DetectWrite-enabled config without MintWrite")
		}
	})
}

// --- helpers ---

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeCacheFile(t *testing.T, path, token string, expiresAt time.Time) {
	t.Helper()
	body, err := json.Marshal(tokencache.Entry{Token: token, ExpiresAt: expiresAt})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mustWrite(t, path, string(body))
}
