// rein is the credential broker CLI.
//
// Phase 0 subcommands:
//   - credential-helper {get|store|erase}: drives the git credential-helper
//     protocol; reads target App config from REIN_* env vars; routes
//     between read and write tiers per the REIN_GIT_OP env (set by the
//     rein-git shim) with a process-tree fallback on Linux.
//   - install-shim: writes rein-git, rein-gh, and rein binaries into a
//     known shim directory under $XDG_STATE_HOME/rein/shim.
//   - gh-auth: mints a read-tier token for the `gh` CLI and writes a
//     sourceable env file (CP3.5).
//   - approval {status|clear|grant}: inspect, revoke, or interactively
//     grant per-run human approvals (keyed by REIN_RUN_ID). status lists
//     all runs; clear takes --run-id <id> (or clears all); grant takes
//     --run-id <id> (CP5/CP5.5).
//   - run -- <cmd> [args...]: launch the wrapped command with rein's
//     git credential helper, gh shim, and session scope ceiling
//     in effect — without polluting the user's global git config (CP6).
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/broker"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/internal/tokencache"
	"github.com/TomHennen/rein/internal/ui/grant"
)

// dispatchRun routes `rein run` to the sandboxed (srt) path when --sandbox is
// present, else to direct mode (run.go). --sandbox is a CP3 opt-in; CP4 makes
// sandboxed the default where srt is healthy. The flag must appear BEFORE the
// "--" separator: `rein run --sandbox -- <cmd>`.
func dispatchRun(argv []string) (int, error) {
	if len(argv) > 0 && argv[0] == "--sandbox" {
		cmdline, err := parseRunArgs(argv[1:])
		if err != nil {
			return 2, err
		}
		return runSandboxed(cmdline)
	}
	return runWrapped(argv)
}

const (
	// mintTimeout caps each installation-token mint. Git users feel this
	// latency directly when the helper is invoked, so keep it tight.
	mintTimeout = 5 * time.Second

	// approvalTTL is stamped into Record.ExpiresAt as a sweep/status
	// heuristic ONLY — it is NOT a re-prompt trigger. The RUN LIFETIME is
	// the bound now (per-run approvals keyed by REIN_RUN_ID; a stale file
	// is never reusable by a future run). 4h is retained as the orphan
	// sweep backstop value and for `status` display.
	approvalTTL = 4 * time.Hour
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "credential-helper":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		if err := runCredentialHelper(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "rein credential-helper: %v\n", err)
			os.Exit(1)
		}
	case "init":
		if err := runInit(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "rein init: %v\n", err)
			os.Exit(1)
		}
	case "doctor":
		if err := runDoctor(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "rein doctor: %v\n", err)
			os.Exit(1)
		}
	case "install-shim":
		if err := installShim(); err != nil {
			fmt.Fprintf(os.Stderr, "rein install-shim: %v\n", err)
			os.Exit(1)
		}
	case "gh-auth":
		if err := ghAuth(); err != nil {
			fmt.Fprintf(os.Stderr, "rein gh-auth: %v\n", err)
			os.Exit(1)
		}
	case "approval":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		if err := runApproval(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "rein approval: %v\n", err)
			os.Exit(1)
		}
	case "run":
		code, err := dispatchRun(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "rein run: %v\n", err)
			os.Exit(code)
		}
		os.Exit(code)
	case "__sandbox-probe":
		// Hidden subcommand: runs INSIDE srt during VerifyConfigApplied to
		// prove the deny-read + seccomp protections took effect. Exits with a
		// srt.Probe* code the parent interprets. Not user-facing.
		os.Exit(srt.RunProbe(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "rein: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  rein init [--owner=<login>] [--port=<n>] [--skip-audit] [--force] [--skip-mint-check] [--no-symlink] [--no-alias] [--shell=bash|zsh|fish]")
	fmt.Fprintln(os.Stderr, "  rein doctor")
	fmt.Fprintln(os.Stderr, "  rein credential-helper {get|store|erase}")
	fmt.Fprintln(os.Stderr, "  rein install-shim")
	fmt.Fprintln(os.Stderr, "  rein gh-auth")
	fmt.Fprintln(os.Stderr, "  rein approval status")
	fmt.Fprintln(os.Stderr, "  rein approval clear [--run-id <id>]")
	fmt.Fprintln(os.Stderr, "  rein approval grant --run-id <id>")
	fmt.Fprintln(os.Stderr, "  rein run [--sandbox] -- <cmd> [args...]")
}

