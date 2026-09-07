package approvals

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/tokencache"
)

// seedApprovedRun writes the three per-run artifacts of a run that has
// declared, been confirmed, and minted a write token.
func seedApprovedRun(t *testing.T, dir, runID string, sess session.Session) string {
	t.Helper()
	sig := SignatureOf(sess)
	if err := WriteRunContext(dir, runID, RunContext{
		Session:       sess,
		SessionFile:   "/etc/rein/dev-session.yaml",
		Direct:        true,
		RunPID:        os.Getpid(),
		PendingIssue:  &ConfirmedIssue{Number: 7, Repo: "o/r", Title: "t", CanonicalURL: "u"},
		PendingNotice: &PendingNotice{Repo: "o/r", Issue: 7},
		WrittenAt:     time.Now(),
	}); err != nil {
		t.Fatalf("write run context: %v", err)
	}
	ci := ConfirmedIssue{Number: 7, Repo: "o/r", Title: "t", CanonicalURL: "u", ConfirmedAt: time.Now()}
	if err := AppendConfirmedIssue(dir, runID, sig, sess.ID, ci, time.Hour); err != nil {
		t.Fatalf("append confirmed issue: %v", err)
	}
	if err := AppendWriteToken(dir, runID, tokencache.Entry{Token: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("append write token: %v", err)
	}
	return sig
}

// TestClearConfirmedIssues is the #190 contract: the run's write approval is
// withdrawn, but everything the NEXT declare needs survives — the session
// snapshot, the pending-notice state, and (until the caller has revoked them)
// the write-token ledger.
func TestClearConfirmedIssues(t *testing.T) {
	dir := t.TempDir()
	sess := session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
	sig := seedApprovedRun(t, dir, "run-1", sess)

	if len(ConfirmedIssues(dir, "run-1", sig)) != 1 {
		t.Fatal("setup: the run should start with one confirmed issue")
	}

	if err := ClearConfirmedIssues(dir, "run-1"); err != nil {
		t.Fatalf("ClearConfirmedIssues: %v", err)
	}
	if got := ConfirmedIssues(dir, "run-1", sig); got != nil {
		t.Errorf("confirmed issues after clear = %+v, want nil (writes must be locked)", got)
	}

	// RunContext is KEPT: the out-of-process grant surfaces need it to render
	// the next declare's prompt.
	rc, err := ReadRunContext(dir, "run-1")
	if err != nil {
		t.Fatalf("run context must survive an idle re-attestation: %v", err)
	}
	if rc.Session.ID != "s1" {
		t.Errorf("run context lost state across the clear: %+v", rc)
	}
	// The ledger is a SEPARATE artifact so the caller can revoke before clearing.
	if _, err := ReadWriteTokens(dir, "run-1"); err != nil {
		t.Errorf("ClearConfirmedIssues must not touch the write-token ledger: %v", err)
	}

	if err := ClearWriteTokens(dir, "run-1"); err != nil {
		t.Fatalf("ClearWriteTokens: %v", err)
	}
	if _, err := ReadWriteTokens(dir, "run-1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ledger after ClearWriteTokens: %v, want not-exist", err)
	}
	if _, err := ReadRunContext(dir, "run-1"); err != nil {
		t.Errorf("ClearWriteTokens must not touch the run context: %v", err)
	}

	// Idempotent: a second pass over an already-cleared run is not an error.
	if err := ClearConfirmedIssues(dir, "run-1"); err != nil {
		t.Errorf("second ClearConfirmedIssues: %v", err)
	}
	if err := ClearWriteTokens(dir, "run-1"); err != nil {
		t.Errorf("second ClearWriteTokens: %v", err)
	}
}

// TestClearPendingDeclaration: the stale declaration snapshot is part of the
// approval surface. `rein approval grant --run-id X` and the tmux popup render
// their prompt FROM it without fetching, so a PendingIssue left behind after a
// withdrawal would let the human re-open writes for an issue nobody
// re-declared — skipping exactly the ceremony the withdrawal forces. Everything
// the next declare needs (Session, SessionFile, Direct, RunPID) must survive.
func TestClearPendingDeclaration(t *testing.T) {
	dir := t.TempDir()
	sess := session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
	seedApprovedRun(t, dir, "run-1", sess)

	before, err := ReadRunContext(dir, "run-1")
	if err != nil {
		t.Fatalf("read seeded context: %v", err)
	}
	if before.PendingIssue == nil || before.PendingNotice == nil {
		t.Fatal("setup: the seeded run context must carry a stale pending declaration")
	}

	if err := ClearPendingDeclaration(dir, "run-1"); err != nil {
		t.Fatalf("ClearPendingDeclaration: %v", err)
	}
	got, err := ReadRunContext(dir, "run-1")
	if err != nil {
		t.Fatalf("run context must survive: %v", err)
	}
	if got.PendingIssue != nil {
		t.Errorf("PendingIssue = %+v, want nil — a grant could re-open writes from it", got.PendingIssue)
	}
	if got.PendingNotice != nil {
		t.Errorf("PendingNotice = %+v, want nil", got.PendingNotice)
	}
	if got.Session.ID != before.Session.ID || got.SessionFile != before.SessionFile ||
		got.Direct != before.Direct || got.RunPID != before.RunPID {
		t.Errorf("clearing the pending declaration dropped context the next declare needs:\n got %+v\nwant %+v", got, before)
	}

	// Idempotent, and a run with no context at all is not an error.
	if err := ClearPendingDeclaration(dir, "run-1"); err != nil {
		t.Errorf("second ClearPendingDeclaration: %v", err)
	}
	if err := ClearPendingDeclaration(dir, "run-absent"); err != nil {
		t.Errorf("ClearPendingDeclaration on a run with no context: %v", err)
	}
}

// TestAppendConfirmedIssueAfterClear: the re-declare after an idle lock builds
// a FRESH record — only the newly confirmed issue, a current ApprovedAt — so a
// cleared confirmation can never be resurrected by a later append.
func TestAppendConfirmedIssueAfterClear(t *testing.T) {
	dir := t.TempDir()
	sess := session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
	sig := seedApprovedRun(t, dir, "run-1", sess)
	first, err := ReadApproval(dir, "run-1")
	if err != nil {
		t.Fatalf("read seeded approval: %v", err)
	}

	if err := ClearConfirmedIssues(dir, "run-1"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	time.Sleep(time.Millisecond) // so the fresh ApprovedAt is distinguishable
	next := ConfirmedIssue{Number: 9, Repo: "o/r", Title: "next", CanonicalURL: "u9", ConfirmedAt: time.Now()}
	if err := AppendConfirmedIssue(dir, "run-1", sig, sess.ID, next, time.Hour); err != nil {
		t.Fatalf("re-declare after clear: %v", err)
	}

	got := ConfirmedIssues(dir, "run-1", sig)
	if len(got) != 1 || got[0].Number != 9 {
		t.Fatalf("confirmed set after re-declare = %+v, want exactly issue 9 (7 must NOT come back)", got)
	}
	rec, err := ReadApproval(dir, "run-1")
	if err != nil {
		t.Fatalf("read re-declared approval: %v", err)
	}
	if !rec.ApprovedAt.After(first.ApprovedAt) {
		t.Errorf("ApprovedAt = %s, want a fresh timestamp after %s (the record must be new, not revived)",
			rec.ApprovedAt, first.ApprovedAt)
	}
}
