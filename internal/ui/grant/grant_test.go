package grant

import (
	"bytes"
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func sampleSession() session.Session {
	return session.Session{
		ID:    "sess_test_001",
		Role:  "implement",
		Repos: []string{"owner/repo"},
		Issue: 1,
	}
}

func defaultCfg(t *testing.T, p prompt.Prompter, stderr io.Writer, tmux TmuxRunner) Config {
	t.Helper()
	return Config{
		StateDir:      t.TempDir(),
		TTL:           4 * time.Hour,
		PromptTimeout: time.Second,
		Stderr:        stderr,
		Prompter:      p,
		TmuxRunner:    tmux,
		Logger:        discardLogger(),
	}
}

// noTTYPrompter simulates the no-controlling-terminal case.
type noTTYPrompter struct{}

func (noTTYPrompter) Confirm(ctx context.Context, _ prompt.Request) (bool, error) {
	return false, prompt.ErrNoTTY
}

// matchingPrompter approves iff the request's Issue matches Answer.
type matchingPrompter struct{ Answer int }

func (m matchingPrompter) Confirm(ctx context.Context, req prompt.Request) (bool, error) {
	return req.Issue == m.Answer, nil
}

func TestObtainApproval_Layer1_ExistingApproval(t *testing.T) {
	sess := sampleSession()
	cfg := defaultCfg(t, nil, nil, nil)
	// Pre-place a valid approval record.
	rec := approvals.Record{
		Signature:  approvals.SignatureOf(sess),
		SessionID:  sess.ID,
		ApprovedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := approvals.Write(approvals.Path(cfg.StateDir), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	approved := ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg)
	if !approved {
		t.Error("existing approval should short-circuit to true")
	}
}

func TestObtainApproval_Layer2_TTYApproved(t *testing.T) {
	sess := sampleSession()
	cfg := defaultCfg(t, matchingPrompter{Answer: 1}, &bytes.Buffer{}, nil)
	approved := ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg)
	if !approved {
		t.Fatal("expected approved via tty prompter")
	}
	// Should have written an approval record.
	rec, err := approvals.Read(approvals.Path(cfg.StateDir))
	if err != nil {
		t.Fatalf("expected approval record to be written: %v", err)
	}
	if rec.SessionID != sess.ID {
		t.Errorf("recorded session = %q, want %q", rec.SessionID, sess.ID)
	}
}

func TestObtainApproval_Layer2_TTYDenied(t *testing.T) {
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	cfg := defaultCfg(t, matchingPrompter{Answer: 9}, stderr, nil)
	approved := ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg)
	if approved {
		t.Fatal("expected denial when prompter returns false")
	}
	// Layer 4 helpful stderr should NOT fire when layer 2 explicitly denied.
	if strings.Contains(stderr.String(), "To grant write access") {
		t.Errorf("explicit denial via tty must not also emit layer-4 stderr")
	}
}

// cancellingPrompter simulates Ctrl-C or 60s prompt-timeout.
type cancellingPrompter struct{}

func (cancellingPrompter) Confirm(ctx context.Context, _ prompt.Request) (bool, error) {
	return false, prompt.ErrCancelled
}

func TestObtainApproval_Layer2_TTYCancelled(t *testing.T) {
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	cfg := defaultCfg(t, cancellingPrompter{}, stderr, nil)
	approved := ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg)
	if approved {
		t.Fatal("Ctrl-C / timeout must deny")
	}
	// Layer 4 stderr must NOT fire — the human cancelled deliberately;
	// telling them to run grant elsewhere is wrong (they had the
	// prompt right in front of them).
	if strings.Contains(stderr.String(), "To grant write access") {
		t.Errorf("Ctrl-C/timeout must not emit layer-4 stderr; got %q", stderr.String())
	}
}

func TestObtainApproval_Layer4_HelpfulStderr(t *testing.T) {
	t.Setenv("TMUX", "") // ensure layer 3 doesn't fire
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	cfg := defaultCfg(t, noTTYPrompter{}, stderr, nil)
	approved := ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/in-scope"}, cfg)
	if approved {
		t.Fatal("expected denial when no tty + no tmux")
	}
	want := []string{
		"rein: write blocked for git push on owner/in-scope",
		"rein approval grant",
		"Then retry your operation",
	}
	for _, w := range want {
		if !strings.Contains(stderr.String(), w) {
			t.Errorf("stderr missing %q\n%s", w, stderr.String())
		}
	}
}

func TestObtainApproval_Layer3_TmuxPopupApproved(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	stateDir := t.TempDir()

	// TmuxRunner stub: simulate the popup minting an approval record
	// in cfg.StateDir, just like the real grant subcommand would.
	tmuxCalled := false
	tmux := func(ctx context.Context, command []string) error {
		tmuxCalled = true
		// Write the approval record as if the popup's grant subcommand
		// had been invoked and the human had approved.
		rec := approvals.Record{
			Signature:  approvals.SignatureOf(sess),
			SessionID:  sess.ID,
			ApprovedAt: time.Now(),
			ExpiresAt:  time.Now().Add(time.Hour),
		}
		return approvals.Write(approvals.Path(stateDir), rec)
	}
	cfg := Config{
		StateDir:      stateDir,
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      noTTYPrompter{},
		TmuxRunner:    tmux,
		Logger:        discardLogger(),
	}
	approved := ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg)
	if !tmuxCalled {
		t.Error("tmux runner should have been called when TMUX is set + tty unavailable")
	}
	if !approved {
		t.Error("expected approved when popup wrote a valid record")
	}
}

func TestObtainApproval_Layer3_TmuxPopupClosedWithoutApproval(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	// Tmux closes without writing approval.
	tmux := func(ctx context.Context, command []string) error { return nil }
	cfg := Config{
		StateDir:      t.TempDir(),
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        stderr,
		Prompter:      noTTYPrompter{},
		TmuxRunner:    tmux,
		Logger:        discardLogger(),
	}
	approved := ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg)
	if approved {
		t.Error("tmux popup closed without record should fall through to layer 4")
	}
	if !strings.Contains(stderr.String(), "rein approval grant") {
		t.Errorf("expected layer-4 stderr after tmux popup didn't approve\n%s", stderr.String())
	}
}

func TestGrantSubcommand_Approve(t *testing.T) {
	sess := sampleSession()
	cfg := defaultCfg(t, matchingPrompter{Answer: 1}, &bytes.Buffer{}, nil)
	ok := Grant(context.Background(), sess, cfg)
	if !ok {
		t.Fatal("Grant should return true on matching answer")
	}
	rec, err := approvals.Read(filepath.Join(cfg.StateDir, "approval.json"))
	if err != nil {
		t.Fatalf("expected approval record: %v", err)
	}
	if rec.SessionID != sess.ID {
		t.Errorf("recorded SessionID = %q, want %q", rec.SessionID, sess.ID)
	}
}

func TestGrantSubcommand_Deny(t *testing.T) {
	sess := sampleSession()
	cfg := defaultCfg(t, matchingPrompter{Answer: 9}, &bytes.Buffer{}, nil)
	ok := Grant(context.Background(), sess, cfg)
	if ok {
		t.Error("Grant should return false on wrong answer")
	}
	_, err := approvals.Read(filepath.Join(cfg.StateDir, "approval.json"))
	if err == nil {
		t.Error("Grant denial should not write an approval record")
	}
}