// runCredentialHelper wires env-derived config to the broker. All errors
// returned here are programming/config errors — credential-mint failures
// are handled inside the broker per TM-G8.
func runCredentialHelper(action string) error {
	// ResolveApp is the read-only resolver: env -> state.json+keystore ->
	// fail. It NEVER touches the network — rein run's eager step owns the
	// install-id fetch. On the state path InstallationID may be 0 (uncached);
	// that is NOT an error here. The only error case is "no config at all"
	// (no env AND no state), which is a genuine config error with no
	// github.com host yet known — same shape as today's LoadAppConfig error.
	logger, closeLog, err := openLog()
	if err != nil {
		return err
	}
	defer closeLog()

	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}

	appCfg, ks, _, rerr := config.ResolveApp()
	if rerr != nil {
		// No config at all (no env AND no state.json). TM-G8: do NOT exit
		// with empty stdout — an empty credential invites git/tooling to
		// run `gh auth setup-git` and silently displace the broker
		// (validated finding §12.1). Route through the broker with a
		// failing minter so a github.com get yields the placeholder plus
		// an actionable stderr diagnostic; non-github hosts still get an
		// empty block (correct). ReadCachePath is intentionally empty so
		// an unconfigured rein never serves a stale cached token.
		logger.Printf("ResolveApp failed: %v; serving TM-G8 placeholder for github.com get", rerr)
		failMint := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
			return "", time.Time{}, rerr
		})
		return broker.RunCredentialHelper(action, os.Stdin, os.Stdout, broker.Config{
			MintRead:    failMint,
			MintWrite:   failMint,
			MintTimeout: mintTimeout,
			Logger:      logger,
			Diag:        os.Stderr,
		})
	}

	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return err
	}
	// Scope the App config to the session's FULL repo set so the minted token
	// covers every repo the scope check accepts (issue #10).
	appCfg.RepoNames = sess.BareRepoNames()
	logger.Printf("session: id=%q role=%q repos=%v source=%s", sess.ID, sess.Role, sess.Repos, sessSource)

	return runCredentialHelperWithConfig(action, os.Stdin, os.Stdout, os.Stderr, appCfg, ks, sess, stateDir, logger)
}

