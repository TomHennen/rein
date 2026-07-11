package grant

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	}
}

func sampleIssue(n int) approvals.ConfirmedIssue {
	return approvals.ConfirmedIssue{
		Number:       n,
		Repo:         "owner/repo",
		Title:        "fix the flux capacitor",
		State:        "open",
		CanonicalURL: "https://api.github.com/repos/owner/repo/issues/73",
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

// seedConfirmed writes an approval record for runID covering sess with
// the given confirmed issues.
func seedConfirmed(t *testing.T, stateDir, runID string, sess session.Session, issues ...approvals.ConfirmedIssue) {
	t.Helper()
	rec := approvals.Record{
		Signature:  approvals.SignatureOf(sess),
		SessionID:  sess.ID,
		Issues:     issues,
		ApprovedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := approvals.WriteApproval(stateDir, runID, rec); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
}

// noTTYPrompter simulates the no-controlling-terminal case.
type noTTYPrompter struct{}

func (noTTYPrompter) Confirm(ctx context.Context, _ prompt.Request) (prompt.Result, error) {
	return prompt.Result{}, prompt.ErrNoTTY
}

// matchingPrompter approves iff the request's Issue matches Answer —
// i.e. the "human" types the displayed number of the RIGHT issue.
type matchingPrompter struct{ Answer int }

func (m matchingPrompter) Confirm(ctx context.Context, req prompt.Request) (prompt.Result, error) {
	return prompt.Result{Approved: req.Issue == m.Answer}, nil
}

func req73(sess session.Session) IssueRequest {
	return IssueRequest{Session: sess, Issue: sampleIssue(73)}
}

func TestObtainIssueApproval_IdempotentReDeclare(t *testing.T) {
	sess := sampleSession()
	stub := &prompt.StubPrompter{Response: "never right"}
	cfg := defaultCfg(t, stub, &bytes.Buffer{}, nil)
	seedConfirmed(t, cfg.StateDir, cfg.RunID, sess, sampleIssue(73))
	if !ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Error("already-confirmed issue must short-circuit to true")
	}
	if stub.Calls != 0 {
		t.Error("idempotent re-declare must NOT re-prompt (§3)")
	}
}

func TestObtainIssueApproval_TTYConfirmed(t *testing.T) {
	sess := sampleSession()
	cfg := defaultCfg(t, matchingPrompter{Answer: 73}, &bytes.Buffer{}, nil)
	if !ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("expected confirmation via tty prompter")
	}
	rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID)
	if err != nil {
		t.Fatalf("expected approval record: %v", err)
	}
	if !rec.HasIssue("owner/repo", 73) {
		t.Errorf("record must carry the confirmed issue, got %+v", rec.Issues)
	}
	if rec.SessionID != sess.ID || rec.Signature != approvals.SignatureOf(sess) {
		t.Errorf("record identity mismatch: %+v", rec)
	}
	if len(rec.Issues) != 1 || rec.Issues[0].ConfirmedAt.IsZero() {
		t.Errorf("confirmed issue must be stamped with ConfirmedAt: %+v", rec.Issues)
	}
	// The confirm path must ALSO write a run-context snapshot (carrying
	// RunPID for Sweep's liveness probe + the PendingIssue transport).
	rc, err := approvals.ReadRunContext(cfg.StateDir, cfg.RunID)
	if err != nil {
		t.Fatalf("snapshot must be written: %v", err)
	}
	if rc.RunPID != cfg.RunPID {
		t.Errorf("snapshot RunPID = %d, want %d", rc.RunPID, cfg.RunPID)
	}
	if rc.PendingIssue == nil || rc.PendingIssue.Number != 73 || rc.PendingIssue.Title == "" {
		t.Errorf("snapshot must carry the fetched PendingIssue for out-of-process surfaces: %+v", rc.PendingIssue)
	}
}

func TestObtainIssueApproval_TTYDenied(t *testing.T) {
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	cfg := defaultCfg(t, matchingPrompter{Answer: 9}, stderr, nil)
	if ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("expected denial when prompter returns false")
	}
	if strings.Contains(stderr.String(), "approval grant") {
		t.Errorf("explicit denial via tty must not also emit the other-terminal stderr")
	}
	if _, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil {
		t.Error("denial must not write an approval record")
	}
}

// cancellingPrompter simulates Ctrl-C or prompt-timeout.
type cancellingPrompter struct{}

