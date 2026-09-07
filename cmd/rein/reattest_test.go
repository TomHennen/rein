package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/runbroker"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/tokencache"
)

func TestResolveApprovalTTL(t *testing.T) {
	if got, over, err := resolveApprovalTTL(""); err != nil || over || got != runbroker.DefaultApprovalTTL {
		t.Errorf("default = (%s,%v,%v), want (%s,false,nil)", got, over, err, runbroker.DefaultApprovalTTL)
	}
	if got, over, err := resolveApprovalTTL("45s"); err != nil || !over || got != 45*time.Second {
		t.Errorf("45s = (%s,%v,%v), want (45s,true,nil)", got, over, err)
	}
	// Same floor and same fail-closed parse as the idle knob, under its OWN name
	// so the error tells the operator which variable to fix.
	for _, raw := range []string{"9s", "0", "30", "soon"} {
		_, _, err := resolveApprovalTTL(raw)
		if err == nil {
			t.Errorf("resolveApprovalTTL(%q) accepted a bad value", raw)
			continue
		}
		if !strings.Contains(err.Error(), envApprovalTTL) {
			t.Errorf("resolveApprovalTTL(%q) error %q must name %s", raw, err, envApprovalTTL)
		}
	}
}

// TestLastApprovalHook is the ApprovalTTL clock's only input: the NEWEST
// confirmation across the run's issues, and false when nothing is confirmed.
func TestLastApprovalHook(t *testing.T) {
	dir := t.TempDir()
	sess := session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
	sig := approvals.SignatureOf(sess)
	hook := lastApprovalHook(sess, dir, "run-1")

	if _, ok := hook(); ok {
		t.Error("a run with no approval record must report no confirmation")
	}

	older := time.Now().Add(-2 * time.Hour).Round(0)
	newer := time.Now().Add(-time.Minute).Round(0)
	for _, ci := range []approvals.ConfirmedIssue{
		{Number: 7, Repo: "o/r", CanonicalURL: "u7", ConfirmedAt: older},
		{Number: 9, Repo: "o/r", CanonicalURL: "u9", ConfirmedAt: newer},
	} {
		if err := approvals.AppendConfirmedIssue(dir, "run-1", sig, sess.ID, ci, time.Hour); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// The clock runs from the LATEST confirmation: a re-declare must buy a full
	// fresh TTL, not inherit the first declare's age.
	got, ok := hook()
	if !ok {
		t.Fatal("hook must report a confirmation once one exists")
	}
	if !got.Equal(newer) {
		t.Errorf("hook = %s, want the newest confirmation %s", got, newer)
	}

	// After a withdrawal there is nothing to age out.
	if err := approvals.ClearConfirmedIssues(dir, "run-1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := hook(); ok {
		t.Error("after the confirmed set is cleared the hook must report no confirmation")
	}
}

func TestResolveIdleTimeout(t *testing.T) {
	cases := []struct {
		raw        string
		want       time.Duration
		overridden bool
		wantErr    string
	}{
		{raw: "", want: runbroker.DefaultIdleTimeout},
		{raw: "45s", want: 45 * time.Second, overridden: true},
		{raw: "10s", want: 10 * time.Second, overridden: true},
		{raw: "2h", want: 2 * time.Hour, overridden: true},
		{raw: "9s", wantErr: "below the 10s minimum"},
		{raw: "0", wantErr: "below the 10s minimum"},
		{raw: "30", wantErr: "not a Go duration"}, // bare number: no unit
		{raw: "soon", wantErr: "not a Go duration"},
	}
	for _, tc := range cases {
		got, overridden, err := resolveIdleTimeout(tc.raw)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("resolveIdleTimeout(%q) error = %v, want one containing %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveIdleTimeout(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want || overridden != tc.overridden {
			t.Errorf("resolveIdleTimeout(%q) = (%s, %v), want (%s, %v)", tc.raw, got, overridden, tc.want, tc.overridden)
		}
	}
}

// seedApprovedRun writes the on-disk state of a run that declared issue 7, was
// confirmed, and minted one write token.
func seedApprovedRun(t *testing.T, dir, runID string, sess session.Session) {
	t.Helper()
	if err := approvals.WriteRunContext(dir, runID, approvals.RunContext{
		Session: sess, RunPID: os.Getpid(), WrittenAt: time.Now(),
		PendingIssue: &approvals.ConfirmedIssue{Number: 7, Repo: "o/r", Title: "t", CanonicalURL: "u"},
	}); err != nil {
		t.Fatalf("write run context: %v", err)
	}
	ci := approvals.ConfirmedIssue{Number: 7, Repo: "o/r", Title: "t", CanonicalURL: "u", ConfirmedAt: time.Now()}
	if err := approvals.AppendConfirmedIssue(dir, runID, approvals.SignatureOf(sess), sess.ID, ci, time.Hour); err != nil {
		t.Fatalf("append confirmed issue: %v", err)
	}
	if err := approvals.AppendWriteToken(dir, runID, tokencache.Entry{Token: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("append write token: %v", err)
	}
}

// TestIdleReattestLocksWritesThenReDeclareUnlocks is the end-to-end #190
// proof, driven through the REAL gate closures the sandboxed proxy calls on
// every write: approved -> idle lock -> both gates refuse -> a fresh Form A
// confirmation -> both gates allow again.
func TestIdleReattestLocksWritesThenReDeclareUnlocks(t *testing.T) {
	dir := t.TempDir()
	runID := "run-1"
	sess := session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
	logger := log.New(io.Discard, "", 0)
	seedApprovedRun(t, dir, runID, sess)

	approve := buildSandboxApprove(sess, dir, runID, logger)
	hooks := buildDeclarationHooks(declareEnv{sess: sess, stateDir: dir, runID: runID, approve: approve, logger: logger})

	if !hooks.WriteApproved("o/r") || !hooks.IssueConfirmed("o/r", 7) {
		t.Fatal("setup: a confirmed run must be write-approved")
	}

	var out bytes.Buffer
	revoked := 0
	withdrawn, err := reattest(reattestDeps{
		stateDir: dir, runID: runID, idle: 30 * time.Minute,
		drainTokens: func() error { revoked++; return approvals.ClearWriteTokens(dir, runID) },
		out:         &out, logger: logger,
	}, runbroker.ExpireIdle)
	if err != nil {
		t.Fatalf("idleReattest: %v", err)
	}
	if !withdrawn {
		t.Error("idleReattest must report that it withdrew an approval (the audit decision depends on it)")
	}

	if revoked != 1 {
		t.Errorf("write-token revoke ran %d times, want 1", revoked)
	}
	if _, err := approvals.ReadWriteTokens(dir, runID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write-token ledger after the lock: %v, want cleared", err)
	}
	if hooks.WriteApproved("o/r") {
		t.Error("WriteApproved is still true after the idle lock: writes were NOT withdrawn")
	}
	if hooks.IssueConfirmed("o/r", 7) {
		t.Error("IssueConfirmed is still true after the idle lock: the push-ref cross-check would still pass")
	}
	// The run keeps everything the next declare needs, MINUS the stale
	// declaration snapshot an out-of-process grant could otherwise re-approve.
	rc, err := approvals.ReadRunContext(dir, runID)
	if err != nil {
		t.Errorf("run context must survive the lock: %v", err)
	} else {
		if rc.Session.ID != sess.ID || rc.RunPID != os.Getpid() {
			t.Errorf("run context lost state the next declare needs: %+v", rc)
		}
		if rc.PendingIssue != nil {
			t.Errorf("stale PendingIssue survived the lock (%+v); `rein approval grant` could re-open writes with no fresh declare", rc.PendingIssue)
		}
	}
	for _, want := range []string{"writes LOCKED", "keeps running", "declare its issue again", "Reads still work"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("banner missing %q; got:\n%s", want, out.String())
		}
	}

	// The human re-confirms through the full ceremony: the gates open again.
	next := approvals.ConfirmedIssue{Number: 7, Repo: "o/r", Title: "t", CanonicalURL: "u", ConfirmedAt: time.Now()}
	if err := approvals.AppendConfirmedIssue(dir, runID, approvals.SignatureOf(sess), sess.ID, next, time.Hour); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	if !hooks.WriteApproved("o/r") || !hooks.IssueConfirmed("o/r", 7) {
		t.Error("a re-declared, re-confirmed issue must unlock writes again")
	}
}

// TestApprovalAgeReattestSharesTheWithdrawal: the age bound performs the SAME
// in-place withdrawal as the idle bound — only the banner's first line differs,
// because the operator needs to know which bound tripped.
func TestApprovalAgeReattestSharesTheWithdrawal(t *testing.T) {
	dir := t.TempDir()
	runID := "run-1"
	sess := session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
	logger := log.New(io.Discard, "", 0)
	seedApprovedRun(t, dir, runID, sess)

	approve := buildSandboxApprove(sess, dir, runID, logger)
	hooks := buildDeclarationHooks(declareEnv{sess: sess, stateDir: dir, runID: runID, approve: approve, logger: logger})

	var out bytes.Buffer
	withdrawn, err := reattest(reattestDeps{
		stateDir: dir, runID: runID, idle: 30 * time.Minute, approvalTTL: 4 * time.Hour,
		drainTokens: func() error { return approvals.ClearWriteTokens(dir, runID) },
		out:         &out, logger: logger,
	}, runbroker.ExpireApprovalAge)
	if err != nil || !withdrawn {
		t.Fatalf("reattest(approval-age) = (%v,%v), want (true,nil)", withdrawn, err)
	}

	// Same withdrawal as the idle path.
	if hooks.WriteApproved("o/r") || hooks.IssueConfirmed("o/r", 7) {
		t.Error("the approval-age withdrawal must lock writes exactly like the idle one")
	}
	if _, err := approvals.ReadWriteTokens(dir, runID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write-token ledger after the age lock: %v, want cleared", err)
	}
	if rc, err := approvals.ReadRunContext(dir, runID); err != nil {
		t.Errorf("run context must survive: %v", err)
	} else if rc.PendingIssue != nil {
		t.Error("the stale pending declaration must be cleared by the age lock too")
	}

	// Different first line, same reassurance.
	got := out.String()
	if !strings.Contains(got, "write approval is 4h0m0s old — writes LOCKED") {
		t.Errorf("age banner must say the approval aged out; got:\n%s", got)
	}
	if strings.Contains(got, "idle for") {
		t.Errorf("age banner must not blame idleness; got:\n%s", got)
	}
	for _, want := range []string{"keeps running", "declare its issue again", "Reads still work"} {
		if !strings.Contains(got, want) {
			t.Errorf("age banner missing %q; got:\n%s", want, got)
		}
	}

	// And a re-confirmation unlocks writes again, same as the idle path.
	next := approvals.ConfirmedIssue{Number: 7, Repo: "o/r", CanonicalURL: "u", ConfirmedAt: time.Now()}
	if err := approvals.AppendConfirmedIssue(dir, runID, approvals.SignatureOf(sess), sess.ID, next, time.Hour); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	if !hooks.WriteApproved("o/r") || !hooks.IssueConfirmed("o/r", 7) {
		t.Error("a re-declared, re-confirmed issue must unlock writes after an age lock")
	}
}

// TestIdleReattestQuietWhenNothingApproved: a run that never declared has
// nothing to withdraw. It must not revoke, not print, and not fail — otherwise
// a read-only agent gets banner-spammed about an approval it never had.
func TestIdleReattestQuietWhenNothingApproved(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	revoked := 0
	withdrawn, err := reattest(reattestDeps{
		stateDir: dir, runID: "run-none", idle: time.Minute,
		drainTokens: func() error { revoked++; return nil },
		out:         &out, logger: log.New(io.Discard, "", 0),
	}, runbroker.ExpireIdle)
	if err != nil {
		t.Fatalf("idleReattest on an unapproved run: %v", err)
	}
	if withdrawn {
		t.Error("a run with nothing to withdraw must report withdrawn=false")
	}
	if revoked != 0 {
		t.Errorf("revoke ran %d times for a run with no tokens, want 0", revoked)
	}
	if out.Len() != 0 {
		t.Errorf("banner printed for a run with nothing to withdraw:\n%s", out.String())
	}
}

// TestIdleReattestFailsClosedWhenRecordUnwritable: if the approval record
// cannot be removed, rein must revoke anyway, tell the human loudly that the
// proxy is going down, and report the error so runbroker tears the host down
// (the pre-#190 behavior).
func TestIdleReattestFailsClosedWhenRecordUnwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	runID := "run-1"
	sess := session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
	seedApprovedRun(t, dir, runID, sess)

	approvalDir := filepath.Dir(approvals.RunApprovalPath(dir, runID))
	if err := os.Chmod(approvalDir, 0o500); err != nil {
		t.Fatalf("chmod approvals dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(approvalDir, 0o700) })

	var out bytes.Buffer
	revoked := 0
	_, err := reattest(reattestDeps{
		stateDir: dir, runID: runID, idle: time.Minute,
		drainTokens: func() error { revoked++; return approvals.ClearWriteTokens(dir, runID) },
		out:         &out, logger: log.New(io.Discard, "", 0),
	}, runbroker.ExpireIdle)
	if err == nil {
		t.Fatal("idleReattest must report an un-withdrawable approval so the host stops the proxy")
	}
	if revoked != 1 {
		t.Errorf("revoke ran %d times, want 1 — tokens must be revoked even on the failure path", revoked)
	}
	// This branch ends the run's credential path, so it must tear the rest of
	// the per-run state down NOW rather than leave it to the deferred exit-time
	// clear (which a SIGKILL skips) or the next launch's sweep.
	if _, err := approvals.ReadRunContext(dir, runID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run context after the fail-closed teardown: %v, want not-exist", err)
	}
	if _, err := approvals.ReadWriteTokens(dir, runID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write-token ledger after the fail-closed teardown: %v, want not-exist", err)
	}
	for _, want := range []string{"SESSION STOPPED", "STOPPED the credential proxy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("fail-closed banner missing %q; got:\n%s", want, out.String())
		}
	}
}
