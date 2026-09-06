package declare

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/ui/grant"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// TestRunNew_PerRunCeremonyCap: this endpoint lets the agent manufacture
// approval prompts out of nothing, so a run gets a fixed budget of them.
// Past it the request is refused WITHOUT reaching the human.
func TestRunNew_PerRunCeremonyCap(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, spy := newDeps(t, "nope") // every ceremony is denied

	for i := 0; i < grant.MaxNewIssueCeremonies; i++ {
		out := RunNew(context.Background(), d, NewIssue{Title: fmt.Sprintf("Proposal number %d", i)})
		if out.Audit != AuditNewDenied {
			t.Fatalf("ceremony %d: audit = %q, want a denial", i, out.Audit)
		}
	}
	prompts := stub.Calls
	if prompts != grant.MaxNewIssueCeremonies {
		t.Fatalf("prompts before the cap = %d, want %d", prompts, grant.MaxNewIssueCeremonies)
	}

	out := RunNew(context.Background(), d, NewIssue{Title: "One request too many"})
	if out.Confirmed || out.Audit != AuditNewLimit {
		t.Fatalf("past the cap: %+v", out)
	}
	if stub.Calls != prompts {
		t.Error("a request past the cap must not reach the human")
	}
	if len(spy.calls) != 0 {
		t.Error("nothing may be filed past the cap")
	}
	if !strings.Contains(out.Message, "rein declare <n>") {
		t.Errorf("the refusal should point at the remaining option: %q", out.Message)
	}
}

// blockingPrompter holds the ceremony open until released, so a second
// concurrent request meets a live lock.
type blockingPrompter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingPrompter) Confirm(ctx context.Context, req prompt.Request) (prompt.Result, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return prompt.Result{}, nil
}

// TestRunNew_ConcurrentRequestRefusedNotQueued: a queued new-issue
// ceremony would surface minutes later asking about something the agent
// long since moved past, and queueing is the prompt-fatigue lever. The
// second request is refused immediately instead.
func TestRunNew_ConcurrentRequestRefusedNotQueued(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, spy := newDeps(t, "irrelevant")
	blocker := &blockingPrompter{entered: make(chan struct{}), release: make(chan struct{})}
	d.Grant.Prompter = blocker
	d.Grant.ApprovalTimeout = 0

	done := make(chan Outcome, 1)
	go func() { done <- RunNew(context.Background(), d, NewIssue{Title: "First proposal here"}) }()
	<-blocker.entered // the first ceremony now holds the lock

	second := RunNew(context.Background(), d, NewIssue{Title: "Second proposal here"})
	if second.Confirmed || second.Audit != AuditNewBusy {
		t.Fatalf("a concurrent request must be refused as busy, got %+v", second)
	}
	if !strings.Contains(second.Message, "already in front of the human") {
		t.Errorf("refusal should explain itself: %q", second.Message)
	}

	close(blocker.release)
	<-done
	if len(spy.calls) != 0 {
		t.Error("neither ceremony approved, so nothing may be filed")
	}
	// The refused request must not have consumed the first one's pending
	// record — the first ceremony still owned it.
	if rc, err := approvals.ReadRunContext(d.StateDir, d.RunID); err == nil && rc.PendingNewIssue != nil {
		t.Error("pending record outlived both ceremonies")
	}
}

// TestRunNew_PromptShowsAlreadyConfirmedIssues: filing another issue when
// the run already has one is usually the agent losing the thread, so the
// human must see what is already confirmed before answering.
func TestRunNew_PromptShowsAlreadyConfirmedIssues(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, _ := newDeps(t, "add")
	if err := approvals.AppendConfirmedIssue(d.StateDir, d.RunID,
		approvals.SignatureOf(d.Session), d.Session.ID,
		approvals.ConfirmedIssue{Number: 41, Repo: "o/r", Title: "the first one", State: "open"},
		time.Hour); err != nil {
		t.Fatal(err)
	}

	if out := RunNew(context.Background(), d, newReq()); !out.Confirmed {
		t.Fatalf("expected confirmation, got %+v", out)
	}
	if len(stub.Last.AlreadyConfirmed) != 1 || stub.Last.AlreadyConfirmed[0] != 41 {
		t.Errorf("prompt must carry the run's confirmed issues, got %v", stub.Last.AlreadyConfirmed)
	}
	if !stub.Last.Expansion {
		t.Error("a run that already has issues must render as an expansion")
	}
}