func (cancellingPrompter) Confirm(ctx context.Context, _ prompt.Request) (prompt.Result, error) {
	return prompt.Result{}, prompt.ErrCancelled
}

func TestObtainIssueApproval_TTYCancelled(t *testing.T) {
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	cfg := defaultCfg(t, cancellingPrompter{}, stderr, nil)
	if ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("Ctrl-C / timeout must deny")
	}
	if strings.Contains(stderr.String(), "approval grant") {
		t.Errorf("Ctrl-C/timeout must not emit the other-terminal stderr; got %q", stderr.String())
	}
}

func TestObtainIssueApproval_HelpfulStderrWhenNoSurface(t *testing.T) {
	t.Setenv("TMUX", "") // no popup either
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	cfg := defaultCfg(t, noTTYPrompter{}, stderr, nil)
	if ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("expected denial when no tty + no tmux")
	}
	want := []string{
		"issue #73",
		"rein approval grant --run-id " + cfg.RunID,
	}
	for _, w := range want {
		if !strings.Contains(stderr.String(), w) {
			t.Errorf("stderr missing %q\n%s", w, stderr.String())
		}
	}
	// Snapshot (with PendingIssue) must have been written so an
	// out-of-process grant can render the prompt.
	rc, err := approvals.ReadRunContext(cfg.StateDir, cfg.RunID)
	if err != nil {
		t.Fatalf("expected snapshot before the helpful-deny: %v", err)
	}
	if rc.PendingIssue == nil || rc.PendingIssue.Number != 73 {
		t.Errorf("snapshot must carry PendingIssue: %+v", rc.PendingIssue)
	}
}

func TestObtainIssueApproval_ExpansionHeaderOnSecondIssue(t *testing.T) {
	sess := sampleSession()
	stub := &prompt.StubPrompter{Response: "99"}
	cfg := defaultCfg(t, stub, &bytes.Buffer{}, nil)
	seedConfirmed(t, cfg.StateDir, cfg.RunID, sess, sampleIssue(73))

	second := sampleIssue(99)
	if !ObtainIssueApproval(context.Background(), IssueRequest{Session: sess, Issue: second}, cfg) {
		t.Fatal("second declare should confirm")
	}
	if !stub.Last.Expansion {
		t.Error("a second issue mid-run must render the scope-EXPANSION prompt (design.md:254-263)")
	}
	rec, _ := approvals.ReadApproval(cfg.StateDir, cfg.RunID)
	if !rec.HasIssue("owner/repo", 73) || !rec.HasIssue("owner/repo", 99) {
		t.Errorf("expansion must APPEND, not replace: %+v", rec.Issues)
	}
}

func TestObtainIssueApproval_PromptCarriesFetchedSnapshot(t *testing.T) {
	sess := sampleSession()
	stub := &prompt.StubPrompter{Response: "73"}
	cfg := defaultCfg(t, stub, &bytes.Buffer{}, nil)
	if !ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("expected confirmation")
	}
	// Decision E: the prompt the human sees must carry the fetched
	// title/state/home-repo — the load-bearing misattribution control.
	if stub.Last.Title != "fix the flux capacitor" || stub.Last.State != "open" || stub.Last.IssueRepo != "owner/repo" {
		t.Errorf("prompt request missing fetched snapshot: %+v", stub.Last)
	}
}

func TestObtainIssueApproval_PopupApproved(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	stateDir := t.TempDir()

	// TmuxRunner stub: assert --run-id is threaded AND that the snapshot
	// (with PendingIssue) was written BEFORE the popup ran. Then simulate
	// the popup's grant appending the confirmed issue.
	tmuxCalled := false
	var sawRunID bool
	tmux := func(ctx context.Context, command []string) error {
		tmuxCalled = true
		joined := strings.Join(command, " ")
		if strings.Contains(joined, "--run-id "+testRunID) {
			sawRunID = true
		}
		rc, err := approvals.ReadRunContext(stateDir, testRunID)
		if err != nil {
			t.Errorf("snapshot must be written before popup runs: %v", err)
		} else if rc.PendingIssue == nil || rc.PendingIssue.Number != 73 {
			t.Errorf("snapshot must carry PendingIssue for the popup to render: %+v", rc.PendingIssue)
		}
		return approvals.AppendConfirmedIssue(stateDir, testRunID, approvals.SignatureOf(sess), sess.ID, sampleIssue(73), time.Hour)
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
	if !ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Error("expected confirmed when popup appended the issue")
	}
	if !tmuxCalled {
		t.Error("tmux runner should have been called when TMUX is set + tty unavailable")
	}
	if !sawRunID {
		t.Error("popup command must carry --run-id")
	}
}

