package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
)

// TestWriteMintCachedAcrossRequests verifies one mint covers repeated write
// requests within the run (until expiry).
func TestWriteMintCachedAcrossRequests(t *testing.T) {
	var mints int32
	core := NewSessionCore(SessionConfig{
		MintWrite: func(ctx context.Context) (string, time.Time, error) {
			atomic.AddInt32(&mints, 1)
			return "wtok", time.Now().Add(time.Hour), nil
		},
	})
	for i := 0; i < 3; i++ {
		cred := core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: true})
		if cred.Password != "wtok" {
			t.Fatalf("write %d: got %q", i, cred.Password)
		}
	}
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Errorf("write mints = %d, want 1 (cached)", got)
	}
}

// TestMintBackoffAfterRateLimit verifies a rate-limited mint opens a backoff
// window so the proxy stops hammering the API.
func TestMintBackoffAfterRateLimit(t *testing.T) {
	var mints int32
	core := NewSessionCore(SessionConfig{
		MintWrite: func(ctx context.Context) (string, time.Time, error) {
			atomic.AddInt32(&mints, 1)
			return "", time.Time{}, fmt.Errorf("403 secondary rate limit exceeded")
		},
		MintBackoff: time.Hour, // long enough that the window stays open in-test
	})
	// First write: attempts the mint, fails, opens backoff → placeholder.
	c1 := core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: true})
	if c1.Password != brokercore.PlaceholderMintFailed {
		t.Fatalf("first: got %q, want mint-failed placeholder", c1.Password)
	}
	// Second write: backoff open → no new mint attempt, still placeholder.
	c2 := core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: true})
	if c2.Password != brokercore.PlaceholderMintFailed {
		t.Fatalf("second: got %q", c2.Password)
	}
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Errorf("mint attempts = %d, want 1 (backoff suppressed the second)", got)
	}
}

// TestApprovalMemoPerRepo verifies approvals are memoized per repo (one prompt
// per repo for the run), not globally.
func TestApprovalMemoPerRepo(t *testing.T) {
	prompts := map[string]int{}
	core := NewSessionCore(SessionConfig{
		MintWrite: func(ctx context.Context) (string, time.Time, error) {
			return "wtok", time.Now().Add(time.Hour), nil
		},
		Approve: func(repo string) bool { prompts[repo]++; return true },
	})
	for i := 0; i < 2; i++ {
		core.Serve(context.Background(), brokercore.Request{Repo: "o/a", WriteIntent: true})
		core.Serve(context.Background(), brokercore.Request{Repo: "o/b", WriteIntent: true})
	}
	if prompts["o/a"] != 1 || prompts["o/b"] != 1 {
		t.Errorf("prompts = %v, want one per repo", prompts)
	}
}

func TestIsRateLimited(t *testing.T) {
	cases := map[string]bool{
		"403 rate limit":                    true,
		"secondary rate limit hit":          true,
		"you have triggered an abuse limit": true,
		"429 too many requests":             true,
		"mint installation token: 500":      false,
		"connection refused":                false,
		// A plain 403 (not a rate limit) must NOT trigger backoff, or every
		// write 502s for 30s on a permission error.
		"403 forbidden: resource not accessible by integration": false,
		"repo403 not found": false,
	}
	for msg, want := range cases {
		if got := isRateLimited(fmt.Errorf("%s", msg)); got != want {
			t.Errorf("isRateLimited(%q) = %v, want %v", msg, got, want)
		}
	}
}
