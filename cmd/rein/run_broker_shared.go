// Substrate-neutral helpers shared by the broker/declare spine (run_broker.go)
// and the sandbox launch path (run_nono.go). Extracted from the old srt
// run_sandboxed.go at the P3 cutover; none of these depend on any sandbox
// backend, so they live here rather than in a backend-specific file.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/declare"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/runbroker"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
)

// sandboxReadMint / sandboxWriteMint name the two permission TIERS the
// sandbox injecting proxy mints. In sandboxed mode ALL of the agent's github
// traffic — git AND gh/REST/GraphQL — is injected by this one proxy, so both
// tiers must carry the issue/PR capability `gh` needs, not just contents.
//
//   - READ tier  = MintGhReadOnlyToken: contents+issues+pull_requests+metadata,
//     all READ (no write permission — the read/write split holds at the wire).
//   - WRITE tier = MintGhSessionToken:  contents+issues+pull_requests WRITE,
//     metadata read — the implement role (design §4.2.2), so a post-declare
//     `gh pr create` / `gh issue comment` lands instead of 403'ing.
//
// They are method-expression vars (not inline `c.Mint...` calls) precisely so
// a unit test can pin the tier CHOICE and the read/write split without a live
// mint. Changing either back to the contents-only git tier
// (MintReadOnlyToken/MintWriteToken) re-opens the bug where in-sandbox gh
// issue/PR reads and writes fail.
var (
	sandboxReadMint  = (*githubapp.Client).MintGhReadOnlyToken
	sandboxWriteMint = (*githubapp.Client).MintGhSessionToken
)

// buildSandboxApprove returns the run's write gate: writes are allowed only once
// the human has confirmed an issue for this run (via `rein declare <n>`).
func buildSandboxApprove(sess session.Session, stateDir, runID string, logger *log.Logger) func(repo string) bool {
	sig := approvals.SignatureOf(sess)
	return func(repo string) bool {
		if issues := approvals.ConfirmedIssues(stateDir, runID, sig); len(issues) > 0 {
			return true
		}
		logger.Printf("sandbox write gate: no confirmed issue for run %s; denying write to %q (agent must run `rein declare <n>`)", runID, repo)
		return false
	}
}

// declareEnv is everything one run's declare handler needs. Grouped into a
// struct because the #69 scope-expansion path added enough dependencies
// (install probe, deep-link, persist target) that a positional arg list
// stopped being readable.
type declareEnv struct {
	sess        session.Session
	sessionFile string // "" when the session came from the env fallback
	stateDir    string
	runID       string
	approve     func(string) bool
	ghReadToken func(context.Context) (string, error)
	appCfg      githubapp.Config
	scopedCfg   func() githubapp.Config
	ks          keystore.Keystore
	logger      *log.Logger
}

// buildDeclarationHooks wires the proxy's #35 declaration gate for a
// sandboxed run: WriteApproved shares the exact gate closure the broker
// core uses; IssueConfirmed is the push-ref cross-check against the
// run's confirmed set; Declare runs the full fetch + Form A + record
// ceremony OUT of the sandbox (internal/declare), blocking while the
// human decides.
//
// Since issue #69 the Declare hook also carries the SCOPE-EXPANSION path:
// the install-coverage probe (a 404 becomes a notice, never a prompt), the
// deep-link, and the session file an approved-and-persisted repo is written
// to. The same-owner rule is enforced inside internal/declare, before any
// of this.
func buildDeclarationHooks(env declareEnv) *proxy.DeclarationHooks {
	sig := approvals.SignatureOf(env.sess)
	appName, installURL := appInstallHints(env.appCfg)
	return &proxy.DeclarationHooks{
		WriteApproved: env.approve,
		IssueConfirmed: func(repo string, n int) bool {
			rec, err := approvals.ReadApproval(env.stateDir, env.runID)
			return err == nil && approvals.Valid(rec, sig) && rec.HasIssue(repo, n)
		},
		Declare: func(issue int, repoArg string) proxy.DeclareOutcome {
			gcfg := grant.Config{
				TTL:           approvalTTL,
				PromptTimeout: 60 * time.Second,
				PreferPopup:   grant.PopupPreferenceFromEnv(),
				StateDir:      env.stateDir,
				RunID:         env.runID,
				SessionFile:   env.sessionFile,
				Logger:        env.logger,
			}
			out := declare.Run(context.Background(), declare.Deps{
				StateDir:   env.stateDir,
				RunID:      env.runID,
				RunPID:     os.Getpid(),
				Session:    env.sess,
				InstallURL: installURL,
				AppName:    appName,
				Fetch:      env.fetchIssue,
				ProbeInstall: func(ctx context.Context, repo string) error {
					owner, name, _ := strings.Cut(repo, "/")
					_, err := fetchRepoInstallationID(ctx, env.appCfg.ClientID, env.ks, config.AppKeystoreRole, owner, name)
					return err
				},
				Notice: func(ctx context.Context, n declare.Notice) {
					grant.ShowInstallNotice(ctx, gcfg, grant.InstallNotice{
						Repo: n.Repo, Issue: n.Issue, InstallURL: n.InstallURL, AppName: n.AppName,
					})
				},
				Grant:  gcfg,
				Logger: env.logger,
			}, issue, repoArg)
			return proxy.DeclareOutcome{OK: out.Confirmed, Issue: out.Issue, Message: out.Message, Audit: out.Audit}
		},
	}
}