func TestObtainIssueApproval_PopupClosedWithoutApproval(t *testing.T) {
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
	if ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Error("popup closed without record should deny")
	}
	if !strings.Contains(stderr.String(), "rein approval grant --run-id") {
		t.Errorf("expected other-terminal stderr after popup didn't approve\n%s", stderr.String())
	}
}

// --- PreferPopup: popup-first ordering (the agent-TUI collision fix) ---

// spyPrompter records whether the inline /dev/tty prompter was consulted.
type spyPrompter struct {
	inner  prompt.Prompter
	called bool
}

func (s *spyPrompter) Confirm(ctx context.Context, req prompt.Request) (prompt.Result, error) {
	s.called = true
	if s.inner == nil {
		return prompt.Result{}, prompt.ErrNoTTY
	}
	return s.inner.Confirm(ctx, req)
}

func TestObtainIssueApproval_PreferPopup_PopupFirstApproved(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	stateDir := t.TempDir()
	tmuxCalled := false
	tmux := func(ctx context.Context, command []string) error {
		tmuxCalled = true
		return approvals.AppendConfirmedIssue(stateDir, testRunID, approvals.SignatureOf(sess), sess.ID, sampleIssue(73), time.Hour)
	}
	spy := &spyPrompter{inner: matchingPrompter{Answer: 73}}
	cfg := Config{
		StateDir:      stateDir,
		RunID:         testRunID,
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      spy,
		TmuxRunner:    tmux,
		PreferPopup:   true,
		Logger:        discardLogger(),
	}
	if !ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("expected confirmation via popup-first path")
	}
	if !tmuxCalled {
		t.Error("popup should have been tried first when PreferPopup is set")
	}
	if spy.called {
		t.Error("inline /dev/tty prompt must NOT be consulted when the popup approves (TUI-collision guard)")
	}
}

func TestObtainIssueApproval_PreferPopup_DeclinedDoesNotFallToTTY(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	tmux := func(ctx context.Context, command []string) error { return nil } // closed, no record
	spy := &spyPrompter{inner: matchingPrompter{Answer: 73}}                 // would approve if (wrongly) consulted
	cfg := Config{
		StateDir:      t.TempDir(),
		RunID:         testRunID,
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        stderr,
		Prompter:      spy,
		TmuxRunner:    tmux,
		PreferPopup:   true,
		Logger:        discardLogger(),
	}
	if ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("a declined popup must deny, not fall through to an auto-approving tty")
	}
	if spy.called {
		t.Error("must NOT fall back to inline /dev/tty after the human declined the popup (would corrupt a TUI)")
	}
	if !strings.Contains(stderr.String(), "rein approval grant --run-id") {
		t.Errorf("expected helpful deny stderr after the popup was declined\n%s", stderr.String())
	}
}

func TestObtainIssueApproval_PreferPopup_PopupUnavailableFallsToTTY(t *testing.T) {
	t.Setenv("TMUX", "/some/socket")
	sess := sampleSession()
	tmux := func(ctx context.Context, command []string) error { return errors.New("tmux too old for display-popup") }
	spy := &spyPrompter{inner: matchingPrompter{Answer: 73}}
	cfg := Config{
		StateDir:      t.TempDir(),
		RunID:         testRunID,
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      spy,
		TmuxRunner:    tmux,
		PreferPopup:   true,
		Logger:        discardLogger(),
	}
	if !ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("expected fallback to inline /dev/tty when the popup can't launch")
	}
	if !spy.called {
		t.Error("inline /dev/tty must be consulted as the fallback when the popup fails to launch")
	}
}

func TestPopupPreferenceFromEnv(t *testing.T) {
	cases := []struct {
		name     string
		approval string
		tmux     string
		want     bool
	}{
		{"force tty even in tmux", "tty", "/sock", false},
		{"force popup even without tmux", "popup", "", true},
		{"default prefers popup inside tmux", "", "/sock", true},
		{"default prefers tty outside tmux", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("REIN_APPROVAL", c.approval)
			t.Setenv("TMUX", c.tmux)
			if got := PopupPreferenceFromEnv(); got != c.want {
				t.Errorf("PopupPreferenceFromEnv() = %v, want %v (REIN_APPROVAL=%q TMUX=%q)", got, c.want, c.approval, c.tmux)
			}
		})
	}
}

