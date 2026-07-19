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
// Containment gate: before exec'ing the agent, runNono runs nono.VerifyContainment
// (the §3e launch gate, nono's counterpart of srt's VerifyConfigApplied) through a
// real `nono run` under the emitted profile and FAILS CLOSED on any leak. UDP-open
// stays a warning (Policy.FailOnUDP unset), matching the prober's residual finding.
//
// In-sandbox `rein declare <n>`: rein stages its own binary into runTmp, exec-grants
// it via `--read-file`, and prepends runTmp to nono's launch-env PATH (step 11b).
//
// claude and gh get rein-owned, writable CONFIG_DIR overlays (steps 5d/5e), since
// nono's default-deny fs hides the host ~/.claude / ~/.claude.json and ~/.config/gh.
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

	"github.com/TomHennen/rein/internal/agentenv"
	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/gitidentity"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/nono"
	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/sandboxutil"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
	"github.com/TomHennen/rein/internal/worktree"
)

// buildNonoParams assembles the per-run inputs to nono.Build from the resolved
// loopback port, CA-bundle path, extra egress domains, and the non-impersonating
// git identity (as extra GIT_CONFIG_* entries). Pure (no I/O) so a unit test can
// pin that the loopback port flows into upstream_proxy, the CA path is carried,
// and the git identity is injected — without a live launch. The security host
// lists (inject / CDN / declare) are NOT passed here — nono.Build reads them
// straight from internal/proxy so the profile and the proxy can never drift.
func buildNonoParams(loopbackPort int, caBundlePath string, extraDomains []string, extraGitConfig []nono.GitConfig, claudeConfigDir, ghConfigDir string) nono.Params {
	return nono.Params{
		ListenAddr:      "127.0.0.1:" + strconv.Itoa(loopbackPort),
		CACertPath:      caBundlePath,
		ExtraDomains:    extraDomains,
		ExtraGitConfig:  extraGitConfig,
		ClaudeConfigDir: claudeConfigDir,
		GhConfigDir:     ghConfigDir,
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

// briefNonoAgent builds the sandbox contract (#63) for a nono run and returns
// the launch argv (contract injected into claude's --append-system-prompt; other
// agents' argv unchanged), the contract text, the one-line banner status, and
// whether the contract should be PRINTED to the agent's stdout (its only channel
// when it is not claude). Pure so the wiring is unit-testable without a live
// launch. Ports the contract mechanism the (now-deleted) srt path used; off
// honors REIN_DISABLE_AGENT_CONTRACT.
func briefNonoAgent(cmdline []string, p contractParams, off bool) (argv []string, contract, statusLine string, printToStdout bool) {
	if off {
		return cmdline, "", contractStatus(true, false), false
	}
	contract = buildAgentContract(p)
	argv, injected := injectContract(cmdline, contract)
	// Non-claude agents have no system-prompt channel: the caller prints the
	// contract to stdout instead (weaker, but honest — the banner says which).
	return argv, contract, contractStatus(false, injected), !injected
}

// runNono is the entry point for `rein run --nono -- <cmd>`. cmdline is the
// agent argv (after "--"). Returns (exitCode, error).
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
	// sandbox's first git op).
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
	allowPaths := make([]string, 0, len(wt.Bindings)+2)
	allowPaths = append(allowPaths, workTree)
	for _, b := range wt.Bindings {
		allowPaths = append(allowPaths, b.Path)
	}

	// (5d) Rein-owned, PERSISTENT, agent-WRITABLE CLAUDE_CONFIG_DIR overlay (#94 —
	// the nono counterpart of srt's sandbox_claude_home.go, reusing the SAME
	// host-side seeding helper unchanged; it is substrate-neutral). Host ~/.claude
	// and ~/.claude.json are hidden by nono's default-deny fs (nothing grants them),
	// so claude is repointed at this overlay via the profile's CLAUDE_CONFIG_DIR
	// (set below) and can still run + resume across runs. Bound WRITABLE via a nono
	// --allow grant (appended to allowPaths). Prepared HOST-SIDE, before launch.
	home, err := os.UserHomeDir()
	if err != nil {
		return 1, fmt.Errorf("resolve home dir for claude overlay: %w", err)
	}
	claudeOverlay, err := prepareClaudeOverlay(logger, home)
	if err != nil {
		return 1, fmt.Errorf("prepare claude overlay: %w", err)
	}
	claudeOverlay, err = proxy.ResolveAbs(claudeOverlay)
	if err != nil {
		return 1, fmt.Errorf("resolve claude overlay: %w", err)
	}
	allowPaths = append(allowPaths, claudeOverlay)

	// (5e) Rein-owned, per-run, agent-WRITABLE GH_CONFIG_DIR overlay (the gh twin
	// of 5d). Host ~/.config/gh is hidden by nono's default-deny fs; without an
	// override gh EACCESes on it and refuses to start. Point gh at this writable
	// overlay (scaffolded with a placeholder hosts.yml so gh sends requests; the
	// proxy injects the real token downstream). Bound WRITABLE via a nono --allow.
	ghOverlay, err := prepareGhOverlay("")
	if err != nil {
		return 1, fmt.Errorf("prepare gh overlay: %w", err)
	}
	defer os.RemoveAll(ghOverlay)
	ghOverlay, err = proxy.ResolveAbs(ghOverlay)
	if err != nil {
		return 1, fmt.Errorf("resolve gh overlay: %w", err)
	}
	allowPaths = append(allowPaths, ghOverlay)

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
	bundle, err := sandboxutil.BuildCABundle(host.CACertPEM())
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
	profile, err := nono.Build(buildNonoParams(loopbackPort, bundlePath, extraDomains, nonoGitIdentityConfig(gitID), claudeOverlay, ghOverlay))
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

	// (11b) Stage the rein binary so the agent can run `rein declare <n>`
	// IN-SANDBOX exactly as the deny messages instruct (the nono counterpart of
	// srt's ExtraPathDir staging). Two facts drove this shape:
	//   - nono REJECTS a set_vars PATH ("PATH is reserved") but PASSES THROUGH the
	//     PATH of nono's own launch env — so we prepend stagedDir to execEnv's PATH
	//     (below), not the profile.
	//   - a bare `--read-file <bin>` grant alone lets nono exec the binary (no
	//     parent --allow needed), so runTmp stays NON-writable to the agent — no
	//     PATH-shadowing hole (the agent cannot overwrite the rein on its PATH).
	self, err := resolveSelf()
	if err != nil {
		return 1, fmt.Errorf("resolve rein binary for in-sandbox staging: %w", err)
	}
	stagedRein := filepath.Join(runTmp, "rein")
	if err := copyFile(self, stagedRein, 0o755); err != nil {
		return 1, fmt.Errorf("stage in-sandbox rein binary: %w", err)
	}

	// (11c) CONTAINMENT GATE (design §3e): before exec'ing the agent, launch the
	// prober through a REAL `nono run` under the profile Build emits and FAIL
	// CLOSED on any leak (credentials readable, planted loopback reachable, direct
	// external TCP allowed). Point it at rein's real listener + CA bundle so the
	// gated config matches the launch. UDP-open stays a WARNING (FailOnUDP unset) —
	// the accepted residual from the prober findings. If nono is somehow absent
	// (it isn't — we digest-verified it above), skip cleanly rather than block.
	fmt.Fprintln(os.Stderr, "rein: verifying nono containment (creds hidden; loopback + direct egress denied)…")
	verdict, verifyErr := nono.VerifyContainment(nono.VerifyParams{
		ReinBin:      stagedRein,
		NonoBin:      nonoPath,
		CACertPath:   bundlePath,
		ListenAddr:   "127.0.0.1:" + strconv.Itoa(loopbackPort),
		ExtraDomains: extraDomains,
	})
	if verifyErr != nil {
		if errors.Is(verifyErr, nono.ErrNonoUnavailable) {
			fmt.Fprintf(os.Stderr, "rein: warning: containment probe skipped (nono unavailable): %v\n", verifyErr)
		} else {
			return 1, fmt.Errorf("nono containment gate failed (refusing to launch): %w", verifyErr)
		}
	} else {
		for _, w := range verdict.Warnings() {
			fmt.Fprintf(os.Stderr, "rein: containment WARNING: %s\n", w)
		}
		fmt.Fprintln(os.Stderr, "rein: containment gate passed.")
	}

	// (11d) SANDBOX CONTRACT (#63). Everything the banner says goes to the HUMAN's
	// terminal; the agent sees none of it. Brief the AGENT itself: claude gets it
	// in its system prompt (--append-system-prompt); every other agent gets it
	// printed into the sandbox's own stdout. Under nono $HOME is always ephemeral
	// (default-deny fs), and the real working tree is always bound (no #64
	// unhardenable-cwd fallback), so WorkTreeEphemeral stays false.
	contractOff := agentenv.DisableClaudeMCPFromEnv(os.Getenv(EnvDisableAgentContract))
	agentArgv, contract, contractLine, printContract := briefNonoAgent(cmdline, contractParams{
		WorkTree:      workTree,
		HomeEphemeral: true,
		ExtraDomains:  extraDomains,
	}, contractOff)

	// (12) Build the nono argv: managed path, run, the profile, one --allow per
	// writable path, a read-only grant on the staged rein (exec, not write), then
	// the (contract-injected) agent command after "--".
	nonoArgv := []string{"run", "--profile", profilePath}
	for _, p := range allowPaths {
		nonoArgv = append(nonoArgv, "--allow", p)
	}
	nonoArgv = append(nonoArgv, "--read-file", stagedRein)
	nonoArgv = append(nonoArgv, "--")
	nonoArgv = append(nonoArgv, agentArgv...)

	// (13) Scrubbed exec environment. nono owns HTTP(S)_PROXY/NO_PROXY and the
	// profile's set_vars carries the CA + git config, so rein must not set those.
	// Scrub ambient GitHub tokens (the agent uses only rein-brokered,
	// downstream-injected credentials), $TMUX/$TMUX_PANE (§3e defense-in-depth: the
	// profile's af_unix_mediation already blocks the approval socket, but don't leak
	// its path in either), and CLAUDE_CONFIG_DIR/GH_CONFIG_DIR (a stale value from a
	// dev's own claude session must not ride along — the profile's set_vars override
	// wins regardless).
	execEnv := os.Environ()
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "TMUX", "TMUX_PANE", "CLAUDE_CONFIG_DIR", "GH_CONFIG_DIR"} {
		execEnv = unsetEnv(execEnv, name)
	}
	// Prepend the staged-rein dir to PATH: nono passes its OWN launch env's PATH
	// through to the sandbox (verified — unlike set_vars PATH, which nono rejects
	// as reserved), so this is what puts `rein` on the in-sandbox PATH for
	// `rein declare <n>`. Prepend so the staged copy wins over any host rein.
	execEnv = setEnv(execEnv, "PATH", runTmp+string(os.PathListSeparator)+os.Getenv("PATH"))

	printNonoBanner(os.Stderr, sess, sessSource, loopbackPort, allowPaths, extraDomains, cmdline)
	// How the agent was briefed with the contract above (injected / printed /
	// disabled) — one honest line so the operator isn't told the agent was briefed
	// when it wasn't.
	fmt.Fprintln(os.Stderr, contractLine)
	// Non-claude agents: print the contract where the AGENT's own output goes, so
	// it lands in its transcript rather than only on the human's side.
	if printContract {
		fmt.Fprintln(os.Stdout, contract)
	}

	cmd := exec.Command(nonoPath, nonoArgv...)
	cmd.Env = execEnv
	// stdin: nono's supervisor reads/forwards the child's stdin, which races the
	// host's inline /dev/tty approval read (bwrap/srt is transparent and never
	// contends). When the tmux popup is NOT the primary approval path
	// (PopupPreferenceFromEnv()==false: REIN_APPROVAL=tty or no $TMUX), the host
	// must own the terminal exclusively, so give the child /dev/null. With the
	// popup, the interactive agent needs the pty and the popup uses its own, so
	// keep os.Stdin.
	if grant.PopupPreferenceFromEnv() {
		cmd.Stdin = os.Stdin
	} else {
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			return 1, fmt.Errorf("open %s for sandbox stdin: %w", os.DevNull, err)
		}
		defer devNull.Close()
		cmd.Stdin = devNull
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = workTree
	// Run nono in its OWN session (no controlling terminal), the nono equivalent of
	// srt's bwrap --new-session. Two load-bearing effects:
	//   1. the host `rein run` process stays the sole owner of the controlling
	//      terminal, so the declare Form A prompt's /dev/tty read is NOT contended
	//      by the sandbox child (without this the child shares the terminal and the
	//      host prompt cancels — the agent's declare never gets confirmed).
	//   2. the sandboxed agent has no controlling terminal, so it cannot open
	//      /dev/tty to read/answer the approval prompt itself (self-approval hole,
	//      the tty-asymmetry srt closes the same way).
	// Also satisfies the run-discipline rule that a nono launch never shares the
	// caller's session (it must not touch the operator's tmux/session).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 127, fmt.Errorf("start nono: %w", err)
	}
	// Forward SIGTERM-to-rein to the nono child. nono is its own session leader
	// (Setsid above), so signal cmd.Process directly; nono propagates to its tree.
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
