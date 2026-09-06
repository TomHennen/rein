// newissue.go — `rein declare --new "<title>"`: the agent asks the human
// to approve FILING an issue, and on approval rein files it under the bot
// identity and it becomes the run's declared issue (issue #180, design
// §2.2). This is the bootstrap for a repo with zero issues, where `gh
// issue create` is write-tier (locked) and `rein declare <n>` has no
// number to name.
//
// Order is load-bearing: validate → resolve repo (in scope ONLY) → probe
// install coverage → PROMPT → file → record. Every failure before the
// prompt is a refusal with nothing filed; a failure after the human
// approved says so plainly and records nothing.
package declare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/ui/grant"
)

// Audit decision tags for the file-a-new-issue path (issue #180).
const (
	AuditConfirmedNew    = "confirmed-new-issue"
	AuditNewDenied       = "refused-declare-new-denied"
	AuditNewBadRequest   = "refused-declare-new-invalid"
	AuditNewCreateFailed = "refused-declare-new-create-failed"
	AuditNewBusy         = "refused-declare-new-busy"  // another ceremony is live
	AuditNewLimit        = "refused-declare-new-limit" // per-run ceremony cap
)

// ErrBrokerLocal marks a CreateIssue failure that happened INSIDE the
// broker — keystore, mint, App config — rather than at GitHub. Its text
// can carry host paths and key locations, so RunNew relays a generic
// message for it and leaves the detail in the host-side log. A GitHub
// error, by contrast, is the useful half of the message ("Resource not
// accessible by integration") and is passed through.
var ErrBrokerLocal = errors.New("broker-side failure")

// NewIssue is one request to file a new issue. Repo is the optional
// --repo owner/name; Title/Body are agent-supplied and re-validated here.
type NewIssue struct {
	Repo  string
	Title string
	Body  string
}

