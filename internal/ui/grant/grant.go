// Package grant implements the layered flow that obtains the human's
// Form A confirmation of a DECLARED issue (issue #35) — the one ceremony
// that unlocks writes for a run, in both modes.
//
// The write gates themselves (credential helper, rein-gh shim, sandboxed
// proxy) do NOT call this package anymore: they only READ the run's
// confirmed-issue set (approvals.ConfirmedIssues). This package is
// invoked at DECLARE time (internal/declare — the `rein declare <n>`
// direct path and the sandboxed declare relay) and by the out-of-process
// `rein approval grant --run-id X` subcommand the tmux popup runs.
//
// # Layers
//
// ObtainIssueApproval tries layers until one resolves:
//
//  1. Existing approval record (internal/approvals). If a valid record
//     already contains the declared issue, return true immediately
//     (idempotent re-declare — no re-prompt).
//  2. Interactive Form A confirmation, via one of two surfaces:
//     - Inline /dev/tty prompt — fast for a developer at a plain shell.
//     - Tmux popup — `tmux popup -E "rein approval grant"`, whose pty is
//     its own /dev/tty. This is REQUIRED when the wrapped process is a
//     full-screen agent TUI (e.g. claude): the inline prompt would render
//     into — and corrupt — the TUI's screen. When Config.PreferPopup is
//     set (the default inside $TMUX; see PopupPreferenceFromEnv) the
//     popup is tried FIRST and the inline prompt is only a fallback for
//     when the popup can't launch.
//  3. Helpful stderr + deny: tell the user to run `rein approval grant`
//     in another terminal, then deny.
//
// # Out-of-process transport
//
// Before any prompt, the FETCHED issue snapshot is written into
// runs/<run-id>.json as RunContext.PendingIssue: the popup and the
// other-terminal grant run outside this process tree (no REIN_RUN_ID, no
// session env), so the on-disk snapshot is the only way they can render
// the same title/state/home-repo the in-process prompt would — the popup
// never fetches (issue #35 §4).
//
// # Shape B limit (direct mode)
//
// Documented in design §5.3 TM-G5 / #35 §2 delta 2: in unsandboxed
// direct mode an agent with shell access shares the operator's terminal
// channel; the tty asymmetry (agent tool subprocesses hit ErrNoTTY) is a
// useful accident, not an airtight gate. Sandboxed mode closes it: srt
// runs the agent with NO controlling terminal (bwrap --new-session,
// re-verified every launch), so the agent cannot answer its own prompt.
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
	"sync"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// ceremonyMu serializes the interactive Form A ceremony: at most ONE live
// prompt at a time in this process (see the lock site in
// ObtainIssueApproval for the full rationale — interleaved prompt blocks
// on one /dev/tty would corrupt the human's read-the-title-then-type-the-
// number decision, and racing tty readers eat keystrokes).
var ceremonyMu sync.Mutex

// IssueRequest describes a declared issue awaiting the human's Form A
// confirmation.
type IssueRequest struct {
	// Session is the active rein session — its signature keys the
	// approval record the confirmed issue is appended to.
	Session session.Session

	// Issue is the FETCHED snapshot of the declared issue (number, home
	// repo, title, state, PR flag, canonical URL). Fetched by the caller
	// (internal/declare) — no prompt without a fetched title (decision E).
	Issue approvals.ConfirmedIssue
}

