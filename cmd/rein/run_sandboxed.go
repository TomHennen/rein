// `rein run --sandbox -- <agent argv>` — the CP3 sandboxed run path.
//
// Unlike direct mode (run.go), the agent runs INSIDE srt and never sees the
// git-credential shim or a real token. rein hosts the injecting proxy + broker
// core in THIS process (out of the sandbox, the CP2 in-process pivot); srt
// routes the three inject hosts to rein's per-run unix socket, where rein
// TLS-terminates and injects the credential. The token is never in the
// sandbox's env, filesystem, or memory.
//
// Fail-closed spine (the CP3 crux):
//   - Preflight hard-gates srt presence+version, bwrap userns, seccomp, and a
//     valid system CA bundle (SSL_CERT_FILE replaces roots in-sandbox). Any
//     hard failure refuses to launch (no silent drop to unsandboxed mode).
//   - VerifyConfigApplied actually launches srt with a probe and proves BOTH
//     srt fail-opens are closed (denyRead applied via a content-sentinel;
//     AF_UNIX socket creation blocked) BEFORE the agent runs.
//   - The exec environment is an explicit allowlist (env -i equivalent), so no
//     ambient secret reaches the agent.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/declare"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/issuemeta"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/runbroker"
	"github.com/TomHennen/rein/internal/runscope"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/internal/tokencache"
	"github.com/TomHennen/rein/internal/ui/grant"
	"github.com/TomHennen/rein/internal/worktree"
)

// stubGHToken is the non-secret placeholder GH_TOKEN inside the sandbox. gh
// reads/writes are injected by rein's proxy, so the value is never used for
// auth; it only stops gh from prompting or reaching the host keyring on a
// GH_TOKEN-absent code path. Deliberately not a real token.
const stubGHToken = "x-access-token-rein-sandbox-stub"

// verifyTimeout caps the pre-launch VerifyConfigApplied srt spawn.
const verifyTimeout = 30 * time.Second

