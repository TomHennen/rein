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
	// Since issue #35 the approval hook is a cheap record read consulted
	// on EVERY write-tier request (16 of the 32 here) — deliberately not
	// memoized, so a mid-run invalidation is never frozen out.
	if got := atomic.LoadInt32(&approvals); got != 16 {
		t.Errorf("approval-hook consults = %d, want one per write request (16)", got)
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

// TestApprovalHookConsultedPerWrite pins the #35 gate semantics: the
// Approve hook (production: a read of the run's confirmed-issue set) is
// consulted on EVERY write-tier request and is deliberately NOT memoized
// — a mid-run invalidation (session edit, transferred-issue removal)
// must take effect on the very next request, not be frozen out by a
// remembered first approval. "One ceremony per run" still holds because
// the ceremony lives at declare time, not in this hook.
func TestApprovalHookConsultedPerWrite(t *testing.T) {
	var consults int32
	gateOpen := atomic.Bool{}
	gateOpen.Store(true)
	core := NewSessionCore(SessionConfig{
		MintWrite: func(ctx context.Context) (string, time.Time, error) {
			return "wtok", time.Now().Add(time.Hour), nil
		},
		Approve: func(repo string) bool {
			atomic.AddInt32(&consults, 1)
			return gateOpen.Load()
		},
	})

	// Writes to two repos + a repo-less GraphQL mutation: all pass while
	// the confirmed set is non-empty.
	for _, repo := range []string{"o/a", "o/b", ""} {
		if cred := core.Serve(context.Background(), brokercore.Request{Repo: repo, WriteIntent: true}); cred.Password != "wtok" {
			t.Errorf("write to %q should be allowed while the gate is open, got %q", repo, cred.Password)
		}
	}
	if got := atomic.LoadInt32(&consults); got != 3 {
		t.Errorf("consults = %d, want one per write request (3)", got)
	}

	// Mid-run invalidation: the record went away (session edit / transfer
	// invalidation) — the very next write must be denied.
	gateOpen.Store(false)
	if cred := core.Serve(context.Background(), brokercore.Request{Repo: "o/a", WriteIntent: true}); cred.Password != brokercore.PlaceholderRefused {
		t.Errorf("write after invalidation must be refused, got %q", cred.Password)
	}
}

// TestOnePushOneMint keeps the user-visible cost invariant: a single git
// push (info/refs advertisement + receive-pack POST) MINTS once — the
// token memo, not the approval hook, is what dedupes.
func TestOnePushOneMint(t *testing.T) {
	var mints int32
	core := NewSessionCore(SessionConfig{
		MintWrite: func(ctx context.Context) (string, time.Time, error) {
			atomic.AddInt32(&mints, 1)
			return "wtok", time.Now().Add(time.Hour), nil
		},
		Approve: func(string) bool { return true },
	})
	// info/refs?service=git-receive-pack then the receive-pack POST — same repo.
	core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: true})
	core.Serve(context.Background(), brokercore.Request{Repo: "o/r", WriteIntent: true})
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Errorf("mints = %d, want exactly 1 for one push", got)
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