// runCredentialHelperWithConfig is the testable core of the credential
// helper. It is the seam that lets a test feed an InstallationID==0 config
// + a github.com `get` request and assert the broker writes the TM-G8
// placeholder.
//
// TM-G8 preservation: githubapp.Client is constructed LAZILY inside each
// MintFunc/Revoke closure. A missing install-id (NewClient rejects 0), a
// keystore failure, or any other construction error therefore surfaces as a
// MintFunc error, which broker.serveRead/serveWrite turn into the
// rein-placeholder-mint-failed credential — never an early return, never an
// empty credential. There must be NO error return between ResolveApp and this
// call on the github.com path.
func runCredentialHelperWithConfig(action string, in io.Reader, out, diag io.Writer, appCfg githubapp.Config, ks keystore.Keystore, sess session.Session, stateDir string, logger *log.Logger) error {
	mintRead := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		return client.MintReadOnlyToken(ctx)
	})
	mintWrite := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		return client.MintWriteToken(ctx)
	})
	// Revoke is best-effort (broker logs+ignores the error). If the client
	// can't be built, swallow it — a token we couldn't mint can't need
	// revoking, and erroring here serves no one.
	revoke := func(ctx context.Context, token string) error {
		client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
		if err != nil {
			return nil
		}
		return client.RevokeToken(ctx, token)
	}

	cfg := broker.Config{
		MintRead:      mintRead,
		MintWrite:     mintWrite,
		MintTimeout:   mintTimeout,
		Logger:        logger,
		Diag:          diag,
		ReadCachePath: filepath.Join(stateDir, "cache", "read-token.json"),
		DetectWrite:   func() bool { return detectWriteIntent(logger) },
		Revoke:        revoke,
		InScope:       sess.Contains,
		// EmptyPathScope stays "" (= allow): existing test setups
		// without useHttpPath=true continue to work. install-shim's
		// instructions recommend setting useHttpPath for strict
		// enforcement.
		ConfirmWrite: buildConfirmWrite(sess, stateDir, logger),
	}

	// Ledger minted write tokens for exit-time revocation (issue #20), but
	// ONLY when invoked inside a `rein run` (REIN_RUN_ID set) — that's the
	// process that drains the ledger on child exit. A helper invoked outside
	// `rein run` has no such parent, so recording would only leak a token
	// value to disk that nothing ever revokes; skip it (the token still
	// expires on GitHub's native ~1h TTL). Best-effort: an append failure is
	// logged and ignored (broker recovers panics; TM-G8 is unaffected).
	if runID := os.Getenv("REIN_RUN_ID"); runID != "" {
		cfg.RecordWrite = func(token string, expiresAt time.Time) {
			if err := approvals.AppendWriteToken(stateDir, runID, tokencache.Entry{Token: token, ExpiresAt: expiresAt}); err != nil {
				logger.Printf("write-token ledger append failed (best-effort): %v", err)
			}
		}
	}

	return broker.RunCredentialHelper(action, in, out, cfg)
}

// buildConfirmWrite returns a ConfirmWrite predicate that delegates to
// internal/ui/grant's layered approval flow: check existing record →
// try /dev/tty → tmux popup if $TMUX → helpful stderr + deny.
//
// Returns nil when the session doesn't bind an issue — no prompt
// requested, no approval needed. Tests bypass this by constructing
// broker.Config directly.
func buildConfirmWrite(sess session.Session, stateDir string, logger *log.Logger) func(repo string) bool {
	if sess.Issue == 0 {
		if isWriteCapableRole(sess.Role) {
			logger.Printf("WARN: ConfirmWrite disabled for write-capable role %q (session has no `issue:` field). Write tokens will mint without human confirmation. Add `issue: <number>` to the session file to enable the prompt.", sess.Role)
		} else {
			logger.Printf("ConfirmWrite: disabled (session has no bound issue)")
		}
		return nil
	}
	cfg := grant.Config{
		StateDir:      stateDir,
		RunID:         os.Getenv("REIN_RUN_ID"),
		RunPID:        envInt("REIN_RUN_PID"),
		TTL:           approvalTTL,
		PromptTimeout: 60 * time.Second,
		Logger:        logger,
		// Stderr, Prompter, TmuxRunner default to production
		// (os.Stderr, TTYPrompter, DefaultTmuxRunner).
	}
	return func(repo string) bool {
		return grant.ObtainApproval(context.Background(), grant.Request{
			Session: sess,
			Action:  "git push (write token mint)",
			Repo:    repo,
		}, cfg)
	}
}