// runSandboxed is the entry point for `rein run --sandbox -- <cmd>`. cmdline is
// the agent argv (after "--"). Returns (exitCode, error) like runWrapped.
func runSandboxed(cmdline []string) (int, error) {
	logger, closeLog, err := openLog()
	if err != nil {
		return 1, err
	}
	defer closeLog()

	// (1) PREFLIGHT — hard-gate srt/bwrap/seccomp. Fail closed, loud, with the
	// exact fix. No silent fallback to unsandboxed mode (design §2-3).
	pf := srt.Preflight(srt.DefaultEnv())
	if !printPreflightAndOK(os.Stderr, pf) {
		return 1, fmt.Errorf("sandbox preflight failed — refusing to launch. Run `rein doctor` for the fix. " +
			"(Fail-closed: rein does NOT silently fall back to unsandboxed mode on a real repo. " +
			"For a throwaway repo you can opt in explicitly with `rein run --direct -- <cmd>`.)")
	}
	srtPath := preflightSrtPath(pf)

	// (2) Session + scope.
	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return 1, fmt.Errorf("load session: %w", err)
	}
	config.WarnPartialAppEnv(os.Stderr)

	// (3) App config + eager install-id resolve (fail loud here, not inside the
	// sandbox's first git op). Then build the mint closures.
	appCfg, ks, appSource, err := config.ResolveApp()
	if err != nil {
		return 1, fmt.Errorf("resolve App config: %w", err)
	}
	appCfg.RepoNames = sess.BareRepoNames()
	ctx, cancel := context.WithTimeout(context.Background(), installIDTimeout)
	err = resolveAndCacheInstallID(ctx, sess, newAppInstallProber)
	cancel()
	if err != nil {
		return 1, err
	}
	// ResolveApp on the state path left InstallationID possibly stale; re-resolve
	// so the mint closures see the cached id written by resolveAndCacheInstallID.
	if appSource == config.SourceState {
		appCfg, ks, _, err = config.ResolveApp()
		if err != nil {
			return 1, fmt.Errorf("re-resolve App config: %w", err)
		}
		appCfg.RepoNames = sess.BareRepoNames()
	}

	stateDir, err := config.StateDir()
	if err != nil {
		return 1, err
	}

	// CA keystore: a DEDICATED writable FileKeystore for the proxy CA cert+key
	// (constraint #6). It must NOT be the App-key keystore returned by
	// ResolveApp — on the env path that is a read-only SingleFileKeystore over
	// the App PEM, so LoadOrCreateCA's first-run Set would fail. The CA lives in
	// ConfigDir alongside the App key and is denyRead'd in-sandbox; trust is
	// delivered to the agent via the CA bundle, never this file.
	configDir, err := config.ConfigDir()
	if err != nil {
		return 1, err
	}
	caKeystore := keystore.NewFileKeystore(configDir)

	// Best-effort sweep of orphaned per-run approval/run-context/write-token
	// ledger files from runs whose owning `rein run` is gone. This is the
	// backstop for the uncatchable/untrapped exits: terminal SIGINT (Ctrl-C) is
	// deliberately NOT trapped (only SIGTERM is forwarded, below) so it reaches
	// this process via the shared process group and kills it under the default
	// disposition — the deferred revoke/ClearRun do NOT fire on that path, nor
	// on SIGKILL. Both are covered here: the next launch's Sweep detects the
	// dead owning pid and reaps its files (including the plaintext write-token
	// ledger). Same division as direct mode (run.go); parity via Sweep is why we
	// don't add SIGINT to the Notify set. Non-fatal: a sweep hiccup must never
	// block a launch.
	if err := approvals.Sweep(stateDir, 24*time.Hour, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "rein: warning: orphan approval sweep: %v\n", err)
	}

	// (4) Per-run identity + teardown ledger (shared with the ledger the
	// broker's RecordWrite appends to and the exit-time revoke drains).
	runID, err := newRunID()
	if err != nil {
		return 1, fmt.Errorf("generate run id: %w", err)
	}

	// (5) Working tree + writable dirs (the srt bind-mounts). Default to the cwd;
	// REIN_SANDBOX_WORKDIR overrides. The socket must live OUTSIDE all of these.
	workTree := os.Getenv("REIN_SANDBOX_WORKDIR")
	if workTree == "" {
		workTree, err = os.Getwd()
		if err != nil {
			return 1, fmt.Errorf("resolve working tree: %w", err)
		}
	}
	workTree, err = filepath.Abs(workTree)
	if err != nil {
		return 1, err
	}
	// Symlink-resolve the working tree BEFORE it is used anywhere (banner,
	// ForbiddenDirs, srt config): a symlinked REIN_SANDBOX_WORKDIR pointing
	// into a denied path must be seen in resolved form by every check (audit
	// finding D6, #44). srt.Build re-resolves as the enforcement backstop.
	workTree, err = proxy.ResolveAbs(workTree)
	if err != nil {
		return 1, fmt.Errorf("resolve working tree symlinks: %w", err)
	}

	// (6) Per-run runtime dir for the proxy socket — user-writable, NOT under the
	// working tree, and itself denyRead'd in-sandbox (defense-in-depth). Prefer
	// $XDG_RUNTIME_DIR.
	runtimeBase := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeBase == "" {
		runtimeBase = stateDir // fallback: state dir is 0700 and user-owned
	}
	socketDir := filepath.Join(runtimeBase, "rein", "run-"+runID)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return 1, fmt.Errorf("create runtime socket dir: %w", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "proxy.sock")

	// (7) denyRead set: ambient credential stores + rein's own key/state dirs +
	// the runtime socket dir + /run/user/<uid>. Missing paths are harmless.
	// FAIL CLOSED if the home stores can't be enumerated: launching with an
	// incomplete denyRead would expose ~/.ssh etc., and neither Validate (which
	// checks structure, not completeness) nor the /tmp-sentinel self-test would
	// catch it — the banner would falsely claim the stores are hidden.
	denyStores, err := credentialDenyReadPaths(stateDir)
	if err != nil {
		return 1, fmt.Errorf("assemble credential deny-read set: %w (refusing to launch — an incomplete deny-read would expose credential stores)", err)
	}
	runtimeDeny := []string{socketDir}
	if ru := runUserDir(); ru != "" {
		runtimeDeny = append(runtimeDeny, ru)
	}

	// (7c) Per-run WRITABLE scratch dir for the agent (the sandboxed child's
	// TMPDIR). srt's getDefaultWritePaths lists /tmp/claude and sets the child's
	// TMPDIR there, but bwrap SKIPS an allowWrite source that does not exist on
	// the host (linux-sandbox-utils.js) and srt never creates /tmp/claude — so the
	// child's TMPDIR lands under the read-only root bind and its first temp write
	// fails EROFS (claude, npm, builds, …). rein instead creates a per-run dir,
	// binds it writable via ExtraAllowWrite, and points the child's TMPDIR at it
	// through srt's CLAUDE_CODE_TMPDIR lever (BuildEnv). It is ephemeral (0700,
	// torn down on exit), agent-scoped, holds nothing sensitive, and sits OUTSIDE
	// every denyRead credential path — a writable temp is not a leak surface. It
	// is deliberately NOT runTmp (which holds the probe binary + settings.json):
	// the agent must not be able to tamper with those.
	agentTmp, err := os.MkdirTemp("", "rein-agent-tmp-*")
	if err != nil {
		return 1, fmt.Errorf("create agent temp dir: %w", err)
	}
	defer os.RemoveAll(agentTmp)

	// (7b) EXTRA egress allowlist (CP4.5). Merge the built-in default (the wrapped
	// agent's own API endpoint, so `rein run -- claude` works out of the box), the
	// machine-wide REIN_ALLOW_DOMAINS, and the session's allow_domains into one
	// list of hosts that are egress-allowed but NEVER injected (no rein token —
	// they get a direct TLS tunnel to their real endpoint). Broad egress is a
	// data-exfiltration surface, so warnings (wildcards / large sets) are printed
	// LOUDLY. A malformed entry fails the launch closed.
	extraDomains, egressWarnings, err := srt.ResolveExtraAllowedDomains(sess.AllowDomains, os.Getenv(srt.EnvAllowDomains))
	if err != nil {
		return 1, fmt.Errorf("resolve extra allowed egress domains: %w", err)
	}
	for _, wmsg := range egressWarnings {
		fmt.Fprintf(os.Stderr, "rein: EGRESS WARNING: %s\n", wmsg)
	}

	// (7e) The developer's EXISTING local checkouts (issue #64). The working
	// tree's repo is AUTODETECTED from its git remote (mocks §3: "detect the
	// repo the user is standing in"); the session's `worktrees:` map (plus the
	// REIN_WORKTREES per-run override) names where the session's OTHER repos are
	// checked out, and each mapped tree is bound READ-WRITE below via
	// ExtraAllowWrite — so the agent edits the developer's real tree, not an
	// ephemeral clone. Fail closed on every mismatch (worktree.Resolve): the
	// path must exist, be a git checkout, and its `origin` remote must really be
	// the mapped repo, which must be in the session's scope ceiling.
	//
	// This can only ever cover repos known at LAUNCH. A repo approved MID-RUN
	// gets no bind (bwrap binds are fixed at launch) — it clones into agentTmp,
	// advertised to the agent as REIN_EPHEMERAL_CLONE_DIR.
	homeForGuard, _ := os.UserHomeDir()
	if homeForGuard != "" {
		if r, rerr := proxy.ResolveAbs(homeForGuard); rerr == nil {
			homeForGuard = r
		}
	}
	wt, err := worktree.Resolve(worktree.Params{
		SessionRepos: sess.Repos,
		FileMap:      sess.Worktrees,
		EnvValue:     os.Getenv(worktree.EnvWorktrees),
		WorkTree:     workTree,
		Home:         homeForGuard,
	})
	if err != nil {
		return 1, fmt.Errorf("resolve local checkouts (worktrees): %w", err)
	}
	for _, w := range wt.Warnings {
		fmt.Fprintf(os.Stderr, "rein: worktrees: %s\n", w)
	}
	worktreeWrites := make([]string, 0, len(wt.Bindings))
	for _, b := range wt.Bindings {
		worktreeWrites = append(worktreeWrites, b.Path)
	}

	// (7f) Decide how each writable checkout's `.git` is protected against the
	// rename-parent host-exec escape (internal/srt/githard.go). The AUTODETECTED
	// cwd and the EXPLICITLY-MAPPED worktrees are treated differently on the
	// unhardenable path, on purpose:
	//
	//   - a MAPPED worktree the human named that cannot be hardened (submodules,
	//     or a linked worktree whose `.git` is a file) FAILS THE LAUNCH CLOSED —
	//     they chose that tree deliberately, so an actionable error is right; and
	//   - the cwd the developer merely happens to be STANDING IN falls back to an
	//     EPHEMERAL working tree rather than locking them out (a submodule
	//     superproject / linked worktree is common). rein does NOT bind the real
	//     tree WRITABLE; the agent gets a fresh, empty, writable scratch tree,
	//     clones its repo into it, and pushes — the durable artifact is the push,
	//     not the local tree (mocks §7). The real tree cannot be MODIFIED (so the
	//     .git host-exec escape is closed and uncommitted work is safe); it stays
	//     readable read-only if it sits outside $HOME, and hidden if under it.
	//
	// REIN_SANDBOX_ALLOW_UNHARDENED_GIT=1 is the informed opt-in: bind the real
	// tree(s) writable with partial (top-level-only) hardening and warn loudly.
	optInUnhardened := srt.AllowUnhardenedGitFromEnv(os.Getenv(srt.EnvSandboxAllowUnhardenedGit))

	// MAPPED worktrees: an unhardenable one fails the launch closed (unless opt-in).
	if gaps := srt.AssessGitHardening(worktreeWrites); len(gaps) > 0 {
		if optInUnhardened {
			printUnhardenedGitWarning(os.Stderr, gaps)
		} else {
			return 1, unhardenedMappedGitError(gaps)
		}
	}

	// The cwd: ephemeral-clone fallback when it cannot be hardened (unless opt-in).
	// On the fallback path we REDIRECT workTree to a fresh scratch dir so every
	// downstream step (the $HOME punch-out `writables`, the srt WorkingTree bind,
	// the socket ForbiddenDirs, the banner, REIN_IN_SANDBOX_WORKTREE) uses the
	// ephemeral tree uniformly and the real cwd is never bound.
	cwdEphemeral := false
	ephemeralCwdPath := "" // the real cwd we DECLINED to bind (for the banner/contract)
	cwdRepo := wt.WorkTreeRepo
	if gaps := srt.AssessGitHardening([]string{workTree}); len(gaps) > 0 {
		if optInUnhardened {
			printUnhardenedGitWarning(os.Stderr, gaps)
		} else {
			ephemeralWork, err := os.MkdirTemp("", "rein-ephemeral-work-*")
			if err != nil {
				return 1, fmt.Errorf("create ephemeral working tree: %w", err)
			}
			defer os.RemoveAll(ephemeralWork)
			resolvedEphemeral, err := proxy.ResolveAbs(ephemeralWork)
			if err != nil {
				return 1, fmt.Errorf("resolve ephemeral working tree: %w", err)
			}
			logger.Printf("cwd checkout %q is not fully hardenable (%s); NOT binding the real tree — falling back to an ephemeral working tree %q (agent clones + pushes; real tree unexposed)",
				workTree, gaps[0].Reason, resolvedEphemeral)
			ephemeralCwdPath = workTree
			workTree = resolvedEphemeral
			cwdEphemeral = true
		}
	}

	// (7d) Wholesale $HOME deny + allow-back set (issue #59, DEFAULT-ON). The
	// targeted denyStores above stay layered as belt-and-suspenders; hiding
	// $HOME closes the unknown-unknown credential-store class structurally.
	// REIN_SANDBOX_SHOW_HOME=1 is the loud kill switch; REIN_SANDBOX_ALLOW_READ
	// adds narrow allow-backs. Both are surfaced in the banner so a
	// broken-path discovery loop is self-serve. All decision logic lives in
	// deriveHomeDenial (sandbox_home.go) — pure and unit-tested, so a
	// regression here (e.g. an inverted kill-switch branch) fails the suite,
	// not just a live run. FAIL CLOSED on a home dir or env value we can't
	// interpret — guessing would either brick the run confusingly or silently
	// expose the home tree.
	home, _ := os.UserHomeDir() // "" on error; deriveHomeDenial fails closed when it is actually needed
	// Every read-WRITE bind, so no allow-back can ro-bind over one of them and
	// abort the launch (#63). agentTmp is an ExtraAllowWrite and lands under
	// $HOME whenever TMPDIR does, so it belongs here too — not just the work tree.
	// Symlink-resolved on both sides or the ancestry comparison silently misses
	// (workTree is already resolved above; agentTmp comes from MkdirTemp, whose
	// TMPDIR may itself be a symlink).
	resolvedAgentTmp, err := proxy.ResolveAbs(agentTmp)
	if err != nil {
		return 1, fmt.Errorf("resolve agent scratch dir: %w", err)
	}
	// Every read-WRITE bind: work tree, agent scratch, AND the #64 mapped local
	// checkouts. Any allow-back that is an ancestor of one would ro-bind over it
	// and abort the launch (#63), so the punch-out must see them all.
	writables := append([]string{workTree, resolvedAgentTmp}, worktreeWrites...)
	homeDeny, allowReadPaths, showHome, err := deriveHomeDenial(
		os.Getenv(srt.EnvSandboxShowHome), os.Getenv(srt.EnvSandboxAllowRead),
		home, srtPath, cmdline, writables)
	if err != nil {
		return 1, err
	}
	if showHome {
		printShowHomeWarning(os.Stderr)
		if strings.TrimSpace(os.Getenv(srt.EnvSandboxAllowRead)) != "" {
			fmt.Fprintf(os.Stderr, "rein: note: %s is IGNORED while %s=1 ($HOME is fully visible anyway).\n",
				srt.EnvSandboxAllowRead, srt.EnvSandboxShowHome)
		}
	}

	// (8) Build + validate the srt config (typed struct; no hand-rolled JSON).
	// baseParams is shared by BOTH the VerifyConfigApplied probe and the real
	// agent launch, so both see the identical allowlist (including the extras).
	//
	// (7f) Harden the writable checkouts' `.git` against the rename-parent
	// escape. Binding a checkout writable (working tree + #64 worktrees)
	// necessarily includes its `.git`, whose hooks/ + config are host-code-
	// execution surfaces. srt ro-binds those by PATH, but a prompt-injected
	// agent frees the path with `mv .git .aside` (the ro-binds follow the
	// rename) and rebuilds a malicious `.git`. Pinning each `.git` as its own
	// mountpoint makes that rename fail EBUSY; rein also lists each one's
	// hooks/config in denyWrite so the protection covers the worktrees srt's
	// CWD-scoped scan misses. See srt.Params.WritableGitDirs for the full
	// rationale and the documented residual gaps (submodules, `.git`-as-file).
	// The set whose top-level `.git` gets pinned. workTree here is EITHER the
	// hardenable real cwd OR the ephemeral scratch (which has no `.git` yet, so
	// writableGitDirs skips it); the unhardenable-cwd and unhardenable-mapped
	// decisions were already made and enforced in step 7f above.
	writableCheckouts := append([]string{workTree}, worktreeWrites...)
	baseParams := srt.Params{
		SocketPath:          socketPath,
		WorkingTree:         workTree,
		ExtraAllowedDomains: extraDomains,
		// The agent's scratch dir + the developer's mapped local checkouts (#64).
		// Both are re-bound READ-WRITE under the $HOME deny tmpfs when they live
		// there (srt pushReadDenyDirMounts; verified in the #59/#63 work), and
		// both become ForbiddenDirs for the socket placement check below.
		ExtraAllowWrite:    append([]string{agentTmp}, worktreeWrites...),
		WritableGitDirs:    writableGitDirs(writableCheckouts),
		DenyReadCredStores: denyStores,
		RuntimeDenyRead:    runtimeDeny,
		DenyReadHome:       homeDeny,
		AllowRead:          allowReadPaths,
	}
	cfg, err := srt.Build(baseParams)
	if err != nil {
		return 1, fmt.Errorf("build srt config: %w", err)
	}

	// (8b) Per-run audit trail (design §6). The token-redacted proxy decision
	// log goes to a file UNDER stateDir, which is in the deny-read set (step 7),
	// so the sandboxed agent cannot read its own audit trail. Best-effort: an
	// audit-open failure degrades to no audit sink rather than blocking the run
	// (fail-closed on security controls, not on observability).
	var auditW io.Writer
	auditPath := auditLogPath(stateDir, runID)
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "rein: warning: could not create audit dir (%v); proceeding without a per-run audit log\n", err)
	} else {
		af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rein: warning: could not open audit log (%v); proceeding without one\n", err)
		} else {
			defer af.Close()
			auditW = af
		}
	}

	// (9) Start the in-process broker/proxy. runbroker.Listen placement-checks
	// the socket against the bind-mounts (working tree) and fails closed if it
	// would land inside one.
	// The run's EFFECTIVE scope ceiling (issue #69): the session's standing
	// repos UNION the repos the human approves as expansions during this
	// run. Every scope-sensitive surface below reads through it — the scope
	// check, BOTH mints (a token is minted AT a scope), and the token caches
	// (via ScopeKey). A launch-time snapshot of sess.Repos in any one of
	// them would make an approved expansion silently fail to arrive.
	rscope := runscope.New(sess, stateDir, runID)
	// scopedAppCfg re-scopes the App config to the ceiling AS OF THIS MINT.
	// appCfg.RepoNames was set at launch from the standing repos; a mint
	// after an approved expansion must cover the wider set.
	scopedAppCfg := func() githubapp.Config {
		c := appCfg
		c.RepoNames = rscope.BareNames()
		return c
	}
	mintRead := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		c, err := githubapp.NewClient(scopedAppCfg(), ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		return c.MintReadOnlyToken(ctx)
	})
	// ghReadToken supplies the issues:read-capable read token the declare
	// fetch and the TM-G6 transfer re-check use (the MintGhReadOnlyToken
	// shape — the plain read mint lacks issues:read). Cached on disk via
	// ghsession so repeated declares/re-checks don't burn mints.
	ghReadToken := func(ctx context.Context) (string, error) {
		c, err := githubapp.NewClient(scopedAppCfg(), ks, config.AppKeystoreRole)
		if err != nil {
			return "", err
		}
		tok, _, err := ghsession.EnsureFresh(ghsession.ReadCachePath(stateDir), c.MintGhReadOnlyToken, c.RevokeToken, 5*time.Minute, mintTimeout, logger)
		return tok, err
	}
	mintWrite := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		// TM-G6 re-check on EVERY write-token mint (#35 §6): a confirmed
		// issue whose canonical URL now 3xx's was transferred — its
		// confirmation is invalidated; an emptied set fails the mint
		// (placeholder ⇒ local deny; the agent is told to re-declare).
		if err := declare.InvalidateTransferred(ctx, stateDir, runID, sess, ghReadToken, logger, os.Stderr); err != nil {
			return "", time.Time{}, err
		}
		c, err := githubapp.NewClient(scopedAppCfg(), ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		return c.MintWriteToken(ctx)
	})

	approve := buildSandboxApprove(sess, stateDir, runID, logger)
	host, err := runbroker.Start(runbroker.Config{
		SessionID:     sess.ID,
		SocketPath:    socketPath,
		ForbiddenDirs: append([]string{workTree}, baseParams.ExtraAllowWrite...),
		MintRead:      mintRead,
		MintWrite:     mintWrite,
		InScope:       rscope.Contains,
		ScopeKey:      rscope.Key,
		Approve:       approve,
		Declaration: buildDeclarationHooks(declareEnv{
			sess:        sess,
			sessionFile: session.SourceFilePath(sessSource),
			stateDir:    stateDir,
			runID:       runID,
			approve:     approve,
			ghReadToken: ghReadToken,
			appCfg:      appCfg,
			scopedCfg:   scopedAppCfg,
			ks:          ks,
			logger:      logger,
		}),
		RecordWrite: func(token string, expiresAt time.Time) {
			if err := approvals.AppendWriteToken(stateDir, runID, tokencache.Entry{Token: token, ExpiresAt: expiresAt}); err != nil {
				logger.Printf("write-token ledger append failed (best-effort): %v", err)
			}
		},
		CAKeystore: caKeystore,
		Audit:      auditW,
		Logger:     logger,
		// Proactive session expiry (design §5.3): bound how long a granted
		// approval + cached write token can stay live if the agent idles or runs
		// forever. On expiry we revoke the run's write tokens and stop the proxy
		// (the agent's next GitHub request then fails closed) but DO NOT kill the
		// agent — it keeps running credential-less with a loud message. The
		// double revoke (here + the deferred exit-time revoke) is harmless:
		// revoke is idempotent/best-effort.
		IdleTimeout: runbroker.DefaultIdleTimeout,
		HardTTL:     runbroker.DefaultHardTTL,
		OnExpire: func(reason string) {
			logger.Printf("session expired (%s): revoking write tokens and stopping the proxy", reason)
			revokeRunWriteTokens(stateDir, runID, productionRevoke(sess), time.Now())
			// Clear the ledger now that its tokens are revoked, so the deferred
			// exit-time revokeRunWriteTokens reads an empty ledger and is a clean
			// no-op — otherwise it would re-revoke already-dead tokens and print a
			// spurious "exit-revoke of a write token failed" per token (F1). The
			// deferred ClearRun still runs at exit (idempotent). A write approved
			// in the brief window before the proxy stops re-appends to the ledger
			// and is caught by that deferred exit-time revoke.
			if err := approvals.ClearRun(stateDir, runID); err != nil {
				logger.Printf("expiry: clear write-token ledger failed (best-effort): %v", err)
			}
			printExpiryBanner(os.Stderr, reason)
		},
	})
	if err != nil {
		return 1, fmt.Errorf("start broker/proxy: %w", err)
	}
	defer host.Close()
	// Exit-time write-token revoke + ledger clear (same discipline as direct
	// mode). Deferred so normal exit and SIGTERM-to-rein both run it.
	defer func() { _ = approvals.ClearRun(stateDir, runID) }()
	defer func() { revokeRunWriteTokens(stateDir, runID, productionRevoke(sess), time.Now()) }()

	// (10) CA bundle = system roots + rein CA. Written to a per-run temp dir NOT
	// under any denyRead path, so it is readable in-sandbox (git/curl/node trust
	// rein's MITM leaf on the inject path AND real certs on the CDN path).
	runTmp, err := os.MkdirTemp("", "rein-sandbox-*")
	if err != nil {
		return 1, fmt.Errorf("create run temp dir: %w", err)
	}
	defer os.RemoveAll(runTmp)
	bundle, err := srt.BuildCABundle(host.CACertPEM())
	if err != nil {
		return 1, fmt.Errorf("build CA bundle: %w", err)
	}
	bundlePath := filepath.Join(runTmp, "ca-bundle.pem")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		return 1, fmt.Errorf("write CA bundle: %w", err)
	}

	// (10b) NON-IMPERSONATING git identity (CP4). Resolve the author/committer
	// name + bot noreply email OUTSIDE the sandbox (network + host `git config`
	// happen here, at launch), write the rein-managed GIT_CONFIG_GLOBAL into the
	// readable per-run temp dir, and feed both into BuildEnv. This stops (a) the
	// agent's commits authoring as the developer and (b) the ~/.gitconfig leak.
	// Fail-open: gitidentity.Resolve never errors — every fallback is a valid,
	// non-impersonating identity — so a lookup hiccup degrades, never blocks.
	cachePath, err := gitIdentityCachePath()
	if err != nil {
		return 1, err
	}
	gitID := resolveGitIdentity(appCfg.ClientID, ks, ownerFromRepo(sess.Repos[0]), "", cachePath, logger)
	managedGitConfig := filepath.Join(runTmp, "gitconfig")
	if err := writeManagedGitConfig(managedGitConfig, gitID); err != nil {
		return 1, fmt.Errorf("write managed gitconfig: %w", err)
	}
	logger.Printf("git identity: author/committer = %q <%s>", gitID.Name, gitID.Email)

	// (11) Scrubbed exec environment (explicit allowlist; gap #1) + the CP4 git
	// identity + git-config redirects.
	// The bound checkouts to advertise to the AGENT (REIN_REPO_WORKTREES). In the
	// ephemeral-cwd fallback the working tree is a fresh EMPTY scratch, not a
	// checkout — so it is NOT listed here (listing it would imply the repo is
	// already checked out there). The contract + banner tell the agent to clone
	// its cwd repo into the working tree and push; the MAPPED worktrees, which
	// ARE real bound checkouts, are still advertised.
	agentBindings := wt.AgentBindings(workTree)
	if cwdEphemeral {
		agentBindings = wt.Bindings
	}
	execEnv := srt.BuildEnv(srt.EnvParams{
		Parent:              os.Environ(),
		CABundlePath:        bundlePath,
		StubGHToken:         stubGHToken,
		GitAuthorName:       gitID.Name,
		GitAuthorEmail:      gitID.Email,
		GitConfigGlobalPath: managedGitConfig,
		AgentTmpDir:         agentTmp,
		DisableClaudeAIMCP:  srt.DisableClaudeMCPFromEnv(os.Getenv(srt.EnvDisableClaudeMCP)),
		// (#64) Tell the AGENT where the developer's checkouts are mounted — it
		// cannot guess where repo B lives — and where to clone a repo that only
		// enters scope mid-run. The human banner alone cannot reach the agent.
		RepoWorktrees:     worktree.AgentEnvValue(agentBindings),
		EphemeralCloneDir: agentTmp,
		// Agent-visible facts (#63): the launch banner tells the HUMAN that
		// $HOME is ephemeral and only the working tree persists; the agent
		// never sees it. These carry the same two facts into the sandbox.
		// homeDeny is non-empty exactly when the deny-read tmpfs is in force.
		WorkTree:      workTree,
		HomeEphemeral: homeDeny != "",
		// The staged rein binary (copied into runTmp below, step 12) goes
		// on the in-sandbox PATH so `rein declare <n>` works exactly as
		// the deny messages instruct (#35 §3).
		ExtraPathDir: runTmp,
	})

	// (12) FAIL-OPEN DEFENSE: prove the config actually applied by launching srt
	// with the probe BEFORE the agent runs. Catches srt's null-fallback (empty
	// denyRead) and a missing/disarmed seccomp filter at runtime — the real
	// guarantee, not trust in srt.
	//
	// The probe is the rein binary itself (`rein __sandbox-probe`). resolveSelf()
	// may point INTO a denyRead'd dir — notably the shim copy at
	// stateDir/shim/rein, which we tmpfs out in-sandbox — so the probe would be
	// unable to exec and the self-test would fail closed on a legitimate config.
	// Copy rein into the readable per-run temp dir (NOT under any denyRead path)
	// and probe with that. One small copy per launch; robust regardless of how
	// rein was invoked.
	self, err := resolveSelf()
	if err != nil {
		return 1, fmt.Errorf("resolve rein binary for self-test: %w", err)
	}
	reinBin := filepath.Join(runTmp, "rein")
	if err := copyFile(self, reinBin, 0o700); err != nil {
		return 1, fmt.Errorf("stage rein probe binary: %w", err)
	}
	fmt.Fprintln(os.Stderr, "rein: verifying sandbox config applies (deny-read + seccomp self-test)…")
	if err := srt.VerifyConfigApplied(srt.VerifyParams{
		Base:    baseParams,
		SrtPath: srtPath,
		ReinBin: reinBin,
		Env:     execEnv,
		Timeout: verifyTimeout,
	}); err != nil {
		return 1, fmt.Errorf("sandbox self-test failed: %w", err)
	}
	fmt.Fprintln(os.Stderr, "rein: sandbox self-test passed (credential stores hidden; unix sockets blocked).")

	// (13) Emit the real settings.json (no sentinel) and exec the agent.
	settingsData, err := cfg.MarshalIndent()
	if err != nil {
		return 1, fmt.Errorf("marshal settings: %w", err)
	}
	settingsPath := filepath.Join(runTmp, "settings.json")
	if err := os.WriteFile(settingsPath, settingsData, 0o600); err != nil {
		return 1, fmt.Errorf("write settings: %w", err)
	}

	// (11b) THE SANDBOX CONTRACT (#63). Everything the banner says goes to the
	// HUMAN's terminal; the agent sees none of it. Brief the AGENT itself: claude
	// gets it in its system prompt (a real context channel), every other agent
	// gets it printed into the sandbox's own output — weaker, but honest, and the
	// banner reports WHICH happened rather than implying the agent was briefed.
	contract := buildAgentContract(contractParams{
		WorkTree:      workTree,
		HomeEphemeral: homeDeny != "", // false under the SHOW_HOME kill switch: $HOME really IS persistent then
		// (#64) when the cwd was unhardenable, the working tree is itself a
		// throwaway — the contract must say clone-and-push, not "your work
		// persists here".
		WorkTreeEphemeral: cwdEphemeral,
		WorkTreeRepo:      cwdRepo,
		ExtraDomains:      extraDomains,
	})
	contractOff := srt.DisableClaudeMCPFromEnv(os.Getenv(EnvDisableAgentContract))
	agentArgv := cmdline
	injected := false
	if !contractOff {
		agentArgv, injected = injectContract(cmdline, contract)
	}

	printSandboxBanner(os.Stderr, sess, sessSource, socketPath, workTree, extraDomains, cmdline, showHome, allowReadPaths,
		contractStatus(contractOff, injected), wt, agentTmp, ephemeralCwdPath, cwdRepo)

	// Non-claude agents: print the contract where the AGENT's own output goes, so
	// it lands in its transcript/scrollback rather than only on the human's side.
	if !contractOff && !injected {
		fmt.Fprintln(os.Stdout, contract)
	}

	srtArgv := append([]string{"-s", settingsPath, "--"}, agentArgv...)
	cmd := exec.Command(srtPath, srtArgv...)
	cmd.Env = execEnv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// In the unhardenable-cwd ephemeral fallback (#64), rein's OWN cwd is still
	// the real (unbound) checkout — which is hidden/read-only in-sandbox. Point
	// srt (and thus the agent) at the ephemeral scratch tree instead, so the
	// agent starts somewhere writable that matches REIN_IN_SANDBOX_WORKTREE. In
	// every other case srt inherits rein's cwd unchanged (the bound real tree).
	if cwdEphemeral {
		cmd.Dir = workTree
	}

	if err := cmd.Start(); err != nil {
		return 127, fmt.Errorf("start srt: %w", err)
	}
	// Forward SIGTERM-to-rein to the srt child (SIGINT reaches both via the
	// shared process group; mirrors direct mode).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	doneCh := make(chan struct{})
	go func() {
		select {
		case s := <-sigCh:
			_ = cmd.Process.Signal(s)
		case <-doneCh:
		}
	}()

	waitErr := cmd.Wait()
	close(doneCh)

	if waitErr == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode(), nil
	}
	return 1, fmt.Errorf("wait srt: %w", waitErr)
}

