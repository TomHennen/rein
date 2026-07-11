// Package prompt implements rein's human-in-the-loop confirmation
// ceremony: the Form A issue-declaration prompt (issue #35 §4).
//
// # Mechanism
//
// The prompt opens /dev/tty directly — the special device that refers
// to the process's controlling terminal — and uses that for both
// displaying the prompt and reading the human's response. A sandboxed
// agent has no /dev/tty at all (bwrap --new-session, re-verified every
// launch), and a direct-mode agent subprocess typically has no
// controlling terminal either (a useful accident, not an airtight gate
// — issue #12; design.md:749).
//
// Standard Unix assumption that this rests on: any co-resident
// process under the same user that shares the controlling terminal
// could write to /dev/tty too. The non-replayability property
// presumes the human's tty isn't shared with hostile processes on
// the same machine.
//
// # Form A (issue #35, settled)
//
// The agent declared issue N; rein fetched N's title, state, and home
// repo (internal/issuemeta — no prompt ever fires without a fetched
// title, decision E); the human approves by typing the DISPLAYED
// number. The answer differs per issue, so a prompt-injected agent
// cannot pre-construct a "yes" (design.md:245-246, 281) — and cannot
// deliver one, lacking the tty. The displayed fetched title + state +
// home repo is the load-bearing misattribution control (decision E;
// probe S1/S4/S5): a wrong-but-plausible issue shows the wrong title.
// Titles are agent-editable in-scope (TM-G7), so the title informs the
// human but never authorizes — the number the human types is
// GitHub-assigned and unforgeable.
//
// # No-tty behavior
//
// If /dev/tty cannot be opened, Confirm returns ErrNoTTY. The caller
// treats this as "denied" — fail-closed — and falls back to the
// out-of-process grant surfaces (tmux popup / another terminal).
package prompt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// ErrNoTTY is returned when /dev/tty cannot be opened (process has no
// controlling terminal). Callers treat this as a denied confirmation.
var ErrNoTTY = errors.New("no controlling terminal (/dev/tty unavailable)")

// ErrCancelled is returned when the prompt was interrupted (SIGINT)
// or a context cancellation fired before the human responded.
var ErrCancelled = errors.New("confirmation cancelled")

// Request describes one Form A confirmation request (issue #35 §4). The
// fields are formatted into the human-readable prompt block; the human
// types Issue (the displayed number) to approve.
type Request struct {
	// SessionID / Role / Repos describe the session, for human context
	// ("session: sess_dev_001 (role=implement, repos=[o/r])").
	SessionID string
	Role      string
	Repos     []string

	// Issue is the number the agent declared — the human types this
	// displayed number to approve (Form A). GitHub-assigned, unforgeable.
	Issue int

	// IssueRepo is the declared issue's HOME repo (owner/name) — shown so
	// a cross-repo binding is visible (misattribution S4).
	IssueRepo string

	// Title and State are the FETCHED snapshot (never agent-supplied
	// directly; sanitized for terminal display by internal/issuemeta).
	// The load-bearing misattribution control (decision E).
	Title string
	State string

	// IsPR labels the declaration `[pull request]` — GitHub shares the
	// number space and PR declarations are valid (§9).
	IsPR bool

	// Expansion marks a second (or later) issue declared mid-run: the
	// design's scope-expansion prompt (design.md:254-263) — same
	// ceremony, distinct header, appends to the run's confirmed set.
	Expansion bool

	// Timeout caps how long Confirm waits for the human. A zero value
	// means "no timeout" — Confirm blocks until input or signal.
	Timeout time.Duration
}

// Prompter handles a single confirmation request and reports whether
// the human approved.
//
// Implementations:
//   - TTYPrompter: opens /dev/tty for production use.
//   - Tests provide a stub that returns canned responses.
type Prompter interface {
	Confirm(ctx context.Context, req Request) (approved bool, err error)
}

// TTYPrompter is the production Prompter. Opens /dev/tty for read +
// write each invocation.
type TTYPrompter struct{}

