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

// TestRevokeRunWriteTokens_DedupesRepeatedToken is the issue #67 regression.
// The proxy memoizes ONE write token for the whole run, but brokercore appends
// a ledger entry on every write-serving request (cache hit included) — a run
// with 3 pushes ledgers the same token 6 times (info/refs + receive-pack each).
// Revoking it 6 times means one 204 and five 404s: five scary warnings and a
// nonsense "revoked 1 of 6". The ledger must be deduped by token value, so the
// token is revoked exactly ONCE and the counters read "revoked 1 of 1".
func TestRevokeRunWriteTokens_DedupesRepeatedToken(t *testing.T) {
	dir := t.TempDir()
	runID := "run-dupes"
	now := time.Now()
	exp := now.Add(time.Hour)

	// Exactly what the live run produced: the same token, six times.
	for i := 0; i < 6; i++ {
		mustAppend(t, dir, runID, "ghs_thesamememoizedtoken", exp)
	}

	rev := &recordingRevoke{}
	revokeRunWriteTokens(dir, runID, rev.fn, now)

	if len(rev.tokens) != 1 {
		t.Fatalf("revoke called %d times (%v); want exactly 1 — the memoized token is one token, not six", len(rev.tokens), rev.tokens)
	}
	if rev.tokens[0] != "ghs_thesamememoizedtoken" {
		t.Errorf("revoked %q, want the memoized token", rev.tokens[0])
	}
}

// A genuinely-distinct second token (a post-expiry or post-backoff re-mint)
// must still be revoked — dedupe must key on the token VALUE, not collapse the
// ledger to a single entry.
func TestRevokeRunWriteTokens_DedupeKeepsDistinctTokens(t *testing.T) {
	dir := t.TempDir()
	runID := "run-mixed"
	now := time.Now()
	exp := now.Add(time.Hour)

	mustAppend(t, dir, runID, "tok-A", exp)
	mustAppend(t, dir, runID, "tok-A", exp) // duplicate of A (cache hit)
	mustAppend(t, dir, runID, "tok-B", exp) // a real re-mint
	mustAppend(t, dir, runID, "tok-A", exp) // duplicate of A again
	mustAppend(t, dir, runID, "tok-B", exp) // duplicate of B

	rev := &recordingRevoke{}
	revokeRunWriteTokens(dir, runID, rev.fn, now)

	if len(rev.tokens) != 2 {
		t.Fatalf("revoked %v; want exactly the 2 DISTINCT tokens", rev.tokens)
	}
	got := map[string]bool{rev.tokens[0]: true, rev.tokens[1]: true}
	if !got["tok-A"] || !got["tok-B"] {
		t.Errorf("revoked %v; want both tok-A and tok-B exactly once each", rev.tokens)
	}
}

func mustAppend(t *testing.T, dir, runID, token string, exp time.Time) {
	t.Helper()
	if err := approvals.AppendWriteToken(dir, runID, tokencache.Entry{Token: token, ExpiresAt: exp}); err != nil {
		t.Fatalf("append %s: %v", token, err)
	}
}