// buildSandboxApprove returns the write-approval hook for sandboxed mode:
// a pure READ of the run's confirmed-issue set (issue #35 §2/§7). It is
// NEVER nil — runbroker.Start fails closed on a nil hook. It NEVER
// prompts: prompts fire only at declare time (the declare.rein.internal
// handler), never inside a relayed request (§3).
//
// Approval invariants (an in-sandbox process cannot self-grant):
//
//   - The Form A prompt runs in rein's PARENT process at declare time, on
//     its foreground /dev/tty (internal/ui/grant). The sandboxed agent has
//     no channel to rein's tty: srt launches the child with bwrap
//     `--new-session` (setsid), so the sandbox has NO controlling terminal
//     — /dev/tty is unopenable (ENXIO) and TIOCSTI injection fails,
//     independent of the host's dev.tty.legacy_tiocsti sysctl.
//     VerifyConfigApplied re-checks this EVERY launch (ProbeControllingTTY),
//     so a future srt that dropped --new-session can't silently reopen the
//     channel (issue #32).
//   - The on-disk approval RECORD this hook consults lives under stateDir,
//     which is deny-read'd in-sandbox AND outside the sandbox's only
//     writable mount (the working tree), so an in-sandbox process can
//     neither read nor forge it. See TestInSandboxSelfGrantStructurallyFails.
//   - There is NO control socket in the in-process model (runbroker package
//     doc); the only unix socket the sandbox can reach is the per-run PROXY
//     socket, whose only local verbs are the declare virtual host — which
//     always routes through the fetch + Form A human ceremony.
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
// INVARIANT (design §6, audit #44 §2): this path MUST stay under stateDir,
// because credentialDenyReadPaths puts stateDir in the sandbox deny-read
// set — that containment is the only thing keeping the sandboxed agent from
// reading (and grooming its behavior against) its own audit trail.
// TestAuditLogPathIsUnderDenyReadSet pins the relationship; if the audit
// log ever moves, it must move to another deny-read location and the test
// updated deliberately.
func auditLogPath(stateDir, runID string) string {
	return filepath.Join(stateDir, "audit", "sandbox-"+runID+".log")
}

