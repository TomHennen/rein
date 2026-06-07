package grant

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

const testRunID = "run_test"

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
		RunID:         testRunID,
		RunPID:        os.Getpid(),
		TTL:           4 * time.Hour,
		PromptTimeout: time.Second,
		Stderr:        stderr,
		Prompter:      p,
		TmuxRunner:    tmux,
		Logger:        discardLogger(),
	}
}

// seedApproval writes a valid approval record for runID covering sess.
func seedApproval(t *testing.T, stateDir, runID string, sess session.Session) {
	t.Helper()
	rec := approvals.Record{
		Signature:  approvals.SignatureOf(sess),
		SessionID:  sess.ID,
		ApprovedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := approvals.WriteApproval(stateDir, runID, rec); err != nil {
		t.Fatalf("seed approval: %v", err)
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
	seedApproval(t, cfg.StateDir, cfg.RunID, sess)
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
	rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID)
	if err != nil {
		t.Fatalf("expected approval record to be written: %v", err)
	}
	if rec.SessionID != sess.ID {
		t.Errorf("recorded session = %q, want %q", rec.SessionID, sess.ID)
	}
	// The tty-approve path must ALSO write a run-context snapshot (carrying
	// RunPID) so a long tty-approved run is never swept mid-run. Regression
	// guard for the "no PID for tty-only runs" Sweep footgun.
	if rc, err := approvals.ReadRunContext(cfg.StateDir, cfg.RunID); err != nil {
		t.Errorf("tty-approve path must write a run-context snapshot: %v", err)
	} else if rc.RunPID != cfg.RunPID {
		t.Errorf("snapshot RunPID = %d, want %d", rc.RunPID, cfg.RunPID)
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
		"rein approval grant --run-id " + cfg.RunID,
		"Then retry your operation",
	}
	for _, w := range want {
		if !strings.Contains(stderr.String(), w) {
			t.Errorf("stderr missing %q\n%s", w, stderr.String())
		}
	}
	// Snapshot must have been written so an out-of-process grant can resolve.
	if _, err := approvals.ReadRunContext(cfg.StateDir, cfg.RunID); err != nil {
		t.Errorf("expected run-context snapshot written before layer-4: %v", err)
	}
}

func TestObtainApproval_Layer3_TmuxPopupApproved(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	stateDir := t.TempDir()

	// TmuxRunner stub: assert --run-id is threaded AND that the snapshot
	// was written BEFORE the popup ran (so the real grant could resolve
	// the session). Then simulate the popup minting the approval.
	tmuxCalled := false
	var sawRunID bool
	tmux := func(ctx context.Context, command []string) error {
		tmuxCalled = true
		joined := strings.Join(command, " ")
		if strings.Contains(joined, "--run-id "+testRunID) {
			sawRunID = true
		}
		if _, err := approvals.ReadRunContext(stateDir, testRunID); err != nil {
			t.Errorf("snapshot must be written before popup runs: %v", err)
		}
		rec := approvals.Record{
			Signature:  approvals.SignatureOf(sess),
			SessionID:  sess.ID,
			ApprovedAt: time.Now(),
			ExpiresAt:  time.Now().Add(time.Hour),
		}
		return approvals.WriteApproval(stateDir, testRunID, rec)
	}
	cfg := Config{
		StateDir:      stateDir,
		RunID:         testRunID,
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
	if !sawRunID {
		t.Error("popup command must carry --run-id")
	}
	if !approved {
		t.Error("expected approved when popup wrote a valid record")
	}
}

func TestObtainApproval_Layer3_TmuxPopupClosedWithoutApproval(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	tmux := func(ctx context.Context, command []string) error { return nil }
	cfg := Config{
		StateDir:      t.TempDir(),
		RunID:         testRunID,
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
	if !strings.Contains(stderr.String(), "rein approval grant --run-id") {
		t.Errorf("expected layer-4 stderr after tmux popup didn't approve\n%s", stderr.String())
	}
}

// --- Per-run isolation (the constraint-6 gate) ---

// TestObtainApproval_NoCrossRunReuse: a valid approval for run A must NOT
// satisfy run B (different run id), even for the same session.
func TestObtainApproval_NoCrossRunReuse(t *testing.T) {
	t.Setenv("TMUX", "")
	stateDir := t.TempDir()
	sess := sampleSession()
	seedApproval(t, stateDir, "A", sess)

	cfgB := Config{
		StateDir:      stateDir,
		RunID:         "B",
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      noTTYPrompter{}, // no auto-approve
		Logger:        discardLogger(),
	}
	if ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfgB) {
		t.Fatal("run B must NOT be approved by run A's record")
	}
	// Sanity: run A IS approved from its own file.
	cfgA := cfgB
	cfgA.RunID = "A"
	if !ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfgA) {
		t.Fatal("run A should be approved from its own seeded record")
	}
}

// TestObtainApproval_ClearOneLeavesOther: clearing run A must not affect B.
func TestObtainApproval_ClearOneLeavesOther(t *testing.T) {
	t.Setenv("TMUX", "")
	stateDir := t.TempDir()
	sess := sampleSession()
	seedApproval(t, stateDir, "A", sess)
	seedApproval(t, stateDir, "B", sess)

	if err := approvals.ClearRun(stateDir, "A"); err != nil {
		t.Fatalf("ClearRun A: %v", err)
	}
	base := Config{
		StateDir:      stateDir,
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      noTTYPrompter{},
		Logger:        discardLogger(),
	}
	a := base
	a.RunID = "A"
	if ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, a) {
		t.Error("cleared run A should re-prompt (not approved)")
	}
	b := base
	b.RunID = "B"
	if !ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, b) {
		t.Error("run B should still be approved after clearing A")
	}
}

