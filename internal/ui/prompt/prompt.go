// Package prompt implements rein's human-in-the-loop confirmation
// ceremony for write operations.
//
// # Mechanism
//
// The credential helper's stdout is reserved for git (it carries the
// credential block git is waiting on). Stderr may be captured. The
// prompt therefore opens /dev/tty directly — the special device that
// refers to the process's controlling terminal — and uses that for
// both displaying the prompt and reading the human's response. The
// agent (running as a child of the helper's caller, but without a
// controlling terminal of its own) has no path to /dev/tty: the
// kernel hands the file to a process based on the process's session,
// and the agent's session is separate.
//
// Standard Unix assumption that this rests on: any co-resident
// process under the same user that shares the controlling terminal
// could write to /dev/tty too. The non-replayability property
// presumes the human's tty isn't shared with hostile processes on
// the same machine. Shape A's sandbox (Phase 1) tightens this by
// running the agent without a tty at all.
//
// # TM-G5 framing (non-replayable input)
//
// The design's "non-replayable input" language (design §2.2) means
// the answer to a prompt for session S would not approve a request
// for session S'. The bound issue number serves as that discriminator
// — a different session is bound to a different issue and has a
// different correct answer. It does NOT mean the issue number must
// be hidden from the prompt: the agent already knows the session's
// scope (it requested it). What the agent cannot do is supply the
// answer itself, because the answer must arrive via /dev/tty, which
// only the human at the terminal can write to.
//
// # No-tty behavior
//
// If /dev/tty cannot be opened (no controlling terminal, e.g. the
// helper was invoked from a non-interactive context like CI or from
// inside an agent that has no terminal of its own), Confirm returns
// ErrNoTTY. The caller treats this as "denied" — fail-closed,
// consistent with the rest of the helper's invariants.
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

// Request describes one confirmation request. Fields are formatted
// into a human-readable prompt block; the human types Expected to
// approve.
type Request struct {
	// SessionID is the broker's session identifier (e.g.
	// "sess_dev_001"). Surfaced for human context.
	SessionID string

	// Role is the design's role name (implement, scan, ...). Surfaced
	// for human context.
	Role string

	// Repo is "owner/name" — the repository the write operation
	// targets. The most-load-bearing piece of context for the human.
	Repo string

	// Action is a short verb describing what's being approved
	// ("git push", "gh issue create", etc.). Surfaced for human
	// context.
	Action string

	// Issue is the numeric GitHub issue the session is bound to. The
	// human types this number to approve. Different sessions bound to
	// different issues have different correct answers — that's the
	// non-replayability property (design §2.2).
	Issue int

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
//   - Phase 1 (sandbox-composed) may add NotificationPrompter, etc.
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

// writePrompt renders the human-facing prompt block. Format is
// deliberately compact — git users see this in their terminal and
// want to act fast.
func writePrompt(w io.Writer, req Request) error {
	_, err := fmt.Fprintf(w, "\n"+
		"=== rein: write access requested ===\n"+
		"   action:  %s\n"+
		"   repo:    %s\n"+
		"   session: %s (role=%s, issue=#%d)\n"+
		"\n"+
		"To approve, type the issue number (%d) and press enter.\n"+
		"To deny, press Ctrl-C or type anything else.\n"+
		"> ",
		req.Action, req.Repo, req.SessionID, req.Role, req.Issue, req.Issue,
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
}

// Confirm satisfies Prompter.
func (s *StubPrompter) Confirm(ctx context.Context, req Request) (bool, error) {
	s.Calls++
	if s.ForceErr != nil {
		return false, s.ForceErr
	}
	expected := fmt.Sprintf("%d", req.Issue)
	return strings.TrimSpace(s.Response) == expected, nil
}