// credentialDenyReadPaths returns the ambient credential stores plus rein's own
// key/state directories to hide from the agent. Absolute paths; missing ones are
// harmless (denyRead of an absent path is a no-op in srt).
//
// Each store is resolved by its tool's ACTUAL env precedence, read from rein's
// own (unscrubbed) launch environment — NOT hardcoded to ~/.config. A developer
// who relocated their stores via XDG_CONFIG_HOME / GH_CONFIG_DIR / GNUPGHOME
// (mainstream dotfiles setups) would otherwise have them left readable in the
// sandbox: the agent does `ls $HOME`, finds the relocated gh config, reads the
// OAuth token — exactly what rein exists to prevent. rein already resolves its
// OWN key dir XDG-aware (config.ConfigDir); the dev's stores must be treated the
// same. The plain ~/… paths are kept too (belt-and-suspenders; denyRead no-ops
// on absent paths).
//
// Fails closed if the home directory OR rein's config dir can't be resolved: the
// home stores (~/.ssh, gh config, …) and rein's own key+CA dir are the
// highest-value secrets to hide, and os.UserHomeDir() errors whenever $HOME is
// empty EVEN when XDG_* (and thus StateDir/ConfigDir) still resolve. Returning a
// partial list would let the run launch with stores readable while every other
// check — Validate (structure only) and the /tmp-sentinel self-test — stays
// green: a silent fail-open. Refuse instead.
func credentialDenyReadPaths(stateDir string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve home dir to hide credential stores (%w); set $HOME", err)
	}
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = filepath.Join(home, ".config")
	}

	// gh: GH_CONFIG_DIR else $XDG_CONFIG_HOME/gh else ~/.config/gh.
	ghDir := os.Getenv("GH_CONFIG_DIR")
	if ghDir == "" {
		ghDir = filepath.Join(xdgConfig, "gh")
	}
	// gpg: GNUPGHOME else ~/.gnupg.
	gpgDir := os.Getenv("GNUPGHOME")
	if gpgDir == "" {
		gpgDir = filepath.Join(home, ".gnupg")
	}
	// Secret Service on-disk keyring DATABASE (#46): the live D-Bus socket is
	// already unreachable (seccomp AF_UNIX block + /run/user denyRead), but the
	// store file itself — $XDG_DATA_HOME/keyrings, default ~/.local/share/keyrings,
	// where git's libsecret credential helper keeps GitHub tokens — sits under
	// srt's read-only root bind and would stay readable. A passwordless
	// auto-unlock keyring is offline-decryptable, so hide the DB too. KWallet's
	// store (~/.local/share/kwalletd) is the same leak class on KDE.
	xdgData := os.Getenv("XDG_DATA_HOME")
	if xdgData == "" {
		xdgData = filepath.Join(home, ".local", "share")
	}
	// cargo: CARGO_HOME else ~/.cargo. Only the CREDENTIAL FILES are denied,
	// not the dir — ~/.cargo is in the #59 read allow-back set (rustup
	// toolchain shims + registry cache), and srt's exact-match-only rule for
	// file denies keeps these hidden UNDER that dir allow-back (verified on
	// 0.0.63: a dir-level allowRead never un-denies an explicitly listed file).
	cargoDir := os.Getenv("CARGO_HOME")
	if cargoDir == "" {
		cargoDir = filepath.Join(home, ".cargo")
	}

	out := []string{
		ghDir,                                              // gh login (env-resolved)
		filepath.Join(home, ".config", "gh"),               // gh default (belt-and-suspenders)
		gpgDir,                                             // gpg (env-resolved)
		filepath.Join(home, ".gnupg"),                      // gpg default
		filepath.Join(xdgConfig, "git"),                    // XDG git credentials (env-resolved)
		filepath.Join(home, ".config", "git"),              // XDG git default
		filepath.Join(home, ".gitconfig"),                  // classic ~/.gitconfig — leaks dev email + credential.helper (CP4)
		filepath.Join(home, ".ssh"),                        // ssh keys + agent socket dir
		filepath.Join(home, ".netrc"),                      // curl/git netrc
		filepath.Join(home, ".git-credentials"),            // git store helper
		filepath.Join(xdgData, "keyrings"),                 // Secret Service keyring DB (env-resolved)
		filepath.Join(home, ".local", "share", "keyrings"), // Secret Service keyring DB default
		filepath.Join(xdgData, "kwalletd"),                 // KWallet store (env-resolved)
		filepath.Join(home, ".local", "share", "kwalletd"), // KWallet store default
		filepath.Join(cargoDir, "credentials.toml"),        // cargo registry token (env-resolved; stays denied under the ~/.cargo allow-back)
		filepath.Join(cargoDir, "credentials"),             // cargo legacy credentials file (env-resolved)
		filepath.Join(home, ".cargo", "credentials.toml"),  // cargo registry token default (belt-and-suspenders)
		filepath.Join(home, ".cargo", "credentials"),       // cargo legacy default
		filepath.Join(xdgConfig, "rein-credentials"),       // App private key (env-resolved)
		filepath.Join(home, ".config", "rein-credentials"), // App private key default
	}
	// rein's managed keystore + CA live in ConfigDir; audit log, token caches,
	// gh-env.sh, and the shim live in StateDir. Hide both — the sandboxed agent
	// uses the proxy, never these. ConfigDir resolution failing is fail-closed:
	// leaving rein's own key+CA dir readable is the same class of hole as the
	// home stores above.
	cfgDir, err := config.ConfigDir()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve rein config dir to hide its key+CA material (%w)", err)
	}
	// Keep the plain ~/.config/rein and ~/.local/state/rein defaults alongside
	// the env-resolved dirs (belt-and-suspenders, mirroring gh/gpg above): a dev
	// who set XDG_* after first use could have a legacy default dir still
	// holding a PEM keystore, state.json, or token ledgers.
	out = append(out, cfgDir)
	out = append(out, filepath.Join(home, ".config", "rein"))
	out = append(out, stateDir)
	out = append(out, filepath.Join(home, ".local", "state", "rein"))

	// The wrapped agent (Claude Code) authenticates from ~/.claude/.credentials.json,
	// which stays READABLE (the agent needs it — see CP4.5). But ~/.claude ALSO
	// holds the developer's cross-project work history — hide those artifacts so a
	// (possibly prompt-injected) sandboxed agent can't read them and exfiltrate via
	// the extra egress the operator opened. denyRead of a DIR is an empty WRITABLE
	// tmpfs in-sandbox (so this doubles as claude's ephemeral per-run scratch);
	// denyRead of a FILE is /dev/null. .credentials.json and settings.json are
	// deliberately NOT hidden. This is a deliberate, minimal expansion of rein's
	// remit (hiding the agent's own work artifacts, not just credential stores);
	// it is safe (added protection) and untested against a live claude here (srt
	// 1.0.0 in the dev box doesn't emit) — re-verify in the dogfood.
	// Hide BOTH the env-resolved dir AND the conventional ~/.claude default
	// (belt-and-suspenders, mirroring the gh/gpg handling above): a dev who set a
	// non-default CLAUDE_CONFIG_DIR could still have a populated legacy ~/.claude
	// with stale cross-project history that would otherwise stay readable. denyRead
	// of a duplicate or absent path is a harmless no-op.
	claudeDirs := []string{filepath.Join(home, ".claude")}
	if cd := os.Getenv("CLAUDE_CONFIG_DIR"); cd != "" {
		claudeDirs = append(claudeDirs, cd)
	}
	// session-env is BOTH a dev-history artifact to hide AND a dir claude's
	// SessionStart machinery writes into per run (mkdir ~/.claude/session-env/<id>).
	// denyRead of a DIR is a writable, EMPTY tmpfs in-sandbox, so listing it here
	// hides the host's accumulated session-env entries (same rationale as
	// projects/sessions) while giving claude a fresh writable scratch dir — without
	// it, that mkdir hits EROFS under the read-only root bind (surfaced as a
	// SessionStart hook error when running a real claude in-sandbox).
	for _, cdir := range claudeDirs {
		for _, sub := range []string{"history.jsonl", "projects", "sessions", "session-env", "todos", "shell-snapshots"} {
			out = append(out, filepath.Join(cdir, sub))
		}
	}
	// Explicit App key path override, if set outside the dirs above.
	if p := os.Getenv("REIN_APP_PRIVATE_KEY_PATH"); p != "" {
		out = append(out, filepath.Dir(p))
	}
	return out, nil
}