// runApproval handles `rein approval {status|clear|grant}` with per-run
// scoping:
//   - status: list ALL active runs (run-id, session, approved/expiry).
//   - clear [--run-id X]: clear one run's files, or ALL with no flag
//     (plus any legacy global approval.json).
//   - grant --run-id X: interactively approve write access for run X. The
//     session is read from runs/X.json (written by the helper) — NOT the
//     default session — so approving from another terminal targets the
//     right concurrent run. Reads the issue-number answer via /dev/tty
//     only (--run-id is routing, not the secret) — see package
//     internal/ui/grant for why.
func runApproval(args []string) error {
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "rein approval: want status|clear|grant")
		os.Exit(2)
	}
	sub := args[0]
	runID := parseRunIDFlag(args[1:])

	switch sub {
	case "status":
		return approvalStatus(stateDir)
	case "clear":
		return approvalClear(stateDir, runID)
	case "grant":
		if runID == "" {
			return fmt.Errorf("grant requires --run-id <id> (see the `rein: write blocked` message for the id)")
		}
		logger, closeLog, err := openLog()
		if err != nil {
			return err
		}
		defer closeLog()
		logger.Printf("approval grant: run-id=%s", runID)
		cfg := grant.Config{
			StateDir:      stateDir,
			RunID:         runID,
			RunPID:        envInt("REIN_RUN_PID"),
			TTL:           approvalTTL,
			PromptTimeout: 60 * time.Second,
			Logger:        logger,
		}
		if err := grant.Grant(context.Background(), cfg); err != nil {
			return err
		}
		fmt.Printf("approval recorded for run %s\n", runID)
		return nil
	default:
		fmt.Fprintf(os.Stderr, "rein approval: unknown subcommand %q (want status|clear|grant)\n", sub)
		os.Exit(2)
	}
	return nil
}

// parseRunIDFlag extracts `--run-id X` or `--run-id=X` from args. Returns
// "" if absent. The issue number is NEVER a flag (it arrives via
// /dev/tty); only --run-id (routing) is accepted here.
func parseRunIDFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--run-id" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if v, ok := strings.CutPrefix(a, "--run-id="); ok {
			return v
		}
	}
	return ""
}

// approvalStatus lists all known runs.
func approvalStatus(stateDir string) error {
	list, err := approvals.List(stateDir)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no active runs")
		return nil
	}
	for _, st := range list {
		liveness := "dead"
		if st.Live {
			liveness = "live"
		}
		fmt.Printf("run-id: %s   pid: %d (%s)\n", st.RunID, st.Context.RunPID, liveness)
		if st.HasContext {
			s := st.Context.Session
			fmt.Printf("  session:    %s role=%s repos=%v issue=#%d\n", s.ID, s.Role, s.Repos, s.Issue)
		} else {
			fmt.Println("  session:    <no run context on disk>")
		}
		if st.HasApproval {
			fmt.Printf("  approval:   VALID (approved %s)\n", st.Approval.ApprovedAt.Format(time.RFC3339))
		} else {
			fmt.Println("  approval:   none (will prompt)")
		}
	}
	return nil
}

// approvalClear clears one run (runID != "") or ALL runs (runID == ""),
// including any legacy global approval.json.
func approvalClear(stateDir, runID string) error {
	if runID != "" {
		if err := approvals.ClearRun(stateDir, runID); err != nil {
			return err
		}
		fmt.Printf("cleared run %s\n", runID)
		return nil
	}
	list, err := approvals.List(stateDir)
	if err != nil {
		return err
	}
	for _, st := range list {
		if err := approvals.ClearRun(stateDir, st.RunID); err != nil {
			return err
		}
	}
	// Best-effort removal of the pre-upgrade global approval file.
	_ = os.Remove(filepath.Join(stateDir, "approval.json"))
	fmt.Printf("cleared %d run(s)\n", len(list))
	return nil
}

// isWriteCapableRole returns true for the design's roles whose
// implement-tier permissions include write access. Used by
// buildConfirmWrite to decide whether silent-disable (no prompt)
// warrants a WARN. CP4-CP5 roles are coarse; CP6+ will move this
// to the role catalog.
func isWriteCapableRole(role string) bool {
	switch role {
	case "implement", "triage", "review", "release":
		return true
	}
	return false
}

