// Package grant implements the layered approval-obtaining flow used by
// both the credential helper (for git operations) and the rein-gh shim
// (for gh operations).
//
// # Layers
//
// ObtainApproval tries layers in order until one resolves:
//
//   1. Existing approval record (internal/approvals). If a valid record
//      covers the current session signature, return true immediately.
//   2. Inline /dev/tty prompt. If a controlling terminal is reachable
//      (typically: a developer at a shell), prompt and record on
//      approval. Fast for interactive use.
//   3. Tmux popup. If $TMUX is set, spawn `tmux popup -E "rein approval
//      grant"` and wait for it to close. The popup runs the grant
//      subcommand, which uses /dev/tty inside the popup's pty. After
//      close, re-check the approval record.
//   4. Helpful stderr + deny. If none of the above resolved, emit a
//      message to stderr telling the user to run `rein approval grant`
//      in another terminal, then deny. The user runs it, retries the
//      operation.
//
// # Shape B limit
//
// Documented in design §5.3 TM-G5: in unsandboxed Shape B, an agent
// with shell access and a readable dev-session.yaml can run the grant
// subcommand itself and answer the issue number it just read. The
// human-in-the-loop is a fail-closed gate for well-behaved agents, not
// an airtight defense against adversarial ones. Shape A's sandbox is
// where the architectural defense lives.
package grant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// Request describes what's being approved. Used both for the prompt
// the human sees and for any helpful-stderr message we emit on
// denial.
type Request struct {
	// Session is the active rein session — the scope being approved.
	Session session.Session

	// Action is a short verb describing what's being approved
	// ("git push", "gh issue create", etc.). Surfaced in the prompt
	// and in the stderr message.
	Action string

	// Repo is the specific repo being acted on (within Session.Repos).
	// Surfaced for human context.
	Repo string
}

// Config controls ObtainApproval's behavior. Exposed so cmd/rein and
// cmd/rein-gh can tune timeouts / disable layers for tests.
type Config struct {
	// StateDir is rein's state directory. The approval record lives at
	// approvals.Path(StateDir).
	StateDir string

	// TTL is how long an approval covers writes for this session
	// before re-prompting. 4h is the cmd/rein default.
	TTL time.Duration

	// PromptTimeout caps the wait inside the /dev/tty prompt.
	PromptTimeout time.Duration

	// Stderr is where the helpful-deny message goes. Defaults to
	// os.Stderr; tests override with a buffer. Git forwards the
	// helper's stderr to the user (or to the agent that ran it).
	Stderr io.Writer

	// Prompter is the /dev/tty prompter. Defaults to a TTYPrompter;
	// tests override with a StubPrompter.
	Prompter prompt.Prompter

	// TmuxRunner runs the tmux popup. Defaults to invoking `tmux
	// popup` via exec.Command; tests override with a stub that
	// doesn't actually shell out.
	TmuxRunner TmuxRunner

	// Logger receives forensic log lines.
	Logger *log.Logger
}

// TmuxRunner is a pluggable tmux-popup invoker. The default opens a
// `tmux popup -E "rein approval grant"`-style window inside the user's
// existing tmux session. Returns nil iff the popup launched and the
// user closed it (regardless of whether they approved); the caller
// then re-checks the approval record.
type TmuxRunner func(ctx context.Context, command []string) error