// runUserDir returns /run/user/<uid> (D-Bus/Secret Service/agent sockets),
// which srt ro-binds by default. Empty if it can't be determined.
func runUserDir() string {
	return filepath.Join("/run", "user", strconv.Itoa(os.Getuid()))
}

// printPreflightAndOK prints each preflight check with a status marker and
// returns true only if no hard (StatusFail) check failed.
func printPreflightAndOK(w io.Writer, checks []srt.Check) bool {
	ok := true
	fmt.Fprintln(w, "rein: sandbox preflight:")
	for _, c := range checks {
		fmt.Fprintf(w, "  %s %s: %s\n", preflightMarker(c.Status), c.Name, flattenMessage(c.Message))
		if c.Status == srt.StatusFail {
			ok = false
		}
	}
	return ok
}

func preflightMarker(s srt.Status) string {
	switch s {
	case srt.StatusOK:
		return "[ok]  "
	case srt.StatusWarn:
		return "[warn]"
	default:
		return "[fail]"
	}
}

// preflightSrtPath extracts the resolved srt binary path from the "srt present"
// check message (StatusOK sets the message to the path).
func preflightSrtPath(checks []srt.Check) string {
	for _, c := range checks {
		if c.Name == "srt present" && c.Status == srt.StatusOK {
			return c.Message
		}
	}
	return "srt" // fall back to PATH lookup by exec
}

