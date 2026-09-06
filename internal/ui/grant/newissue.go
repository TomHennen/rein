// newissue.go — the Form A variant for FILING a new issue (issue #180,
// design §2.2 "agent is requesting to file a new issue").
//
// Same layering as ObtainIssueApproval (popup / inline /dev/tty / helpful
// deny) with one structural difference: there is no issue number yet, so
// an approval cannot be expressed as a confirmed issue in the run's
// record. The out-of-process surfaces instead nonce-match the pending
// request and set its Approved marker; the BROKER — the only process
// holding the App key — reads that, files the issue, and records it.
package grant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
)

// NewIssueRequest is one agent request to file a new issue. Repo is
// already resolved and in scope; Title/Body are already validated and
// sanitized by the caller (internal/declare).
type NewIssueRequest struct {
	Session session.Session
	Repo    string
	Title   string
	Body    string
}

// ObtainNewIssueApproval returns true iff the human confirmed filing this
// issue. It records NOTHING — the caller files the issue and records the
// resulting number — and it clears the pending request on the way out so
// an approval marker can never be read twice.
//
// Not idempotent by design: every call is a fresh ask, because there is no
// number to recognize a repeat by.
func ObtainNewIssueApproval(ctx context.Context, req NewIssueRequest, cfg Config) bool {
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
	if cfg.RunID == "" {
		cfg.Logger.Printf("grant: no REIN_RUN_ID; a new-issue declare cannot be keyed to a run — denying")
		fmt.Fprintln(cfg.Stderr, "rein: no run context (REIN_RUN_ID unset) — launch your agent via `rein run -- <cmd>` and declare from within it.")
		return false
	}

	nonce, err := newNonce()
	if err != nil {
		cfg.Logger.Printf("grant: could not mint a new-issue nonce (%v); denying", err)
		return false
	}
	pending := approvals.PendingNewIssue{
		Nonce:     nonce,
		Repo:      req.Repo,
		Title:     req.Title,
		Body:      req.Body,
		WrittenAt: time.Now(),
	}
	snap := approvals.RunContext{
		Session:         req.Session,
		SessionFile:     cfg.SessionFile,
		Direct:          cfg.Direct,
		RunPID:          cfg.RunPID,
		PendingNewIssue: &pending,
		WrittenAt:       time.Now(),
	}
	if err := approvals.WriteRunContext(cfg.StateDir, cfg.RunID, snap); err != nil {
		// Fail CLOSED, unlike the declared-issue path: the popup and the
		// other-terminal surface answer ONLY through this file, so without
		// it an approval could not be delivered at all — and the inline
		// prompt alone is not worth pretending the ceremony is intact.
		cfg.Logger.Printf("grant: new-issue snapshot write failed; denying: %v", err)
		fmt.Fprintf(cfg.Stderr, "rein: could not record the pending new-issue request (%v); nothing was filed.\n", err)
		return false
	}
	defer func() {
		if err := approvals.ClearPendingNewIssue(cfg.StateDir, cfg.RunID); err != nil {
			cfg.Logger.Printf("grant: clearing the pending new-issue request failed (best-effort): %v", err)
		}
	}()

	// One live ceremony at a time in this process — see the lock site in
	// ObtainIssueApproval for the full rationale.
	ceremonyMu.Lock()
	defer ceremonyMu.Unlock()

	pr := formANewRequest(req, cfg)

	if cfg.PreferPopup {
		if approved, launched := attemptPopupNew(ctx, cfg, nonce); approved {
			return true
		} else if launched {
			cfg.Logger.Printf("grant: new-issue popup unanswered/declined; denying without /dev/tty fallback")
			fmt.Fprintln(cfg.Stderr, "rein: new issue NOT filed (popup closed unanswered) — the agent can re-run `rein declare --new`")
			return false
		}
		cfg.Logger.Printf("grant: popup preferred but unavailable; trying inline /dev/tty")
	}

	res, err := cfg.Prompter.Confirm(ctx, pr)
	switch {
	case err == nil && res.Approved:
		cfg.Logger.Printf("grant: filing a new issue in %s CONFIRMED via /dev/tty", req.Repo)
		return true
	case err == nil:
		cfg.Logger.Printf("grant: new-issue DENIED via /dev/tty (input mismatched)")
		return false
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant: new-issue CANCELLED via /dev/tty (Ctrl-C or timeout)")
		return false
	default:
		cfg.Logger.Printf("grant: /dev/tty unavailable (%v)", err)
	}

	if !cfg.PreferPopup {
		if approved, _ := attemptPopupNew(ctx, cfg, nonce); approved {
			return true
		}
	}
	return denyHelpfulNew(req, cfg)
}

// formANewRequest builds the prompt for a file-a-new-issue request. The
// title is sanitized once, HERE, and both the displayed text and the
// expected word derive from that same string.
func formANewRequest(req NewIssueRequest, cfg Config) prompt.Request {
	title := issuemeta.SanitizeTitle(req.Title)
	return prompt.Request{
		SessionID:   req.Session.ID,
		Role:        req.Session.Role,
		Repos:       req.Session.Repos,
		NewIssue:    true,
		IssueRepo:   req.Repo,
		Title:       title,
		Body:        issuemeta.BodyExcerpt(req.Body),
		ConfirmWord: issuemeta.FirstWord(title),
		Timeout:     approvalTimeout(cfg),
	}
}

