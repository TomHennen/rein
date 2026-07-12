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

	// AddRepo, when non-empty, marks the declaration as a REPO scope
	// expansion (issue #69): the declared issue's home repo is OUTSIDE the
	// session's standing ceiling, and approving ADDS it to what this run
	// may touch. The prompt must say so — a human who reads only "issue
	// #41, looks fine" would otherwise widen the repo ceiling without
	// noticing (mocks §1.2). Same Form A token: the issue number.
	AddRepo string

	// AskPersist adds the second question — "Also save <AddRepo> to the
	// session for future runs? [y/N]" — asked ONLY after the number
	// matched (Tom's decision, mocks §1.2/§1.3). The number remains the
	// SOLE approval token: a stray "y" can never approve anything, because
	// this question is never reached unless approval already succeeded.
	// Ignored unless AddRepo is set.
	AskPersist bool

	// Timeout caps how long Confirm waits for the human. A zero value
	// means "no timeout" — Confirm blocks until input or signal.
	Timeout time.Duration
}

// Result is the outcome of one confirmation.
//
// Persist is ONLY ever meaningful when Approved is true: it is read from a
// second question the prompt asks after the issue number matched. Callers
// MUST NOT act on Persist when Approved is false (and no Prompter may set
// it in that case) — persistence follows approval, it never grants it.
type Result struct {
	// Approved is true iff the human typed the displayed issue number.
	Approved bool

	// Persist is true iff the human ALSO answered `y` to the
	// save-to-session question (Request.AskPersist). Default false = N =
	// run-only, keeping the standing ceiling a deliberate act.
	Persist bool
}

// Prompter handles a single confirmation request and reports whether
// the human approved (and, for a repo expansion, whether they asked to
// persist the repo to the session).
//
// Implementations:
//   - TTYPrompter: opens /dev/tty for production use.
//   - Tests provide a stub that returns canned responses.
type Prompter interface {
	Confirm(ctx context.Context, req Request) (Result, error)
}

// TTYPrompter is the production Prompter. Opens /dev/tty for read +
// write each invocation.
type TTYPrompter struct{}

// Confirm displays the prompt to /dev/tty, reads a single line, and
// returns Approved=true iff the line (trimmed) equals fmt.Sprint(req.Issue).
//
// On approval of a repo expansion with req.AskPersist, it then asks the
// save-to-session question and reads ONE more line; `y`/`yes` (case
// insensitive) sets Persist. Anything else — including a read error, EOF,
// or a timeout on that second question — leaves Persist false (default N =
// run-only). The second question can never change Approved: by the time it
// is asked, approval has already succeeded.
//
// Returns (zero, ErrNoTTY) if /dev/tty is unavailable.
// Returns (zero, ErrCancelled) on context cancellation or SIGINT.
// Returns (zero, nil) for any wrong answer or EOF — i.e. an explicit
// "denied" with no error condition.
func (TTYPrompter) Confirm(ctx context.Context, req Request) (Result, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNoTTY, err)
	}
	defer tty.Close()

	if err := writePrompt(tty, req); err != nil {
		return Result{}, fmt.Errorf("write prompt: %w", err)
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
		return Result{}, ErrCancelled
	}
	expected := fmt.Sprintf("%d", req.Issue)
	if strings.TrimSpace(line) != expected {
		fmt.Fprintln(tty, "  [denied: input did not match the issue number]")
		return Result{}, nil
	}
	if req.AddRepo == "" {
		fmt.Fprintln(tty, "  [approved]")
		return Result{Approved: true}, nil
	}
	// A repo expansion: say what was just granted, THEN (optionally) ask
	// about persistence. Approval is already locked in at this point.
	fmt.Fprintln(tty, "  [approved for this run]")
	if !req.AskPersist {
		return Result{Approved: true}, nil
	}
	fmt.Fprintf(tty, "Also save %s to the session for future runs? [y/N]\n> ", req.AddRepo)
	answer, perr := readLineCtx(sigCtx, tty)
	if perr != nil {
		// Cancelled/timed out on the SECOND question: the approval stands
		// (it was already given); persistence defaults to N.
		fmt.Fprintln(tty, "\n  [not saved (run-only)]")
		return Result{Approved: true}, nil
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return Result{Approved: true, Persist: true}, nil
	default:
		fmt.Fprintln(tty, "  [not saved (run-only)]")
		return Result{Approved: true}, nil
	}
}

