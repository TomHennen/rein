package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
)

// TestConcurrentWriteDedup drives N parallel requests (a mix of reads and
// writes on the same repo) through one core and asserts the write-mint and
// approval dedup hold under contention: exactly one write mint and one approval
// (run -race). Reads are served throughout.
func TestConcurrentWriteDedup(t *testing.T) {
	var writeMints, approvals int32
	core := NewSessionCore(SessionConfig{
		MintRead: func(context.Context) (string, time.Time, error) {
			return "rtok", time.Now().Add(time.Hour), nil
		},
		MintWrite: func(context.Context) (string, time.Time, error) {
			atomic.AddInt32(&writeMints, 1)
			return "wtok", time.Now().Add(time.Hour), nil
		},
		Approve:   func(string) bool { atomic.AddInt32(&approvals, 1); return true },
		ReadCache: NewMemCache(),
	})

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		write := i%2 == 0
		go func(write bool) {
			defer wg.Done()
			cred := core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: write})
			want := "rtok"
			if write {
				want = "wtok"
			}
			if cred.Password != want {
				t.Errorf("write=%v got %q, want %q", write, cred.Password, want)
			}
		}(write)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&writeMints); got != 1 {
		t.Errorf("write mints = %d, want exactly 1 under contention", got)
	}
	if got := atomic.LoadInt32(&approvals); got != 1 {
		t.Errorf("approvals = %d, want exactly 1 under contention", got)
	}
}

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

// TestApprovalRunScoped verifies write approval is RUN-SCOPED, not per repo:
// the first in-scope write prompts once (naming the triggering repo), and every
// later in-scope write — any repo, git OR GraphQL (repo-less, key "") — proceeds
// without re-prompting. brokercore runs inScope before confirm, so only in-scope
// requests ever reach here.
func TestApprovalRunScoped(t *testing.T) {
	var prompts int32
	var firstRepo string
	core := NewSessionCore(SessionConfig{
		MintWrite: func(ctx context.Context) (string, time.Time, error) {
			return "wtok", time.Now().Add(time.Hour), nil
		},
		Approve: func(repo string) bool {
			if atomic.AddInt32(&prompts, 1) == 1 {
				firstRepo = repo
			}
			return true
		},
	})

	// (a) write to repoA then repoB (both in the session's scope) → ONE prompt.
	core.Serve(context.Background(), brokercore.Request{Repo: "o/a", WriteIntent: true})
	core.Serve(context.Background(), brokercore.Request{Repo: "o/b", WriteIntent: true})
	// (b) a GraphQL mutation (repo-less, empty key) after the approved push →
	//     NO second prompt.
	core.Serve(context.Background(), brokercore.Request{Repo: "", WriteIntent: true})

	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("prompts = %d, want exactly 1 (run-scoped)", got)
	}
	if firstRepo != "o/a" {
		t.Errorf("first prompt named repo %q, want the triggering repo o/a", firstRepo)
	}
}

// TestApprovalInfoRefsThenReceivePack keeps the existing invariant: a single
// git push (info/refs advertisement + receive-pack) prompts at most once.
func TestApprovalInfoRefsThenReceivePack(t *testing.T) {
	var prompts int32
	core := NewSessionCore(SessionConfig{
		MintWrite: func(ctx context.Context) (string, time.Time, error) {
			return "wtok", time.Now().Add(time.Hour), nil
		},
		Approve: func(string) bool { atomic.AddInt32(&prompts, 1); return true },
	})
	// info/refs?service=git-receive-pack then the receive-pack POST — same repo.
	core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: true})
	core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: true})
	if got := atomic.LoadInt32(&prompts); got != 1 {
		t.Errorf("prompts = %d, want exactly 1 for one push", got)
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
