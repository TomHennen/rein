// `rein run --nono -- <agent argv>` — the nono-sandboxed run path (design
// docs/design-nono-pivot.md §3). The nono counterpart of run_sandboxed.go: it
// reuses the broker/mint/scope/declare/approval spine UNCHANGED (startRunBroker,
// run_broker.go) and swaps only the sandbox FRONT and launch:
//
//   - the proxy also binds a 127.0.0.1 HTTP-CONNECT front (runbroker
//     LoopbackFront); nono chains the agent's GitHub egress to it as its
//     upstream_proxy. The token is injected on the rein→GitHub leg, downstream
//     of the sandbox — its value never enters the sandbox.
//   - the sandbox is nono, launched by ABSOLUTE managed path (never LookPath),
//     digest-verified before every launch (fail closed → `rein init`).
//   - the security boundary is the emitted nono PROFILE (deny_credentials,
//     af_unix_mediation:"pathname", host routing, CA trust) plus the CLI --allow
//     grants for the writable working tree.
//
// NOT ported from the srt path (nono's model differs): bwrap/seccomp preflight
// (nono uses Landlock) and the bind-mount deny-read/home-hiding/git-hardening
// machinery (nono is default-deny fs + the deny_credentials group). The
// non-impersonating git identity IS ported — as GIT_CONFIG_* in the profile
// (not srt's GIT_CONFIG_GLOBAL file), since nono owns env injection.
//
// TODO (post-merge integration): this launch does NOT yet run the nono
// containment prober before the agent. When this branch merges with current
// nono-pivot-design (which now carries internal/nono.VerifyContainment +
// RunContainmentProbe + the `__nono-probe` subcommand — the §3e launch gate,
// the nono counterpart of srt's VerifyConfigApplied), wire VerifyContainment in
// as the fail-closed pre-launch gate here. Until then the launch trusts the
// profile applies (digest-verified binary + Build's invariant checks only).
//
// DEFERRED write-journey enablers (a full declare→approve→push journey is the
// next wave; these are needed for it):
//   - in-sandbox `rein declare <n>`: the declare-host TUNNEL is verified (nono
//     tunnels the unresolvable declare.rein.internal by CONNECT hostname, no
//     DNS; rein terminates TLS + answers locally), but the agent invoking the
//     `rein` BINARY in-sandbox needs it reachable+executable under nono (srt
//     staged a copy + PATH; nono owns PATH — that path is unverified).
//   - CLAUDE_CONFIG_DIR / #94 overlay: `rein run --nono -- claude` needs a
//     rein-owned CLAUDE_CONFIG_DIR (host ~/.claude is hidden by default-deny);
//     requires an ExtraEnv channel in the profile generator.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/gitidentity"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/nono"
	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/sandboxutil"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/internal/worktree"
)

// buildNonoParams assembles the per-run inputs to nono.Build from the resolved
// loopback port, CA-bundle path, extra egress domains, and the non-impersonating
// git identity (as extra GIT_CONFIG_* entries). Pure (no I/O) so a unit test can
// pin that the loopback port flows into upstream_proxy, the CA path is carried,
// and the git identity is injected — without a live launch. The security host
// lists (inject / CDN / declare) are NOT passed here — nono.Build reads them
// straight from internal/proxy so the profile and the proxy can never drift.
func buildNonoParams(loopbackPort int, caBundlePath string, extraDomains []string, extraGitConfig []nono.GitConfig) nono.Params {
	return nono.Params{
		ListenAddr:     "127.0.0.1:" + strconv.Itoa(loopbackPort),
		CACertPath:     caBundlePath,
		ExtraDomains:   extraDomains,
		ExtraGitConfig: extraGitConfig,
		// UnixSockets left EMPTY: never grant the tmux/approval socket (the
		// af_unix_mediation crux, §3e). A real agent that needs a specific
		// pathname socket gets it added deliberately, never by default.
		Name:        "rein-sandbox",
		Description: "rein credential-broker sandbox profile (rein run --nono).",
	}
}

