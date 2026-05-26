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
//     grant the cached human approval (CP5/CP5.5).
//   - run -- <cmd> [args...]: launch the wrapped command with rein's
//     git credential helper, gh shim, and session scope ceiling
//     in effect — without polluting the user's global git config (CP6).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/broker"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
)

const (
	// mintTimeout caps each installation-token mint. Git users feel this
	// latency directly when the helper is invoked, so keep it tight.
	mintTimeout = 5 * time.Second

	// approvalTTL is how long an approval covers writes for a session
	// before the human must re-confirm. 4h matches design §4.2.2's
	// default_read_ttl for the implement role — same span of human
	// attention.
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
		if err := runApproval(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "rein approval: %v\n", err)
			os.Exit(1)
		}
	case "run":
		code, err := runWrapped(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "rein run: %v\n", err)
			os.Exit(code)
		}
		os.Exit(code)
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
	fmt.Fprintln(os.Stderr, "  rein init [--skip-mint-check] [--no-symlink] [--no-alias] [--shell=bash|zsh|fish]")
	fmt.Fprintln(os.Stderr, "  rein doctor")
	fmt.Fprintln(os.Stderr, "  rein credential-helper {get|store|erase}")
	fmt.Fprintln(os.Stderr, "  rein install-shim")
	fmt.Fprintln(os.Stderr, "  rein gh-auth")
	fmt.Fprintln(os.Stderr, "  rein approval {status|clear|grant}")
	fmt.Fprintln(os.Stderr, "  rein run -- <cmd> [args...]")
}

// runCredentialHelper wires env-derived config to the broker. All errors
// returned here are programming/config errors — credential-mint failures
// are handled inside the broker per TM-G8.
func runCredentialHelper(action string) error {
	appCfg, err := config.LoadAppConfig()
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
	// Override the App config's repo with the session's first repo. CP4
	// has single-repo sessions; multi-repo sessions in CP5+ will need
	// per-mint repo selection.
	appCfg.RepoName = bareRepoName(sess.Repos[0])
	logger.Printf("session: id=%q role=%q repos=%v source=%s", sess.ID, sess.Role, sess.Repos, sessSource)

	client, err := githubapp.NewClient(appCfg)
	if err != nil {
		return err
	}

	mintRead := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		return client.MintReadOnlyToken(ctx)
	})
	mintWrite := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		return client.MintWriteToken(ctx)
	})

	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}

	return broker.RunCredentialHelper(action, os.Stdin, os.Stdout, broker.Config{
		MintRead:      mintRead,
		MintWrite:     mintWrite,
		MintTimeout:   mintTimeout,
		Logger:        logger,
		ReadCachePath: filepath.Join(stateDir, "cache", "read-token.json"),
		DetectWrite:   func() bool { return detectWriteIntent(logger) },
		Revoke:        client.RevokeToken,
		InScope:       sess.Contains,
		// EmptyPathScope stays "" (= allow): existing test setups
		// without useHttpPath=true continue to work. install-shim's
		// instructions recommend setting useHttpPath for strict
		// enforcement.
		ConfirmWrite: buildConfirmWrite(sess, stateDir, logger),
	})
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

// runApproval handles `rein approval {status|clear|grant}`:
//   - status: show the cached approval (or "none")
//   - clear:  remove the approval; next write re-prompts
//   - grant:  interactively approve write access for the current
//     session. Reads /dev/tty only (no CLI flag) — see
//     package internal/ui/grant for why.
func runApproval(sub string) error {
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	path := approvals.Path(stateDir)

	switch sub {
	case "status":
		rec, err := approvals.Read(path)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("no approval record (next write will prompt)")
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now()
		fmt.Printf("approval record: %s\n", path)
		fmt.Printf("  session_id:  %s\n", rec.SessionID)
		fmt.Printf("  signature:   %s\n", rec.Signature[:12]+"...")
		fmt.Printf("  approved_at: %s\n", rec.ApprovedAt.Format(time.RFC3339))
		fmt.Printf("  expires_at:  %s (%s remaining)\n",
			rec.ExpiresAt.Format(time.RFC3339),
			time.Until(rec.ExpiresAt).Round(time.Second))
		if now.After(rec.ExpiresAt) {
			fmt.Println("  status:      EXPIRED (next write will prompt)")
		} else {
			fmt.Println("  status:      VALID (writes for this session skip the prompt)")
		}
		return nil
	case "clear":
		if err := approvals.Clear(path); err != nil {
			return err
		}
		fmt.Println("approval cleared; next write will prompt")
		return nil
	case "grant":
		sess, src, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
		if err != nil {
			return err
		}
		if sess.Issue == 0 {
			return fmt.Errorf("session %q (source=%s) has no `issue:` field; nothing to grant", sess.ID, src)
		}
		logger, closeLog, err := openLog()
		if err != nil {
			return err
		}
		defer closeLog()
		logger.Printf("approval grant: session=%q source=%s", sess.ID, src)
		cfg := grant.Config{
			StateDir:      stateDir,
			TTL:           approvalTTL,
			PromptTimeout: 60 * time.Second,
			Logger:        logger,
		}
		if grant.Grant(context.Background(), sess, cfg) {
			fmt.Printf("approval recorded (valid for %s)\n", approvalTTL)
			return nil
		}
		return fmt.Errorf("approval denied or cancelled")
	default:
		fmt.Fprintf(os.Stderr, "rein approval: unknown subcommand %q (want status|clear|grant)\n", sub)
		os.Exit(2)
	}
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
	appCfg, err := config.LoadAppConfig()
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
	appCfg.RepoName = bareRepoName(sess.Repos[0])
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
	client, err := githubapp.NewClient(appCfg)
	if err != nil {
		return err
	}
	token, expiresAt, err := ghsession.EnsureFresh(
		ghsession.ReadCachePath(stateDir),
		appCfg,
		client.MintGhReadOnlyToken,
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