// envInt parses a non-negative integer env var, returning 0 on unset or
// parse error (e.g. REIN_RUN_PID). 0 means "unknown" to the approval
// snapshot's Sweep liveness probe.
func envInt(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// bareRepoName extracts "name" from "owner/name". The App installation
// pins the owner, so the mint API only accepts the bare name.
func bareRepoName(ownerSlashName string) string {
	_, name, ok := strings.Cut(ownerSlashName, "/")
	if !ok {
		return ownerSlashName
	}
	return name
}

// detectWriteIntent is the Shape B discriminator. Primary signal: REIN_GIT_OP
// in this process's environment, set by the rein-git shim before git was
// invoked. Fallback: a per-platform proc-tree walk that looks for `git push`
// or `git send-pack` in the ancestor chain. The walk is implemented in
// proctree_{linux,darwin,other}.go; platforms without a real implementation
// have a no-op stub that returns no match.
//
// Fail-closed: returns false (read) when no positive evidence of a write op
// exists. Misclassification at this layer routes a push through the read
// path and yields a 403 — observable and recoverable. The reverse would
// silently over-grant.
func detectWriteIntent(logger *log.Logger) bool {
	switch op := os.Getenv("REIN_GIT_OP"); op {
	case "write":
		logger.Printf("write intent: REIN_GIT_OP=write (shim)")
		return true
	case "read":
		logger.Printf("read intent: REIN_GIT_OP=read (shim)")
		return false
	case "":
		// No shim signal; fall through to fallback.
	default:
		logger.Printf("REIN_GIT_OP=%q is unrecognized; falling back to process-tree detection", op)
	}

	if procTreePlatform == "unsupported" {
		logger.Printf("no REIN_GIT_OP and proc-tree fallback not implemented for %q; defaulting to read", runtime.GOOS)
		return false
	}

	if write, src := detectFromProcTree(); write {
		logger.Printf("write intent: process-tree fallback found %q in ancestor chain (platform=%s)", src, procTreePlatform)
		return true
	}
	return false
}

// isGitVerb returns true if argv looks like `git <verb>` or `git <opts...> <verb>`.
// Mirrors the shim's classifier in shape but only checks for one specific verb.
func isGitVerb(argv []string, verb string) bool {
	if len(argv) < 2 {
		return false
	}
	base := filepath.Base(argv[0])
	if base != "git" {
		return false
	}
	// Reuse the same global-option skipping logic as cmd/rein-git.
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "" {
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return a == verb
		}
		if optionConsumesNextArg(a) {
			i++
		}
	}
	return false
}

// optionsThatTakeArg is the keep-in-sync twin of cmd/rein-git's list.
var optionsThatTakeArg = map[string]bool{
	"-C":             true,
	"-c":             true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--exec-path":    true,
	"--attr-source":  true,
	"--config-env":   true,
	"--list-cmds":    true,
	"--super-prefix": true,
}

func optionConsumesNextArg(a string) bool {
	if strings.Contains(a, "=") {
		return false
	}
	return optionsThatTakeArg[a]
}

