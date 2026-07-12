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
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// expansionDeps builds Deps for a session scoped to "TomH/repo-a", with an
// approving prompter and a probe/fetch the test can steer.
func expansionDeps(t *testing.T, answer string) (*Deps, *prompt.StubPrompter, *int) {
	t.Helper()
	stub := &prompt.StubPrompter{Response: answer}
	probes := 0
	d := &Deps{
		StateDir: t.TempDir(),
		RunID:    "runE",
		RunPID:   1,
		Session:  session.Session{ID: "sE", Role: "implement", Repos: []string{"TomH/repo-a"}},
		Fetch: func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
			return metaFor(repo, n), nil
		},
		ProbeInstall: func(ctx context.Context, repo string) error {
			probes++
			return nil // installed
		},
		InstallURL: "https://github.com/apps/rein/installations/new",
		AppName:    "rein-test-app",
		Grant: grant.Config{
			TTL:           time.Hour,
			PromptTimeout: time.Second,
			Prompter:      stub,
			Stderr:        io.Discard,
			TmuxRunner:    func(ctx context.Context, cmd []string) error { return errors.New("no tmux") },
		},
		Logger: log.New(io.Discard, "", 0),
	}
	return d, stub, &probes
}

// TestExpansion_SameOwnerApproved is the spine: a same-owner repo outside the
// session, approved, joins the run's confirmed set as an expansion.
func TestExpansion_SameOwnerApproved(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, probes := expansionDeps(t, "7")
	out := Run(context.Background(), *d, 7, "TomH/repo-b")
	if !out.Confirmed || out.Audit != AuditScopeExpanded {
		t.Fatalf("same-owner expansion must confirm with AuditScopeExpanded, got %+v", out)
	}
	if *probes != 1 {
		t.Errorf("install coverage must be probed exactly once, got %d", *probes)
	}
	if stub.Last.AddRepo != "TomH/repo-b" {
		t.Errorf("prompt must be marked as a repo expansion (AddRepo), got %q", stub.Last.AddRepo)
	}
	// The expansion must be recorded so the effective ceiling widens.
	rec, err := approvals.ReadApproval(d.StateDir, d.RunID)
	if err != nil || !rec.HasIssue("TomH/repo-b", 7) {
		t.Fatalf("expansion must append a ConfirmedIssue for the new repo; rec=%+v err=%v", rec, err)
	}
	if !strings.Contains(out.Message, "now in scope") {
		t.Errorf("agent message must confirm the repo joined scope: %q", out.Message)
	}
}

// TestExpansion_CrossOwnerDeniedNoPrompt: a different-owner repo is refused
// structurally — before any fetch, probe, or prompt.
func TestExpansion_CrossOwnerDeniedNoPrompt(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, probes := expansionDeps(t, "7")
	fetches := 0
	d.Fetch = func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
		fetches++
		return metaFor(repo, n), nil
	}
	out := Run(context.Background(), *d, 7, "other-owner/thing")
	if out.Confirmed || out.Audit != AuditCrossOwner {
		t.Fatalf("cross-owner must be refused structurally, got %+v", out)
	}
	if stub.Calls != 0 {
		t.Error("no prompt may fire for a cross-owner repo")
	}
	if fetches != 0 || *probes != 0 {
		t.Errorf("cross-owner must short-circuit before fetch/probe (fetches=%d probes=%d)", fetches, *probes)
	}
	if !strings.Contains(out.Message, "single-owner") {
		t.Errorf("denial must explain the single-owner rule: %q", out.Message)
	}
}

