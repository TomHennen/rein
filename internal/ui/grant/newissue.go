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

// NewIssueOutcome is how a file-a-new-issue ceremony resolved. Denial and
// refusal are distinguished because they need different messages: a
// refusal means the human was never asked.
type NewIssueOutcome int

const (
	// NewIssueDenied: the human saw the request and said no (or no
	// approval surface reached them, or it timed out).
	NewIssueDenied NewIssueOutcome = iota
	// NewIssueConfirmed: the human typed the word.
	NewIssueConfirmed
	// NewIssueBusy: another ceremony already holds the human's attention.
	NewIssueBusy
	// NewIssueLimited: this run hit MaxNewIssueCeremonies.
	NewIssueLimited
)

// MaxNewIssueCeremonies caps how many file-a-new-issue prompts one run
// may put in front of the human. `rein declare <n>` is self-limiting (it
// needs a real issue that fetches); this endpoint is not, so without a cap
// a prompt-injected agent can manufacture approval prompts indefinitely
// and wear the human down into a wrong answer.
const MaxNewIssueCeremonies = 5

// ObtainNewIssueApproval runs one file-a-new-issue ceremony. It records
// NOTHING — the caller files the issue and records the resulting number —
// and it clears the pending request on the way out so an approval marker
// can never be read twice.
//
// Not idempotent by design: every call is a fresh ask, because there is no
// number to recognize a repeat by.
func ObtainNewIssueApproval(ctx context.Context, req NewIssueRequest, cfg Config) NewIssueOutcome {
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
		return NewIssueDenied
	}

	// Take the ceremony lock BEFORE writing the pending snapshot: the
	// snapshot IS the ceremony's shared state, so writing it outside the
	// lock lets two concurrent requests clobber each other's pending
	// record (and each other's nonce).
	//
	// TryLock, not Lock: a queued new-issue request would sit until the
	// ceremony ahead of it is answered and then prompt for something the
	// agent asked about minutes ago. Refuse immediately and let the agent
	// retry — it is the one path where the agent can generate prompts at
	// will, so queueing them is the fatigue lever.
	if !ceremonyMu.TryLock() {
		cfg.Logger.Printf("grant: another approval ceremony is live; refusing this new-issue request")
		return NewIssueBusy
	}
	defer ceremonyMu.Unlock()

	if n := approvals.NewIssueCeremonyCount(cfg.StateDir, cfg.RunID); n >= MaxNewIssueCeremonies {
		cfg.Logger.Printf("grant: run %s already used %d new-issue ceremonies; refusing", cfg.RunID, n)
		return NewIssueLimited
	}

	nonce, err := newNonce()
	if err != nil {
		cfg.Logger.Printf("grant: could not mint a new-issue nonce (%v); denying", err)
		return NewIssueDenied
	}
	pending := approvals.PendingNewIssue{
		Nonce:     nonce,
		Repo:      req.Repo,
		Title:     req.Title,
		Body:      req.Body,
		WrittenAt: time.Now(),
	}
	snap := approvals.RunContext{
		Session:            req.Session,
		SessionFile:        cfg.SessionFile,
		Direct:             cfg.Direct,
		RunPID:             cfg.RunPID,
		PendingNewIssue:    &pending,
		NewIssueCeremonies: approvals.NewIssueCeremonyCount(cfg.StateDir, cfg.RunID) + 1,
		WrittenAt:          time.Now(),
	}
	if err := approvals.WritePending(cfg.StateDir, cfg.RunID, snap); err != nil {
		// Fail CLOSED, unlike the declared-issue path: the popup and the
		// other-terminal surface answer ONLY through this file, so without
		// it an approval could not be delivered at all — and the inline
		// prompt alone is not worth pretending the ceremony is intact.
		cfg.Logger.Printf("grant: new-issue snapshot write failed; denying: %v", err)
		fmt.Fprintf(cfg.Stderr, "rein: could not record the pending new-issue request (%v); nothing was filed.\n", err)
		return NewIssueDenied
	}
	defer func() {
		if err := approvals.ClearPendingNewIssue(cfg.StateDir, cfg.RunID); err != nil {
			cfg.Logger.Printf("grant: clearing the pending new-issue request failed (best-effort): %v", err)
		}
	}()

	pr := formANewRequest(req, cfg)

	if cfg.PreferPopup {
		if approved, launched := attemptPopupNew(ctx, cfg, nonce); approved {
			return NewIssueConfirmed
		} else if launched {
			cfg.Logger.Printf("grant: new-issue popup unanswered/declined; denying without /dev/tty fallback")
			fmt.Fprintln(cfg.Stderr, "rein: new issue NOT filed (popup closed unanswered) — the agent can re-run `rein declare --new`")
			return NewIssueDenied
		}
		cfg.Logger.Printf("grant: popup preferred but unavailable; trying inline /dev/tty")
	}

	res, err := cfg.Prompter.Confirm(ctx, pr)
	switch {
	case err == nil && res.Approved:
		cfg.Logger.Printf("grant: filing a new issue in %s CONFIRMED via /dev/tty", req.Repo)
		return NewIssueConfirmed
	case err == nil:
		cfg.Logger.Printf("grant: new-issue DENIED via /dev/tty (input mismatched)")
		return NewIssueDenied
	case errors.Is(err, prompt.ErrCancelled):
		cfg.Logger.Printf("grant: new-issue CANCELLED via /dev/tty (Ctrl-C or timeout)")
		return NewIssueDenied
	default:
		cfg.Logger.Printf("grant: /dev/tty unavailable (%v)", err)
	}

	if !cfg.PreferPopup {
		if approved, _ := attemptPopupNew(ctx, cfg, nonce); approved {
			return NewIssueConfirmed
		}
	}
	denyHelpfulNew(req, cfg)
	return NewIssueDenied
}