// printExpiryBanner is the loud, human-facing notice when a session hits its
// idle or hard-TTL bound. The agent process is deliberately NOT killed (that
// would abruptly drop the user's work); instead it keeps running but can no
// longer reach GitHub. reason is "idle" or "hard-ttl".
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

// printShowHomeWarning is the unmissable notice for the REIN_SANDBOX_SHOW_HOME
// kill switch (issue #59): the operator explicitly opted OUT of hiding $HOME,
// so every credential store NOT on the targeted denylist is readable by the
// sandboxed agent. Never silent — this must be as loud as the expiry banner.
func printShowHomeWarning(w io.Writer) {
	fmt.Fprintln(w, "===============================================================")
	fmt.Fprintf(w, "rein: WARNING: %s is set — $HOME is READABLE in-sandbox.\n", srt.EnvSandboxShowHome)
	fmt.Fprintln(w, "  The wholesale home-directory hiding (issue #59) is DISABLED for")
	fmt.Fprintln(w, "  this run. The targeted credential denylist (~/.ssh, gh/gpg config,")
	fmt.Fprintln(w, "  keyrings, …) still applies, but ANY OTHER token or secret file")
	fmt.Fprintln(w, "  under your home directory is exposed to the sandboxed agent.")
	fmt.Fprintf(w, "  Prefer narrow allow-backs instead: %s=/abs/path[:/more]\n", srt.EnvSandboxAllowRead)
	fmt.Fprintln(w, "===============================================================")
}

