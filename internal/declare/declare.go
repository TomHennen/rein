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
	"strings"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/githubapp"
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

	// Scope-expansion outcomes (issue #69).
	AuditScopeExpanded   = "expanded-scope"                    // a REPO joined the run's ceiling
	AuditCrossOwner      = "refused-expansion-cross-owner"     // structural deny, no prompt
	AuditNotInstalled    = "refused-expansion-not-installed"   // App not on the repo; NOTICE, no prompt
	AuditCoverageUnknown = "refused-expansion-coverage-failed" // transient probe failure; fail closed
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
	//
	// SECURITY NOTE (issue #69): for a SCOPE EXPANSION, repo is outside the
	// session's standing ceiling, so this fetch mints a read token that
	// covers a not-yet-approved repo. That is unavoidable and deliberate:
	// decision E forbids prompting without a fetched title, and a title can
	// only be read with a credential. It is bounded — the expansion path
	// runs the same-owner check and the install-coverage probe BEFORE
	// calling Fetch, so the repo is same-owner and already inside the App
	// installation the owner consented to; the token is read-only,
	// short-lived, and (in production) NOT written to the run's shared read
	// cache, so a DENIED expansion leaves the agent's own read path exactly
	// as narrow as it was.
	Fetch func(ctx context.Context, repo string, number int) (issuemeta.Meta, error)

	// ProbeInstall reports whether the GitHub App is installed on repo
	// (the D4 install-coverage check `rein run` does at launch). It is
	// consulted ONLY for a scope expansion — the session's standing repos
	// were already probed at launch.
	//
	//   nil error                    -> covered; proceed to the prompt.
	//   githubapp.ErrAppNotInstalled -> definitive 404: NO prompt fires
	//                                   (nothing approvable exists); the
	//                                   human gets the install NOTICE.
	//   any other error              -> fail the declare CLOSED
	//                                   ("could not verify, retry").
	//
	// A nil ProbeInstall fails every expansion closed: an unprobed
	// expansion is exactly the #53 404 the probe exists to prevent.
	ProbeInstall func(ctx context.Context, repo string) error

	// InstallURL is the App's installation deep-link, carried in the 404
	// notice and the agent-visible message.
	InstallURL string

	// AppName names the App in those messages ("the GitHub App <name> is
	// not installed on ..."). Cosmetic.
	AppName string

	// Notice shows the install NOTICE on the human's approval surface for
	// the 404 case: it carries the deep-link and NO approval authority
	// (there is nothing to approve — the App cannot mint for a repo it is
	// not installed on). It BLOCKS until the human acknowledges, and the
	// real scope-expansion approval fires FRESH when the agent retries the
	// declare — a prompt left open for the minutes an install takes trains
	// answering stale prompts unread.
	//
	// nil degrades to no interactive notice (the agent still gets the
	// instructive message).
	Notice func(ctx context.Context, n Notice)

	// Grant is the Form A confirmation config (prompt surfaces, TTL,
	// popup preference). StateDir/RunID/RunPID are overwritten from the
	// fields above so the two can't disagree.
	Grant grant.Config

	Logger *log.Logger
}