// writePrompt renders the Form A prompt block (issue #35 §4). Format is
// deliberately compact — a developer sees this mid-flow and wants to
// read the title, check it is the RIGHT issue, and act.
//
// A REPO expansion (req.AddRepo, issue #69) gets a distinct header and an
// explicit blast-radius line: approving does not just bind an issue, it
// grows the set of repos this run can write to, and every later write to
// that repo flows without a further prompt. The mocks (§1.2) make this
// unmistakable on purpose — the human must approve knowing it.
func writePrompt(w io.Writer, req Request) error {
	kind := ""
	if req.IsPR {
		kind = "  [pull request]"
	}
	state := req.State
	if state == "" {
		state = "unknown"
	}
	if req.AddRepo != "" {
		_, err := fmt.Fprintf(w, "\n"+
			"=== rein: SCOPE EXPANSION requested ===\n"+
			"   session:   %s (role=%s, repos=[%s])\n"+
			"   agent asks to ADD repo:  %s\n"+
			"   for issue:  #%d %q  [%s]%s\n"+
			"               in %s\n"+
			"   approving ADDS this repo to the scope ceiling\n"+
			"   (all writes to it then flow without further prompts).\n"+
			"\n"+
			"To approve, type the issue number (%d) and press enter.\n"+
			"To deny, press Ctrl-C or type anything else.\n"+
			"> ",
			req.SessionID, req.Role, strings.Join(req.Repos, ", "),
			req.AddRepo,
			req.Issue, req.Title, state, kind,
			req.IssueRepo,
			req.Issue,
		)
		return err
	}
	header := "=== rein: agent declares work on an issue ==="
	if req.Expansion {
		// A second-or-later issue in the SAME run whose repo is already in
		// the ceiling (AddRepo == "" — the repo-expansion branch above
		// returned). This is an issue-set expansion, NOT a repo scope
		// expansion, so the header must not borrow "scope expansion"
		// vocabulary: that phrase is reserved for the AddRepo path, which
		// is the one that actually widens the ceiling and offers persist.
		// A reader who sees "scope expansion" here reasonably expects the
		// "Also save <repo> to the session?" question that never comes.
		header = "=== rein: agent wants to ALSO work on an issue ==="
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

// ReadLine reads one \n-terminated line from r, honoring ctx cancellation
// (and the caller's signal trap). Exported for the sibling surfaces that
// read a NON-authorizing acknowledgement from the same /dev/tty — the
// install NOTICE (internal/ui/grant), which grants nothing.
//
// It is deliberately NOT a confirmation primitive: the Form A approval
// token is only ever compared inside Confirm.
func ReadLine(ctx context.Context, r io.Reader) (string, error) {
	return readLineCtx(ctx, r)
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
// to req.Issue, same as TTYPrompter would. Or set ForceErr to override.
type StubPrompter struct {
	// Response is what the "human" types. Compared against the issue
	// number; matches → approved.
	Response string

	// PersistResponse is what the "human" types at the second
	// (save-to-session) question. Only consulted when the request asks it
	// AND the number matched — mirroring TTYPrompter, so a test cannot
	// accidentally demonstrate a persist-without-approval that production
	// could not produce.
	PersistResponse string

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
func (s *StubPrompter) Confirm(ctx context.Context, req Request) (Result, error) {
	s.Calls++
	s.Last = req
	if s.ForceErr != nil {
		return Result{}, s.ForceErr
	}
	expected := fmt.Sprintf("%d", req.Issue)
	if strings.TrimSpace(s.Response) != expected {
		return Result{}, nil
	}
	if req.AddRepo == "" || !req.AskPersist {
		return Result{Approved: true}, nil
	}
	switch strings.ToLower(strings.TrimSpace(s.PersistResponse)) {
	case "y", "yes":
		return Result{Approved: true, Persist: true}, nil
	default:
		return Result{Approved: true}, nil
	}
}