// DefaultTmuxRunner is the production TmuxRunner. Requires `tmux`
// binary on PATH.
func DefaultTmuxRunner(ctx context.Context, command []string) error {
	args := append([]string{"popup", "-E"}, command...)
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr // popup detail goes to the helper's stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ObtainApproval is the layered entry point. Returns true iff write
// access is approved (either from an existing record or freshly
// granted by the human).
//
// Pure-Go return shape:
//   - approved=true: caller proceeds to mint
//   - approved=false: caller treats as TM-G8 placeholder
func ObtainApproval(ctx context.Context, req Request, cfg Config) bool {
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Prompter == nil {
		cfg.Prompter = prompt.TTYPrompter{}
	}
	if cfg.TmuxRunner == nil {
		cfg.TmuxRunner = DefaultTmuxRunner
	}

	// Layer 1: existing approval record.
	sig := approvals.SignatureOf(req.Session)
	path := approvals.Path(cfg.StateDir)
	if rec, err := approvals.Read(path); err == nil {
		if approvals.Valid(rec, sig, time.Now()) {
			cfg.Logger.Printf("grant: covered by existing approval (granted at %s, valid until %s)",
				rec.ApprovedAt.Format(time.RFC3339),
				rec.ExpiresAt.Format(time.RFC3339))
			return true
		}
		cfg.Logger.Printf("grant: existing approval mismatched or expired; trying interactive layers")
	}

	// Layer 2: inline /dev/tty prompt. Works for users at a regular
	// shell. Inside agent TUIs that detach the controlling terminal,
	// the prompter returns ErrNoTTY and we fall through to layer 3.
	// A Ctrl-C OR prompt-timeout is an EXPLICIT human denial — that
	// returns ErrCancelled, and we short-circuit (no further layers,
	// no helpful-stderr; the human knew what they were doing).
	pr := prompt.Request{
		SessionID: req.Session.ID,
		Role:      req.Session.Role,
		Repo:      req.Repo,
		Action:    fmt.Sprintf("%s (covers writes until +%s)", req.Action, cfg.TTL),
		Issue:     req.Session.Issue,
		Timeout:   cfg.PromptTimeout,
	}
	approved, err := cfg.Prompter.Confirm(ctx, pr)
	switch {
	case err == nil && approved:
		recordApproval(cfg, sig, req.Session.ID)
		cfg.Logger.Printf("grant: APPROVED via /dev/tty")
		return true
	case err == nil:
		cfg.Logger.Printf("grant: DENIED via /dev/tty (input mismatched)")
		return false
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant: CANCELLED via /dev/tty (Ctrl-C or timeout)")
		return false
	default:
		// ErrNoTTY or other open-failure: prompter couldn't even ask.
		// Fall through to tmux popup / helpful stderr.
		cfg.Logger.Printf("grant: /dev/tty unavailable (%v); trying tmux popup", err)
	}

	// Layer 3: tmux popup if we're inside tmux.
	if os.Getenv("TMUX") != "" {
		// Pass an absolute path to rein so the popup's shell doesn't
		// need a configured PATH. Same sibling-of-shim trick as the
		// stderr message.
		reinCmd := "rein"
		if abs, err := os.Executable(); err == nil {
			if rp := filepath.Join(filepath.Dir(abs), "rein"); fileExists(rp) {
				reinCmd = rp
			}
		}
		cfg.Logger.Printf("grant: launching tmux popup (%s approval grant)", reinCmd)
		ctxPopup, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if err := cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant"}); err != nil {
			cfg.Logger.Printf("grant: tmux popup failed: %v; falling through", err)
		}
		if rec, err := approvals.Read(path); err == nil && approvals.Valid(rec, sig, time.Now()) {
			cfg.Logger.Printf("grant: APPROVED via tmux popup")
			return true
		}
		cfg.Logger.Printf("grant: tmux popup closed without approval")
	}

	// Layer 4: emit helpful stderr and deny. The user has to run
	// `rein approval grant` in another terminal and retry.
	//
	// We lead with the absolute path because a fresh terminal often
	// doesn't have the shim dir on PATH. Mention the bare form as
	// the convenience option for users with PATH set up.
	reinCmd := "rein"
	if abs, err := os.Executable(); err == nil {
		if rp := filepath.Join(filepath.Dir(abs), "rein"); fileExists(rp) {
			reinCmd = rp
		} else {
			reinCmd = abs
		}
	}
	fmt.Fprintln(cfg.Stderr)
	fmt.Fprintf(cfg.Stderr, "rein: write blocked for %s on %s\n", req.Action, req.Repo)
	fmt.Fprintln(cfg.Stderr, "  To grant write access for this session, in ANOTHER terminal run:")
	fmt.Fprintf(cfg.Stderr, "    %s approval grant\n", reinCmd)
	fmt.Fprintln(cfg.Stderr, "  (or just `rein approval grant` if you've added the shim dir to your PATH)")
	fmt.Fprintln(cfg.Stderr, "  Then retry your operation here.")
	fmt.Fprintln(cfg.Stderr)
	fmt.Fprintln(cfg.Stderr, "  Note: invoking grant from this same terminal (e.g. claude's `!` shell")
	fmt.Fprintln(cfg.Stderr, "  escape) won't work — the agent's bash subprocess has no /dev/tty.")
	fmt.Fprintln(cfg.Stderr)
	cfg.Logger.Printf("grant: DENIED — no /dev/tty, no tmux; emitted helpful stderr")
	return false
}

