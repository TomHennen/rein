package daemon

import (
	"testing"
	"time"
)

func TestMemReadCache(t *testing.T) {
	const skew = 30 * time.Second

	t.Run("miss when empty", func(t *testing.T) {
		c := NewMemReadCache()
		if tok, ok := c.Get(skew); ok {
			t.Fatalf("empty cache returned ok=true, tok=%q", tok)
		}
	})

	t.Run("hit when fresh", func(t *testing.T) {
		c := NewMemReadCache()
		c.Put("tok-fresh", time.Now().Add(10*time.Minute))
		tok, ok := c.Get(skew)
		if !ok || tok != "tok-fresh" {
			t.Fatalf("fresh token: got (%q,%v), want (tok-fresh,true)", tok, ok)
		}
	})

	t.Run("miss when within skew of expiry", func(t *testing.T) {
		c := NewMemReadCache()
		// Expires in 10s, but skew is 30s -> not enough headroom.
		c.Put("tok-soon", time.Now().Add(10*time.Second))
		if tok, ok := c.Get(skew); ok {
			t.Fatalf("token within skew returned ok=true, tok=%q", tok)
		}
	})

	t.Run("miss when already expired", func(t *testing.T) {
		c := NewMemReadCache()
		c.Put("tok-old", time.Now().Add(-1*time.Minute))
		if _, ok := c.Get(skew); ok {
			t.Fatal("expired token returned ok=true")
		}
	})

	t.Run("put overwrites", func(t *testing.T) {
		c := NewMemReadCache()
		c.Put("tok-1", time.Now().Add(1*time.Minute))
		c.Put("tok-2", time.Now().Add(10*time.Minute))
		tok, ok := c.Get(skew)
		if !ok || tok != "tok-2" {
			t.Fatalf("after overwrite: got (%q,%v), want (tok-2,true)", tok, ok)
		}
	})

	t.Run("zero skew accepts barely-valid token", func(t *testing.T) {
		c := NewMemReadCache()
		c.Put("tok", time.Now().Add(5*time.Second))
		if _, ok := c.Get(0); !ok {
			t.Fatal("zero skew should accept a token with time left")
		}
	})
}