// --- Per-run isolation (the constraint-6 gate) ---

func TestObtainIssueApproval_NoCrossRunReuse(t *testing.T) {
	t.Setenv("TMUX", "")
	stateDir := t.TempDir()
	sess := sampleSession()
	seedConfirmed(t, stateDir, "A", sess, sampleIssue(73))

	cfgB := Config{
		StateDir:      stateDir,
		RunID:         "B",
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      noTTYPrompter{}, // no auto-approve
		Logger:        discardLogger(),
	}
	if ObtainIssueApproval(context.Background(), req73(sess), cfgB) {
		t.Fatal("run B must NOT be satisfied by run A's confirmed set")
	}
	cfgA := cfgB
	cfgA.RunID = "A"
	if !ObtainIssueApproval(context.Background(), req73(sess), cfgA) {
		t.Fatal("run A should be satisfied from its own seeded record")
	}
}

func TestObtainIssueApproval_MidRunScopeEdit(t *testing.T) {
	t.Setenv("TMUX", "")
	stateDir := t.TempDir()
	orig := sampleSession()
	seedConfirmed(t, stateDir, testRunID, orig, sampleIssue(73))

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
	if ObtainIssueApproval(context.Background(), req73(expanded), cfg) {
		t.Fatal("expanded-scope session must NOT be covered by the pre-expansion record (issue set included)")
	}
}

func TestObtainIssueApproval_NoRunIDFailsClosed(t *testing.T) {
	t.Setenv("TMUX", "")
	sess := sampleSession()
	stderr := &bytes.Buffer{}
	cfg := Config{
		StateDir:      t.TempDir(),
		RunID:         "", // outside any rein run
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        stderr,
		Prompter:      matchingPrompter{Answer: 73}, // would approve if (wrongly) consulted
		Logger:        discardLogger(),
	}
	if ObtainIssueApproval(context.Background(), req73(sess), cfg) {
		t.Fatal("no run id must fail closed — a declare cannot be keyed to a run")
	}
	if !strings.Contains(stderr.String(), "rein run") {
		t.Errorf("expected the launch instruction, got %q", stderr.String())
	}
}

// --- Grant subcommand (renders ONLY from the snapshot) ---

