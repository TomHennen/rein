package approvals

import (
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/session"
)

// TestResign_KeepsRunAuthorizedAfterPersist proves the direct-mode landmine
// is closed (issue #69): when the human persists the expansion repo mid-run,
// the session signature changes, and WITHOUT Resign the credential helper's
// next reload would invalidate the very approval just given. Resign re-keys
// the record to the new session so the run stays authorized.
func TestResign_KeepsRunAuthorizedAfterPersist(t *testing.T) {
	dir := t.TempDir()
	runID := "run"
	before := session.Session{ID: "s", Role: "implement", Repos: []string{"TomH/a"}}
	after := session.Session{ID: "s", Role: "implement", Repos: []string{"TomH/a", "TomH/b"}}
	oldSig := SignatureOf(before)
	newSig := SignatureOf(after)

	// The human approved the expansion issue on repo b.
	if err := AppendConfirmedIssue(dir, runID, oldSig, before.ID,
		ConfirmedIssue{Number: 7, Repo: "TomH/b", State: "open"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Before Resign: under the WIDER (persisted) session the record is invalid.
	if ci := ConfirmedIssues(dir, runID, newSig); len(ci) != 0 {
		t.Fatal("precondition: the wider-session read should be invalid before Resign")
	}
	if err := Resign(dir, runID, oldSig, newSig, after.ID); err != nil {
		t.Fatalf("Resign: %v", err)
	}
	// After Resign: the confirmed set is served under the new session, so the
	// run the human just approved is NOT re-locked.
	ci := ConfirmedIssues(dir, runID, newSig)
	if len(ci) != 1 || ci[0].Repo != "TomH/b" {
		t.Fatalf("Resign must carry the confirmed set forward, got %v", ci)
	}
}

// TestResign_LeavesAHandEditedRecordInvalid proves Resign is not a scope
// laundromat: it only carries forward a record that is VALID under the old
// signature. A record already invalidated (e.g. by a hand-edited yaml, a
// different old signature) is left alone — the invalidation rule stands.
func TestResign_LeavesAHandEditedRecordInvalid(t *testing.T) {
	dir := t.TempDir()
	runID := "run"
	real := session.Session{ID: "s", Role: "implement", Repos: []string{"TomH/a"}}
	if err := AppendConfirmedIssue(dir, runID, SignatureOf(real), real.ID,
		ConfirmedIssue{Number: 7, Repo: "TomH/a", State: "open"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Someone hand-widened the yaml to a DIFFERENT session; the caller tries
	// to Resign from THAT (wrong) old signature to a newer one.
	handEdited := session.Session{ID: "s", Role: "implement", Repos: []string{"TomH/a", "TomH/x"}}
	target := session.Session{ID: "s", Role: "implement", Repos: []string{"TomH/a", "TomH/x", "TomH/y"}}
	if err := Resign(dir, runID, SignatureOf(handEdited), SignatureOf(target), target.ID); err != nil {
		t.Fatal(err)
	}
	// The record must NOT now be valid under the hand-edited/target session:
	// Resign refused because the record didn't match the claimed old sig.
	if ci := ConfirmedIssues(dir, runID, SignatureOf(target)); len(ci) != 0 {
		t.Fatal("Resign must NOT resurrect a record that didn't match the old signature")
	}
	// It is still valid under the genuine original session (untouched).
	if ci := ConfirmedIssues(dir, runID, SignatureOf(real)); len(ci) != 1 {
		t.Fatal("the genuine record must be left intact")
	}
}

func TestResign_NoopWhenSigsEqual(t *testing.T) {
	dir := t.TempDir()
	if err := Resign(dir, "run", "sig", "sig", "s"); err != nil {
		t.Fatalf("equal sigs must be a no-op, got %v", err)
	}
}
