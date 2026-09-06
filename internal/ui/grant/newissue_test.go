package grant

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// seedPendingNew writes a pending new-issue request the way
// ObtainNewIssueApproval does, and returns its nonce.
func seedPendingNew(t *testing.T, stateDir, runID string, sess session.Session) string {
	t.Helper()
	const nonce = "test-nonce"
	rc := approvals.RunContext{
		Session: sess,
		RunPID:  os.Getpid(),
		PendingNewIssue: &approvals.PendingNewIssue{
			Nonce: nonce, Repo: "owner/snap", Title: "Add a thing", Body: "because reasons",
			WrittenAt: time.Now(),
		},
		WrittenAt: time.Now(),
	}
	if err := approvals.WriteRunContext(stateDir, runID, rc); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	return nonce
}

// TestGrant_NewIssueMarksApprovedAndRecordsNothing is the out-of-process
// half of the #180 handoff: this surface holds no App key, so it may only
// mark the request approved — the broker files and records.
func TestGrant_NewIssueMarksApprovedAndRecordsNothing(t *testing.T) {
	stateDir := t.TempDir()
	snapSess := session.Session{ID: "sess_snapshot", Role: "implement", Repos: []string{"owner/snap"}}
	nonce := seedPendingNew(t, stateDir, "X", snapSess)

	stub := &prompt.StubPrompter{Response: "add"} // first word, wrong case on purpose
	cfg := Config{
		StateDir: stateDir, RunID: "X", TTL: time.Hour, ApprovalTimeout: time.Second,
		Stderr: &bytes.Buffer{}, Prompter: stub, Logger: discardLogger(),
	}
	if err := Grant(context.Background(), cfg); err != nil {
		t.Fatalf("Grant should confirm the new-issue request: %v", err)
	}
	// Rendered from the snapshot alone, with the first word as the token.
	if !stub.Last.NewIssue || stub.Last.Title != "Add a thing" || stub.Last.IssueRepo != "owner/snap" {
		t.Errorf("prompt not rendered from the snapshot: %+v", stub.Last)
	}
	if stub.Last.ConfirmWord != "Add" {
		t.Errorf("ConfirmWord = %q, want %q", stub.Last.ConfirmWord, "Add")
	}
	if !approvals.NewIssueApproved(stateDir, "X", nonce) {
		t.Error("the approval marker was not set")
	}
	// Nothing is confirmed yet — the issue does not exist.
	if _, err := approvals.ReadApproval(stateDir, "X"); err == nil {
		t.Error("this surface must record no confirmed issue")
	}
}

func TestGrant_NewIssueDenyLeavesNoMarker(t *testing.T) {
	stateDir := t.TempDir()
	sess := session.Session{ID: "s", Role: "implement", Repos: []string{"owner/snap"}}
	nonce := seedPendingNew(t, stateDir, "X", sess)

	cfg := Config{
		StateDir: stateDir, RunID: "X", TTL: time.Hour, ApprovalTimeout: time.Second,
		Stderr: &bytes.Buffer{}, Prompter: &prompt.StubPrompter{Response: "thing"}, Logger: discardLogger(),
	}
	err := Grant(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "first word") {
		t.Fatalf("err = %v, want a denial naming the expected token", err)
	}
	if approvals.NewIssueApproved(stateDir, "X", nonce) {
		t.Error("a denied request must leave no approval marker")
	}
}

// TestObtainNewIssueApproval_DeniedFilesNothing pins the in-process
// inline path and its cleanup: a wrong answer approves nothing, and the
// pending request never outlives the ceremony.
func TestObtainNewIssueApproval_InlineDenyClearsPending(t *testing.T) {
	t.Setenv("TMUX", "")
	stateDir := t.TempDir()
	sess := session.Session{ID: "s", Role: "implement", Repos: []string{"o/r"}}
	cfg := Config{
		StateDir: stateDir, RunID: "X", TTL: time.Hour, ApprovalTimeout: time.Second,
		Stderr: &bytes.Buffer{}, Prompter: &prompt.StubPrompter{Response: "nope"},
		TmuxRunner: func(context.Context, []string) error { return os.ErrNotExist },
		Logger:     discardLogger(),
	}
	if ObtainNewIssueApproval(context.Background(), NewIssueRequest{
		Session: sess, Repo: "o/r", Title: "Add a thing",
	}, cfg) {
		t.Fatal("a wrong answer must not approve")
	}
	rc, err := approvals.ReadRunContext(stateDir, "X")
	if err == nil && rc.PendingNewIssue != nil {
		t.Error("pending new-issue request outlived the ceremony")
	}
}

// TestObtainNewIssueApproval_NoRunIDFailsClosed: without a run to key it
// to, the ceremony cannot be recorded at all.
func TestObtainNewIssueApproval_NoRunIDFailsClosed(t *testing.T) {
	t.Setenv("TMUX", "")
	stub := &prompt.StubPrompter{Response: "Add"}
	cfg := Config{StateDir: t.TempDir(), Stderr: &bytes.Buffer{}, Prompter: stub, Logger: discardLogger()}
	if ObtainNewIssueApproval(context.Background(), NewIssueRequest{Repo: "o/r", Title: "Add a thing"}, cfg) {
		t.Fatal("no run id must deny")
	}
	if stub.Calls != 0 {
		t.Error("no prompt may fire without a run id")
	}
}

// TestMarkNewIssueApproved_NonceBinding: a marker must name the request
// that is actually pending, so a stale approval cannot carry over.
func TestMarkNewIssueApproved_NonceBinding(t *testing.T) {
	stateDir := t.TempDir()
	sess := session.Session{ID: "s", Role: "implement", Repos: []string{"owner/snap"}}
	nonce := seedPendingNew(t, stateDir, "X", sess)

	if err := approvals.MarkNewIssueApproved(stateDir, "X", "wrong"); err == nil {
		t.Error("a mismatched nonce must not mark anything")
	}
	if approvals.NewIssueApproved(stateDir, "X", nonce) {
		t.Error("the request must still be unapproved")
	}
	if err := approvals.MarkNewIssueApproved(stateDir, "X", nonce); err != nil {
		t.Fatalf("matching nonce: %v", err)
	}
	if !approvals.NewIssueApproved(stateDir, "X", nonce) {
		t.Error("marker not set")
	}
	// Once cleared, the marker is gone for good.
	if err := approvals.ClearPendingNewIssue(stateDir, "X"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if approvals.NewIssueApproved(stateDir, "X", nonce) {
		t.Error("a cleared request must not read as approved")
	}
}