func TestGrant_RendersPendingIssueFromSnapshot(t *testing.T) {
	stateDir := t.TempDir()
	snapSess := session.Session{ID: "sess_snapshot", Role: "implement", Repos: []string{"owner/snap"}}
	pending := approvals.ConfirmedIssue{Number: 42, Repo: "owner/snap", Title: "the snapshot title", State: "open"}
	if err := approvals.WriteRunContext(stateDir, "X", approvals.RunContext{Session: snapSess, RunPID: os.Getpid(), PendingIssue: &pending, WrittenAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	// Point the env at a DIFFERENT session file — Grant must ignore it.
	t.Setenv("REIN_SESSION_FILE", writeSessionFile(t, session.Session{ID: "sess_other", Role: "implement", Repos: []string{"owner/other"}}))

	stub := &prompt.StubPrompter{Response: "42"}
	cfg := Config{
		StateDir:      stateDir,
		RunID:         "X",
		TTL:           time.Hour,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      stub,
		Logger:        discardLogger(),
	}
	if err := Grant(context.Background(), cfg); err != nil {
		t.Fatalf("Grant should confirm using snapshot: %v", err)
	}
	// The prompt must be rendered from the SNAPSHOT (the popup never
	// fetches): title + home repo from PendingIssue, session from snapshot.
	if stub.Last.Title != "the snapshot title" || stub.Last.IssueRepo != "owner/snap" || stub.Last.SessionID != "sess_snapshot" {
		t.Errorf("Grant must render from the snapshot only: %+v", stub.Last)
	}
	rec, err := approvals.ReadApproval(stateDir, "X")
	if err != nil {
		t.Fatalf("expected approval written: %v", err)
	}
	if rec.Signature != approvals.SignatureOf(snapSess) {
		t.Error("approval signature must match the SNAPSHOT session, not the env session")
	}
	if !rec.HasIssue("owner/snap", 42) {
		t.Errorf("confirmed issue must be recorded: %+v", rec.Issues)
	}
}

func TestGrant_NothingPending(t *testing.T) {
	stateDir := t.TempDir()
	sess := sampleSession()
	if err := approvals.WriteRunContext(stateDir, testRunID, approvals.RunContext{Session: sess, RunPID: os.Getpid(), WrittenAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	cfg := Config{
		StateDir:      stateDir,
		RunID:         testRunID,
		PromptTimeout: time.Second,
		Prompter:      matchingPrompter{Answer: 1},
		Logger:        discardLogger(),
	}
	err := Grant(context.Background(), cfg)
	if err == nil {
		t.Fatal("Grant with no PendingIssue must error")
	}
	if !strings.Contains(err.Error(), "rein declare") {
		t.Errorf("error must name the declare command, got %v", err)
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
	pending := sampleIssue(73)
	if err := approvals.WriteRunContext(stateDir, testRunID, approvals.RunContext{Session: sess, RunPID: os.Getpid(), PendingIssue: &pending, WrittenAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	cfg := Config{
		StateDir:      stateDir,
		RunID:         testRunID,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      matchingPrompter{Answer: 9}, // wrong answer
		Logger:        discardLogger(),
	}
	if err := Grant(context.Background(), cfg); err == nil {
		t.Error("Grant should error on wrong answer")
	}
	if _, err := approvals.ReadApproval(stateDir, testRunID); err == nil {
		t.Error("Grant denial should not write an approval record")
	}
}

func TestGrant_IdempotentWhenAlreadyConfirmed(t *testing.T) {
	stateDir := t.TempDir()
	sess := sampleSession()
	pending := sampleIssue(73)
	if err := approvals.WriteRunContext(stateDir, testRunID, approvals.RunContext{Session: sess, RunPID: os.Getpid(), PendingIssue: &pending, WrittenAt: time.Now()}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	seedConfirmed(t, stateDir, testRunID, sess, sampleIssue(73))
	stub := &prompt.StubPrompter{Response: "wrong"}
	cfg := Config{
		StateDir:      stateDir,
		RunID:         testRunID,
		PromptTimeout: time.Second,
		Stderr:        &bytes.Buffer{},
		Prompter:      stub,
		Logger:        discardLogger(),
	}
	if err := Grant(context.Background(), cfg); err != nil {
		t.Fatalf("already-confirmed grant must succeed without prompting: %v", err)
	}
	if stub.Calls != 0 {
		t.Error("already-confirmed grant must not re-prompt")
	}
}

// writeSessionFile writes a minimal dev-session.yaml and returns its path.
func writeSessionFile(t *testing.T, s session.Session) string {
	t.Helper()
	path := t.TempDir() + "/dev-session.yaml"
	body := "id: " + s.ID + "\nrole: " + s.Role + "\nrepos:\n  - " + s.Repos[0] + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return path
}

// TestObtainIssueApproval_CeremonySerialized pins MEDIUM-1 (security review
// round 2): concurrent declares must NOT interleave Form A prompts on the
// one /dev/tty. The prompter asserts it is never re-entered, and a queued
// declare for an issue confirmed while it waited must not re-prompt.
func TestObtainIssueApproval_CeremonySerialized(t *testing.T) {
	t.Setenv("TMUX", "")
	sess := sampleSession()
	cfg := defaultCfg(t, nil, io.Discard, nil)

	var live atomic.Int32
	var prompts atomic.Int32
	cfg.Prompter = promptFunc(func(_ context.Context, req prompt.Request) (prompt.Result, error) {
		if live.Add(1) != 1 {
			t.Error("two Form A ceremonies were live at once — prompt blocks would interleave on the tty")
		}
		prompts.Add(1)
		time.Sleep(20 * time.Millisecond) // hold the "tty"
		live.Add(-1)
		return prompt.Result{Approved: true}, nil // the human types the displayed number
	})

	// Ten concurrent declares of the SAME issue (a prompt storm).
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !ObtainIssueApproval(context.Background(), req73(sess), cfg) {
				t.Error("declare should confirm")
			}
		}()
	}
	wg.Wait()

	if got := prompts.Load(); got != 1 {
		t.Errorf("prompts = %d, want exactly 1 (the rest must see the confirmation and not re-prompt)", got)
	}
	rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID)
	if err != nil || len(rec.Issues) != 1 {
		t.Errorf("the confirmed set must hold exactly one entry, got %+v (err=%v)", rec.Issues, err)
	}
}

// promptFunc adapts a func to prompt.Prompter.
type promptFunc func(context.Context, prompt.Request) (prompt.Result, error)

func (f promptFunc) Confirm(ctx context.Context, req prompt.Request) (prompt.Result, error) {
	return f(ctx, req)
}
