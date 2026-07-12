package main

// Issue #67, the ledger-dedupe half, as a RUNNABLE demonstration.
//
// A single `rein run` doing writes ledgers the SAME memoized write token many
// times (the proxy memoizes one token; brokercore appends a ledger entry per
// write-serving request — info/refs + receive-pack per push). The live run that
// found this held SIX byte-identical copies of ONE token. Pre-#67, exit-revoke
// walked all six: revoked the token once (204), then hit 404 five more times,
// printing a scary warning each and the nonsense line "revoked 1 of 6".
//
// The fix dedupes by token VALUE at the consumer (revokeRunWriteTokens) and maps
// a revoke 404 to "already revoked". This demo reproduces the exact six-identical
// -entry ledger and captures the REAL stderr the exit path prints, showing the
// clean "revoked 1 of 1 write token(s) on exit" with zero warnings.
//
//	go test ./cmd/rein -run TestDemo_RevokeLedgerDedupe -v
//
// The eventual-consistency half of #67 (revoke is not a synchronous kill; a
// revoked token keeps authenticating for ~2-5s) is proven by the gated live test
// internal/githubapp/live_revoke_test.go (REIN_LIVE=1), whose poll can't regress
// into a false negative.

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
)

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns what fn wrote
// to os.Stderr. revokeRunWriteTokens writes its summary/warnings straight to
// os.Stderr, so this is the only way to observe the user-facing line.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	os.Stderr = orig
	w.Close()
	out := <-done
	r.Close()
	return out
}

func TestDemo_RevokeLedgerDedupe(t *testing.T) {
	dir := t.TempDir()
	runID := "run-demo-dupes"
	now := time.Now()
	exp := now.Add(time.Hour)

	// Exactly what the live run produced: ONE token, ledgered SIX times.
	const token = "ghs_theSameMemoizedWriteTokenAcross3Pushes"
	for i := 0; i < 6; i++ {
		mustAppend(t, dir, runID, token, exp)
	}

	entries, err := approvals.ReadWriteTokens(dir, runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	// The fix: dedupe by value, revoke once. recordingRevoke returns nil (the
	// one live token revokes with a 204); because dedupe collapses the six
	// entries to one, revoke is called exactly once and no 404s occur.
	rev := &recordingRevoke{}
	summary := captureStderr(t, func() {
		revokeRunWriteTokens(dir, runID, rev.fn, now)
	})
	summary = strings.TrimRight(summary, "\n")

	t.Log("")
	t.Log("issue #67 — the write-token ledger dedupe")
	t.Log("")
	t.Logf("  ledger entries written this run : %d (all the SAME memoized token)", len(entries))
	t.Logf("  distinct tokens                 : 1")
	t.Logf("  revoke() calls made             : %d", len(rev.tokens))
	t.Log("")
	t.Logf("  BEFORE #67 : revoked 1 of 6 write token(s) on exit   (+5 '404 already gone' warnings)")
	t.Logf("               ^ RECORDED pre-fix observation from the live run (not re-executed here;")
	t.Logf("                 reconstructing the old no-dedup loop would be over-building)")
	t.Logf("  AFTER  #67 : %s   <- LIVE, captured from this run's stderr", summary)
	t.Log("")

	// --- assertions --------------------------------------------------------
	if len(rev.tokens) != 1 {
		t.Errorf("revoke called %d times; want exactly 1 (six identical entries are ONE token)", len(rev.tokens))
	}
	if summary != "rein: revoked 1 of 1 write token(s) on exit" {
		t.Errorf("exit summary = %q; want the deduped 'revoked 1 of 1' line", summary)
	}
	// NOTE: no "summary must not contain 'warning'" assertion — with a
	// nil-returning fake revoke the warning branch (run.go:570) can't fire
	// regardless of the dedupe fix, so such a check would be tautological. The
	// two assertions above (revoke called once, exact 'revoked 1 of 1' string)
	// are what actually catch a dedupe regression.
}
