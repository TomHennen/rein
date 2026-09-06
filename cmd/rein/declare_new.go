// declare_new.go — the host-side half of `rein declare --new` (issue
// #180): filing the issue the human just approved.
//
// This is the ONLY place rein creates an issue, and it runs OUT of the
// sandbox in both modes. The token is minted here, used for one POST, and
// revoked; it is never cached, never ledgered (the ledger exists for
// cross-process revoke of tokens the agent holds via injection — this one
// never leaves the broker), and never crosses back into the sandbox.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/tokencache"
)

// createIssueFunc returns the declare.Deps.CreateIssue hook. cfg is
// resolved per call so a run-scoped config (sandboxed mode) is picked up
// as it stands at filing time; owner names the human the issue is filed on
// behalf of.
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
func createIssueFunc(cfg func() githubapp.Config, ks keystore.Keystore, owner, stateDir, runID string, logger *log.Logger) func(context.Context, string, string, string) (issuemeta.Meta, error) {
	return func(ctx context.Context, repo, title, body string) (issuemeta.Meta, error) {
		c, err := githubapp.NewClient(cfg(), ks, config.AppKeystoreRole)
		if err != nil {
			return issuemeta.Meta{}, err
		}
		mctx, cancel := context.WithTimeout(ctx, mintTimeout)
		token, expiresAt, err := c.MintIssueWriteToken(mctx)
		cancel()
		if err != nil {
			return issuemeta.Meta{}, fmt.Errorf("mint issues:write token: %w", err)
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
		return issuemeta.Create(ctx, os.Getenv("REIN_GITHUB_API_BASE"), token, repo, title, newIssueBody(body, owner))
	}
}

// newIssueBody appends rein's attribution trailer. The issue is filed
// under the App's bot identity, so the body has to say whose work it is.
func newIssueBody(body, owner string) string {
	trailer := "_Filed by rein on behalf of the operator._"
	if owner != "" {
		trailer = "_Filed by rein on behalf of @" + owner + "._"
	}
	if strings.TrimSpace(body) == "" {
		return trailer
	}
	return strings.TrimRight(body, "\n") + "\n\n" + trailer
}
