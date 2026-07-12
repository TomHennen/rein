package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
)

// TestWriteTokenDroppedOnScopeChange proves the #69 cache-busting: a write
// token minted at one scope is NOT reused after the ceiling grows, so an
// approved expansion actually reaches the mint (a stale narrow token would
// 403 on the new repo).
func TestWriteTokenDroppedOnScopeChange(t *testing.T) {
	var mints int32
	scope := "a"
	core := NewSessionCore(SessionConfig{
		ScopeKey: func() string { return scope },
		MintWrite: func(context.Context) (string, time.Time, error) {
			atomic.AddInt32(&mints, 1)
			return "wtok", time.Now().Add(time.Hour), nil
		},
	})
	// Two writes at scope "a" => one mint (memoized).
	core.Serve(context.Background(), brokercore.Request{Repo: "o/a", WriteIntent: true})
	core.Serve(context.Background(), brokercore.Request{Repo: "o/a", WriteIntent: true})
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("same scope must reuse the token, mints=%d", got)
	}
	// The ceiling grows: the memoized token is now scoped too narrow.
	scope = "a,b"
	core.Serve(context.Background(), brokercore.Request{Repo: "o/b", WriteIntent: true})
	if got := atomic.LoadInt32(&mints); got != 2 {
		t.Fatalf("a scope change must force a re-mint, mints=%d", got)
	}
}

// stubCache is a trivial ReadCache for the scope-keyed wrapper test.
type stubCache struct {
	token string
	puts  int32
}

func (c *stubCache) Get(time.Duration) (string, bool) {
	if c.token == "" {
		return "", false
	}
	return c.token, true
}
func (c *stubCache) Put(token string, _ time.Time) {
	atomic.AddInt32(&c.puts, 1)
	c.token = token
}

// TestReadCacheBustsOnScopeChange proves the same for READS: cloning a newly
// approved repo is a read, and a read token cached at the old scope 404s on
// it. The scope-keyed wrapper treats it as a miss once the ceiling changes.
func TestReadCacheBustsOnScopeChange(t *testing.T) {
	scope := "a"
	inner := &stubCache{}
	wrapped := scopedReadCache(inner, func() string { return scope })

	wrapped.Put("rtok-a", time.Now().Add(time.Hour))
	if tok, ok := wrapped.Get(0); !ok || tok != "rtok-a" {
		t.Fatalf("same scope must hit, got %q ok=%v", tok, ok)
	}
	scope = "a,b"
	if _, ok := wrapped.Get(0); ok {
		t.Error("a scope change must be a cache MISS (the token predates the widening)")
	}
	// After a re-mint at the new scope, it hits again.
	wrapped.Put("rtok-ab", time.Now().Add(time.Hour))
	if tok, ok := wrapped.Get(0); !ok || tok != "rtok-ab" {
		t.Fatalf("re-mint at the new scope must hit, got %q ok=%v", tok, ok)
	}
}

func TestScopedReadCacheNilStaysNil(t *testing.T) {
	if scopedReadCache(nil, func() string { return "x" }) != nil {
		t.Error("a nil inner cache must stay nil (caching disabled)")
	}
}