// RunNew performs one file-a-new-issue declaration. It BLOCKS while the
// human decides. Not idempotent: a new issue has no number to recognize a
// repeat by, so a re-run asks again (see the package's known limits).
func RunNew(ctx context.Context, d Deps, req NewIssue) Outcome {
	logger := d.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if d.RunID == "" {
		return Outcome{Message: "rein: no run context — launch your agent via `rein run -- <cmd>` and declare from within it", Audit: AuditNewBadRequest}
	}

	title, err := issuemeta.ValidateTitle(req.Title)
	if err != nil {
		return Outcome{Message: "rein: " + err.Error(), Audit: AuditNewBadRequest}
	}
	body, err := issuemeta.ValidateBody(req.Body)
	if err != nil {
		return Outcome{Message: "rein: " + err.Error(), Audit: AuditNewBadRequest}
	}
	if d.CreateIssue == nil {
		// No filing wiring ⇒ nothing approvable exists. Refuse BEFORE the
		// prompt: asking the human to approve something rein cannot then
		// perform is exactly the rubber-stamp training §2.2 avoids.
		logger.Printf("declare --new: no CreateIssue wired; refusing")
		return Outcome{Message: "rein: filing new issues is not available in this run", Audit: AuditNewBadRequest}
	}

	// resolveRepo's ambiguity message teaches `rein declare <n> --repo`,
	// which is the wrong command here — answer in this path's own terms.
	if req.Repo == "" && len(d.Session.Repos) > 1 {
		return Outcome{Audit: AuditNewBadRequest, Message: fmt.Sprintf(
			"rein: this session scopes multiple repos (%s); say which one to file in:\n"+
				"      rein declare --new \"<title>\" --repo %s",
			strings.Join(d.Session.Repos, ", "), d.Session.Repos[0])}
	}
	repo, expanding, err := resolveRepo(d.Session, req.Repo)
	if err != nil {
		return Outcome{Message: "rein: " + err.Error(), Audit: AuditNewBadRequest}
	}
	if expanding {
		// Deliberately no scope EXPANSION through this path: expansion is
		// approved against a FETCHED issue in the candidate repo (decision
		// E), and a title the agent just made up is not that.
		return Outcome{Repo: repo, Audit: AuditNewBadRequest, Message: fmt.Sprintf(
			"rein: %s is not in this run's scope (%s) — `rein declare --new` cannot widen it.\n"+
				"      Declare an existing issue there first (`rein declare <n> --repo %s`), which asks the human to add the repo.",
			repo, strings.Join(d.Session.Repos, ", "), repo)}
	}

	// Nothing can be filed on a repo the App is not installed on: a 404
	// here becomes the install NOTICE, never a prompt.
	if out, stop := d.checkInstallCoverage(ctx, repo, 0, logger); stop {
		return out
	}

	gcfg := d.Grant
	gcfg.StateDir = d.StateDir
	gcfg.RunID = d.RunID
	gcfg.RunPID = d.RunPID
	if gcfg.Logger == nil {
		gcfg.Logger = logger
	}

	logger.Printf("declare --new: agent proposes filing %q in %s; prompting", title, repo)
	switch grant.ObtainNewIssueApproval(ctx, grant.NewIssueRequest{
		Session: d.Session, Repo: repo, Title: title, Body: body,
	}, gcfg) {
	case grant.NewIssueConfirmed:
		// fall through to the filing below
	case grant.NewIssueBusy:
		return Outcome{Repo: repo, Audit: AuditNewBusy, Message: "rein: another approval is already in front of the human; " +
			"nothing was filed. Retry `rein declare --new` after that one is answered."}
	case grant.NewIssueLimited:
		return Outcome{Repo: repo, Audit: AuditNewLimit, Message: fmt.Sprintf(
			"rein: this run has already asked the human to file %d new issues; no more will be requested.\n"+
				"      Nothing was filed. Use an issue that already exists (`rein declare <n>`), or start a new run.",
			grant.MaxNewIssueCeremonies)}
	default:
		return Outcome{Repo: repo, Audit: AuditNewDenied, Message: fmt.Sprintf(
			"rein: filing a new issue in %s was NOT confirmed (denied, timed out, or no approval surface). Nothing was filed.", repo)}
	}

	meta, err := d.CreateIssue(ctx, repo, title, body)
	if err != nil {
		// The human APPROVED and rein cannot confirm what happened remotely.
		// Do NOT claim nothing was filed: a read timeout, a parse failure, or
		// a numberless response all land here with the issue possibly
		// created. Only the local half is knowable — nothing is confirmed —
		// and the retry can duplicate, so say to look first.
		logger.Printf("declare --new: create in %s FAILED after approval: %v", repo, err)
		if errors.Is(err, ErrBrokerLocal) {
			// A broker-local failure's text names host paths (keystore, App
			// config). The agent gets the fact, the human gets the detail.
			return Outcome{Repo: repo, Audit: AuditNewCreateFailed, Message: fmt.Sprintf(
				"rein: APPROVED, but rein could not file the issue in %s: a broker-side error.\n"+
					"      The detail is on the human's terminal and in rein's log. Nothing is confirmed,\n"+
					"      so writes stay locked; nothing was filed (the failure was before any GitHub call).",
				repo)}
		}
		return Outcome{Repo: repo, Audit: AuditNewCreateFailed, Message: fmt.Sprintf(
			"rein: APPROVED, but rein could not complete filing the issue in %s: %s\n"+
				"      Nothing is confirmed, so writes stay locked. The issue MAY OR MAY NOT have been\n"+
				"      created — check %s before retrying, or a retry can file it twice. If the error\n"+
				"      says the App lacks access, it needs issues:write on %s.\n"+
				"      If the issue IS there, run `rein declare <n>` on it instead.",
			repo, issuemeta.BodyExcerpt(err.Error()), repo, repo)}
	}

	ci := approvals.ConfirmedIssue{
		Number:       meta.Number,
		Repo:         meta.Repo,
		Title:        meta.Title,
		State:        meta.State,
		CanonicalURL: meta.CanonicalURL,
		ConfirmedAt:  time.Now(),
	}
	if err := approvals.AppendConfirmedIssue(d.StateDir, d.RunID, approvals.SignatureOf(d.Session), d.Session.ID, ci, gcfg.TTL); err != nil {
		// The issue EXISTS but writes stay locked. Unlike the declared-issue
		// path (where a lost append just re-prompts), the human's approval
		// cannot be re-derived, so name the recovery command.
		logger.Printf("declare --new: issue #%d filed but recording it FAILED: %v", meta.Number, err)
		return Outcome{Issue: meta.Number, Repo: repo, Audit: AuditNewCreateFailed, Message: fmt.Sprintf(
			"rein: issue #%d WAS filed in %s, but its confirmation could not be recorded (%v).\n"+
				"      Run `rein declare %d` to confirm it.", meta.Number, repo, err, meta.Number)}
	}

	logger.Printf("declare --new: issue #%d filed in %s and confirmed for run %s", meta.Number, repo, d.RunID)
	return Outcome{Confirmed: true, Issue: meta.Number, Repo: repo, Audit: AuditConfirmedNew, Message: fmt.Sprintf(
		"issue #%d filed and confirmed — writes are unlocked (push to agent/%d/<nonce>)", meta.Number, meta.Number)}
}