// fetchIssue reads one issue's metadata for the declare prompt.
//
// For a repo already inside the run's ceiling it uses the run's CACHED
// gh-read token (ghsession). For a SCOPE EXPANSION — a repo the human has
// not approved yet — it mints a SEPARATE, short-lived token scoped to the
// session repos PLUS the candidate, uses it for exactly this one GET, and
// revokes it. It is deliberately NOT written to the run's shared read cache:
// if the human then DENIES the expansion, no credential covering the
// candidate repo outlives the prompt, and the agent's own read path (which
// serves from that cache) never widened. See the Deps.Fetch security note.
func (env declareEnv) fetchIssue(ctx context.Context, repo string, number int) (issuemeta.Meta, error) {
	apiBase := os.Getenv("REIN_GITHUB_API_BASE")
	if env.sess.Contains(repo) {
		tok, err := env.ghReadToken(ctx)
		if err != nil {
			return issuemeta.Meta{}, fmt.Errorf("obtain read token for issue fetch: %w", err)
		}
		return issuemeta.Fetch(ctx, apiBase, tok, repo, number)
	}

	cfg := env.appCfg
	cfg.RepoNames = append(env.sess.BareRepoNames(), bareRepoName(repo))
	c, err := githubapp.NewClient(cfg, env.ks, config.AppKeystoreRole)
	if err != nil {
		return issuemeta.Meta{}, err
	}
	mctx, cancel := context.WithTimeout(ctx, mintTimeout)
	tok, _, err := c.MintGhReadOnlyToken(mctx)
	cancel()
	if err != nil {
		return issuemeta.Meta{}, fmt.Errorf("mint candidate-scoped read token for issue fetch: %w", err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), mintTimeout)
		defer rcancel()
		if rerr := c.RevokeToken(rctx, tok); rerr != nil {
			env.logger.Printf("declare: revoke of the candidate-scoped read token failed (it expires on its own): %v", rerr)
		}
	}()
	return issuemeta.Fetch(ctx, apiBase, tok, repo, number)
}

// appInstallHints returns the App's display name and installation
// deep-link, best-effort, for the #69 not-installed notice. The
// manifest-flow state path knows both; the env path (REIN_APP_*) knows
// neither, so it falls back to GitHub's installations page.
func appInstallHints(appCfg githubapp.Config) (name, installURL string) {
	configDir, err := config.ConfigDir()
	if err == nil {
		if st, serr := appsetup.ReadState(configDir); serr == nil && st.Primary != nil {
			name = st.Primary.Slug
			installURL = st.Primary.HTMLURL
			if installURL == "" && name != "" {
				installURL = "https://github.com/apps/" + name
			}
			if installURL != "" {
				return name, installURL + "/installations/new"
			}
		}
	}
	return "", "https://github.com/settings/installations"
}

// auditLogPath returns the per-run audit log path: stateDir/audit/sandbox-<runID>.log.
//
// INVARIANT (design §6, audit #44 §2): this path MUST stay under stateDir, which
// nono's default-deny filesystem keeps unreadable in-sandbox — that containment
// is the only thing keeping the sandboxed agent from reading (and grooming its
// behavior against) its own audit trail. If the audit log ever moves, it must
// move to another location the sandbox cannot read.
func auditLogPath(stateDir, runID string) string {
	return filepath.Join(stateDir, "audit", "sandbox-"+runID+".log")
}

// printExpiryBanner is the loud notice printed when a run's broker/proxy is torn
// down early (idle or hard-TTL): the agent keeps running but can no longer reach
// GitHub.
func printExpiryBanner(w io.Writer, reason string) {
	var why string
	switch reason {
	case "idle":
		why = fmt.Sprintf("no proxy activity for %s (idle timeout)", runbroker.DefaultIdleTimeout)
	case "hard-ttl":
		why = fmt.Sprintf("run exceeded the %s hard limit", runbroker.DefaultHardTTL)
	default:
		why = reason
	}
	fmt.Fprintln(w, "\n===============================================================")
	fmt.Fprintf(w, "rein: SESSION EXPIRED — %s.\n", why)
	fmt.Fprintln(w, "  Revoked this run's write tokens and STOPPED the credential proxy.")
	fmt.Fprintln(w, "  The agent is still running but can no longer reach GitHub — its")
	fmt.Fprintln(w, "  git/gh requests will now fail. Exit it and re-run `rein run` to")
	fmt.Fprintln(w, "  continue with a fresh, re-authorized session.")
	fmt.Fprintln(w, "===============================================================")
}

// contractStatus renders the agent-contract line for the launch banner: whether
// the agent was told $HOME is ephemeral, credentials are absent, and how to
// declare its issue.
func contractStatus(off, injected bool) string {
	switch {
	case off:
		return "  WARNING: agent contract DISABLED (" + EnvDisableAgentContract + ") — the agent was NOT told that $HOME is\n    ephemeral, that credentials are absent, or how to declare its issue. It will find out by failing."
	case injected:
		return "  agent contract injected via --append-system-prompt (claude): $HOME is ephemeral, no creds, declare-then-push."
	default:
		return "  agent contract PRINTED to the agent's output (this agent has no system-prompt channel, so it may or\n    may not reach the model's context; the REIN_IN_SANDBOX_* env vars carry the same facts machine-readably)."
	}
}
