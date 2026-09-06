package declare

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/ui/grant"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// createSpy records what RunNew asked to be filed and answers with a
// canned issue (or an error).
type createSpy struct {
	calls []string // "repo|title|body" per call
	meta  issuemeta.Meta
	err   error
}

func (c *createSpy) fn(ctx context.Context, repo, title, body string) (issuemeta.Meta, error) {
	c.calls = append(c.calls, repo+"|"+title+"|"+body)
	if c.err != nil {
		return issuemeta.Meta{}, c.err
	}
	m := c.meta
	m.Repo = repo
	return m, nil
}

// newDeps wires RunNew against a fresh state dir with a stub prompter.
// ProbeInstall must be non-nil — a nil probe fails every declare closed.
func newDeps(t *testing.T, answer string) (Deps, *prompt.StubPrompter, *createSpy) {
	t.Helper()
	stub := &prompt.StubPrompter{Response: answer}
	spy := &createSpy{meta: issuemeta.Meta{
		Number: 7, Title: "Add a thing", State: "open",
		CanonicalURL: "https://api.github.com/repos/o/r/issues/7",
	}}
	d := Deps{
		StateDir:     t.TempDir(),
		RunID:        "run1",
		RunPID:       1,
		Session:      sess1(),
		ProbeInstall: func(context.Context, string) error { return nil },
		CreateIssue:  spy.fn,
		Grant: grant.Config{
			TTL:             time.Hour,
			ApprovalTimeout: time.Second,
			Prompter:        stub,
			Stderr:          io.Discard,
			TmuxRunner:      func(ctx context.Context, cmd []string) error { return errors.New("no tmux in tests") },
		},
		Logger: log.New(io.Discard, "", 0),
	}
	return d, stub, spy
}

func newReq() NewIssue { return NewIssue{Title: "Add a thing", Body: "because reasons"} }

func TestRunNew_ApproveFilesAndRecords(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, spy := newDeps(t, "add") // the first word, case-insensitively

	out := RunNew(context.Background(), d, newReq())
	if !out.Confirmed || out.Audit != AuditConfirmedNew || out.Issue != 7 {
		t.Fatalf("expected a confirmed filing, got %+v", out)
	}
	if !strings.Contains(out.Message, "agent/7/") {
		t.Errorf("message must teach the push convention: %q", out.Message)
	}
	if len(spy.calls) != 1 || spy.calls[0] != "o/r|Add a thing|because reasons" {
		t.Fatalf("CreateIssue calls = %v", spy.calls)
	}
	// The human saw the proposal, and the token was the title's first word.
	if !stub.Last.NewIssue || stub.Last.Title != "Add a thing" || stub.Last.IssueRepo != "o/r" {
		t.Errorf("prompt did not carry the proposal: %+v", stub.Last)
	}
	if stub.Last.ConfirmWord != "Add" {
		t.Errorf("ConfirmWord = %q, want %q", stub.Last.ConfirmWord, "Add")
	}
	if !strings.Contains(stub.Last.Body, "because reasons") {
		t.Errorf("prompt must show the proposed body: %q", stub.Last.Body)
	}
	// And the issue is now confirmed for the run, so writes flow.
	rec, err := approvals.ReadApproval(d.StateDir, d.RunID)
	if err != nil || !rec.HasIssue("o/r", 7) {
		t.Fatalf("filed issue not recorded: %+v err=%v", rec, err)
	}
	if rec.Issues[0].CanonicalURL == "" {
		t.Error("recorded issue has no canonical URL — the TM-G6 re-check would never cover it")
	}
	// The pending request must not outlive the ceremony.
	if rc, err := approvals.ReadRunContext(d.StateDir, d.RunID); err == nil && rc.PendingNewIssue != nil {
		t.Error("pending new-issue request was left behind")
	}
}