// TestObtainApproval_FailClosedWithoutRunID: no RunID => interactive-only,
// persists nothing.
func TestObtainApproval_FailClosedWithoutRunID(t *testing.T) {
	t.Setenv("TMUX", "")
	sess := sampleSession()

	t.Run("tty approves this op but persists nothing", func(t *testing.T) {
		stateDir := t.TempDir()
		cfg := Config{
			StateDir:      stateDir,
			RunID:         "", // no run id
			TTL:           time.Hour,
			PromptTimeout: time.Second,
			Stderr:        &bytes.Buffer{},
			Prompter:      matchingPrompter{Answer: 1},
			Logger:        discardLogger(),
		}
		if !ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg) {
			t.Fatal("interactive approval should succeed even without run id")
		}
		// No file may be created under approvals/ or runs/.
		for _, sub := range []string{"approvals", "runs"} {
			if entries, err := os.ReadDir(stateDir + "/" + sub); err == nil && len(entries) > 0 {
				t.Errorf("no-run-id path must persist nothing, found %d entries in %s", len(entries), sub)
			}
		}
	})

	t.Run("no tty denies", func(t *testing.T) {
		stderr := &bytes.Buffer{}
		cfg := Config{
			StateDir:      t.TempDir(),
			RunID:         "",
			TTL:           time.Hour,
			PromptTimeout: time.Second,
			Stderr:        stderr,
			Prompter:      noTTYPrompter{},
			Logger:        discardLogger(),
		}
		if ObtainApproval(context.Background(), Request{Session: sess, Action: "git push", Repo: "owner/repo"}, cfg) {
			t.Fatal("no tty + no run id must deny")
		}
		if !strings.Contains(stderr.String(), "OUTSIDE `rein run`") {
			t.Errorf("expected fail-closed stderr explaining missing run context\n%s", stderr.String())
		}
	})
}

// TestObtainApproval_MidRunScopeEdit: a record from before a scope
// expansion (extra repo added to the live session) must NOT cover the
// expanded scope — live-vs-record signature check.
func TestObtainApproval_MidRunScopeEdit(t *testing.T) {
	t.Setenv("TMUX", "")
	stateDir := t.TempDir()
	orig := sampleSession()
	seedApproval(t, stateDir, testRunID, orig)

	expanded := orig
	expanded.Repos = []string{"owner/repo", "owner/extra"} // scope grew mid-run

	cfg := Config{
		StateDir:      stateDir,
		RunID:         testRunID,
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      noTTYPrompter{}, // no auto-approve, so we observe the miss
		Logger:        discardLogger(),
	}
	if ObtainApproval(context.Background(), Request{Session: expanded, Action: "git push", Repo: "owner/extra"}, cfg) {
		t.Fatal("expanded-scope session must NOT be covered by the pre-expansion approval")
	}
}