// Notice is the interactive install NOTICE shown on the approval surface
// when a scope expansion targets a repo the App is not installed on.
type Notice struct {
	// Repo is the repo the agent asked for and the App does not cover.
	Repo string

	// Issue is the issue number the agent declared (context only — it was
	// never fetched: no credential can read it).
	Issue int

	// InstallURL is the deep-link the human follows to install the App.
	InstallURL string

	// AppName names the App, if known.
	AppName string
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

	repo, expandingScope, err := resolveRepo(d.Session, repoFlag)
	if err != nil {
		audit := AuditBadRequest
		if errors.Is(err, errCrossOwner) {
			audit = AuditCrossOwner
		}
		return Outcome{Issue: number, Message: "rein: " + err.Error(), Audit: audit}
	}

	sig := approvals.SignatureOf(d.Session)

	// Idempotent fast path: already confirmed ⇒ succeed without a fetch
	// or prompt (issue #35 §3). Covers an already-approved EXPANSION too
	// (its repo is in the record), so a re-declare after expansion never
	// re-probes or re-prompts.
	if rec, rerr := approvals.ReadApproval(d.StateDir, d.RunID); rerr == nil && approvals.Valid(rec, sig) && rec.HasIssue(repo, number) {
		logger.Printf("declare: issue #%d (%s) already confirmed for run %s", number, repo, d.RunID)
		return Outcome{Confirmed: true, Issue: number, Repo: repo,
			Message: fmt.Sprintf("issue #%d in %s is already confirmed for this run", number, repo), Audit: AuditConfirmed}
	}

	// SCOPE EXPANSION pre-checks (issue #69). Both run BEFORE any prompt
	// and before the fetch: a prompt the human cannot usefully answer (the
	// App isn't installed) trains rubber-stamping, and a cross-owner
	// request is structurally impossible rather than a human decision.
	if expandingScope {
		if out, stop := d.checkExpansionCoverage(ctx, repo, number, logger); stop {
			return out
		}
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
		if expandingScope {
			// Deny ≠ error: the run continues at its ORIGINAL scope and
			// nothing is torn down (#35 decision table; mocks §1.2).
			return Outcome{Issue: number, Repo: repo, Audit: AuditDenied,
				Message: fmt.Sprintf("rein: DENIED by the human. %s remains out of scope for this run.\n      Continue working within: %s.",
					repo, strings.Join(d.Session.Repos, ", "))}
		}
		return Outcome{Issue: number, Repo: repo,
			Message: fmt.Sprintf("rein: declaration of issue #%d in %s was NOT confirmed (denied, timed out, or no approval surface)", number, repo), Audit: AuditDenied}
	}

	if expandingScope {
		logger.Printf("declare: SCOPE EXPANDED — repo %s joined run %s's ceiling via issue #%d", repo, d.RunID, number)
		return Outcome{Confirmed: true, Issue: number, Repo: repo, Audit: AuditScopeExpanded,
			Message: expansionApprovedMessage(number, repo)}
	}

	audit := AuditConfirmed
	if expanding {
		audit = AuditExpanded
	}
	logger.Printf("declare: issue #%d (%s) confirmed for run %s (%s)", number, repo, d.RunID, audit)
	return Outcome{Confirmed: true, Issue: number, Repo: repo,
		Message: fmt.Sprintf("issue #%d in %s confirmed — writes are unlocked for this run (push to agent/%d/<nonce>)", number, repo, number), Audit: audit}
}

// expansionApprovedMessage is what the AGENT sees when its expansion is
// approved. Besides the grant, it answers the question the agent asks next
// — "where do I put the checkout?" — because a mid-run expansion grants
// CREDENTIALS, not filesystem: the sandbox's bind mounts are fixed at
// launch and no approval can make a new path writable (mocks §7). The
// agent must clone into an already-writable location, and NOT nest the
// clone inside the current workdir, where it would risk being committed
// into the first repo's tree.
//
// (Binding an EXISTING local checkout of the second repo at launch is
// issue #64, built separately — that is the "next run" half of §7.)
func expansionApprovedMessage(number int, repo string) string {
	return fmt.Sprintf("rein: approved — issue #%d in %s is confirmed for this run, and %s is now in scope.\n"+
		"      Push to branches named agent/%d/<nonce>.\n"+
		"      NOTE: scope grew, but the sandbox's writable paths did NOT (they are fixed at launch).\n"+
		"      Clone %s into a writable scratch dir (e.g. $HOME/ or $TMPDIR) — NOT inside the current\n"+
		"      working tree, where a nested repo can end up committed into the other repo. The clone is\n"+
		"      ephemeral; the durable artifact is the PUSH.",
		number, repo, repo, number, repo)
}