func TestRunNew_DenyFilesNothing(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, spy := newDeps(t, "nope")

	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || out.Audit != AuditNewDenied {
		t.Fatalf("expected a denial, got %+v", out)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("a denied request must file nothing, got %v", spy.calls)
	}
	if _, err := approvals.ReadApproval(d.StateDir, d.RunID); err == nil {
		t.Error("a denied request must record nothing")
	}
}

// TestRunNew_CreateFailureAfterApproval: the human approved and nothing
// exists — the message must say both halves, and nothing may be recorded.
func TestRunNew_CreateFailureAfterApproval(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, spy := newDeps(t, "add")
	spy.err = errors.New("403 Resource not accessible by integration")

	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || out.Audit != AuditNewCreateFailed {
		t.Fatalf("expected a create failure, got %+v", out)
	}
	for _, want := range []string{"APPROVED", "could not complete filing", "MAY OR MAY NOT", "issues:write"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message missing %q: %q", want, out.Message)
		}
	}
	// rein cannot know the remote outcome: a read timeout or an unparseable
	// response lands here with the issue possibly created. Claiming
	// otherwise would send the human away without checking, and the
	// suggested retry would then file a duplicate.
	if strings.Contains(out.Message, "Nothing was filed") {
		t.Errorf("message asserts a remote fact it cannot know: %q", out.Message)
	}
	if _, err := approvals.ReadApproval(d.StateDir, d.RunID); err == nil {
		t.Error("a failed create must record nothing")
	}
}

// TestRunNew_OutOfScopeRepoRefused: --new must not become a scope
// expansion channel — expansion is approved against a FETCHED issue in
// the candidate repo, not a title the agent made up.
func TestRunNew_OutOfScopeRepoRefused(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, spy := newDeps(t, "add")

	out := RunNew(context.Background(), d, NewIssue{Repo: "o/other", Title: "Add a thing"})
	if out.Confirmed || out.Audit != AuditNewBadRequest {
		t.Fatalf("expected a refusal, got %+v", out)
	}
	if !strings.Contains(out.Message, "cannot widen") {
		t.Errorf("message must explain the refusal: %q", out.Message)
	}
	if stub.Calls != 0 || len(spy.calls) != 0 {
		t.Error("an out-of-scope repo must be refused before the prompt")
	}
}

func TestRunNew_CrossOwnerRepoRefused(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, _ := newDeps(t, "add")
	out := RunNew(context.Background(), d, NewIssue{Repo: "someone-else/r", Title: "Add a thing"})
	if out.Confirmed || stub.Calls != 0 {
		t.Fatalf("cross-owner must be refused structurally, got %+v", out)
	}
}

// TestRunNew_NotInstalledRefusedBeforePrompt: nothing can be filed on a
// repo the App is not installed on, so no prompt fires — the human gets
// the install notice instead.
func TestRunNew_NotInstalledRefusedBeforePrompt(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, spy := newDeps(t, "add")
	d.ProbeInstall = func(context.Context, string) error { return githubapp.ErrAppNotInstalled }
	noticed := 0
	d.Notice = func(context.Context, Notice) { noticed++ }

	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || out.Audit != AuditNotInstalled {
		t.Fatalf("expected the not-installed refusal, got %+v", out)
	}
	if stub.Calls != 0 || len(spy.calls) != 0 {
		t.Error("no prompt may fire for a repo the App cannot write to")
	}
	if noticed != 1 {
		t.Errorf("install notice shown %d times, want 1", noticed)
	}
}

func TestRunNew_ProbeFailureFailsClosed(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, spy := newDeps(t, "add")
	d.ProbeInstall = func(context.Context, string) error { return errors.New("network blip") }
	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || out.Audit != AuditCoverageUnknown || len(spy.calls) != 0 {
		t.Fatalf("a probe failure must fail closed, got %+v", out)
	}
}