// ghAuth mints a gh-session installation token and writes a sourceable
// POSIX-shell file at <state-dir>/gh-env.sh that exports GH_TOKEN.
// Sourcing the file makes `gh` route through rein's mint without touching
// the user's ~/.config/gh/hosts.yml.
//
// Why GH_TOKEN env over rewriting hosts.yml: no backup/restore lifecycle,
// no risk of clobbering the user's gh auth, and matches CP6's `rein run`
// model where the env is injected per-process. Trade-off: the env file
// has to be re-sourced when the token expires (~1h GitHub-imposed TTL).
//
// Token tier: READ-ONLY (issues:read + pulls:read + contents:read +
// metadata:read), shared with the rein-gh shim's read-tier cache (CP3.7).
// The env file gets sourced into a user shell and the token sits in env
// for the shell's lifetime — read-only keeps blast radius tight if
// exfiltrated. Users who need write capability via gh should use the
// rein-gh shim (rein install-shim; PATH-front), which mints fresh write
// tokens JIT per call and revokes them when gh exits. Adding a
// `--tier=write` flag here is a deliberate future opt-in, not a default.
//
// Why only GH_TOKEN (not GITHUB_TOKEN): GITHUB_TOKEN is honored by many
// tools beyond gh (Go SDK, hub, act, gitleaks, etc.). Setting it from
// here would broadly shadow other tools' auth choices for the shell's
// lifetime. Users who want it elsewhere can `export GITHUB_TOKEN=$GH_TOKEN`
// themselves.
//
// File mode 0600 in a 0700 parent. The token is never logged. POSIX-sh
// syntax only — fish users will need to translate.
func ghAuth() error {
	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		return err
	}

	logger, closeLog, err := openLog()
	if err != nil {
		return err
	}
	defer closeLog()

	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return err
	}
	appCfg.RepoNames = sess.BareRepoNames()
	logger.Printf("gh-auth session: id=%q repos=%v source=%s", sess.ID, sess.Repos, sessSource)

	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}

	// Use the same read-tier token + cache as the rein-gh shim. The
	// token written to the env file has only {issues:read, pulls:read,
	// contents:read, metadata:read} — exfil from a shell's env grants
	// read-only capability. Users who need write capability via gh
	// should use the shim (PATH-front), which mints write tokens JIT
	// per-call and revokes them on gh exit.
	client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
	if err != nil {
		return err
	}
	token, expiresAt, err := ghsession.EnsureFresh(
		ghsession.ReadCachePath(stateDir),
		client.MintGhReadOnlyToken,
		client.RevokeToken,
		5*time.Minute,
		mintTimeout,
		logger,
	)
	if err != nil {
		return err
	}
	envPath := filepath.Join(stateDir, "gh-env.sh")
	body := fmt.Sprintf(""+
		"# rein gh-auth env (CP3.5/CP3.7). POSIX-shell syntax — sh/bash/zsh only.\n"+
		"# Source this file before launching an agent that uses `gh`:\n"+
		"#   . %s\n"+
		"# Token tier: READ-ONLY (issues:read + pulls:read + contents:read + metadata:read).\n"+
		"# For write capability via gh, use the rein-gh shim (rein install-shim;\n"+
		"# PATH-front). The shim mints fresh write tokens JIT per-call and\n"+
		"# revokes them on gh exit.\n"+
		"# Expires at: %s (re-run `rein gh-auth` and re-source before then).\n"+
		"export GH_TOKEN=%s\n",
		envPath,
		expiresAt.Format(time.RFC3339),
		shellQuote(token),
	)
	// Atomic write: CreateTemp in the destination dir, write+chmod, rename.
	// CreateTemp avoids the fixed-name race when two `rein gh-auth` runs
	// happen concurrently.
	tmp, err := os.CreateTemp(stateDir, "gh-env.sh.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write gh-env: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close gh-env: %w", err)
	}
	if err := os.Rename(tmpName, envPath); err != nil {
		return fmt.Errorf("rename gh-env: %w", err)
	}

	// Print sourcing instructions to stderr so callers piping stdout
	// (eval, command substitution) don't get noise. Stdout is silent on
	// purpose — there's no good thing to put there that doesn't risk
	// token leakage.
	fmt.Fprintf(os.Stderr, "wrote %s (mode 0600)\n", envPath)
	fmt.Fprintf(os.Stderr, "expires at %s (TTL %s)\n",
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Round(time.Second))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To activate, source the env file in the shell that will launch your agent:")
	fmt.Fprintf(os.Stderr, "  . %s\n", envPath)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Re-run `rein gh-auth` and re-source before the TTL elapses.")
	fmt.Fprintln(os.Stderr, "CP6's `rein run` will inject this env per-process automatically.")
	return nil
}

// resolveSelf returns the absolute, symlink-resolved path of the running
// rein binary. EvalSymlinks failure falls back to the unresolved path —
// the only well-known case where it errors is genuinely exotic and
// aborting init/install-shim would be a worse outcome than copying
// through one extra layer of symlink.
func resolveSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		return resolved, nil
	}
	return self, nil
}

// installShim is the top-level `rein install-shim` entry point: places
// shim binaries and then prints the full activation help. `rein init`
// uses installShimFiles directly so it can roll up the chatty
// activation help into its own end-of-run summary.
func installShim() error {
	shimDir, installed, err := installShimFiles()
	if err != nil {
		return err
	}
	for _, line := range installed {
		fmt.Println(line)
	}
	printShimActivationHelp(shimDir)
	return nil
}