// TestRunNew_BrokerLocalErrorIsNotRelayed: a keystore/mint failure's text
// names host paths. The agent gets the fact; the human gets the detail.
func TestRunNew_BrokerLocalErrorIsNotRelayed(t *testing.T) {
	t.Setenv("TMUX", "")
	d, _, spy := newDeps(t, "add")
	spy.err = fmt.Errorf("%w: mint issues:write token: open /home/dev/.config/rein-credentials/app.pem: permission denied",
		ErrBrokerLocal)

	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || out.Audit != AuditNewCreateFailed {
		t.Fatalf("expected a create failure, got %+v", out)
	}
	for _, leak := range []string{"app.pem", "rein-credentials", "/home/dev", "permission denied"} {
		if strings.Contains(out.Message, leak) {
			t.Errorf("broker-side detail %q leaked into the agent-visible message: %q", leak, out.Message)
		}
	}
	if !strings.Contains(out.Message, "broker-side error") {
		t.Errorf("the agent should still learn what class of failure it was: %q", out.Message)
	}
	// A GITHUB error, by contrast, is the useful half and is passed through.
	d2, _, spy2 := newDeps(t, "add")
	spy2.err = errors.New("403 Resource not accessible by integration")
	if out := RunNew(context.Background(), d2, newReq()); !strings.Contains(out.Message, "Resource not accessible") {
		t.Errorf("a GitHub error should reach the agent: %q", out.Message)
	}
}

// TestRunNew_MultiRepoNamesTheNewForm: resolveRepo's ambiguity message
// teaches `rein declare <n> --repo`, which cannot file anything.
func TestRunNew_MultiRepoNamesTheNewForm(t *testing.T) {
	t.Setenv("TMUX", "")
	d, stub, _ := newDeps(t, "add")
	d.Session.Repos = []string{"o/r", "o/other"}

	out := RunNew(context.Background(), d, newReq())
	if out.Confirmed || out.Audit != AuditNewBadRequest {
		t.Fatalf("an ambiguous repo must be refused, got %+v", out)
	}
	if !strings.Contains(out.Message, `rein declare --new "<title>" --repo`) {
		t.Errorf("message must name the --new form of the command: %q", out.Message)
	}
	if stub.Calls != 0 {
		t.Error("ambiguity must be resolved before the human is asked")
	}
}

// TestRunNew_WeakConfirmWordRefused: the first word IS the token, so a
// title starting with a bare number (which collides with the `rein
// declare <n>` token) or a single letter is refused before the prompt.
func TestRunNew_WeakConfirmWordRefused(t *testing.T) {
	t.Setenv("TMUX", "")
	for _, title := range []string{"42 is wrong", "a broken thing", "1 crash"} {
		d, stub, spy := newDeps(t, "42")
		out := RunNew(context.Background(), d, NewIssue{Title: title})
		if out.Confirmed || out.Audit != AuditNewBadRequest {
			t.Errorf("%q: expected a refusal, got %+v", title, out)
		}
		if stub.Calls != 0 || len(spy.calls) != 0 {
			t.Errorf("%q: reached the human or the create hook", title)
		}
	}
	// Sanity: the same shape with a usable first word still works.
	d, _, _ := newDeps(t, "crash")
	if out := RunNew(context.Background(), d, NewIssue{Title: "crash on save"}); !out.Confirmed {
		t.Errorf("a usable first word must still be accepted: %+v", out)
	}
}