// attemptPopupNew is attemptPopup for a new-issue request: approval is
// proven by the nonce-matched marker on the pending request, not by an
// issue appearing in the approval record.
func attemptPopupNew(ctx context.Context, cfg Config, nonce string) (approved, launched bool) {
	if os.Getenv("TMUX") == "" {
		return false, false
	}
	reinCmd := resolveReinCmd()
	cfg.Logger.Printf("grant: launching tmux popup for a new-issue request (%s approval grant --run-id %s)", reinCmd, cfg.RunID)
	ctxPopup, cancel := popupContext(ctx, cfg)
	defer cancel()
	runErr := cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant", "--run-id", cfg.RunID})
	if runErr != nil {
		cfg.Logger.Printf("grant: tmux popup failed: %v", runErr)
	}
	if approvals.NewIssueApproved(cfg.StateDir, cfg.RunID, nonce) {
		cfg.Logger.Printf("grant: new issue CONFIRMED via tmux popup")
		return true, true
	}
	if errors.Is(ctxPopup.Err(), context.DeadlineExceeded) {
		cfg.Logger.Printf("grant: tmux popup timed out unanswered (%s)", approvalTimeout(cfg))
		return false, true
	}
	if runErr == nil {
		cfg.Logger.Printf("grant: tmux popup closed without confirming")
		return false, true
	}
	return false, false
}

// denyHelpfulNew is denyHelpful for a new-issue request.
func denyHelpfulNew(req NewIssueRequest, cfg Config) bool {
	reinCmd := "rein"
	if abs, err := os.Executable(); err == nil {
		if rp := filepath.Join(filepath.Dir(abs), "rein"); fileExists(rp) {
			reinCmd = rp
		} else {
			reinCmd = abs
		}
	}
	fmt.Fprintln(cfg.Stderr)
	fmt.Fprintf(cfg.Stderr, "rein: the agent asked to FILE a new issue in %s — NOT confirmed, no approval surface reached you.\n", req.Repo)
	fmt.Fprintf(cfg.Stderr, "  proposed title: %q\n", issuemeta.SanitizeTitle(req.Title))
	fmt.Fprintln(cfg.Stderr, "  To decide, in ANOTHER terminal run:")
	fmt.Fprintf(cfg.Stderr, "    %s approval grant --run-id %s\n", reinCmd, cfg.RunID)
	fmt.Fprintln(cfg.Stderr, "  Nothing was filed. The agent can re-run `rein declare --new`.")
	fmt.Fprintln(cfg.Stderr)
	cfg.Logger.Printf("grant: new-issue DENIED — emitted helpful stderr")
	return false
}

// grantNewIssue is the `rein approval grant` branch for a pending
// new-issue request. It renders the prompt ONLY from the on-disk snapshot
// (like the declared-issue branch — it never fetches and never resolves a
// session of its own), and on approval sets the nonce-matched marker.
//
// It records NO confirmed issue and files nothing: this process holds no
// obligation (or wiring) to mint a token. The broker that wrote the
// request is watching for the marker and does both.
func grantNewIssue(ctx context.Context, cfg Config, rc approvals.RunContext) error {
	pending := *rc.PendingNewIssue
	if pending.Approved {
		cfg.Logger.Printf("grant subcommand: new-issue request already approved")
		return nil
	}
	req := NewIssueRequest{Session: rc.Session, Repo: pending.Repo, Title: pending.Title, Body: pending.Body}

	res, err := cfg.Prompter.Confirm(ctx, formANewRequest(req, cfg))
	switch {
	case err == nil && res.Approved:
		if merr := approvals.MarkNewIssueApproved(cfg.StateDir, cfg.RunID, pending.Nonce); merr != nil {
			cfg.Logger.Printf("grant subcommand: marking the new-issue approval failed: %v", merr)
			return fmt.Errorf("could not record the approval (%w); nothing was filed", merr)
		}
		cfg.Logger.Printf("grant subcommand: new issue in %s APPROVED via /dev/tty", pending.Repo)
		fmt.Fprintln(cfg.Stderr, "rein: approved — rein is filing the issue now.")
		return nil
	case err == nil:
		cfg.Logger.Printf("grant subcommand: new-issue denied (wrong answer)")
		return errors.New("confirmation denied (input did not match the title's first word)")
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant subcommand: new-issue cancelled")
		return errors.New("confirmation cancelled")
	}

	// No /dev/tty — same popup fallback as the declared-issue branch.
	cfg.Logger.Printf("grant subcommand: /dev/tty unavailable (%v); trying tmux popup", err)
	if os.Getenv("TMUX") != "" {
		reinCmd := resolveReinCmd()
		ctxPopup, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		_ = cfg.TmuxRunner(ctxPopup, []string{reinCmd, "approval", "grant", "--run-id", cfg.RunID})
		if approvals.NewIssueApproved(cfg.StateDir, cfg.RunID, pending.Nonce) {
			cfg.Logger.Printf("grant subcommand: new issue CONFIRMED via tmux popup")
			return nil
		}
	}
	cfg.Logger.Printf("grant subcommand: new-issue DENIED — no /dev/tty, no tmux popup")
	return errors.New("could not obtain confirmation: no /dev/tty and no tmux popup available")
}

// newNonce mints the per-request nonce that binds an approval marker to
// exactly one pending new-issue request.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