// installShimFiles places the rein-git, rein-gh, and rein binaries in
// the state dir's shim subdirectory and returns one "installed X" line
// per binary for the caller to log however it wants. Idempotent — safe
// to re-run.
//
// Splitting this out from installShim lets `rein init` keep its tight
// progress output (no 14-line PATH-activation block interleaved with
// init's other steps) while standalone `rein install-shim` keeps its
// full operator-friendly explanation.
func installShimFiles() (shimDir string, installed []string, err error) {
	stateDir, err := config.StateDir()
	if err != nil {
		return "", nil, err
	}
	shimDir = filepath.Join(stateDir, "shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create shim dir: %w", err)
	}

	self, err := resolveSelf()
	if err != nil {
		return "", nil, err
	}
	selfDir := filepath.Dir(self)

	shims := []struct{ name, intent string }{
		{"rein-git", "git"},
		{"rein-gh", "gh"},
	}
	for _, s := range shims {
		src, err := locateBinary(s.name, selfDir)
		if err != nil {
			return "", nil, err
		}
		dst := filepath.Join(shimDir, s.intent)
		if err := copyFile(src, dst, 0o700); err != nil {
			return "", nil, fmt.Errorf("install %s: %w", s.intent, err)
		}
		installed = append(installed, fmt.Sprintf("installed shim: %s -> %s", dst, s.name))
	}

	// Place a copy of rein itself in the shim dir so users who prepend
	// the shim dir to PATH get `rein` available without a separate
	// install step. Useful for `rein approval grant` from a fresh
	// terminal.
	reinDst := filepath.Join(shimDir, "rein")
	if err := copyFile(self, reinDst, 0o700); err != nil {
		return "", nil, fmt.Errorf("install rein into shim dir: %w", err)
	}
	installed = append(installed, fmt.Sprintf("installed: %s (rein itself, so adding shim dir to PATH gives you `rein`)", reinDst))
	return shimDir, installed, nil
}

// printShimActivationHelp emits the operator-facing PATH-prepend +
// useHttpPath guidance. Stable output for standalone install-shim;
// suppressed by `rein init` (which rolls its own end-of-run summary).
func printShimActivationHelp(shimDir string) {
	fmt.Println()
	fmt.Println("To activate, prepend the shim dir to $PATH before launching agents:")
	fmt.Printf("  export PATH=%s:$PATH\n\n", shellQuote(shimDir))
	fmt.Println("Verify with:")
	fmt.Println("  which git gh    # both should resolve to the shim dir")
	fmt.Println()
	fmt.Println("For strict session scope enforcement (CP4+), also set:")
	fmt.Println("  git config --global credential.useHttpPath true")
	fmt.Println("Without it, the helper can't tell which repo git is asking for and")
	fmt.Println("will defer to the token's server-side scope check (still safe; less")
	fmt.Println("informative when an out-of-scope request is refused).")
	fmt.Println()
	fmt.Println("CP6's `rein run` wrapper will set PATH + git config per-wrapped-process.")
}

func locateBinary(name, selfDir string) (string, error) {
	candidate := filepath.Join(selfDir, name)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%s binary not found next to %s or on PATH; build it first (go build -o bin/%s ./cmd/%s)", name, filepath.Join(selfDir, "rein"), name, name)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, in, mode); err != nil {
		return err
	}
	return nil
}

// openLog returns a logger writing to <state-dir>/helper.log.
func openLog() (*log.Logger, func(), error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "helper.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return log.New(f, fmt.Sprintf("[pid %d] ", os.Getpid()), log.LstdFlags|log.LUTC),
		func() { _ = f.Close() },
		nil
}

// shellQuote returns a POSIX-safe single-quoted form for embedding in
// shell commands. Single quotes preserve all characters except themselves,
// which we escape via the '...'\”... idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
