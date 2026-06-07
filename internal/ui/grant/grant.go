// Package grant implements the layered approval-obtaining flow used by
// both the credential helper (for git operations) and the rein-gh shim
// (for gh operations).
//
// # Layers
//
// ObtainApproval tries layers in order until one resolves:
//
//  1. Existing approval record (internal/approvals). If a valid record
//     covers the current session signature, return true immediately.
//  2. Inline /dev/tty prompt. If a controlling terminal is reachable
//     (typically: a developer at a shell), prompt and record on
//     approval. Fast for interactive use.
//  3. Tmux popup. If $TMUX is set, spawn `tmux popup -E "rein approval
//     grant"` and wait for it to close. The popup runs the grant
//     subcommand, which uses /dev/tty inside the popup's pty. After
//     close, re-check the approval record.
//  4. Helpful stderr + deny. If none of the above resolved, emit a
//     message to stderr telling the user to run `rein approval grant`
//     in another terminal, then deny. The user runs it, retries the
//     operation.
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
	// StateDir is rein's state directory. Per-run approval and run-context
	// files live under approvals/<run-id>.json and runs/<run-id>.json.
	StateDir string

	// RunID is the per-run nonce (REIN_RUN_ID, set by `rein run`). It
	// keys this run's approval + run-context files. An empty RunID means
	// the helper was invoked OUTSIDE `rein run` — ObtainApproval then
	// fails closed (interactive-only, persists nothing). The caller
	// populates this from the env; ObtainApproval does NOT read the env
	// itself (keeps tests env-free).
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

	// No REIN_RUN_ID => the helper was invoked OUTSIDE `rein run` (e.g.
	// the user ran git directly with rein as the GLOBAL credential
	// helper). We cannot key an approval to a run, and reusing any other
	// run's approval would be cross-run leakage. DELIBERATE BEHAVIOR
	// CHANGE from the old global-reuse model: fail closed. Run the
	// interactive layers (so a human at a terminal can still approve THIS
	// op) but PERSIST NOTHING — every run-id-less op re-prompts. TM-G8 is
	// preserved: a denial still returns false (caller serves the
	// placeholder), never an error.
	if cfg.RunID == "" {
		cfg.Logger.Printf("grant: no REIN_RUN_ID; fail-closed (interactive-only, no record persisted)")
		return obtainInteractiveNoPersist(ctx, req, cfg)
	}

	// Layer 1: existing per-run approval record. The validity check is
	// LIVE session signature vs the recorded signature — this catches a
	// mid-run dev-session.yaml scope edit (the recorded sig was computed
	// from the snapshot at approval time, so live-vs-record IS live-vs-
	// snapshot, persisted). We deliberately do NOT re-read runs/<id>.json
	// here: the snapshot is transport for the out-of-process grant, not
	// part of this check (snapshot-sig vs record-sig is always equal).
	sig := approvals.SignatureOf(req.Session)
	if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil {
		if approvals.Valid(rec, sig) {
			cfg.Logger.Printf("grant: covered by run %s approval (granted at %s)",
				cfg.RunID, rec.ApprovedAt.Format(time.RFC3339))
			return true
		}
		cfg.Logger.Printf("grant: run %s approval mismatched (mid-run scope edit?); re-prompting", cfg.RunID)
	}

	// Persist the session snapshot NOW, before any prompt. Two reasons:
	//  1. An OUT-OF-PROCESS grant (tmux popup, or the layer-4
	//     other-terminal command) has neither REIN_RUN_ID nor
	//     REIN_SESSION_FILE — runs/<id>.json is the only way it can learn
	//     WHICH session it is approving.
	//  2. The snapshot carries RunPID, which Sweep uses for its liveness
	//     probe. Writing it here (not just before the popup) means even a
	//     run that approves via the inline /dev/tty path gets a
	//     PID-carrying snapshot — so a long (>maxAge) tty-approved run is
	//     never swept mid-run (constraint: never a re-prompt within a run).
	// The helper is the sole writer: it alone holds both the run id and the
	// resolved session with the real REIN_RUN_PID. (Grant's out-of-process
	// recordApproval must NOT write this — it would clobber RunPID with 0.)
	snap := approvals.RunContext{Session: req.Session, RunPID: cfg.RunPID, WrittenAt: time.Now()}
	if err := approvals.WriteRunContext(cfg.StateDir, cfg.RunID, snap); err != nil {
		cfg.Logger.Printf("grant: snapshot write failed (out-of-process grant may not resolve; sweep may treat run as orphan): %v", err)
		// Continue: the in-process tty prompt still works; layer 4 still
		// prints a command (the grant subcommand errors helpfully if the
		// snapshot is absent).
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

	// Layer 3: tmux popup if we're inside tmux. (Snapshot already written
	// above, so the popup's out-of-process grant can resolve the session.)
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
		cfg.Logger.Printf("grant: launching tmux popup (%s approval grant --run-id %s)", reinCmd, cfg.RunID)
		ctxPopup, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if err := cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant", "--run-id", cfg.RunID}); err != nil {
			cfg.Logger.Printf("grant: tmux popup failed: %v; falling through", err)
		}
		if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil && approvals.Valid(rec, sig) {
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
	fmt.Fprintln(cfg.Stderr, "  To grant write access for this run, in ANOTHER terminal run:")
	fmt.Fprintf(cfg.Stderr, "    %s approval grant --run-id %s\n", reinCmd, cfg.RunID)
	fmt.Fprintf(cfg.Stderr, "  (or just `rein approval grant --run-id %s` if the shim dir is on your PATH)\n", cfg.RunID)
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
	if err := approvals.WriteApproval(cfg.StateDir, cfg.RunID, rec); err != nil {
		cfg.Logger.Printf("grant: approval write failed (continuing): %v", err)
		return
	}
	cfg.Logger.Printf("grant: approval recorded for run %s", cfg.RunID)
}

