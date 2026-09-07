// declare_new.go — the host-side half of `rein declare --new` (issue
// #180): filing the issue the human just approved.
//
// This is the ONLY place rein creates an issue, and it runs OUT of the
// sandbox in both modes. The token is minted here, used for one POST, and
// revoked; it is never cached and never crosses back into the sandbox. It
// IS ledgered, so a broker that dies before the deferred revoke still has
// it revoked by the run-exit drain.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/declare"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/tokencache"
)

// createIssueFunc returns the declare.Deps.CreateIssue hook. cfg is
// resolved per call so a run-scoped config (sandboxed mode) is picked up
// as it stands at filing time; the returned hook then narrows it to the
// one repo being filed in.
//
// The tier is MintIssueWriteToken — issues:write + metadata:read, and
// nothing else. MintWriteToken (contents only) 403s on POST /issues, and
// MintGhSessionToken would work but also carries contents:write and
// pull_requests:write across the session scope: a push-and-merge capable
// credential minted for a ceremony the human approved as "file an issue".
//
// stateDir/runID, when set, ledger the token (approvals.AppendWriteToken)
// so a broker that dies between the mint and the deferred revoke still has
// it revoked by the run-exit drain. Without that, a crash strands a live
// token for its native ~1h with nothing tracking it.
func createIssueFunc(cfg func() githubapp.Config, ks keystore.Keystore, stateDir, runID string, logger *log.Logger) func(context.Context, string, string, string) (issuemeta.Meta, error) {
	return func(ctx context.Context, repo, title, body string) (issuemeta.Meta, error) {
		// Scope the token to the TARGET repo alone, not the session ceiling.
		// The human approved filing one issue in one repo; a token covering
		// every repo in scope is capability they were never shown. (The
		// Fetch hook narrows per call the same way.)
		scoped := cfg()
		scoped.RepoNames = []string{bareRepoName(repo)}
		c, err := githubapp.NewClient(scoped, ks, config.AppKeystoreRole)
		if err != nil {
			return issuemeta.Meta{}, fmt.Errorf("%w: build App client: %v", declare.ErrBrokerLocal, err)
		}
		mctx, cancel := context.WithTimeout(ctx, mintTimeout)
		token, expiresAt, err := c.MintIssueWriteToken(mctx)
		cancel()
		if err != nil {
			// Names the keystore/App config on failure — host-side only.
			logger.Printf("declare --new: mint of the issues:write token failed: %v", err)
			return issuemeta.Meta{}, fmt.Errorf("%w: mint issues:write token: %v", declare.ErrBrokerLocal, err)
		}
		if stateDir != "" && runID != "" {
			if lerr := approvals.AppendWriteToken(stateDir, runID, tokencache.Entry{Token: token, ExpiresAt: expiresAt}); lerr != nil {
				logger.Printf("declare --new: write-token ledger append failed (best-effort): %v", lerr)
			}
		}
		defer func() {
			rctx, rcancel := context.WithTimeout(context.Background(), mintTimeout)
			defer rcancel()
			if rerr := c.RevokeToken(rctx, token); rerr != nil {
				logger.Printf("declare --new: revoke of the issue-filing token failed (it expires on its own): %v", rerr)
			}
		}()
		return issuemeta.Create(ctx, os.Getenv("REIN_GITHUB_API_BASE"), token, repo, title, newIssueBody(body))
	}
}

// newIssueBody appends rein's attribution trailer. The issue is filed
// under the App's bot identity, so the body has to say it was filed on a
// human's behalf rather than by a bot of its own accord.
//
// It deliberately @-mentions NOBODY. rein knows the repo OWNER (which for
// an org repo is the org, not a person) but never learns the operator's
// GitHub login, so any @mention it could construct would be wrong — and a
// wrong mention on a public issue notifies the wrong account.
func newIssueBody(body string) string {
	const trailer = "_Filed by rein on behalf of the operator of this session._"
	if strings.TrimSpace(body) == "" {
		return trailer
	}
	return strings.TrimRight(body, "\n") + "\n\n" + trailer
}
