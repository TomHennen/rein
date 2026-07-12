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
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

func sess1() session.Session {
	return session.Session{ID: "s1", Role: "implement", Repos: []string{"o/r"}}
}

func metaFor(repo string, n int) issuemeta.Meta {
	return issuemeta.Meta{Number: n, Repo: repo, Title: "the fetched title", State: "open",
		CanonicalURL: "https://api.github.com/repos/" + repo + "/issues/x"}
}

// deps returns a Deps with a succeeding fetch and an approving stub
// prompter, against a fresh state dir.
func deps(t *testing.T, answer string) (Deps, *prompt.StubPrompter) {
	t.Helper()
	stub := &prompt.StubPrompter{Response: answer}
	d := Deps{
		StateDir: t.TempDir(),
		RunID:    "run1",
		RunPID:   1,
		Session:  sess1(),
		Fetch: func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
			return metaFor(repo, n), nil
		},
		Grant: grant.Config{
			TTL:           time.Hour,
			PromptTimeout: time.Second,
			Prompter:      stub,
			Stderr:        io.Discard,
			TmuxRunner:    func(ctx context.Context, cmd []string) error { return errors.New("no tmux in tests") },
		},
		Logger: log.New(io.Discard, "", 0),
	}
	return d, stub
}

func TestRun_ConfirmHappyPath(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub := deps(t, "73")
	out := Run(context.Background(), d, 73, "")
	if !out.Confirmed || out.Audit != AuditConfirmed {
		t.Fatalf("expected confirmed outcome, got %+v", out)
	}
	if !strings.Contains(out.Message, "agent/73/") {
		t.Errorf("confirmation message should teach the push convention, got %q", out.Message)
	}
	// The prompt carried the FETCHED snapshot (decision E).
	if stub.Last.Title != "the fetched title" || stub.Last.IssueRepo != "o/r" {
		t.Errorf("prompt must carry fetched title + home repo: %+v", stub.Last)
	}
	// And the record now holds the confirmed issue.
	rec, err := approvals.ReadApproval(d.StateDir, d.RunID)
	if err != nil || !rec.HasIssue("o/r", 73) {
		t.Errorf("confirmed issue not recorded: %+v err=%v", rec, err)
	}
}

func TestRun_HumanDenies(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _ := deps(t, "wrong")
	out := Run(context.Background(), d, 73, "")
	if out.Confirmed || out.Audit != AuditDenied {
		t.Fatalf("expected denied outcome, got %+v", out)
	}
	if _, err := approvals.ReadApproval(d.StateDir, d.RunID); err == nil {
		t.Error("denied declare must record nothing")
	}
}

func TestRun_FetchFailuresFailClosed(t *testing.T) {
	t.Setenv("TMUX", "")
	cases := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{"not found", issuemeta.ErrNotFound, "not found in o/r"},
		{"transferred", issuemeta.ErrTransferred, "TRANSFERRED"},
		{"network", errors.New("dial tcp: timeout"), "could not verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, stub := deps(t, "73")
			d.Fetch = func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
				return issuemeta.Meta{}, tc.err
			}
			out := Run(context.Background(), d, 73, "")
			if out.Confirmed {
				t.Fatal("fetch failure must fail the declare closed (§6)")
			}
			if out.Audit != AuditUnverified {
				t.Errorf("audit = %q, want %q", out.Audit, AuditUnverified)
			}
			if !strings.Contains(out.Message, tc.wantMsg) {
				t.Errorf("message %q missing %q", out.Message, tc.wantMsg)
			}
			if stub.Calls != 0 {
				t.Error("no prompt may fire without a fetched title (decision E)")
			}
		})
	}
}

func TestRun_IdempotentNoRefetchNoReprompt(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub := deps(t, "73")
	if out := Run(context.Background(), d, 73, ""); !out.Confirmed {
		t.Fatalf("first declare should confirm: %+v", out)
	}
	fetches := 0
	d.Fetch = func(ctx context.Context, repo string, n int) (issuemeta.Meta, error) {
		fetches++
		return metaFor(repo, n), nil
	}
	stub.Response = "never right"
	out := Run(context.Background(), d, 73, "")
	if !out.Confirmed {
		t.Fatalf("re-declare of a confirmed issue must return confirmed: %+v", out)
	}
	if fetches != 0 {
		t.Error("idempotent re-declare must not refetch")
	}
	if stub.Calls != 1 { // only the first declare prompted
		t.Errorf("idempotent re-declare must not re-prompt (calls=%d)", stub.Calls)
	}
}

