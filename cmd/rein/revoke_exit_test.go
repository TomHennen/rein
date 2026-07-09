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

// TestExpiryClearsLedgerSoExitRevokeIsNoop is the F1 regression: on expiry,
// OnExpire revokes the run's write tokens AND clears the ledger, so the deferred
// exit-time revokeRunWriteTokens re-reads an empty ledger and does NOT re-revoke
// already-dead tokens (which would print a spurious "exit-revoke failed" per
// token). This asserts the sequence the OnExpire closure performs.
func TestExpiryClearsLedgerSoExitRevokeIsNoop(t *testing.T) {
	dir := t.TempDir()
	runID := "run-expiry"
	now := time.Now()
	mustAppend(t, dir, runID, "tok-1", now.Add(time.Hour))
	mustAppend(t, dir, runID, "tok-2", now.Add(time.Hour))

	// (1) OnExpire step 1: revoke the ledger's tokens.
	expiry := &recordingRevoke{}
	revokeRunWriteTokens(dir, runID, expiry.fn, now)
	if len(expiry.tokens) != 2 {
		t.Fatalf("expiry revoke = %v, want both tokens", expiry.tokens)
	}
	// (2) OnExpire step 2: clear the ledger (the F1 fix).
	if err := approvals.ClearRun(dir, runID); err != nil {
		t.Fatalf("ClearRun: %v", err)
	}

	// (3) Deferred exit-time revoke re-runs — must be a clean no-op (no
	// re-revocation of the already-dead tokens, hence no spurious warning).
	exit := &recordingRevoke{err: errors.New("would-be-non-204")}
	revokeRunWriteTokens(dir, runID, exit.fn, now)
	if len(exit.tokens) != 0 {
		t.Errorf("exit-time revoke re-revoked %v after ClearRun; want none (would print spurious per-token warnings)", exit.tokens)
	}
}

func mustAppend(t *testing.T, dir, runID, token string, exp time.Time) {
	t.Helper()
	if err := approvals.AppendWriteToken(dir, runID, tokencache.Entry{Token: token, ExpiresAt: exp}); err != nil {
		t.Fatalf("append %s: %v", token, err)
	}
}