// obtainInteractiveNoPersist runs the interactive layers (2-4) WITHOUT
// persisting any record or run-context. It is the fail-closed path taken
// when there is no REIN_RUN_ID: with no run id to name the files, we
// cannot key an approval, and the out-of-process layers (popup,
// other-terminal grant) cannot resolve a session either — so they are
// disabled and only the in-process /dev/tty prompt can approve THIS op.
// Every such op re-prompts. TM-G8 preserved: a denial returns false, the
// caller serves the placeholder.
func obtainInteractiveNoPersist(ctx context.Context, req Request, cfg Config) bool {
	pr := prompt.Request{
		SessionID: req.Session.ID,
		Role:      req.Session.Role,
		Repo:      req.Repo,
		Action:    fmt.Sprintf("%s (no rein run context: approves THIS op only)", req.Action),
		Issue:     req.Session.Issue,
		Timeout:   cfg.PromptTimeout,
	}
	approved, err := cfg.Prompter.Confirm(ctx, pr)
	switch {
	case err == nil && approved:
		cfg.Logger.Printf("grant: APPROVED via /dev/tty (no-persist, outside rein run)")
		return true
	case err == nil:
		cfg.Logger.Printf("grant: DENIED via /dev/tty (no-persist)")
		return false
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant: CANCELLED via /dev/tty (no-persist)")
		return false
	default:
		cfg.Logger.Printf("grant: /dev/tty unavailable (%v); no rein run context, cannot use popup/other-terminal grant", err)
	}

	// No tty AND no run id: emit a helpful message and deny. We cannot
	// route an out-of-process grant without a run id.
	fmt.Fprintln(cfg.Stderr)
	fmt.Fprintf(cfg.Stderr, "rein: write blocked for %s on %s\n", req.Action, req.Repo)
	fmt.Fprintln(cfg.Stderr, "  This operation ran OUTSIDE `rein run` (no REIN_RUN_ID), so rein cannot")
	fmt.Fprintln(cfg.Stderr, "  route an approval to it. Launch your agent via `rein run -- <cmd>` so")
	fmt.Fprintln(cfg.Stderr, "  writes can be approved per-run.")
	fmt.Fprintln(cfg.Stderr)
	cfg.Logger.Printf("grant: DENIED — no /dev/tty and no REIN_RUN_ID")
	return false
}

// Grant is the entry point for the `rein approval grant --run-id X`
// subcommand. It loads the session ONLY from the on-disk snapshot the
// helper wrote (runs/<run-id>.json) — it MUST NOT call
// session.LoadOrFallback: the popup / other-terminal process has no
// REIN_SESSION_FILE, and resolving the DEFAULT session would silently
// approve the WRONG session when multiple `rein run`s on different
// sessions run concurrently (the linchpin failure mode this whole change
// exists to prevent).
//
// It tries /dev/tty first (no CLI flag accepts the issue number — design
// §5.3 TM-G5 wants the answer to arrive via /dev/tty so a confused or
// malicious agent can't supply it via a CLI arg; --run-id is routing,
// not the secret). If /dev/tty isn't available AND $TMUX is set, it
// falls back to spawning a tmux popup that runs the same subcommand
// (with --run-id) — the popup's pty IS a /dev/tty, so the inner
// invocation completes via the tty branch and writes the approval
// record. Recursion is bounded: the popup's invocation has a tty.
//
// Returns nil on approval, a helpful error otherwise (missing run id,
// missing/stale snapshot, denial, or no resolvable prompt path).
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
			return fmt.Errorf("no run context for run-id %s (the run may have exited, or this is a stale id); have the wrapped agent retry the operation so rein re-writes the context", cfg.RunID)
		}
		return fmt.Errorf("read run context for run-id %s: %w", cfg.RunID, err)
	}
	sess := rc.Session
	sig := approvals.SignatureOf(sess)
	pr := prompt.Request{
		SessionID: sess.ID,
		Role:      sess.Role,
		Repo:      joinRepos(sess.Repos),
		Action:    "grant write access (covers writes for this run)",
		Issue:     sess.Issue,
		Timeout:   cfg.PromptTimeout,
	}
	approved, err := cfg.Prompter.Confirm(ctx, pr)
	switch {
	case err == nil && approved:
		recordApproval(cfg, sig, sess.ID)
		return nil
	case err == nil:
		cfg.Logger.Printf("grant subcommand: denied (wrong answer)")
		return errors.New("approval denied (issue number did not match)")
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant subcommand: cancelled")
		return errors.New("approval cancelled")
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
		_ = cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant", "--run-id", cfg.RunID})
		if rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID); err == nil {
			if approvals.Valid(rec, sig) {
				cfg.Logger.Printf("grant subcommand: APPROVED via tmux popup")
				return nil
			}
		}
	}
	cfg.Logger.Printf("grant subcommand: DENIED — no /dev/tty, no tmux popup")
	return errors.New("could not obtain approval: no /dev/tty and no tmux popup available")
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