// formANewRequest builds the prompt for a file-a-new-issue request. The
// title is sanitized once, HERE, and both the displayed text and the
// expected word derive from that same string.
func formANewRequest(req NewIssueRequest, cfg Config) prompt.Request {
	// The whole proposed title, not a 140-rune prefix of it: what the
	// human confirms must be what gets filed.
	title := issuemeta.SanitizeProposedTitle(req.Title)
	already := confirmedNumbers(cfg, req.Session)
	return prompt.Request{
		SessionID: req.Session.ID,
		Role:      req.Session.Role,
		Repos:     req.Session.Repos,
		NewIssue:  true,
		IssueRepo: req.Repo,
		Title:     title,
		Body:      issuemeta.BodyExcerpt(req.Body),
		// The declared-issue prompt gets this signal from Expansion; the
		// new-issue prompt needs it more, because "file another issue" when
		// the run already has one is usually the agent losing the thread.
		AlreadyConfirmed: already,
		Expansion:        len(already) > 0,
		ConfirmWord:      issuemeta.FirstWord(title),
		Timeout:          approvalTimeout(cfg),
	}
}

// confirmedNumbers returns the issue numbers this run has already
// confirmed, for the prompt's "you already have an issue" line. A
// missing or signature-mismatched record reads as none.
func confirmedNumbers(cfg Config, sess session.Session) []int {
	rec, err := approvals.ReadApproval(cfg.StateDir, cfg.RunID)
	if err != nil || !approvals.Valid(rec, approvals.SignatureOf(sess)) {
		return nil
	}
	out := make([]int, 0, len(rec.Issues))
	for _, ci := range rec.Issues {
		out = append(out, ci.Number)
	}
	return out
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
//
// It deliberately does NOT offer `rein approval grant --run-id X`, the way
// the declared-issue path does: this function returns straight into the
// deferred ClearPendingNewIssue, so by the time a human could type that
// command the pending request is gone and it would answer "no pending
// new-issue request". Printing a command that cannot work is worse than
// printing none. Re-running the declare is the real recovery.
func denyHelpfulNew(req NewIssueRequest, cfg Config) {
	fmt.Fprintln(cfg.Stderr)
	fmt.Fprintf(cfg.Stderr, "rein: the agent asked to FILE a new issue in %s — NOT confirmed, no approval surface reached you.\n", req.Repo)
	fmt.Fprintf(cfg.Stderr, "  proposed title: %q\n", issuemeta.SanitizeProposedTitle(req.Title))
	fmt.Fprintln(cfg.Stderr, "  Nothing was filed. The agent can re-run `rein declare --new`.")
	fmt.Fprintln(cfg.Stderr)
	cfg.Logger.Printf("grant: new-issue DENIED — emitted helpful stderr")
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
