package daemon

import (
	"sync"
	"time"
)

// MemReadCache is an in-memory brokercore.ReadCache. In Phase 1 the daemon
// holds minted read tokens in memory only (design §6: "in daemon memory (no
// disk)"), so this replaces the file-backed cache (internal/tokencache) used
// in direct mode. It is safe for concurrent use.
type MemReadCache struct {
	mu        sync.RWMutex
	token     string
	expiresAt time.Time
	present   bool
}

// NewMemReadCache returns an empty cache.
func NewMemReadCache() *MemReadCache {
	return &MemReadCache{}
}

// Get returns the cached read token if one is present and has more than skew
// left before expiry; otherwise ok=false. Matches tokencache.Entry.Valid:
// time.Until(expiresAt) > skew.
func (c *MemReadCache) Get(skew time.Duration) (token string, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.present {
		return "", false
	}
	if time.Until(c.expiresAt) <= skew {
		return "", false
	}
	return c.token, true
}

// Put stores a freshly minted read token, overwriting any prior entry.
func (c *MemReadCache) Put(token string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	c.expiresAt = expiresAt
	c.present = true
}
