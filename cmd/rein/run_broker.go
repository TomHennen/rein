// Shared broker/proxy start path for the sandboxed run modes (srt + nono).
//
// The mint/scope/declare/approval brains are IDENTICAL across sandbox backends —
// only the proxy FRONT (srt unix socket vs. nono loopback TCP) and the launch
// mechanics differ. startRunBroker centralizes that shared spine so the two run
// paths cannot drift on the security-critical logic (scope ceiling, read/write
// tiers, the #35 declare gate, the write-approval hook, expiry+revoke).
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/declare"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/ruleset"
	"github.com/TomHennen/rein/internal/runbroker"
	"github.com/TomHennen/rein/internal/runscope"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/tokencache"
)

// runBrokerParams carries the substrate-neutral inputs both run paths feed into
// runbroker.Start. The per-backend bits — the socket placement (forbiddenDirs)
// and whether to also bind the loopback TCP front — are set by the caller.
type runBrokerParams struct {
	sess       session.Session
	sessSource string // session.LoadOrFallback's source tag (for the declare hook's file path)
	appCfg     githubapp.Config
	ks         keystore.Keystore
	caKeystore keystore.Keystore
	stateDir   string
	runID      string

	socketPath    string
	forbiddenDirs []string // socket-placement guard (srt bind-mounts); empty is fine for nono
	loopbackFront bool     // also bind the 127.0.0.1 HTTP-CONNECT front (nono)

	auditW io.Writer
	logger *log.Logger
}

// startRunBroker builds the per-run scope resolver + mint closures + declare
// gate + approval hook and starts the in-process broker/proxy. It is the ONE
// place both run paths mint tokens, so the read/write tier split, the scope
// ceiling (#69), the #35 declaration gate, and the exit-time revoke behave
// identically under srt and nono. Returns the Host the caller closes on exit.
func startRunBroker(p runBrokerParams) (*runbroker.Host, error) {
	// The run's EFFECTIVE scope ceiling (#69): standing session repos UNION the
	// repos the human approves as expansions during this run. Every
	// scope-sensitive surface below reads through it.
	rscope := runscope.New(p.sess, p.stateDir, p.runID)
	// scopedAppCfg re-scopes the App config to the ceiling AS OF THIS MINT — a
	// mint after an approved expansion must cover the wider set.
	scopedAppCfg := func() githubapp.Config {
		c := p.appCfg
		c.RepoNames = rscope.BareNames()
		return c
	}
	mintRead := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		c, err := githubapp.NewClient(scopedAppCfg(), p.ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		// gh-shaped read tier (contents+issues+pull_requests+metadata, all READ):
		// in sandboxed mode ALL github traffic — git AND gh/REST/GraphQL — flows
		// through this one injecting proxy, so the read token backs issue/PR reads
		// too. TIER SPLIT PRESERVED: no write permission.
		return sandboxReadMint(c, ctx)
	})
	// ghReadToken: the issues:read-capable token the declare fetch + the TM-G6
	// transfer re-check use, cached on disk (scope-tagged by the effective
	// ceiling, #95) so repeated declares don't burn mints.
	ghReadToken := func(ctx context.Context) (string, error) {
		c, err := githubapp.NewClient(scopedAppCfg(), p.ks, config.AppKeystoreRole)
		if err != nil {
			return "", err
		}
		tok, _, err := ghsession.EnsureFresh(ghsession.ReadCachePathForScope(p.stateDir, rscope.Key()), c.MintGhReadOnlyToken, c.RevokeToken, 5*time.Minute, mintTimeout, p.logger)
		return tok, err
	}
	mintWrite := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		// TM-G6 re-check on EVERY write-token mint (#35 §6): a confirmed issue
		// whose canonical URL now 3xx's was transferred — its confirmation is
		// invalidated; an emptied set fails the mint (agent must re-declare).
		if err := declare.InvalidateTransferred(ctx, p.stateDir, p.runID, p.sess, ghReadToken, p.logger, os.Stderr); err != nil {
			return "", time.Time{}, err
		}
		c, err := githubapp.NewClient(scopedAppCfg(), p.ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		// implement-role write tier (contents+issues+pull_requests WRITE, metadata
		// read): backs git push AND gh/API issue-or-PR writes. Scope ceiling still
		// confines it; the declare gate still gates it. Known residual #86/#6.
		return sandboxWriteMint(c, ctx)
	})

	// Server-authoritative `agent/**` branch floor (decision-rein-broker.md):
	// install the GitHub rulesets that bind the per-run push token BEFORE the
	// agent can push. Fail closed (hard-constraint #3) — a run must not proceed
	// if GitHub can't be made to enforce the floor.
	if err := ensureBranchFloor(context.Background(), scopedAppCfg(), rscope, p); err != nil {
		return nil, err
	}

	approve := buildSandboxApprove(p.sess, p.stateDir, p.runID, p.logger)
	revoke := productionRevoke(p.sess)

	return runbroker.Start(runbroker.Config{
		SessionID:     p.sess.ID,
		SocketPath:    p.socketPath,
		ForbiddenDirs: p.forbiddenDirs,
		LoopbackFront: p.loopbackFront,
		MintRead:      mintRead,
		MintWrite:     mintWrite,
		InScope:       rscope.Contains,
		ScopeKey:      rscope.Key,
		Approve:       approve,
		Declaration: buildDeclarationHooks(declareEnv{
			sess:        p.sess,
			sessionFile: session.SourceFilePath(p.sessSource),
			stateDir:    p.stateDir,
			runID:       p.runID,
			approve:     approve,
			ghReadToken: ghReadToken,
			appCfg:      p.appCfg,
			scopedCfg:   scopedAppCfg,
			ks:          p.ks,
			logger:      p.logger,
		}),
		RecordWrite: func(token string, expiresAt time.Time) {
			if err := approvals.AppendWriteToken(p.stateDir, p.runID, tokencache.Entry{Token: token, ExpiresAt: expiresAt}); err != nil {
				p.logger.Printf("write-token ledger append failed (best-effort): %v", err)
			}
		},
		CAKeystore:  p.caKeystore,
		Audit:       p.auditW,
		Logger:      p.logger,
		IdleTimeout: runbroker.DefaultIdleTimeout,
		HardTTL:     runbroker.DefaultHardTTL,
		OnExpire: func(reason string) {
			p.logger.Printf("session expired (%s): revoking write tokens and stopping the proxy", reason)
			revokeRunWriteTokens(p.stateDir, p.runID, revoke, time.Now())
			// Clear the ledger now so the deferred exit-time revoke reads empty and
			// is a clean no-op. A write approved in the brief pre-stop window
			// re-appends and is caught by that deferred exit-time revoke.
			if err := approvals.ClearRun(p.stateDir, p.runID); err != nil {
				p.logger.Printf("expiry: clear write-token ledger failed (best-effort): %v", err)
			}
			printExpiryBanner(os.Stderr, reason)
		},
	})
}