// checkExpansionCoverage runs the install-coverage probe for a repo the
// session does not yet cover. stop=true means the caller must return the
// returned Outcome — no prompt fires.
func (d Deps) checkExpansionCoverage(ctx context.Context, repo string, number int, logger *log.Logger) (Outcome, bool) {
	if d.ProbeInstall == nil {
		// Fail closed: an unprobed expansion is the #53 mid-run 404.
		logger.Printf("declare: no install-coverage probe wired; refusing expansion to %s", repo)
		return Outcome{Issue: number, Repo: repo, Audit: AuditCoverageUnknown,
			Message: fmt.Sprintf("rein: could not verify that the GitHub App covers %s; retry", repo)}, true
	}
	err := d.ProbeInstall(ctx, repo)
	switch {
	case err == nil:
		return Outcome{}, false
	case errors.Is(err, githubapp.ErrAppNotInstalled):
		// Definitive 404. NOTHING is approvable — the App cannot mint a
		// token for a repo it is not installed on — so no approval prompt
		// fires. The human gets an interactive NOTICE carrying the
		// deep-link and no approval authority; the agent is told to retry,
		// which fires the REAL approval prompt fresh (orchestrator's
		// decision on the mocks' open question §5.2).
		logger.Printf("declare: App not installed on %s; showing install notice (no approval prompt)", repo)
		if d.Notice != nil {
			d.Notice(ctx, Notice{Repo: repo, Issue: number, InstallURL: d.InstallURL, AppName: d.AppName})
		}
		return Outcome{Issue: number, Repo: repo, Audit: AuditNotInstalled,
			Message: notInstalledMessage(repo, d.AppName, d.InstallURL)}, true
	default:
		// Transient: fail the declare CLOSED (mocks §1.4 — unlike the
		// launch path, there is no cached install id to fall back to).
		logger.Printf("declare: install-coverage probe for %s failed: %v", repo, err)
		return Outcome{Issue: number, Repo: repo, Audit: AuditCoverageUnknown,
			Message: fmt.Sprintf("rein: could not verify that the GitHub App covers %s (%v); retry", repo, err)}, true
	}
}

// notInstalledMessage is the agent-visible 404 text (mocks §1.4).
func notInstalledMessage(repo, appName, installURL string) string {
	app := appName
	if app == "" {
		app = "rein's GitHub App"
	}
	msg := fmt.Sprintf("rein: cannot request %s — the GitHub App %s is not installed on it.\n"+
		"      The human must install it first", repo, app)
	if installURL != "" {
		msg += ":\n      " + installURL
	}
	return msg + "\n      Then run this command again."
}

// errCrossOwner tags the structural cross-owner denial so Run can audit it
// distinctly, without polluting the user-facing message (which is carried
// verbatim by crossOwnerError).
var errCrossOwner = errors.New("cross-owner expansion")

type crossOwnerError struct{ msg string }

func (e *crossOwnerError) Error() string        { return e.msg }
func (e *crossOwnerError) Is(target error) bool { return target == errCrossOwner }

// resolveRepo picks the repo a declaration targets and reports whether that
// repo is a SCOPE EXPANSION (outside the session's standing ceiling).
//
// Rules:
//   - no --repo, single-repo session      -> the session repo.
//   - no --repo, multi-repo session       -> ambiguous; instruct.
//   - --repo inside the session's scope   -> that repo, no expansion.
//   - --repo outside it, SAME owner       -> expansion (the human decides).
//   - --repo outside it, DIFFERENT owner  -> denied here, structurally.
//     No prompt fires: the App installation is single-owner and
//     BareRepoNames mints by bare name against it, so a mixed-owner ceiling
//     could mint a token for the session owner's identically-named repo.
//     This is not a decision a human is allowed to get wrong (mocks §1).
func resolveRepo(sess session.Session, repoFlag string) (repo string, expansion bool, err error) {
	if repoFlag == "" {
		if len(sess.Repos) == 1 {
			return sess.Repos[0], false, nil
		}
		return "", false, fmt.Errorf("this session scopes multiple repos %v; pass --repo owner/name (e.g. `rein declare <n> --repo %s`)", sess.Repos, sess.Repos[0])
	}
	if sess.Contains(repoFlag) {
		return brokercore.RepoFromPath(repoFlag), false, nil
	}
	norm := brokercore.RepoFromPath(repoFlag)
	if norm == "" || strings.Count(norm, "/") != 1 {
		return "", false, fmt.Errorf("--repo %q is not owner/name-shaped", repoFlag)
	}
	owner, _, _ := strings.Cut(norm, "/")
	sessOwner := session.OwnerOf(sess)
	if !strings.EqualFold(owner, sessOwner) {
		return "", false, &crossOwnerError{msg: fmt.Sprintf(
			"session is scoped to owner %s (the App installation is single-owner); %s cannot be added. A separate session/App would be required",
			sessOwner, norm)}
	}
	return norm, true, nil
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