// TestRunNew_NoCreateHookRefusesBeforePrompt: asking the human to approve
// something rein cannot then perform trains rubber-stamping.
func TestRunNew_NoCreateHookRefusesBeforePrompt(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, _ := newDeps(t, "add")
	d.CreateIssue = nil
	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || out.Audit != AuditNewBadRequest || stub.Calls != 0 {
		t.Fatalf("a nil CreateIssue must refuse before prompting, got %+v (prompts=%d)", out, stub.Calls)
	}
}

// TestRunNew_ValidatesHostSide: the broker re-checks what the sandbox
// sent — a title carrying an escape never reaches a terminal.
func TestRunNew_ValidatesHostSide(t *testing.T) {
	t.Setenv("TMUX", "")
	for _, bad := range []NewIssue{
		{Title: "ok\x1b[2Jgone"},
		{Title: "   "},
		{Title: "--- ???"},
		{Title: strings.Repeat("x", 201)},
		{Title: "fine", Body: "a\x00b"},
	} {
		d, stub, spy := newDeps(t, "ok")
		out := RunNew(context.Background(), d, bad)
		if out.Confirmed || out.Audit != AuditNewBadRequest {
			t.Errorf("%q accepted: %+v", bad.Title, out)
		}
		if stub.Calls != 0 || len(spy.calls) != 0 {
			t.Errorf("%q reached the prompt or the create hook", bad.Title)
		}
	}
}

func TestRunNew_NoRunIDFailsClosed(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, spy := newDeps(t, "add")
	d.RunID = ""
	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || !strings.Contains(out.Message, "rein run") || len(spy.calls) != 0 {
		t.Fatalf("outside a run, --new must fail with the launch instruction: %+v", out)
	}
}

// --- the out-of-process (popup) handoff ---
//
// A new issue has no number, so the popup cannot answer by appending to
// the approval record the way a declared issue does. It nonce-matches the
// pending request and sets its Approved marker instead; only the broker
// acts on that. These two tests are the mechanism and its negative twin.

func TestRunNew_ApprovedViaPopupHandoff(t *testing.T) {
	t.Setenv("TMUX", "fake")
	d, stub, spy := newDeps(t, "wrong-if-asked")
	d.Grant.PreferPopup = true
	stateDir, runID := d.StateDir, d.RunID
	d.Grant.TmuxRunner = func(ctx context.Context, cmd []string) error {
		// Stand in for `rein approval grant` in the popup: read the pending
		// request and mark it approved by its nonce.
		rc, err := approvals.ReadRunContext(stateDir, runID)
		if err != nil || rc.PendingNewIssue == nil {
			t.Errorf("popup found no pending new-issue request: %v", err)
			return nil
		}
		return approvals.MarkNewIssueApproved(stateDir, runID, rc.PendingNewIssue.Nonce)
	}

	out := RunNew(context.Background(), d, newReq())
	if !out.Confirmed || out.Issue != 7 {
		t.Fatalf("popup approval did not confirm: %+v", out)
	}
	if len(spy.calls) != 1 {
		t.Errorf("CreateIssue calls = %v, want 1", spy.calls)
	}
	if stub.Calls != 0 {
		t.Error("the inline prompt must not also fire when the popup answered")
	}
	if rec, err := approvals.ReadApproval(stateDir, runID); err != nil || !rec.HasIssue("o/r", 7) {
		t.Error("popup-approved issue was not recorded")
	}
}

func TestRunNew_PopupWrongNonceDenies(t *testing.T) {
	t.Setenv("TMUX", "fake")
	d, _, spy := newDeps(t, "no")
	d.Grant.PreferPopup = true
	stateDir, runID := d.StateDir, d.RunID
	d.Grant.TmuxRunner = func(ctx context.Context, cmd []string) error {
		// A marker that does not name THIS request must not approve it.
		_ = approvals.MarkNewIssueApproved(stateDir, runID, "some-other-nonce")
		return nil
	}

	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || len(spy.calls) != 0 {
		t.Fatalf("a mismatched nonce must not approve: %+v calls=%v", out, spy.calls)
	}
}