// TestExpansion_NotInstalledShowsNoticeNoPrompt: a 404 from the probe means
// nothing is approvable — a NOTICE fires, never an approval prompt.
func TestExpansion_NotInstalledShowsNoticeNoPrompt(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, _ := expansionDeps(t, "7")
	d.ProbeInstall = func(ctx context.Context, repo string) error {
		return githubapp.ErrAppNotInstalled
	}
	noticed := ""
	d.Notice = func(ctx context.Context, n Notice) { noticed = n.Repo }
	fetches := 0
	d.Fetch = func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
		fetches++
		return metaFor(repo, n), nil
	}
	out := Run(context.Background(), *d, 7, "TomH/repo-b")
	if out.Confirmed || out.Audit != AuditNotInstalled {
		t.Fatalf("uninstalled repo must refuse with AuditNotInstalled, got %+v", out)
	}
	if stub.Calls != 0 {
		t.Error("no APPROVAL prompt may fire when nothing is installable")
	}
	if noticed != "TomH/repo-b" {
		t.Errorf("the install NOTICE must fire for the repo, got %q", noticed)
	}
	if fetches != 0 {
		t.Error("no fetch may happen for an uninstalled repo (no token could read it anyway)")
	}
	if !strings.Contains(out.Message, "not installed") || !strings.Contains(out.Message, "rein-test-app") {
		t.Errorf("agent message must name the App and say not installed: %q", out.Message)
	}
}

// TestExpansion_TransientProbeFailsClosed: a non-404 probe error fails the
// declare closed (retry), never prompts — there is no cached fallback here.
func TestExpansion_TransientProbeFailsClosed(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, _ := expansionDeps(t, "7")
	d.ProbeInstall = func(ctx context.Context, repo string) error {
		return errors.New("github 503")
	}
	out := Run(context.Background(), *d, 7, "TomH/repo-b")
	if out.Confirmed || out.Audit != AuditCoverageUnknown {
		t.Fatalf("transient probe error must fail closed, got %+v", out)
	}
	if stub.Calls != 0 {
		t.Error("no prompt on an unverified expansion")
	}
	if !strings.Contains(out.Message, "could not verify") {
		t.Errorf("message must say retry/verify: %q", out.Message)
	}
}

// TestExpansion_NilProbeFailsClosed: a caller that forgets to wire the probe
// must not get an unprobed (potentially 404) expansion.
func TestExpansion_NilProbeFailsClosed(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, _ := expansionDeps(t, "7")
	d.ProbeInstall = nil
	out := Run(context.Background(), *d, 7, "TomH/repo-b")
	if out.Confirmed || out.Audit != AuditCoverageUnknown {
		t.Fatalf("a nil probe must fail the expansion closed, got %+v", out)
	}
}

// TestExpansion_DeniedContinuesAtOriginalScope: a wrong answer denies; the
// message tells the agent to keep working within the original repos and
// nothing is recorded.
func TestExpansion_DeniedContinuesAtOriginalScope(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, _ := expansionDeps(t, "999") // wrong answer
	out := Run(context.Background(), *d, 7, "TomH/repo-b")
	if out.Confirmed || out.Audit != AuditDenied {
		t.Fatalf("wrong answer must deny, got %+v", out)
	}
	if !strings.Contains(out.Message, "remains out of scope") || !strings.Contains(out.Message, "TomH/repo-a") {
		t.Errorf("deny message must steer back to the original scope: %q", out.Message)
	}
	rec, err := approvals.ReadApproval(d.StateDir, d.RunID)
	if err == nil && rec.HasIssue("TomH/repo-b", 7) {
		t.Error("a denied expansion must record nothing")
	}
}

// TestExpansion_InScopeRepoIsNotAnExpansion: --repo naming a repo already in
// the session takes the ordinary confirm path (no probe, no expansion audit).
func TestExpansion_InScopeRepoIsNotAnExpansion(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, probes := expansionDeps(t, "7")
	d.Session.Repos = []string{"TomH/repo-a", "TomH/repo-b"}
	out := Run(context.Background(), *d, 7, "TomH/repo-b")
	if !out.Confirmed || out.Audit != AuditConfirmed {
		t.Fatalf("an in-scope --repo is an ordinary confirm, got %+v", out)
	}
	if *probes != 0 {
		t.Error("an in-scope repo must not be install-probed (it was covered at launch)")
	}
}

var _ = prompt.Request{}