// Confirm displays the prompt to /dev/tty, reads a single line, and
// returns approved=true iff the line (trimmed) equals fmt.Sprint(req.Issue).
//
// Returns (false, ErrNoTTY) if /dev/tty is unavailable.
// Returns (false, ErrCancelled) on context cancellation or SIGINT.
// Returns (false, nil) for any wrong answer or EOF — i.e. an explicit
// "denied" with no error condition.
func (TTYPrompter) Confirm(ctx context.Context, req Request) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrNoTTY, err)
	}
	defer tty.Close()

	if err := writePrompt(tty, req); err != nil {
		return false, fmt.Errorf("write prompt: %w", err)
	}

	// Wrap ctx with a Timeout (if set) and a signal trap so Ctrl-C in
	// the user's terminal interrupts the read cleanly.
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	line, err := readLineCtx(sigCtx, tty)
	if err != nil {
		fmt.Fprintln(tty, "  [cancelled]")
		return false, ErrCancelled
	}
	expected := fmt.Sprintf("%d", req.Issue)
	if strings.TrimSpace(line) == expected {
		fmt.Fprintln(tty, "  [approved]")
		return true, nil
	}
	fmt.Fprintln(tty, "  [denied: input did not match the issue number]")
	return false, nil
}

// writePrompt renders the Form A prompt block (issue #35 §4). Format is
// deliberately compact — a developer sees this mid-flow and wants to
// read the title, check it is the RIGHT issue, and act.
func writePrompt(w io.Writer, req Request) error {
	header := "=== rein: agent declares work on an issue ==="
	if req.Expansion {
		header = "=== rein: agent wants to ALSO work on an issue (scope expansion) ==="
	}
	kind := ""
	if req.IsPR {
		kind = "  [pull request]"
	}
	state := req.State
	if state == "" {
		state = "unknown"
	}
	_, err := fmt.Fprintf(w, "\n"+
		"%s\n"+
		"   issue:    #%d %q  [%s]%s\n"+
		"             in %s\n"+
		"   session:  %s (role=%s, repos=[%s])\n"+
		"   approving covers ALL writes for this run (git push, gh, API).\n"+
		"\n"+
		"To approve, type the issue number (%d) and press enter.\n"+
		"To deny, press Ctrl-C or type anything else.\n"+
		"> ",
		header, req.Issue, req.Title, state, kind,
		req.IssueRepo,
		req.SessionID, req.Role, strings.Join(req.Repos, ", "),
		req.Issue,
	)
	return err
}

// readLineCtx reads a single \n-terminated line from r, returning
// early if ctx is cancelled or signal fires. Implemented with a
// goroutine + select so the cancellation isn't lost in a blocked
// syscall.
//
// The blocking goroutine continues until the read completes; in the
// cancellation case we leak the goroutine until the next \n on the
// terminal, which is acceptable because the process exits shortly
// after.
func readLineCtx(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		if scanner.Scan() {
			ch <- result{line: scanner.Text()}
			return
		}
		ch <- result{err: scanner.Err()}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.line, r.err
	}
}

// StubPrompter is a Prompter for tests. The Response field is the
// raw string the human "types"; Approved is computed by comparing it
// to req.Issue, same as TTYPrompter would. Or set Force to override.
type StubPrompter struct {
	// Response is what the "human" types. Compared against the issue
	// number; matches → approved.
	Response string

	// ForceErr, if non-nil, is returned instead of running the compare.
	// Useful for testing ErrNoTTY / ErrCancelled handling.
	ForceErr error

	// Calls counts invocations for test assertions.
	Calls int

	// Last records the most recent request, so tests can assert what the
	// human WOULD have seen (title, repo, expansion header).
	Last Request
}

// Confirm satisfies Prompter.
func (s *StubPrompter) Confirm(ctx context.Context, req Request) (bool, error) {
	s.Calls++
	s.Last = req
	if s.ForceErr != nil {
		return false, s.ForceErr
	}
	expected := fmt.Sprintf("%d", req.Issue)
	return strings.TrimSpace(s.Response) == expected, nil
}