// --- Grant subcommand (loads session from snapshot by run-id) ---

// TestGrant_LoadsSessionFromRunContext is the linchpin: Grant must read
// the session from runs/<id>.json, NOT from REIN_SESSION_FILE. We set
// REIN_SESSION_FILE to a DIFFERENT session and confirm the snapshot's
// session is the one that gets approved.
func TestGrant_LoadsSessionFromRunContext(t *testing.T) {
	stateDir := t.TempDir()
	snapSess := session.Session{ID: "sess_snapshot", Role: "implement", Repos: []string{"owner/snap"}, Issue: 42}
	if err := approvals.WriteRunContext(stateDir, "X", approvals.RunContext{Session: snapSess, RunPID: os.Getpid(), WrittenAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	// Point the env at a DIFFERENT session file — Grant must ignore it.
	other := session.Session{ID: "sess_other", Role: "implement", Repos: []string{"owner/other"}, Issue: 99}
	t.Setenv("REIN_SESSION_FILE", writeSessionFile(t, other))

	cfg := Config{
		StateDir:      stateDir,
		RunID:         "X",
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      matchingPrompter{Answer: 42}, // matches the SNAPSHOT's issue, not 99
		Logger:        discardLogger(),
	}
	if err := Grant(context.Background(), cfg); err != nil {
		t.Fatalf("Grant should approve using snapshot session: %v", err)
	}
	rec, err := approvals.ReadApproval(stateDir, "X")
	if err != nil {
		t.Fatalf("expected approval written: %v", err)
	}
	if rec.Signature != approvals.SignatureOf(snapSess) {
		t.Error("approval signature must match the SNAPSHOT session, not the env session")
	}
	if rec.SessionID != snapSess.ID {
		t.Errorf("recorded SessionID = %q, want snapshot %q", rec.SessionID, snapSess.ID)
	}
}

func TestGrant_MissingSnapshot(t *testing.T) {
	cfg := Config{
		StateDir:      t.TempDir(),
		RunID:         "absent",
		PromptTimeout: time.Second,
		Prompter:      matchingPrompter{Answer: 1},
		Logger:        discardLogger(),
	}
	err := Grant(context.Background(), cfg)
	if err == nil {
		t.Fatal("Grant with no snapshot must error")
	}
	if !strings.Contains(err.Error(), "no run context") {
		t.Errorf("expected helpful missing-snapshot error, got %v", err)
	}
}

func TestGrant_NoRunID(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), RunID: "", Logger: discardLogger()}
	if err := Grant(context.Background(), cfg); err == nil {
		t.Fatal("Grant without --run-id must error")
	}
}

func TestGrant_Deny(t *testing.T) {
	stateDir := t.TempDir()
	sess := sampleSession()
	if err := approvals.WriteRunContext(stateDir, testRunID, approvals.RunContext{Session: sess, RunPID: os.Getpid(), WrittenAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	cfg := Config{
		StateDir:      stateDir,
		RunID:         testRunID,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      matchingPrompter{Answer: 9}, // wrong answer (sess.Issue=1)
		Logger:        discardLogger(),
	}
	if err := Grant(context.Background(), cfg); err == nil {
		t.Error("Grant should error on wrong answer")
	}
	if _, err := approvals.ReadApproval(stateDir, testRunID); err == nil {
		t.Error("Grant denial should not write an approval record")
	}
}

// writeSessionFile writes a minimal dev-session.yaml and returns its path.
func writeSessionFile(t *testing.T, s session.Session) string {
	t.Helper()
	path := t.TempDir() + "/dev-session.yaml"
	body := "id: " + s.ID + "\nrole: " + s.Role + "\nrepos:\n  - " + s.Repos[0] + "\nissue: " + strconv.Itoa(s.Issue) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return path
}
