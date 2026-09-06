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
)

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
	if !grant.ObtainNewIssueApproval(ctx, grant.NewIssueRequest{
		Session: d.Session, Repo: repo, Title: title, Body: body,
	}, gcfg) {
		return Outcome{Repo: repo, Audit: AuditNewDenied, Message: fmt.Sprintf(
			"rein: filing a new issue in %s was NOT confirmed (denied, timed out, or no approval surface). Nothing was filed.", repo)}
	}

	meta, err := d.CreateIssue(ctx, repo, title, body)
	if err != nil {
		// The human APPROVED and nothing exists. Say both halves plainly —
		// this is the one path where a silent failure would leave the human
		// believing they filed something.
		logger.Printf("declare --new: create in %s FAILED after approval: %v", repo, err)
		return Outcome{Repo: repo, Audit: AuditNewCreateFailed, Message: fmt.Sprintf(
			"rein: APPROVED, but the issue could NOT be created in %s: %s\n"+
				"      Nothing was filed and nothing is confirmed. (If this says the App lacks access,\n"+
				"      the GitHub App needs issues:write on %s.) Retry `rein declare --new`.",
			repo, issuemeta.BodyExcerpt(err.Error()), repo)}
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
