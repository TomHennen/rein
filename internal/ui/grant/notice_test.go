package grant

import (
	"context"
	"io"
	"log"
	"os"
	"testing"

	"github.com/TomHennen/rein/internal/approvals"
)

// TestShowInstallNotice_SnapshotBeforePopup pins the 2026-09-05 bug: on a run
// whose FIRST declare hits the not-installed branch, no run context exists yet.
// The snapshot must be written from empty (not skipped), or the popup-side
// `rein approval notice` finds nothing, exits 1, and the notice falls back
// inline over the agent's TUI.
func TestShowInstallNotice_SnapshotBeforePopup(t *testing.T) {
	stateDir := t.TempDir()
	const runID = "run-notice-snapshot"
	t.Setenv("TMUX", "/tmp/fake-tmux-sock,1,0")

	n := InstallNotice{
		Repo: "owner/uncovered", Issue: 7,
		InstallURL: "https://github.com/apps/x/installations/new", AppName: "x",
	}
	var popupErr error
	popupRan := false
	cfg := Config{
		StateDir:    stateDir,
		RunID:       runID,
		SessionFile: "/tmp/session.yaml",
		PreferPopup: true,
		Logger:      log.New(io.Discard, "", 0),
		Stderr:      io.Discard,
		TmuxRunner: func(ctx context.Context, command []string) error {
			popupRan = true
			// Stand in for the popup-side subcommand: render from disk.
			_, popupErr = NoticeFromRunContext(stateDir, runID)
			return popupErr
		},
	}
	ShowInstallNotice(context.Background(), cfg, n)

	if !popupRan {
		t.Fatal("popup was not attempted (PreferPopup + TMUX + RunID were all set)")
	}
	if popupErr != nil {
		t.Fatalf("popup-side NoticeFromRunContext failed — the snapshot was not written before the popup: %v", popupErr)
	}
	rc, err := approvals.ReadRunContext(stateDir, runID)
	if err != nil || rc.PendingNotice == nil {
		t.Fatalf("run context missing/empty after notice: %v", err)
	}
	if rc.PendingNotice.Repo != "owner/uncovered" || rc.SessionFile != "/tmp/session.yaml" {
		t.Errorf("snapshot content wrong: %+v", rc)
	}
	_ = os.Remove(approvals.RunContextPath(stateDir, runID))
}