// nonoGitIdentityConfig renders the non-impersonating git identity as GIT_CONFIG_*
// entries (user.name / user.email). Under nono's default-deny fs the developer's
// ~/.gitconfig is hidden, so WITHOUT this the agent's `git commit` fails
// ("unable to auto-detect email"); WITH it the agent commits as rein's bot, never
// the developer. This is the nono equivalent of srt's GIT_CONFIG_GLOBAL file —
// GIT_CONFIG_* is the highest-precedence git config, overriding any repo-local
// user.* the developer set, which is the desired non-impersonation.
func nonoGitIdentityConfig(id gitidentity.Identity) []nono.GitConfig {
	return []nono.GitConfig{
		{Key: "user.name", Value: id.Name},
		{Key: "user.email", Value: id.Email},
	}
}

// runNono is the entry point for `rein run --nono -- <cmd>`. cmdline is the
// agent argv (after "--"). Returns (exitCode, error) like runSandboxed.
func runNono(cmdline []string) (int, error) {
	logger, closeLog, err := openLog()
	if err != nil {
		return 1, err
	}
	defer closeLog()

	// (1) PREFLIGHT — the managed nono binary must be installed AND match the
	// vendored digest. Fail closed with the exact fix; rein NEVER LookPaths nono
	// (the agent's $PATH could shadow it) and never runs an unverified binary.
	nonoPath, err := nono.ManagedNonoPath()
	if err != nil {
		return 1, fmt.Errorf("resolve managed nono path: %w", err)
	}
	platform, err := nono.DetectPlatform()
	if err != nil {
		return 1, err
	}
	if err := nono.VerifyInstalled(nonoPath, nono.PinnedVersion, platform); err != nil {
		return 1, fmt.Errorf("nono sandbox unavailable: %w\n      Run `rein init` to install the pinned nono runtime, then retry. "+
			"(Fail-closed: rein does NOT fall back to unsandboxed mode.)", err)
	}

	// (2) Session + scope.
	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return 1, fmt.Errorf("load session: %w", err)
	}
	config.WarnPartialAppEnv(os.Stderr)

	// (3) App config + eager install-id resolve (fail loud here, not inside the
	// sandbox's first git op). Mirrors runSandboxed.
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
	configDir, err := config.ConfigDir()
	if err != nil {
		return 1, err
	}
	// DEDICATED writable keystore for the proxy CA cert+key (constraint #6): NOT
	// the read-only App-key keystore ResolveApp returns.
	caKeystore := keystore.NewFileKeystore(configDir)

	// Best-effort sweep of orphaned per-run ledgers (same backstop as srt mode).
	if err := approvals.Sweep(stateDir, 24*time.Hour, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "rein: warning: orphan approval sweep: %v\n", err)
	}

	// (4) Per-run identity + teardown ledger.
	runID, err := newRunID()
	if err != nil {
		return 1, fmt.Errorf("generate run id: %w", err)
	}

	// (5) Working tree (agent-writable, granted via nono `--allow`). Default cwd;
	// REIN_SANDBOX_WORKDIR overrides. Symlink-resolve before it is used anywhere.
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
	workTree, err = proxy.ResolveAbs(workTree)
	if err != nil {
		return 1, fmt.Errorf("resolve working tree symlinks: %w", err)
	}

	// (5b) EXTRA egress allowlist (default agent endpoint + REIN_ALLOW_DOMAINS +
	// session allow_domains). Egress-allowed but NEVER injected; a malformed
	// entry fails the launch closed. Same resolver srt uses (neutral package).
	extraDomains, egressWarnings, err := sandboxutil.ResolveExtraAllowedDomains(sess.AllowDomains, os.Getenv(sandboxutil.EnvAllowDomains))
	if err != nil {
		return 1, fmt.Errorf("resolve extra allowed egress domains: %w", err)
	}
	for _, wmsg := range egressWarnings {
		fmt.Fprintf(os.Stderr, "rein: EGRESS WARNING: %s\n", wmsg)
	}

	// (5c) The developer's mapped local checkouts (#64) — each bound writable via
	// a nono `--allow` grant. Fail closed on every mismatch (worktree.Resolve).
	// nono has no bind-mount rename-escape surface, so srt's .git-hardening gate
	// does not apply here.
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
	// allow paths = the working tree + every mapped checkout (read+write grants).
	allowPaths := make([]string, 0, len(wt.Bindings)+1)
	allowPaths = append(allowPaths, workTree)
	for _, b := range wt.Bindings {
		allowPaths = append(allowPaths, b.Path)
	}

	// (6) Per-run runtime dir for the proxy's unix socket — user-writable, NOT
	// under the working tree. nono reaches the proxy over the LOOPBACK front, not
	// this socket, but runbroker still creates+serves it (dual-front during the
	// pivot; the socket is unreachable from the nono sandbox). Placement-checked
	// against the writable paths so it never lands inside one.
	runtimeBase := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeBase == "" {
		runtimeBase = stateDir
	}
	socketDir := filepath.Join(runtimeBase, "rein", "run-"+runID)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return 1, fmt.Errorf("create runtime socket dir: %w", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "proxy.sock")

	// (7) Per-run readable temp dir for the CA bundle + the emitted profile. NOT
	// under any writable/allowed path. The CA bundle is granted to the agent via
	// the profile's filesystem.read_file; the profile itself is read HOST-SIDE by
	// nono (never granted to the agent).
	runTmp, err := os.MkdirTemp("", "rein-nono-*")
	if err != nil {
		return 1, fmt.Errorf("create run temp dir: %w", err)
	}
	defer os.RemoveAll(runTmp)

	// (8) Per-run audit trail (design §6), under stateDir (unreadable in-sandbox).
	var auditW io.Writer
	auditPath := auditLogPath(stateDir, runID)
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "rein: warning: could not create audit dir (%v); proceeding without a per-run audit log\n", err)
	} else if af, oerr := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); oerr != nil {
		fmt.Fprintf(os.Stderr, "rein: warning: could not open audit log (%v); proceeding without one\n", oerr)
	} else {
		defer af.Close()
		auditW = af
	}

	// (9) Start the in-process broker/proxy with the loopback front enabled — the
	// shared spine (run_broker.go). Same mints/scope/declare/approval as srt.
	host, err := startRunBroker(runBrokerParams{
		sess:          sess,
		sessSource:    sessSource,
		appCfg:        appCfg,
		ks:            ks,
		caKeystore:    caKeystore,
		stateDir:      stateDir,
		runID:         runID,
		socketPath:    socketPath,
		forbiddenDirs: allowPaths,
		loopbackFront: true,
		auditW:        auditW,
		logger:        logger,
	})
	if err != nil {
		return 1, fmt.Errorf("start broker/proxy: %w", err)
	}
	defer host.Close()
	defer func() { _ = approvals.ClearRun(stateDir, runID) }()
	defer func() { revokeRunWriteTokens(stateDir, runID, productionRevoke(sess), time.Now()) }()

	loopbackPort := host.LoopbackPort()
	if loopbackPort == 0 {
		return 1, errors.New("loopback proxy front did not bind a port (internal error) — refusing to launch")
	}

	// (10) CA bundle = system roots + rein CA, written to the readable temp dir.
	// System roots are REQUIRED: CDN hosts get direct TLS with GitHub's real cert
	// (a rein-only bundle would reject them). The four CA env vars point at this
	// bundle via the profile's environment.set_vars.
	bundle, err := srt.BuildCABundle(host.CACertPEM())
	if err != nil {
		return 1, fmt.Errorf("build CA bundle: %w", err)
	}
	bundlePath := filepath.Join(runTmp, "ca-bundle.pem")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		return 1, fmt.Errorf("write CA bundle: %w", err)
	}

	// (10b) NON-IMPERSONATING git identity (CP4). Resolve the bot author/committer
	// OUTSIDE the sandbox (network + host `git config` at launch) and inject it as
	// GIT_CONFIG_* via the profile. Fail-open: gitidentity.Resolve never errors —
	// every fallback is a valid, non-impersonating identity.
	cachePath, err := gitIdentityCachePath()
	if err != nil {
		return 1, err
	}
	owner := ""
	if len(sess.Repos) > 0 {
		owner = ownerFromRepo(sess.Repos[0])
	}
	gitID := resolveGitIdentity(appCfg.ClientID, ks, owner, "", cachePath, logger)
	logger.Printf("git identity: author/committer = %q <%s>", gitID.Name, gitID.Email)

	// (11) Generate + write the nono profile. Build enforces the six security
	// invariants and fails CLOSED rather than emit a permissive profile.
	profile, err := nono.Build(buildNonoParams(loopbackPort, bundlePath, extraDomains, nonoGitIdentityConfig(gitID)))
	if err != nil {
		return 1, fmt.Errorf("build nono profile: %w", err)
	}
	profileJSON, err := profile.MarshalIndent()
	if err != nil {
		return 1, fmt.Errorf("marshal nono profile: %w", err)
	}
	profilePath := filepath.Join(runTmp, "profile.json")
	if err := os.WriteFile(profilePath, profileJSON, 0o600); err != nil {
		return 1, fmt.Errorf("write nono profile: %w", err)
	}

	// (12) Build the nono argv: managed path, run, the profile, one --allow per
	// writable path, then the agent command after "--".
	nonoArgv := []string{"run", "--profile", profilePath}
	for _, p := range allowPaths {
		nonoArgv = append(nonoArgv, "--allow", p)
	}
	nonoArgv = append(nonoArgv, "--")
	nonoArgv = append(nonoArgv, cmdline...)

	// (13) Scrubbed exec environment. nono OWNS HTTP(S)_PROXY/NO_PROXY and the
	// profile's set_vars carries the CA + git config — rein must not set those.
	// Scrub ambient GitHub tokens (the agent uses only rein-brokered,
	// downstream-injected credentials) AND $TMUX/$TMUX_PANE (design §3e:
	// defense-in-depth — af_unix_mediation already blocks the approval socket, but
	// don't leak its path into the sandbox in the first place).
	execEnv := os.Environ()
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "TMUX", "TMUX_PANE"} {
		execEnv = unsetEnv(execEnv, name)
	}

	printNonoBanner(os.Stderr, sess, sessSource, loopbackPort, allowPaths, extraDomains, cmdline)

	cmd := exec.Command(nonoPath, nonoArgv...)
	cmd.Env = execEnv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = workTree

	if err := cmd.Start(); err != nil {
		return 127, fmt.Errorf("start nono: %w", err)
	}
	// Forward SIGTERM-to-rein to the nono child (SIGINT reaches both via the
	// shared process group; mirrors srt/direct mode).
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
	return 1, fmt.Errorf("wait nono: %w", waitErr)
}