// recordApproval writes a fresh approval record with the requested TTL.
// Best-effort: failures are logged and ignored (the caller already got
// a thumbs-up; missing the record just means re-prompting next time).
func recordApproval(cfg Config, sig, sessionID string) {
	now := time.Now()
	rec := approvals.Record{
		Signature:  sig,
		SessionID:  sessionID,
		ApprovedAt: now,
		ExpiresAt:  now.Add(cfg.TTL),
	}
	if err := approvals.Write(approvals.Path(cfg.StateDir), rec); err != nil {
		cfg.Logger.Printf("grant: approval write failed (continuing): %v", err)
		return
	}
	cfg.Logger.Printf("grant: approval recorded (valid until %s)", rec.ExpiresAt.Format(time.RFC3339))
}

// Grant is the entry point for the `rein approval grant` subcommand.
// It tries /dev/tty first (no CLI flag accepts the issue number —
// design §5.3 TM-G5 wants the answer to arrive via /dev/tty so a
// confused or malicious agent can't supply it via a CLI arg). If
// /dev/tty isn't available AND $TMUX is set, it falls back to
// spawning a tmux popup that runs `rein approval grant` itself —
// the popup's pty IS a /dev/tty, so the inner invocation completes
// via layer 1 and writes the approval record. The outer call then
// sees the record and returns true.
//
// Recursion is bounded: the popup's grant invocation has a tty, so
// it never hits the popup branch itself.
//
// If neither path resolves (no tty, no tmux), Grant returns false
// with a helpful error logged. The subcommand surfaces this to the
// user.
func Grant(ctx context.Context, sess session.Session, cfg Config) bool {
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.Prompter == nil {
		cfg.Prompter = prompt.TTYPrompter{}
	}
	if cfg.TmuxRunner == nil {
		cfg.TmuxRunner = DefaultTmuxRunner
	}
	sig := approvals.SignatureOf(sess)
	pr := prompt.Request{
		SessionID: sess.ID,
		Role:      sess.Role,
		Repo:      joinRepos(sess.Repos),
		Action:    fmt.Sprintf("grant write access (covers writes until +%s)", cfg.TTL),
		Issue:     sess.Issue,
		Timeout:   cfg.PromptTimeout,
	}
	approved, err := cfg.Prompter.Confirm(ctx, pr)
	switch {
	case err == nil && approved:
		recordApproval(cfg, sig, sess.ID)
		return true
	case err == nil:
		cfg.Logger.Printf("grant subcommand: denied (wrong answer)")
		return false
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant subcommand: cancelled")
		return false
	}

	// No /dev/tty. Try tmux popup if available — common case is the
	// user running `! rein approval grant` from inside claude that
	// itself was launched from inside tmux.
	cfg.Logger.Printf("grant subcommand: /dev/tty unavailable (%v); trying tmux popup", err)
	if os.Getenv("TMUX") != "" {
		reinCmd := "rein"
		if abs, err := os.Executable(); err == nil {
			if rp := filepath.Join(filepath.Dir(abs), "rein"); fileExists(rp) {
				reinCmd = rp
			}
		}
		ctxPopup, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		_ = cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant"})
		if rec, err := approvals.Read(approvals.Path(cfg.StateDir)); err == nil {
			if approvals.Valid(rec, sig, time.Now()) {
				cfg.Logger.Printf("grant subcommand: APPROVED via tmux popup")
				return true
			}
		}
	}
	cfg.Logger.Printf("grant subcommand: DENIED — no /dev/tty, no tmux popup")
	return false
}

// fileExists reports whether path resolves to a regular file/symlink
// that we can stat. Used to detect whether `rein` is co-located with
// the calling shim binary.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}

func joinRepos(repos []string) string {
	if len(repos) == 0 {
		return "<none>"
	}
	if len(repos) == 1 {
		return repos[0]
	}
	out := repos[0]
	for _, r := range repos[1:] {
		out += ", " + r
	}
	return out
}
