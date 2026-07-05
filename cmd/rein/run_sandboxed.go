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
		return 1, fmt.Errorf("sandbox preflight failed — refusing to launch (fail-closed; direct unsandboxed mode on a real repo is NOT an automatic fallback)")
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

	// (8) Build + validate the srt config (typed struct; no hand-rolled JSON).
	baseParams := srt.Params{
		SocketPath:         socketPath,
		WorkingTree:        workTree,
		DenyReadCredStores: denyStores,
		RuntimeDenyRead:    runtimeDeny,
	}
	cfg, err := srt.Build(baseParams)
	if err != nil {
		return 1, fmt.Errorf("build srt config: %w", err)
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
		Logger:     logger,
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

	// (11) Scrubbed exec environment (explicit allowlist; gap #1).
	execEnv := srt.BuildEnv(srt.EnvParams{
		Parent:       os.Environ(),
		CABundlePath: bundlePath,
		StubGHToken:  stubGHToken,
	})

	// (12) FAIL-OPEN DEFENSE: prove the config actually applied by launching srt
	// with the probe BEFORE the agent runs. Catches srt's null-fallback (empty
	// denyRead) and a missing/disarmed seccomp filter at runtime — the real
	// guarantee, not trust in srt.
	reinBin, err := resolveSelf()
	if err != nil {
		return 1, fmt.Errorf("resolve rein binary for self-test: %w", err)
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

	printSandboxBanner(os.Stderr, sess, sessSource, socketPath, workTree, cmdline)

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
// Fails closed if the home directory can't be resolved: the home stores
// (~/.ssh, ~/.config/gh, …) are the highest-value secrets to hide, and
// os.UserHomeDir() errors whenever $HOME is empty EVEN when XDG_* (and thus
// StateDir/ConfigDir) still resolve. Returning a partial list there would let
// the run launch with those stores readable while every other check — Validate
// (structure only) and the /tmp-sentinel self-test — stays green: a silent
// fail-open. Refuse instead.
func credentialDenyReadPaths(stateDir string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve home dir to hide credential stores (%w); set $HOME", err)
	}
	out := []string{
		filepath.Join(home, ".config", "gh"),               // gh login (keyring fallback file)
		filepath.Join(home, ".ssh"),                        // ssh keys + agent socket dir
		filepath.Join(home, ".netrc"),                      // curl/git netrc
		filepath.Join(home, ".git-credentials"),            // git store helper
		filepath.Join(home, ".config", "git"),              // XDG git credentials
		filepath.Join(home, ".config", "rein-credentials"), // App private key (default path)
		filepath.Join(home, ".gnupg"),                      // gpg
	}
	// rein's managed keystore + CA live in ConfigDir; audit log, token caches,
	// gh-env.sh, and the shim live in StateDir. Hide both — the sandboxed agent
	// uses the proxy, never these.
	if cfgDir, err := config.ConfigDir(); err == nil {
		out = append(out, cfgDir)
	}
	out = append(out, stateDir)
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

func printSandboxBanner(w io.Writer, sess session.Session, sessSource, socketPath, workTree string, cmdline []string) {
	fmt.Fprintln(w, "rein: launching SANDBOXED (srt) run:")
	fmt.Fprintf(w, "  session: %s (role=%s, repos=%v, issue=#%d) [source=%s]\n",
		sess.ID, sess.Role, sess.Repos, sess.Issue, sessSource)
	fmt.Fprintf(w, "  proxy socket (out of sandbox): %s\n", socketPath)
	fmt.Fprintf(w, "  working tree (writable in sandbox): %s\n", workTree)
	fmt.Fprintln(w, "  the agent sees NO real token; git/gh are injected at the proxy.")
	fmt.Fprintln(w, "  credential stores, ~/.ssh, keyring/agent sockets are hidden.")
	if sess.Issue == 0 && isWriteCapableRole(sess.Role) {
		fmt.Fprintln(w, "  WARN: session has no `issue:` — WRITES ARE BLOCKED (reads flow); add `issue:` to enable approvals.")
	} else if sess.Issue != 0 {
		fmt.Fprintln(w, "  first write triggers an approval prompt on THIS terminal (rein hosts the broker out of the sandbox).")
	}
	fmt.Fprintln(w, "rein: running:", strings.Join(cmdline, " "))
	fmt.Fprintln(w, "---")
}
