package approvals

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/tokencache"
)

// Concurrent helper invocations within one run (e.g. parallel pushes) append
// to the same ledger. O_APPEND keeps each well-below-PIPE_BUF line atomic, so
// every entry must survive intact with no interleaving/corruption.
func TestAppendWriteToken_ConcurrentAppendsAtomic(t *testing.T) {
	dir := t.TempDir()
	runID := "run-concurrent"
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := fmt.Sprintf("tok-%02d", i)
			if err := AppendWriteToken(dir, runID, tokencache.Entry{Token: tok, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := ReadWriteTokens(dir, runID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d entries, want %d (lost/corrupted concurrent appends)", len(got), n)
	}
	seen := map[string]bool{}
	for _, e := range got {
		if e.Token == "" {
			t.Errorf("empty/corrupt token entry: %+v", e)
		}
		seen[e.Token] = true
	}
	if len(seen) != n {
		t.Errorf("expected %d distinct tokens, got %d", n, len(seen))
	}
}

func TestWriteTokenLedger_AppendReadClear(t *testing.T) {
	dir := t.TempDir()
	runID := "run-abc"

	// Missing ledger reads as os.ErrNotExist (a clean "no writes" signal).
	if _, err := ReadWriteTokens(dir, runID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist for missing ledger, got %v", err)
	}

	exp1 := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	exp2 := exp1.Add(2 * time.Hour)
	if err := AppendWriteToken(dir, runID, tokencache.Entry{Token: "tok-1", ExpiresAt: exp1}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := AppendWriteToken(dir, runID, tokencache.Entry{Token: "tok-2", ExpiresAt: exp2}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	// Ledger file is mode 0600.
	info, err := os.Stat(WriteTokenLedgerPath(dir, runID))
	if err != nil {
		t.Fatalf("stat ledger: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("ledger mode = %o, want 0600", info.Mode().Perm())
	}

	got, err := ReadWriteTokens(dir, runID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Token != "tok-1" || got[1].Token != "tok-2" {
		t.Errorf("entries out of order or wrong: %+v", got)
	}
	if !got[0].ExpiresAt.Equal(exp1) {
		t.Errorf("entry 1 expiry = %v, want %v", got[0].ExpiresAt, exp1)
	}

	// ClearRun removes the ledger (along with the other per-run files).
	if err := ClearRun(dir, runID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := ReadWriteTokens(dir, runID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ledger gone after ClearRun, got %v", err)
	}
}

func TestReadWriteTokens_SkipsTornFinalLine(t *testing.T) {
	dir := t.TempDir()
	runID := "run-torn"
	if err := AppendWriteToken(dir, runID, tokencache.Entry{Token: "good", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Simulate a process killed mid-append: a partial, unparseable line.
	f, err := os.OpenFile(WriteTokenLedgerPath(dir, runID), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"token":"trunca`); err != nil {
		t.Fatalf("write torn: %v", err)
	}
	f.Close()

	got, err := ReadWriteTokens(dir, runID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].Token != "good" {
		t.Errorf("expected only the well-formed entry, got %+v", got)
	}
}

// A run whose ONLY artifact is a write-token ledger (the write-capable
// role with no bound issue: ConfirmWrite is nil, so no runs/ or approvals/
// file is ever written) must still be enumerated by List — otherwise Sweep
// can never reap its orphaned token material.
func TestList_EnumeratesWritesOnlyRun(t *testing.T) {
	dir := t.TempDir()
	runID := "run-writesonly"
	if err := AppendWriteToken(dir, runID, tokencache.Entry{Token: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("append: %v", err)
	}
	list, err := List(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, st := range list {
		if st.RunID == runID {
			found = true
			if st.HasContext || st.HasApproval {
				t.Errorf("writes-only run should have no context/approval, got %+v", st)
			}
		}
	}
	if !found {
		t.Fatalf("List did not enumerate writes-only run %q", runID)
	}
}

// A writes-only run (no run context, no pid to probe) is judged by the age
// backstop against the ledger's mtime: a FRESH one is kept (it may belong to
// a still-live no-bound-issue run, and reaping it would clobber its
// not-yet-revoked tokens), while one older than maxAge is reaped.
func TestSweep_WritesOnlyRun_AgeBackstop(t *testing.T) {
	dir := t.TempDir()

	// Fresh ledger: must survive a sweep with a generous maxAge.
	fresh := "run-fresh"
	if err := AppendWriteToken(dir, fresh, tokencache.Entry{Token: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("append fresh: %v", err)
	}
	if err := Sweep(dir, time.Hour, time.Now()); err != nil {
		t.Fatalf("sweep fresh: %v", err)
	}
	if _, err := ReadWriteTokens(dir, fresh); err != nil {
		t.Fatalf("fresh writes-only ledger should survive sweep, got %v", err)
	}

	// Old ledger: with now advanced well past maxAge, it is reaped.
	old := "run-old"
	if err := AppendWriteToken(dir, old, tokencache.Entry{Token: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("append old: %v", err)
	}
	if err := Sweep(dir, time.Hour, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("sweep old: %v", err)
	}
	if _, err := ReadWriteTokens(dir, old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected aged orphaned ledger reaped by Sweep, got %v", err)
	}
}