// Config controls the flow. Exposed so cmd/rein and tests can tune
// timeouts / substitute surfaces.
type Config struct {
	// StateDir is rein's state directory. Per-run approval and run-context
	// files live under approvals/<run-id>.json and runs/<run-id>.json.
	StateDir string

	// RunID is the per-run nonce (REIN_RUN_ID, set by `rein run`). It
	// keys this run's approval + run-context files. Empty means "outside
	// any rein run" — ObtainIssueApproval fails closed (a declare cannot
	// be keyed to a run; the caller already errors with the launch
	// instruction, this is the belt-and-suspenders).
	RunID string

	// RunPID is the pid of the owning `rein run` (REIN_RUN_PID), recorded
	// into the run-context snapshot for Sweep's liveness probe. 0 if
	// unknown.
	RunPID int

	// TTL is stamped into Record.ExpiresAt as a sweep/status heuristic
	// ONLY — it is NOT a re-prompt trigger. The run lifetime is the bound.
	TTL time.Duration

	// PromptTimeout caps the wait inside the /dev/tty prompt.
	PromptTimeout time.Duration

	// Stderr is where the helpful-deny message goes. Defaults to
	// os.Stderr; tests override with a buffer.
	Stderr io.Writer

	// Prompter is the /dev/tty prompter. Defaults to a TTYPrompter;
	// tests override with a StubPrompter.
	Prompter prompt.Prompter

	// TmuxRunner runs the tmux popup. Defaults to invoking `tmux
	// popup` via exec.Command; tests override with a stub.
	TmuxRunner TmuxRunner

	// PreferPopup makes the tmux popup the PRIMARY approval surface, tried
	// before the inline /dev/tty prompt. Set it whenever the wrapped process
	// may be a full-screen TUI that shares this controlling terminal (the
	// default inside $TMUX). Callers compute it via PopupPreferenceFromEnv
	// (which honors REIN_APPROVAL=tty|popup); this package reads no env for
	// this.
	PreferPopup bool

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
	cmd.Stdout = os.Stderr // popup detail goes to the caller's stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// PopupPreferenceFromEnv decides whether the tmux popup is the PRIMARY
// approval surface (tried before the inline /dev/tty prompt), from
// REIN_APPROVAL and $TMUX:
//
//	REIN_APPROVAL=tty    -> false (force the inline /dev/tty prompt first)
//	REIN_APPROVAL=popup  -> true  (force the popup first)
//	otherwise            -> true iff $TMUX is set
//
// The default-inside-tmux behavior exists because `rein run -- <agent>`
// keeps the parent's controlling terminal, so the inline prompt collides
// with a full-screen agent TUI (e.g. claude). Callers pass the result as
// Config.PreferPopup.
func PopupPreferenceFromEnv() bool {
	switch os.Getenv("REIN_APPROVAL") {
	case "tty":
		return false
	case "popup":
		return true
	default:
		return os.Getenv("TMUX") != ""
	}
}

// resolveReinCmd returns the best path to invoke `rein` for an
// out-of-process grant: the sibling of the calling binary if present
// (so the popup's shell needs no configured PATH), else the bare name.
func resolveReinCmd() string {
	if abs, err := os.Executable(); err == nil {
		if rp := filepath.Join(filepath.Dir(abs), "rein"); fileExists(rp) {
			return rp
		}
	}
	return "rein"
}

// ObtainIssueApproval is the layered entry point for a declare (issue
// #35 §3-§4). Returns true iff the declared issue is in the run's
// confirmed set on return (pre-existing, or freshly confirmed by the
// human).
//
// Pure-Go return shape:
//   - true: the declare succeeded; write gates that consult the set now
//     pass for this issue.
//   - false: denied (wrong answer, Ctrl-C, timeout, no prompt surface) —
//     the caller reports the declare as refused. Nothing was recorded.
func ObtainIssueApproval(ctx context.Context, req IssueRequest, cfg Config) bool {
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

	// No REIN_RUN_ID ⇒ nothing to key the confirmation to. The declare
	// entry points fail earlier with the launch instruction (§6); this is
	// fail-closed defense in depth, mirroring the old no-run-id rule.
	if cfg.RunID == "" {
		cfg.Logger.Printf("grant: no REIN_RUN_ID; a declare cannot be keyed to a run — denying")
		fmt.Fprintln(cfg.Stderr, "rein: no run context (REIN_RUN_ID unset) — launch your agent via `rein run -- <cmd>` and declare from within it.")
		return false
	}

	sig := approvals.SignatureOf(req.Session)

	// Layer 1: the issue is already in this run's confirmed set —
	// idempotent re-declare, no re-prompt (issue #35 §3).
	expansion := false
	if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil && approvals.Valid(rec, sig) {
		if rec.HasIssue(req.Issue.Repo, req.Issue.Number) {
			cfg.Logger.Printf("grant: issue #%d (%s) already confirmed for run %s", req.Issue.Number, req.Issue.Repo, cfg.RunID)
			return true
		}
		// A second issue mid-run is the design's scope-expansion prompt
		// (design.md:254-263): same ceremony, distinct header.
		expansion = len(rec.Issues) > 0
	}

	// Persist the snapshot (session + PENDING issue) NOW, before any
	// prompt: the out-of-process surfaces (popup, other-terminal grant)
	// can only render the Form A prompt from this file — they never
	// fetch. It also carries RunPID for Sweep's liveness probe.
	snap := approvals.RunContext{
		Session:      req.Session,
		RunPID:       cfg.RunPID,
		PendingIssue: &req.Issue,
		WrittenAt:    time.Now(),
	}
	if err := approvals.WriteRunContext(cfg.StateDir, cfg.RunID, snap); err != nil {
		cfg.Logger.Printf("grant: snapshot write failed (out-of-process grant may not resolve): %v", err)
		// Continue: the in-process tty prompt still works; layer 3 still
		// prints a command (the grant subcommand errors helpfully if the
		// snapshot is absent).
	}

	// SERIALIZE the ceremony (security review round 2, MEDIUM-1). Only ONE
	// live Form A ceremony at a time in this process: concurrent declares
	// would otherwise interleave their prompt blocks on the single
	// /dev/tty (the human could read a title from prompt A and type a
	// number answering prompt B — degrading exactly the display control
	// decision E made load-bearing) and race the reader goroutines
	// (prompt.readLineCtx leaks its scanner until the next newline, so a
	// prompt storm leaves goroutines eating the human's keystrokes). It
	// also makes the 60s prompt timeout a real spam bound: declares queue
	// instead of piling onto the tty.
	//
	// Scope note: this is a per-PROCESS lock. It covers every declare that
	// rides this run's broker (the sandboxed relay and the direct CLI both
	// funnel through one process per run). The out-of-process grant surface
	// (`rein approval grant --run-id X`, the tmux popup) is a separate
	// process by construction and is serialized by the human instead.
	ceremonyMu.Lock()
	defer ceremonyMu.Unlock()

	// Re-check under the lock: a ceremony we queued behind may have already
	// confirmed this very issue (two identical declares racing) — don't
	// prompt the human twice for the same thing.
	if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil && approvals.Valid(rec, sig) {
		if rec.HasIssue(req.Issue.Repo, req.Issue.Number) {
			cfg.Logger.Printf("grant: issue #%d (%s) confirmed while this declare waited; not re-prompting", req.Issue.Number, req.Issue.Repo)
			return true
		}
		expansion = len(rec.Issues) > 0
	}

	// Interactive confirmation. When PreferPopup is set (default inside
	// tmux), try the tmux popup FIRST: the inline /dev/tty prompt would
	// render into a full-screen agent TUI sharing this controlling
	// terminal and corrupt it.
	if cfg.PreferPopup {
		if approved, launched := attemptPopup(ctx, cfg, sig, req.Issue); approved {
			return true
		} else if launched {
			// The human saw the popup and closed it without approving. Do
			// NOT fall back to the inline /dev/tty prompt — that collision is
			// exactly what PreferPopup exists to avoid. Deny helpfully.
			cfg.Logger.Printf("grant: popup declined; not falling back to /dev/tty")
			return denyHelpful(req, cfg)
		}
		cfg.Logger.Printf("grant: popup preferred but unavailable; trying inline /dev/tty")
	}

	// Inline /dev/tty Form A prompt. A Ctrl-C OR prompt-timeout is an
	// EXPLICIT human denial — short-circuit (no further layers, no
	// helpful-stderr; the human knew what they were doing).
	approved, err := cfg.Prompter.Confirm(ctx, formARequest(req, expansion, cfg.PromptTimeout))
	switch {
	case err == nil && approved:
		recordIssue(cfg, sig, req.Session.ID, req.Issue)
		cfg.Logger.Printf("grant: issue #%d (%s) CONFIRMED via /dev/tty", req.Issue.Number, req.Issue.Repo)
		return true
	case err == nil:
		cfg.Logger.Printf("grant: DENIED via /dev/tty (input mismatched)")
		return false
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant: CANCELLED via /dev/tty (Ctrl-C or timeout)")
		return false
	default:
		// ErrNoTTY or other open-failure: prompter couldn't even ask.
		cfg.Logger.Printf("grant: /dev/tty unavailable (%v)", err)
	}

	// Tmux popup fallback — only when we did NOT already prefer it above.
	if !cfg.PreferPopup {
		if approved, _ := attemptPopup(ctx, cfg, sig, req.Issue); approved {
			return true
		}
	}

	// Helpful stderr + deny.
	return denyHelpful(req, cfg)
}

// formARequest builds the prompt.Request for a declared issue.
func formARequest(req IssueRequest, expansion bool, timeout time.Duration) prompt.Request {
	return prompt.Request{
		SessionID: req.Session.ID,
		Role:      req.Session.Role,
		Repos:     req.Session.Repos,
		Issue:     req.Issue.Number,
		IssueRepo: req.Issue.Repo,
		Title:     req.Issue.Title,
		State:     req.Issue.State,
		IsPR:      req.Issue.IsPR,
		Expansion: expansion,
		Timeout:   timeout,
	}
}

// attemptPopup tries the tmux popup approval surface. approved is true
// iff the popup's `rein approval grant` subcommand appended the declared
// issue to the run's record. launched is true iff the popup actually ran
// ($TMUX set and the runner returned without error): when launched is
// true but approved is false, the human saw the popup and declined, so a
// PreferPopup caller must NOT fall back to the inline /dev/tty prompt;
// when launched is false the popup was unavailable and the caller should
// fall back.
func attemptPopup(ctx context.Context, cfg Config, sig string, issue approvals.ConfirmedIssue) (approved, launched bool) {
	if os.Getenv("TMUX") == "" {
		return false, false
	}
	reinCmd := resolveReinCmd()
	cfg.Logger.Printf("grant: launching tmux popup (%s approval grant --run-id %s)", reinCmd, cfg.RunID)
	ctxPopup, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	runErr := cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant", "--run-id", cfg.RunID})
	if runErr != nil {
		cfg.Logger.Printf("grant: tmux popup failed: %v", runErr)
	}
	if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil &&
		approvals.Valid(rec, sig) && rec.HasIssue(issue.Repo, issue.Number) {
		cfg.Logger.Printf("grant: issue #%d CONFIRMED via tmux popup", issue.Number)
		return true, true
	}
	if runErr == nil {
		cfg.Logger.Printf("grant: tmux popup closed without confirming")
		return false, true
	}
	return false, false
}

// denyHelpful emits the "grant in another terminal" message and denies.
// We lead with the absolute path because a fresh terminal often doesn't
// have the shim dir on PATH.
func denyHelpful(req IssueRequest, cfg Config) bool {
	reinCmd := "rein"
	if abs, err := os.Executable(); err == nil {
		if rp := filepath.Join(filepath.Dir(abs), "rein"); fileExists(rp) {
			reinCmd = rp
		} else {
			reinCmd = abs
		}
	}
	fmt.Fprintln(cfg.Stderr)
	fmt.Fprintf(cfg.Stderr, "rein: issue #%d (%s) declared but NOT confirmed — no approval surface reached you.\n", req.Issue.Number, req.Issue.Repo)
	fmt.Fprintln(cfg.Stderr, "  To confirm it, in ANOTHER terminal run:")
	fmt.Fprintf(cfg.Stderr, "    %s approval grant --run-id %s\n", reinCmd, cfg.RunID)
	fmt.Fprintf(cfg.Stderr, "  (or just `rein approval grant --run-id %s` if the shim dir is on your PATH)\n", cfg.RunID)
	fmt.Fprintln(cfg.Stderr, "  Then re-run the declare (it is idempotent) or retry the write.")
	fmt.Fprintln(cfg.Stderr)
	fmt.Fprintln(cfg.Stderr, "  Note: invoking grant from this same terminal (e.g. an agent's `!` shell")
	fmt.Fprintln(cfg.Stderr, "  escape) won't work — the agent's bash subprocess has no /dev/tty.")
	fmt.Fprintln(cfg.Stderr)
	cfg.Logger.Printf("grant: DENIED — emitted helpful stderr")
	return false
}

// recordIssue appends the confirmed issue to the run's approval record.
// Best-effort: failures are logged and ignored (the caller already got a
// thumbs-up; a missing record just means the next write re-teaches the
// declare instruction).
func recordIssue(cfg Config, sig, sessionID string, ci approvals.ConfirmedIssue) {
	ci.ConfirmedAt = time.Now()
	if err := approvals.AppendConfirmedIssue(cfg.StateDir, cfg.RunID, sig, sessionID, ci, cfg.TTL); err != nil {
		cfg.Logger.Printf("grant: confirmed-issue append failed (continuing): %v", err)
		return
	}
	cfg.Logger.Printf("grant: issue #%d (%s) recorded for run %s", ci.Number, ci.Repo, cfg.RunID)
}

// Grant is the entry point for the `rein approval grant --run-id X`
// subcommand (the tmux popup / other-terminal surface). It renders the
// Form A prompt ONLY from the on-disk snapshot the declare wrote
// (runs/<run-id>.json: session + PendingIssue) — it MUST NOT call
// session.LoadOrFallback (the popup has no REIN_SESSION_FILE, and
// resolving the DEFAULT session would approve the WRONG session when
// multiple runs are concurrent) and it never fetches (issue #35 §4:
// "the popup never fetches").
//
// It tries /dev/tty first (no CLI flag accepts the issue number — the
// answer must arrive via /dev/tty so a confused or malicious agent can't
// supply it as an argument; --run-id is routing, not the secret). If
// /dev/tty isn't available AND $TMUX is set, it falls back to spawning a
// tmux popup that runs the same subcommand — the popup's pty IS a
// /dev/tty, so the inner invocation completes via the tty branch.
//
// Returns nil on confirmation, a helpful error otherwise (missing run
// id, missing snapshot, NOTHING PENDING, denial, or no prompt path).
func Grant(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.Prompter == nil {
		cfg.Prompter = prompt.TTYPrompter{}
	}
	if cfg.TmuxRunner == nil {
		cfg.TmuxRunner = DefaultTmuxRunner
	}
	if cfg.RunID == "" {
		return errors.New("grant requires --run-id")
	}

	rc, err := approvals.ReadRunContext(cfg.StateDir, cfg.RunID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no run context for run-id %s (the run may have exited, or this is a stale id); have the wrapped agent run `rein declare <n>` so rein writes the context", cfg.RunID)
		}
		return fmt.Errorf("read run context for run-id %s: %w", cfg.RunID, err)
	}
	if rc.PendingIssue == nil {
		return fmt.Errorf("run %s has no pending issue declaration to confirm; have the wrapped agent run `rein declare <n>` first", cfg.RunID)
	}
	sess := rc.Session
	sig := approvals.SignatureOf(sess)
	pending := *rc.PendingIssue

	// Idempotent: already confirmed (e.g. the in-process tty prompt won a
	// race, or grant was run twice) — succeed without re-prompting.
	expansion := false
	if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil && approvals.Valid(rec, sig) {
		if rec.HasIssue(pending.Repo, pending.Number) {
			cfg.Logger.Printf("grant subcommand: issue #%d already confirmed", pending.Number)
			return nil
		}
		expansion = len(rec.Issues) > 0
	}

	pr := formARequest(IssueRequest{Session: sess, Issue: pending}, expansion, cfg.PromptTimeout)
	approved, err := cfg.Prompter.Confirm(ctx, pr)
	switch {
	case err == nil && approved:
		recordIssue(cfg, sig, sess.ID, pending)
		return nil
	case err == nil:
		cfg.Logger.Printf("grant subcommand: denied (wrong answer)")
		return errors.New("confirmation denied (input did not match the issue number)")
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant subcommand: cancelled")
		return errors.New("confirmation cancelled")
	}

	// No /dev/tty. Try tmux popup if available — common case is the
	// user running `! rein approval grant` from inside an agent that
	// itself was launched from inside tmux.
	cfg.Logger.Printf("grant subcommand: /dev/tty unavailable (%v); trying tmux popup", err)
	if os.Getenv("TMUX") != "" {
		reinCmd := resolveReinCmd()
		ctxPopup, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		_ = cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant", "--run-id", cfg.RunID})
		if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil {
			if approvals.Valid(rec, sig) && rec.HasIssue(pending.Repo, pending.Number) {
				cfg.Logger.Printf("grant subcommand: CONFIRMED via tmux popup")
				return nil
			}
		}
	}
	cfg.Logger.Printf("grant subcommand: DENIED — no /dev/tty, no tmux popup")
	return errors.New("could not obtain confirmation: no /dev/tty and no tmux popup available")
}

// fileExists reports whether path resolves to a regular file/symlink
// that we can stat.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}