// ensureBranchFloor installs/verifies the server-side `agent/**` branch-floor
// rulesets on every repo in the run's initial ceiling, BEFORE the broker
// starts. It mints a short-lived administration:write token (host-side only,
// never injected) and revokes it immediately after. Fails closed: any error
// aborts the run so the agent cannot push against an unenforced repo.
//
// The admin token is the ONLY place administration is minted; the per-run PUSH
// tokens never carry it (see githubapp.MintAdminToken), so a captured push
// token can never delete the ruleset binding it.
//
// LIMITATION: only the INITIAL ceiling is covered here. A repo added mid-run
// via `rein declare` (scope expansion) is NOT floored — the client-side parser
// still backstops it this cycle. Flooring scope-expansion repos is a
// prerequisite for dropping the parser (tracked as future work).
func ensureBranchFloor(ctx context.Context, cfg githubapp.Config, rscope *runscope.Resolver, p runBrokerParams) error {
	owner := session.OwnerOf(p.sess)
	if owner == "" {
		return fmt.Errorf("cannot install the agent/** branch-floor ruleset: the session has no owner")
	}
	repos := rscope.BareNames()
	if len(repos) == 0 {
		return fmt.Errorf("cannot install the agent/** branch-floor ruleset: the session has no repositories")
	}

	c, err := githubapp.NewClient(cfg, p.ks, config.AppKeystoreRole)
	if err != nil {
		return err
	}
	mctx, cancel := context.WithTimeout(ctx, mintTimeout)
	tok, _, err := c.MintAdminToken(mctx)
	cancel()
	if err != nil {
		return fmt.Errorf("mint administration token for the branch-floor ruleset failed "+
			"(the GitHub App must be granted 'Administration: write'; if you recently updated rein, re-approve the App's new permission): %w", err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), mintTimeout)
		defer rcancel()
		if rerr := c.RevokeToken(rctx, tok); rerr != nil {
			p.logger.Printf("branch-floor: revoke of the admin token failed (it expires on its own): %v", rerr)
		}
	}()

	apiBase := os.Getenv("REIN_GITHUB_API_BASE")
	for _, name := range repos {
		ectx, ecancel := context.WithTimeout(ctx, mintTimeout)
		err := ruleset.Ensure(ectx, http.DefaultClient, apiBase, tok, owner, name)
		ecancel()
		if err != nil {
			return fmt.Errorf("install the agent/** branch-floor ruleset on %s/%s: %w", owner, name, err)
		}
		p.logger.Printf("branch-floor: agent/** ruleset active on %s/%s", owner, name)
	}
	return nil
}
