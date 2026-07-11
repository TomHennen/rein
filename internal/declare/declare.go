// Package declare orchestrates one issue declaration (issue #35 §3):
// resolve the target repo, FETCH the issue's metadata (title/state/home
// repo — decision E: no prompt without a fetched title), fire the
// mode-independent Form A grant machinery, and record the confirmation
// in the run's approval record.
//
// Two transports share this one implementation:
//
//   - Direct mode: `rein declare <n>` runs as a plain CLI inside the
//     wrapped agent's shell (REIN_RUN_ID in env) and calls Run in-process.
//   - Sandboxed mode: the in-sandbox `rein declare <n>` hits the
//     declare.rein.internal virtual host on the per-run proxy socket; the
//     broker side (cmd/rein run_sandboxed) calls Run out-of-sandbox.
//
// Run BLOCKS while the human decides (the prompt has its own timeout) —
// the same unbounded approval-pause discipline the proxy applies. It is
// idempotent: re-declaring an already-confirmed issue succeeds without
// re-prompting.
package declare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
)

// Audit decision tags for declare outcomes (recorded by the sandboxed
// proxy's audit log; direct mode logs them to helper.log).
const (
	AuditConfirmed  = "confirmed-issue"
	AuditExpanded   = "expanded-issue"
	AuditUnverified = "refused-issue-unverified" // fetch failed / 404 / 301
	AuditDenied     = "refused-declare-denied"   // human denied / no surface
	AuditBadRequest = "refused-declare-invalid"  // bad number / repo resolution
)

// Outcome is the result of one declaration attempt. Message is
// agent-visible (it crosses the proxy back into the sandbox) and MUST
// never carry a token — it is built only from fixed strings, the issue
// number, and the repo name.
type Outcome struct {
	Confirmed bool
	Issue     int
	Repo      string
	Message   string
	Audit     string // one of the Audit* tags
}

// Deps carries the environment a declaration runs against. Callers fill
// it from their mode's wiring; tests stub Fetch and the grant surfaces.
type Deps struct {
	StateDir string
	RunID    string
	RunPID   int
	Session  session.Session

	// Fetch fetches the declared issue's metadata using a read-tier,
	// issues:read-capable credential (the MintGhReadOnlyToken shape).
	// Production wires internal/issuemeta.Fetch under a fresh mint;
	// tests stub. Must honor ctx.
	Fetch func(ctx context.Context, repo string, number int) (issuemeta.Meta, error)

	// Grant is the Form A confirmation config (prompt surfaces, TTL,
	// popup preference). StateDir/RunID/RunPID are overwritten from the
	// fields above so the two can't disagree.
	Grant grant.Config

	Logger *log.Logger
}

// Run performs one declaration: number is the agent-declared issue,
// repoFlag the optional --repo owner/name (required for multi-repo
// sessions; must be within the session scope).
func Run(ctx context.Context, d Deps, number int, repoFlag string) Outcome {
	logger := d.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if number <= 0 {
		return Outcome{Issue: number, Message: "rein: issue number must be a positive integer", Audit: AuditBadRequest}
	}
	if d.RunID == "" {
		// §6: declare outside any run fails with the launch instruction.
		return Outcome{Issue: number, Message: "rein: no run context — launch your agent via `rein run -- <cmd>` and declare from within it", Audit: AuditBadRequest}
	}

	repo, err := resolveRepo(d.Session, repoFlag)
	if err != nil {
		return Outcome{Issue: number, Message: "rein: " + err.Error(), Audit: AuditBadRequest}
	}

	sig := approvals.SignatureOf(d.Session)

	// Idempotent fast path: already confirmed ⇒ succeed without a fetch
	// or prompt (issue #35 §3).
	if rec, rerr := approvals.ReadApproval(d.StateDir, d.RunID); rerr == nil && approvals.Valid(rec, sig) && rec.HasIssue(repo, number) {
		logger.Printf("declare: issue #%d (%s) already confirmed for run %s", number, repo, d.RunID)
		return Outcome{Confirmed: true, Issue: number, Repo: repo,
			Message: fmt.Sprintf("issue #%d in %s is already confirmed for this run", number, repo), Audit: AuditConfirmed}
	}

	// Fetch — every failure fails the declare closed (§6). The issue
	// number resolves ONLY against the declared repo, which closes S4
	// structurally (a number valid only in another repo 404s here).
	meta, err := d.Fetch(ctx, repo, number)
	if err != nil {
		logger.Printf("declare: fetch issue #%d (%s) failed: %v", number, repo, err)
		switch {
		case errors.Is(err, issuemeta.ErrNotFound):
			return Outcome{Issue: number, Repo: repo,
				Message: fmt.Sprintf("rein: issue #%d not found in %s", number, repo), Audit: AuditUnverified}
		case errors.Is(err, issuemeta.ErrTransferred):
			return Outcome{Issue: number, Repo: repo,
				Message: fmt.Sprintf("rein: issue #%d in %s was TRANSFERRED to another repo; declare it against its new home", number, repo), Audit: AuditUnverified}
		default:
			return Outcome{Issue: number, Repo: repo,
				Message: fmt.Sprintf("rein: could not verify issue #%d in %s; retry (fetch failed)", number, repo), Audit: AuditUnverified}
		}
	}

	// Expansion detection for the audit tag (the prompt's own header is
	// derived inside grant from the record state at prompt time).
	expanding := false
	if rec, rerr := approvals.ReadApproval(d.StateDir, d.RunID); rerr == nil && approvals.Valid(rec, sig) && len(rec.Issues) > 0 {
		expanding = true
	}

	gcfg := d.Grant
	gcfg.StateDir = d.StateDir
	gcfg.RunID = d.RunID
	gcfg.RunPID = d.RunPID
	if gcfg.Logger == nil {
		gcfg.Logger = logger
	}

	ci := approvals.ConfirmedIssue{
		Number:       meta.Number,
		Repo:         meta.Repo,
		Title:        meta.Title,
		State:        meta.State,
		IsPR:         meta.IsPR,
		CanonicalURL: meta.CanonicalURL,
	}
	if !grant.ObtainIssueApproval(ctx, grant.IssueRequest{Session: d.Session, Issue: ci}, gcfg) {
		return Outcome{Issue: number, Repo: repo,
			Message: fmt.Sprintf("rein: declaration of issue #%d in %s was NOT confirmed (denied, timed out, or no approval surface)", number, repo), Audit: AuditDenied}
	}

	audit := AuditConfirmed
	if expanding {
		audit = AuditExpanded
	}
	logger.Printf("declare: issue #%d (%s) confirmed for run %s (%s)", number, repo, d.RunID, audit)
	return Outcome{Confirmed: true, Issue: number, Repo: repo,
		Message: fmt.Sprintf("issue #%d in %s confirmed — writes are unlocked for this run (push to agent/%d/<nonce>)", number, repo, number), Audit: audit}
}

