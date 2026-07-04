package proxy

import (
	"sync"
	"time"
)

// MemCache is an in-memory brokercore.ReadCache. In sandboxed mode the minted
// read token lives only in the in-process broker host's memory (design §6: no
// disk), so this replaces the file-backed cache (internal/tokencache) used in
// direct mode. Build a FRESH one per session — with the mint scoped to the
// session's repo set (#10), a shared cache would cross-serve one session's
// repo-scoped token to another. Safe for concurrent use.
type MemCache struct {
	mu        sync.RWMutex
	token     string
	expiresAt time.Time
	present   bool
}

// NewMemCache returns an empty cache.
func NewMemCache() *MemCache { return &MemCache{} }

// Get returns the cached token if present and more than skew before expiry.
func (c *MemCache) Get(skew time.Duration) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.present || time.Until(c.expiresAt) <= skew {
		return "", false
	}
	return c.token, true
}

// Put stores a freshly minted read token, overwriting any prior entry.
func (c *MemCache) Put(token string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.expiresAt, c.present = token, expiresAt, true
}