// writableGitDirs returns, for each writable checkout, its `<tree>/.git` path
// WHEN that path is a real directory — the set srt.Params.WritableGitDirs pins
// against the rename-parent escape. A checkout whose `.git` is a FILE (a linked
// worktree: `gitdir: …`) is skipped: its exec surfaces live in the common
// gitdir, not at `<tree>/.git`, and ro-binding a non-existent `<tree>/.git/*`
// would make bwrap fail the launch. A missing `.git` (not yet a repo) is also
// skipped. Both residual cases are documented on srt.Params.WritableGitDirs.
func writableGitDirs(trees []string) []string {
	out := make([]string, 0, len(trees))
	for _, t := range trees {
		if t == "" {
			continue
		}
		gitPath := filepath.Join(t, ".git")
		fi, err := os.Stat(gitPath)
		if err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, gitPath)
	}
	return out
}

// unhardenedMappedGitError is the fail-closed refusal when an EXPLICITLY-MAPPED
// worktree's `.git` cannot be fully hardened (submodules / linked worktree). The
// cwd never reaches this — it falls back to an ephemeral working tree instead
// (see step 7f). A mapped tree is one the human named deliberately, so the right
// answer is an actionable error, not a silent ephemeral swap they didn't ask for.
func unhardenedMappedGitError(gaps []srt.GitHardeningGap) error {
	var b strings.Builder
	b.WriteString("refusing to bind a MAPPED writable checkout whose .git cannot be fully hardened against\n")
	b.WriteString("the rename-parent escape (a prompt-injected agent could plant git hooks that run AS YOU,\n")
	b.WriteString("ON THE HOST). Affected worktree(s):\n")
	for _, g := range gaps {
		fmt.Fprintf(&b, "  - %s\n      %s\n", g.Tree, g.Reason)
	}
	fmt.Fprintf(&b, "Remedy: remove it from the session's `worktrees:` map (or unset %s) so the\n", worktree.EnvWorktrees)
	fmt.Fprintf(&b, "agent clones it ephemerally and pushes instead of writing your real tree; OR, to bind it\n")
	fmt.Fprintf(&b, "writable anyway with partial (top-level-only) hardening, set %s=1.", srt.EnvSandboxAllowUnhardenedGit)
	return fmt.Errorf("%s", b.String())
}

// printUnhardenedGitWarning is the loud banner when the operator opts in via
// REIN_SANDBOX_ALLOW_UNHARDENED_GIT: the run proceeds, but the named surfaces
// stay writable and can execute as the developer on the host.
func printUnhardenedGitWarning(w io.Writer, gaps []srt.GitHardeningGap) {
	fmt.Fprintln(w, "===============================================================")
	fmt.Fprintf(w, "rein: WARNING: %s=1 — proceeding with PARTIAL git hardening.\n", srt.EnvSandboxAllowUnhardenedGit)
	fmt.Fprintln(w, "  These writable checkouts have a .git exec surface that is NOT protected;")
	fmt.Fprintln(w, "  a prompt-injected agent can plant hooks that run AS YOU, ON THE HOST:")
	for _, g := range gaps {
		fmt.Fprintf(w, "    - %s\n        %s\n", g.Tree, g.Reason)
	}
	fmt.Fprintln(w, "===============================================================")
}