// printNonoBanner is the human-facing launch summary for nono mode: the backend,
// the loopback port GitHub egress tunnels through, the writable paths, and the
// declared/allowed domains. The agent sees none of this (§3).
func printNonoBanner(w io.Writer, sess session.Session, sessSource string, loopbackPort int, allowPaths, extraDomains, cmdline []string) {
	fmt.Fprintln(w, "rein: launching SANDBOXED (nono) run:")
	fmt.Fprintf(w, "  session: %s (role=%s, repos=%v) [source=%s]\n", sess.ID, sess.Role, sess.Repos, sessSource)
	fmt.Fprintf(w, "  GitHub egress tunnels through rein's loopback proxy: 127.0.0.1:%d\n", loopbackPort)
	fmt.Fprintln(w, "  the agent sees NO real token; git/gh are injected at the proxy (downstream of the sandbox).")
	fmt.Fprintln(w, "  credentials are hidden (deny_credentials); the approval channel is isolated (af_unix_mediation).")
	fmt.Fprintf(w, "  writable in sandbox (--allow): %v\n", allowPaths)
	if len(extraDomains) > 0 {
		fmt.Fprintf(w, "  extra egress ALLOWED (direct, NOT injected, no rein token): %v\n", extraDomains)
	}
	sess.WarnIgnoredIssue(w)
	fmt.Fprintln(w, "  writes are LOCKED until the agent declares its issue:  rein declare <n>")
	fmt.Fprintln(w, "  the declaration will prompt on THIS terminal.")
	fmt.Fprintln(w, "rein: running:", strings.Join(cmdline, " "))
	fmt.Fprintln(w, "---")
}
