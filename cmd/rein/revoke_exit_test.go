package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/tokencache"
)

// recordingRevoke is a stub revokeTokenFunc that captures the tokens it was
// asked to revoke and can be made to fail.
type recordingRevoke struct {
	mu     sync.Mutex
	tokens []string
	err    error
}

func (r *recordingRevoke) fn(ctx context.Context, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = append(r.tokens, token)
	return r.err
}

func TestRevokeRunWriteTokens_RevokesValidSkipsExpired(t *testing.T) {
	dir := t.TempDir()
	runID := "run-1"
	now := time.Now()

	// Two valid (future expiry) and one already-expired token.
	mustAppend(t, dir, runID, "tok-valid-1", now.Add(30*time.Minute))
	mustAppend(t, dir, runID, "tok-expired", now.Add(-time.Minute))
	mustAppend(t, dir, runID, "tok-valid-2", now.Add(time.Hour))

	rev := &recordingRevoke{}
	revokeRunWriteTokens(dir, runID, rev.fn, now)

	if len(rev.tokens) != 2 {
		t.Fatalf("revoked %v, want exactly the 2 valid tokens", rev.tokens)
	}
	got := map[string]bool{rev.tokens[0]: true, rev.tokens[1]: true}
	if !got["tok-valid-1"] || !got["tok-valid-2"] {
		t.Errorf("expected both valid tokens revoked, got %v", rev.tokens)
	}
	if got["tok-expired"] {
		t.Errorf("expired token should not be revoked, got %v", rev.tokens)
	}
}

func TestRevokeRunWriteTokens_NoLedgerIsNoop(t *testing.T) {
	dir := t.TempDir()
	rev := &recordingRevoke{}
	// Must not panic or call revoke when there's no ledger for the run.
	revokeRunWriteTokens(dir, "absent-run", rev.fn, time.Now())
	if len(rev.tokens) != 0 {
		t.Errorf("expected no revokes for absent ledger, got %v", rev.tokens)
	}
}

func TestRevokeRunWriteTokens_RevokeFailureIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	runID := "run-fail"
	now := time.Now()
	mustAppend(t, dir, runID, "tok-a", now.Add(time.Hour))
	mustAppend(t, dir, runID, "tok-b", now.Add(time.Hour))

	// A failing revoke must not stop the loop (both attempted) and must not
	// panic / surface a fatal error.
	rev := &recordingRevoke{err: errors.New("network boom")}
	revokeRunWriteTokens(dir, runID, rev.fn, now)
	if len(rev.tokens) != 2 {
		t.Errorf("expected both tokens attempted despite failures, got %v", rev.tokens)
	}
}

func mustAppend(t *testing.T, dir, runID, token string, exp time.Time) {
	t.Helper()
	if err := approvals.AppendWriteToken(dir, runID, tokencache.Entry{Token: token, ExpiresAt: exp}); err != nil {
		t.Fatalf("append %s: %v", token, err)
	}
}