// resolveRepo picks the repo a declaration targets: single-repo sessions
// use the session repo; multi-repo sessions require an explicit --repo
// within the session scope (an ambiguous declare is denied with that
// instruction — multi-repo polish is deferred, §9).
func resolveRepo(sess session.Session, repoFlag string) (string, error) {
	if repoFlag != "" {
		if !sess.Contains(repoFlag) {
			return "", fmt.Errorf("repo %q is not in this session's scope %v", repoFlag, sess.Repos)
		}
		return repoFlag, nil
	}
	if len(sess.Repos) == 1 {
		return sess.Repos[0], nil
	}
	return "", fmt.Errorf("this session scopes multiple repos %v; pass --repo owner/name (e.g. `rein declare <n> --repo %s`)", sess.Repos, sess.Repos[0])
}

// InvalidateTransferred is the per-write-mint TM-G6 re-check (issue #35
// §6 "issue transferred" row): re-GET the canonical URL of every issue
// in the run's confirmed set; any 3xx means the issue was transferred —
// that confirmation is REMOVED from the record (loudly), and if the set
// empties the mint itself fails (the caller's write degrades to the
// fail-closed placeholder, and the agent is told to re-declare).
//
// Bounded verification failures (network blip, 5xx) KEEP the
// confirmation and log: "cannot verify" is not "transferred", and the
// write mint immediately after would surface a real outage anyway.
// token supplies a read-tier, issues:read-capable token.
func InvalidateTransferred(ctx context.Context, stateDir, runID string, sess session.Session, token func(ctx context.Context) (string, error), logger *log.Logger, stderr io.Writer) error {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if stderr == nil {
		stderr = io.Discard
	}
	sig := approvals.SignatureOf(sess)
	rec, err := approvals.ReadApproval(stateDir, runID)
	if err != nil || !approvals.Valid(rec, sig) || len(rec.Issues) == 0 {
		return nil // nothing confirmed — the write gates already deny
	}
	tok, err := token(ctx)
	if err != nil {
		logger.Printf("transfer re-check: no read token (%v); keeping confirmations (the mint right after will surface a real outage)", err)
		return nil
	}
	kept := make([]approvals.ConfirmedIssue, 0, len(rec.Issues))
	removed := 0
	for _, ci := range rec.Issues {
		cerr := issuemeta.CheckCanonical(ctx, tok, ci.CanonicalURL)
		switch {
		case errors.Is(cerr, issuemeta.ErrTransferred):
			removed++
			logger.Printf("transfer re-check: issue #%d (%s) was TRANSFERRED; confirmation invalidated", ci.Number, ci.Repo)
			fmt.Fprintf(stderr, "rein: issue #%d (%s) was TRANSFERRED to another repo — its confirmation is INVALIDATED.\n", ci.Number, ci.Repo)
			fmt.Fprintln(stderr, "      Re-declare it against its new home: rein declare <n> --repo <owner/name>")
		case cerr != nil:
			logger.Printf("transfer re-check: could not verify issue #%d (%v); keeping its confirmation", ci.Number, cerr)
			kept = append(kept, ci)
		default:
			kept = append(kept, ci)
		}
	}
	if removed == 0 {
		return nil
	}
	rec.Issues = kept
	if err := approvals.WriteApproval(stateDir, runID, rec); err != nil {
		return fmt.Errorf("rewrite approval record after transfer invalidation: %w", err)
	}
	if len(kept) == 0 {
		return errors.New("all confirmed issues were transferred; run `rein declare <n>` against the issue's new home before writing")
	}
	return nil
}