// printWritableTrees is the #64 disclosure: EVERY real directory on the
// developer's disk that the sandboxed agent can WRITE this run, named
// explicitly, with the repo each one belongs to.
//
// This is deliberately the loudest thing in the banner after the credential
// story. A mapped checkout is the developer's live tree — it may hold
// uncommitted work, and a prompt-injected agent can modify it. The mitigation
// is not obscurity (the agent is told the paths — it must be, or it cannot use
// them); it is that the human named these trees and sees, at launch, exactly
// which ones went in. If a listed path is a surprise, that is the signal to
// Ctrl-C and fix the `worktrees:` map.
func printWritableTrees(w io.Writer, workTree string, wt worktree.Result, cloneDir, ephemeralCwdPath, cwdRepo string) {
	if ephemeralCwdPath != "" {
		// The unhardenable-cwd fallback (#64): the real tree is NOT bound; the
		// working tree is a fresh throwaway. Say so loudly — the developer needs
		// to know their real checkout was declined and nothing local survives.
		repo := cwdRepo
		if repo == "" {
			repo = "your cwd repo"
		}
		fmt.Fprintln(w, "  ===========================================================")
		fmt.Fprintf(w, "  YOUR REAL CHECKOUT WAS NOT BOUND (unhardenable .git): %s\n", ephemeralCwdPath)
		fmt.Fprintln(w, "    (a submodule superproject or a linked worktree — its .git cannot be pinned")
		fmt.Fprintln(w, "     against the host-code-execution escape, so binding it writable is unsafe).")
		fmt.Fprintf(w, "  The agent works in an EPHEMERAL throwaway tree instead: %s\n", workTree)
		fmt.Fprintf(w, "    it clones %s there and PUSHES — nothing local survives the run.\n", repo)
		fmt.Fprintf(w, "    to bind your REAL tree writable anyway (accepting the .git risk): %s=1\n", srt.EnvSandboxAllowUnhardenedGit)
		fmt.Fprintln(w, "  ===========================================================")
	} else if wt.WorkTreeRepo != "" {
		fmt.Fprintf(w, "  working tree (writable in sandbox): %s  [%s]\n", workTree, wt.WorkTreeRepo)
	} else {
		fmt.Fprintf(w, "  working tree (writable in sandbox): %s\n", workTree)
	}
	if len(wt.Bindings) > 0 {
		fmt.Fprintln(w, "  ===========================================================")
		fmt.Fprintf(w, "  YOUR LOCAL CHECKOUTS ARE AGENT-WRITABLE THIS RUN (%d):\n", len(wt.Bindings))
		for _, b := range wt.Bindings {
			fmt.Fprintf(w, "    %s  ->  %s  (rw, from %s)\n", b.Repo, b.Path, b.Source)
		}
		fmt.Fprintln(w, "  These are REAL trees, not clones. The agent can modify ANY writable file in")
		fmt.Fprintln(w, "  them — commit or stash anything you can't lose.")
		fmt.Fprintln(w, "  Each tree's top-level .git is HARDENED: .git is pinned (rename fails) and")
		fmt.Fprintln(w, "  .git/hooks, .git/config, .git/config.worktree are read-only in-sandbox — the")
		fmt.Fprintln(w, "  main plant-a-hook / hooksPath route to running code AS YOU, ON THE HOST, is")
		fmt.Fprintln(w, "  closed. (Cost: in-sandbox `git config --local` writes fail; commits still work.)")
		fmt.Fprintln(w, "  But a writable tree is NOT risk-free: the agent can still change tracked build")
		fmt.Fprintln(w, "  scripts, bury a hostile gitdir/config in a subdir, or plant a submodule/bare-repo")
		fmt.Fprintln(w, "  gitdir (issue #76) — any of which run on the HOST if YOU later build or run git")
		fmt.Fprintln(w, "  there. Only map trees you'd let the agent run code from.")
		fmt.Fprintln(w, "  Trees whose .git cannot be fully hardened (submodules, or a linked worktree")
		fmt.Fprintf(w, "  where .git is a file) are REFUSED unless you set %s=1.\n", srt.EnvSandboxAllowUnhardenedGit)
		fmt.Fprintf(w, "  Remove an entry from the session's `worktrees:` map (or unset %s) to withhold it.\n", worktree.EnvWorktrees)
		fmt.Fprintln(w, "  ===========================================================")
	}
	fmt.Fprintf(w, "  a repo approved MID-RUN cannot be bound (bwrap binds are fixed at launch);\n")
	fmt.Fprintf(w, "    the agent clones it into %s=%s\n", srt.EnvAgentCloneDir, cloneDir)
	fmt.Fprintln(w, "    (writable, DISCARDED at run end — the durable artifact is the push, not the tree;")
	fmt.Fprintln(w, "     never inside the working tree, where a nested clone can be committed into it).")
	fmt.Fprintf(w, "    map it under `worktrees:` to use your own checkout on the NEXT run.\n")
}

// contractStatus renders the ONE banner line saying how (or whether) the agent
// was briefed. This line matters: without it the operator cannot tell a briefed
// agent from an un-briefed one, and "rein told the agent" would be an
// assumption rather than an observation.
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

func printSandboxBanner(w io.Writer, sess session.Session, sessSource, socketPath, workTree string, extraDomains, cmdline []string, showHome bool, allowReadPaths []string, contractLine string, wt worktree.Result, cloneDir, ephemeralCwdPath, cwdRepo string) {
	fmt.Fprintln(w, "rein: launching SANDBOXED (srt) run:")
	fmt.Fprintf(w, "  session: %s (role=%s, repos=%v) [source=%s]\n",
		sess.ID, sess.Role, sess.Repos, sessSource)
	fmt.Fprintf(w, "  proxy socket (out of sandbox): %s\n", socketPath)
	printWritableTrees(w, workTree, wt, cloneDir, ephemeralCwdPath, cwdRepo)
	fmt.Fprintln(w, "  the agent sees NO real token; git/gh are injected at the proxy.")
	fmt.Fprintln(w, "  credential stores, ~/.ssh, on-disk keyrings, and keyring/agent sockets are hidden.")
	// Denial UX (#59, first-class requirement): when a hidden path breaks a
	// tool in-sandbox, the failure shows up as ENOENT/empty-dir with no hint
	// of WHY — this banner is where the remediation syntax lives, so the
	// discovery loop (hit a wall -> copy the env var line -> re-run) needs no
	// docs lookup.
	//
	// The three $HOME behaviors below are DISTINCT and were each verified
	// against real srt 0.0.63 + bwrap (#63 review). The banner used to collapse
	// them into "reads see an empty dir; writes are discarded", which was wrong
	// in both directions: it implied allowed-back writes evaporate (they ERROR)
	// and buried the only fact the operator actually needs — that nothing
	// outside the working tree survives the run. Do not re-collapse these.
	if showHome {
		fmt.Fprintf(w, "  WARNING: $HOME is VISIBLE in-sandbox (%s=1) — unknown credential stores in it are exposed.\n", srt.EnvSandboxShowHome)
	} else {
		fmt.Fprintln(w, "  $HOME is HIDDEN in-sandbox. Three different behaviors — know which one you get:")
		fmt.Fprintln(w, "    READS of a hidden path FAIL LOUDLY: ENOENT, or an empty listing for a dir.")
		fmt.Fprintln(w, "    WRITES under hidden $HOME SILENTLY SUCCEED into a scratch tmpfs, then are")
		fmt.Fprintln(w, "      DISCARDED at run end — caches (~/.cache, ~/.npm) work, but start cold every run.")
		fmt.Fprintln(w, "    WRITES to the allowed-back paths below ERROR (read-only): 'Read-only file system'.")
		if ephemeralCwdPath != "" {
			// Ephemeral working tree: NOTHING local survives — not even the tree.
			fmt.Fprintln(w, "  NOTHING local PERSISTS this run — not even the working tree (it is a throwaway).")
			fmt.Fprintln(w, "    => the ONLY durable artifact is the agent's PUSH to GitHub.")
		} else {
			fmt.Fprintf(w, "  NOTHING under $HOME PERSISTS. Only the working tree does: %s\n", workTree)
			fmt.Fprintln(w, "    => work the agent must keep has to be written INTO the working tree.")
		}
		fmt.Fprintf(w, "    allowed back read-only: %s\n", strings.Join(allowReadPaths, ", "))
		fmt.Fprintln(w, "    if a tool breaks on a hidden $HOME path, allow it back narrowly:")
		fmt.Fprintf(w, "      %s=/abs/path[:/more] rein run --sandbox -- …\n", srt.EnvSandboxAllowRead)
		fmt.Fprintf(w, "    or (NOT recommended — exposes your whole home dir): %s=1\n", srt.EnvSandboxShowHome)
	}
	if len(extraDomains) > 0 {
		fmt.Fprintf(w, "  extra egress ALLOWED (direct TLS, NOT injected, no rein token): %s\n", strings.Join(extraDomains, ", "))
	}
	sess.WarnIgnoredIssue(w)
	fmt.Fprintln(w, "  writes are LOCKED until the agent declares its issue:  rein declare <n>")
	fmt.Fprintln(w, "  then push to agent/<n>/<nonce>. The declaration will prompt on THIS terminal.")
	if len(cmdline) > 0 {
		agent := filepath.Base(cmdline[0])
		fmt.Fprintf(w, "  to run %s WITHOUT rein for one command: `\\%s` (bash/zsh) or `command %s` (fish)\n", agent, agent, agent)
	}
	if contractLine != "" {
		fmt.Fprintln(w, contractLine)
	}
	fmt.Fprintln(w, "rein: running:", strings.Join(cmdline, " "))
	fmt.Fprintln(w, "---")
}