func TestRun_SecondIssueIsExpansion(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub := deps(t, "73")
	if out := Run(context.Background(), d, 73, ""); !out.Confirmed {
		t.Fatal("first declare should confirm")
	}
	stub.Response = "99"
	out := Run(context.Background(), d, 99, "")
	if !out.Confirmed || out.Audit != AuditExpanded {
		t.Fatalf("second issue should be an expansion, got %+v", out)
	}
	if !stub.Last.Expansion {
		t.Error("second declare must render the expansion prompt")
	}
	rec, _ := approvals.ReadApproval(d.StateDir, d.RunID)
	if !rec.HasIssue("o/r", 73) || !rec.HasIssue("o/r", 99) {
		t.Errorf("both issues must be in the set: %+v", rec.Issues)
	}
}

func TestRun_RepoResolution(t *testing.T) {
	t.Setenv("TMUX", "")

	t.Run("cross-owner repo denied structurally, no prompt", func(t *testing.T) {
		// Session owner is "o"; "evil/other" is a DIFFERENT owner, so it can
		// never be added — the App installation is single-owner (issue #69).
		// This is a structural deny, not a human decision: no prompt fires.
		d, stub := deps(t, "73")
		out := Run(context.Background(), d, 73, "evil/other")
		if out.Confirmed || out.Audit != AuditCrossOwner {
			t.Fatalf("cross-owner --repo must be refused structurally, got %+v", out)
		}
		if stub.Calls != 0 {
			t.Error("no prompt for a cross-owner repo")
		}
		if !strings.Contains(out.Message, "single-owner") {
			t.Errorf("denial must explain the single-owner rule: %q", out.Message)
		}
	})

	t.Run("multi-repo session requires --repo", func(t *testing.T) {
		d, _ := deps(t, "73")
		d.Session.Repos = []string{"o/a", "o/b"}
		out := Run(context.Background(), d, 73, "")
		if out.Confirmed || out.Audit != AuditBadRequest {
			t.Fatalf("ambiguous declare must be denied, got %+v", out)
		}
		if !strings.Contains(out.Message, "--repo") {
			t.Errorf("denial must name the --repo instruction: %q", out.Message)
		}
	})

	t.Run("explicit in-scope repo works in multi-repo session", func(t *testing.T) {
		d, _ := deps(t, "73")
		d.Session.Repos = []string{"o/a", "o/b"}
		out := Run(context.Background(), d, 73, "o/b")
		if !out.Confirmed {
			t.Fatalf("in-scope --repo should confirm, got %+v", out)
		}
		if out.Repo != "o/b" {
			t.Errorf("resolved repo = %q, want o/b", out.Repo)
		}
	})
}

func TestRun_InvalidInputs(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _ := deps(t, "73")
	if out := Run(context.Background(), d, 0, ""); out.Confirmed || out.Audit != AuditBadRequest {
		t.Error("issue 0 must be refused")
	}
	if out := Run(context.Background(), d, -5, ""); out.Confirmed {
		t.Error("negative issue must be refused")
	}
	d.RunID = ""
	out := Run(context.Background(), d, 73, "")
	if out.Confirmed || !strings.Contains(out.Message, "rein run") {
		t.Errorf("no-run-id declare must fail with the launch instruction, got %+v", out)
	}
}

// TestRun_MessagesNeverCarryATokenShape is a cheap redaction guard: the
// outcome Message crosses the proxy back into the sandbox, so it must be
// built only from fixed strings + number + repo.
func TestRun_MessagesNeverCarryATokenShape(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _ := deps(t, "73")
	for _, out := range []Outcome{
		Run(context.Background(), d, 73, ""),
		Run(context.Background(), d, 0, ""),
		Run(context.Background(), d, 73, "evil/other"),
	} {
		if strings.Contains(out.Message, "ghs_") || strings.Contains(out.Message, "Bearer") {
			t.Errorf("outcome message must never resemble a credential: %q", out.Message)
		}
	}
}
