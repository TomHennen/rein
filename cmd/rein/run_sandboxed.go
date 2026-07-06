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
//   - Preflight hard-gates srt presence+version, bwrap userns, and seccomp.
//     Any hard failure refuses to launch (no silent drop to unsandboxed mode).
//   - VerifyConfigApplied actually launches srt with a probe and proves BOTH
//     srt fail-opens are closed (denyRead applied via a content-sentinel;
//     AF_UNIX socket creation blocked) BEFORE the agent runs.
//   - The exec environment is an explicit allowlist (env -i equivalent), so no
//     ambient secret reaches the agent.
package main

import (
	"context"
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
	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/runbroker"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/internal/tokencache"
	"github.com/TomHennen/rein/internal/ui/grant"
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
	err = resolveAndCacheInstallID(ctx, sess, fetchRepoInstallationID)
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

	// (8) Build + validate the srt config (typed struct; no hand-rolled JSON).
	// baseParams is shared by BOTH the VerifyConfigApplied probe and the real
	// agent launch, so both see the identical allowlist (including the extras).
	baseParams := srt.Params{
		SocketPath:          socketPath,
		WorkingTree:         workTree,
		ExtraAllowedDomains: extraDomains,
		ExtraAllowWrite:     []string{agentTmp},
		DenyReadCredStores:  denyStores,
		RuntimeDenyRead:     runtimeDeny,
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
	auditDir := filepath.Join(stateDir, "audit")
	if err := os.MkdirAll(auditDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "rein: warning: could not create audit dir (%v); proceeding without a per-run audit log\n", err)
	} else {
		auditPath := filepath.Join(auditDir, "sandbox-"+runID+".log")
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
	mintRead := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		c, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		return c.MintReadOnlyToken(ctx)
	})
	mintWrite := brokercore.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		c, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		return c.MintWriteToken(ctx)
	})

	host, err := runbroker.Start(runbroker.Config{
		SessionID:     sess.ID,
		SocketPath:    socketPath,
		ForbiddenDirs: append([]string{workTree}, baseParams.ExtraAllowWrite...),
		MintRead:      mintRead,
		MintWrite:     mintWrite,
		InScope:       sess.Contains,
		Approve:       buildSandboxApprove(sess, stateDir, runID, logger),
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
	execEnv := srt.BuildEnv(srt.EnvParams{
		Parent:              os.Environ(),
		CABundlePath:        bundlePath,
		StubGHToken:         stubGHToken,
		GitAuthorName:       gitID.Name,
		GitAuthorEmail:      gitID.Email,
		GitConfigGlobalPath: managedGitConfig,
		AgentTmpDir:         agentTmp,
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

	printSandboxBanner(os.Stderr, sess, sessSource, socketPath, workTree, extraDomains, cmdline)

	srtArgv := append([]string{"-s", settingsPath, "--"}, cmdline...)
	cmd := exec.Command(srtPath, srtArgv...)
	cmd.Env = execEnv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

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
	if ee, ok := waitErr.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 1, fmt.Errorf("wait srt: %w", waitErr)
}

// buildSandboxApprove returns the write-approval hook for sandboxed mode. Unlike
// direct mode's buildConfirmWrite, it is NEVER nil — runbroker.Start fails
// closed on a nil hook, and a nil hook would auto-approve every write. When the
// session binds no issue, writes are DENIED (with a loud, actionable message)
// rather than silently allowed: reads still flow; a push is blocked until the
// session gets an `issue:`.
//
// CP4 approval invariants (an in-sandbox process cannot self-grant):
//
//   - This hook runs in rein's PARENT process, on its foreground /dev/tty, and
//     the issue-number secret is read from that tty (internal/ui/grant). The
//     sandboxed agent has no channel to rein's tty and does not know it will be
//     asked — it cannot pre-answer or self-answer the prompt. Concretely: srt
//     launches the child with bwrap `--new-session` (setsid), so the sandbox has
//     NO controlling terminal — /dev/tty is unopenable (ENXIO) and TIOCSTI
//     injection fails, independent of the host's dev.tty.legacy_tiocsti sysctl.
//     VerifyConfigApplied re-checks this EVERY launch (ProbeControllingTTY: the
//     in-sandbox probe opens /dev/tty and fails the launch closed if it
//     succeeds), so a future srt that dropped --new-session can't silently
//     reopen the channel (issue #32).
//   - The on-disk approval RECORD the hook consults lives under stateDir, which
//     is deny-read'd in-sandbox AND outside the sandbox's only writable mount
//     (the working tree), so an in-sandbox process can neither read nor forge
//     it. See TestInSandboxSelfGrantStructurallyFails.
//   - There is NO control socket in the in-process model (runbroker package
//     doc), so the daemon-era "#12 control socket reachable in-sandbox" vector
//     is closed structurally: the only unix socket the sandbox can reach is the
//     per-run PROXY socket, which speaks TLS/HTTP to GitHub, not approval verbs.
func buildSandboxApprove(sess session.Session, stateDir, runID string, logger *log.Logger) func(repo string) bool {
	if sess.Issue == 0 {
		logger.Printf("sandbox: write-approval DENIES all writes (session has no `issue:`); reads still flow")
		return func(repo string) bool {
			fmt.Fprintf(os.Stderr, "rein: write to %s BLOCKED — session has no `issue:` field, so no approval channel is bound. Add `issue: <n>` to the session and re-run to enable write approval.\n", repo)
			return false
		}
	}
	cfg := grant.Config{
		StateDir:      stateDir,
		RunID:         runID,
		RunPID:        os.Getpid(),
		TTL:           approvalTTL,
		PromptTimeout: 60 * time.Second,
		Logger:        logger,
	}
	return func(repo string) bool {
		return grant.ObtainApproval(context.Background(), grant.Request{
			Session: sess,
			Action:  "git push / write (sandboxed run)",
			Repo:    repo,
		}, cfg)
	}
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
	out = append(out, cfgDir)
	out = append(out, stateDir)

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

func printSandboxBanner(w io.Writer, sess session.Session, sessSource, socketPath, workTree string, extraDomains, cmdline []string) {
	fmt.Fprintln(w, "rein: launching SANDBOXED (srt) run:")
	fmt.Fprintf(w, "  session: %s (role=%s, repos=%v, issue=#%d) [source=%s]\n",
		sess.ID, sess.Role, sess.Repos, sess.Issue, sessSource)
	fmt.Fprintf(w, "  proxy socket (out of sandbox): %s\n", socketPath)
	fmt.Fprintf(w, "  working tree (writable in sandbox): %s\n", workTree)
	fmt.Fprintln(w, "  the agent sees NO real token; git/gh are injected at the proxy.")
	fmt.Fprintln(w, "  credential stores, ~/.ssh, keyring/agent sockets are hidden.")
	if len(extraDomains) > 0 {
		fmt.Fprintf(w, "  extra egress ALLOWED (direct TLS, NOT injected, no rein token): %s\n", strings.Join(extraDomains, ", "))
	}
	if sess.Issue == 0 && isWriteCapableRole(sess.Role) {
		fmt.Fprintln(w, "  WARN: session has no `issue:` — WRITES ARE BLOCKED (reads flow); add `issue:` to enable approvals.")
	} else if sess.Issue != 0 {
		fmt.Fprintln(w, "  first write triggers an approval prompt on THIS terminal (rein hosts the broker out of the sandbox).")
	}
	if len(cmdline) > 0 {
		agent := filepath.Base(cmdline[0])
		fmt.Fprintf(w, "  to run %s WITHOUT rein for one command: `\\%s` (bash/zsh) or `command %s` (fish)\n", agent, agent, agent)
	}
	fmt.Fprintln(w, "rein: running:", strings.Join(cmdline, " "))
	fmt.Fprintln(w, "---")
}
