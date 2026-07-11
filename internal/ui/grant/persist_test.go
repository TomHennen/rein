package grant

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// TestObtainIssueApproval_ExpansionPersistWritesFile proves the in-prompt
// persist path (issue #69): approving an expansion issue with a `y` at the
// save question appends the repo to the session file, and OnPersist fires
// with the wider session.
func TestObtainIssueApproval_ExpansionPersistWritesFile(t *testing.T) {
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	sessFile := filepath.Join(dir, "session.yaml")
	if err := os.WriteFile(sessFile, []byte("id: sess_test_001\nrole: implement\nrepos:\n  - owner/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := sampleSession()
	// The declared issue is on owner/other — OUTSIDE the session => expansion.
	issue := approvals.ConfirmedIssue{Number: 7, Repo: "owner/other", Title: "t", State: "open",
		CanonicalURL: "https://api.github.com/repos/owner/other/issues/7"}

	var persisted session.Session
	cfg := Config{
		StateDir:    t.TempDir(),
		RunID:       testRunID,
		RunPID:      os.Getpid(),
		SessionFile: sessFile,
		Prompter:    &prompt.StubPrompter{Response: "7", PersistResponse: "y"},
		TmuxRunner:  func(context.Context, []string) error { return nil },
		Logger:      discardLogger(),
		OnPersist:   func(s session.Session) { persisted = s },
	}
	if !ObtainIssueApproval(context.Background(), IssueRequest{Session: sess, Issue: issue}, cfg) {
		t.Fatal("expansion issue must be approved")
	}
	// The session file now contains owner/other.
	updated, err := session.LoadFromFile(sessFile)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if !updated.Contains("owner/other") {
		t.Errorf("persist=y must add owner/other to the session file, got %v", updated.Repos)
	}
	if !persisted.Contains("owner/other") {
		t.Errorf("OnPersist must receive the wider session, got %v", persisted.Repos)
	}
}

// TestObtainIssueApproval_ExpansionRunOnlyDoesNotWrite proves the default
// (N = run-only) does not touch the session file.
func TestObtainIssueApproval_ExpansionRunOnlyDoesNotWrite(t *testing.T) {
	t.Setenv("TMUX", "")
	dir := t.TempDir()
	sessFile := filepath.Join(dir, "session.yaml")
	orig := "id: sess_test_001\nrole: implement\nrepos:\n  - owner/repo\n"
	if err := os.WriteFile(sessFile, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	issue := approvals.ConfirmedIssue{Number: 7, Repo: "owner/other", State: "open"}
	cfg := Config{
		StateDir:    t.TempDir(),
		RunID:       testRunID,
		SessionFile: sessFile,
		Prompter:    &prompt.StubPrompter{Response: "7", PersistResponse: "n"},
		TmuxRunner:  func(context.Context, []string) error { return nil },
		Logger:      discardLogger(),
	}
	if !ObtainIssueApproval(context.Background(), IssueRequest{Session: sampleSession(), Issue: issue}, cfg) {
		t.Fatal("must still approve for the run")
	}
	body, _ := os.ReadFile(sessFile)
	if string(body) != orig {
		t.Errorf("run-only must NOT modify the session file:\n%s", body)
	}
}
