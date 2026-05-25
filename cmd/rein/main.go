// rein is the credential broker CLI.
//
// Phase 0 subcommands:
//   - credential-helper {get|store|erase}: drives the git credential-helper
//     protocol; reads target App config from REIN_* env vars; routes
//     between read and write tiers per the REIN_GIT_OP env (set by the
//     rein-git shim) with a process-tree fallback on Linux.
//   - install-shim: writes the rein-git shim binary to a known location
//     and prints the PATH-prepend instruction.
//   - gh-auth: mints an implement-role token for the `gh` CLI and writes
//     a sourceable env file (CP3.5).
//
// Future checkpoints add sessions, scope ceilings, prompts, and a top-level
// `rein run` wrapper that does the helper + shim + gh-auth wiring
// per-process.
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
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/broker"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/prompt"
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
	fmt.Fprintln(os.Stderr, "  rein credential-helper {get|store|erase}")
	fmt.Fprintln(os.Stderr, "  rein install-shim")
	fmt.Fprintln(os.Stderr, "  rein gh-auth")
	fmt.Fprintln(os.Stderr, "  rein approval {status|clear}")
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

// buildConfirmWrite returns a ConfirmWrite predicate that:
//  1. Honors an existing approval record (no prompt) if it covers
//     the current session signature and hasn't expired.
//  2. Otherwise prompts via /dev/tty; on approval, writes a new
//     approval record valid for approvalTTL.
//
// Per-write prompting was the original CP5 interpretation but felt
// excessive for productive sessions; design §2.2's prompt is a
// scope-establishment ceremony, not a per-operation gate. The
// approval record makes it once-per-session.
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
	sig := approvals.SignatureOf(sess)
	approvalPath := approvals.Path(stateDir)
	prompter := prompt.TTYPrompter{}

	return func(repo string) bool {
		// Check for existing approval first.
		if rec, err := approvals.Read(approvalPath); err == nil {
			if approvals.Valid(rec, sig, time.Now()) {
				logger.Printf("ConfirmWrite: covered by existing approval (granted at %s, valid until %s)",
					rec.ApprovedAt.Format(time.RFC3339),
					rec.ExpiresAt.Format(time.RFC3339))
				return true
			}
			logger.Printf("ConfirmWrite: existing approval mismatched or expired (sig_match=%v, expires=%s); re-prompting",
				rec.Signature == sig, rec.ExpiresAt.Format(time.RFC3339))
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.Printf("ConfirmWrite: approval load failed: %v; re-prompting", err)
		}

		req := prompt.Request{
			SessionID: sess.ID,
			Role:      sess.Role,
			Repo:      repo,
			Action:    fmt.Sprintf("write access for this session (covers writes until +%s)", approvalTTL),
			Issue:     sess.Issue,
			Timeout:   60 * time.Second,
		}
		ok, err := prompter.Confirm(context.Background(), req)
		if err != nil {
			logger.Printf("ConfirmWrite: prompter error: %v; denying", err)
			return false
		}
		if !ok {
			return false
		}

		// Persist approval so subsequent writes within TTL skip prompt.
		now := time.Now()
		newRec := approvals.Record{
			Signature:  sig,
			SessionID:  sess.ID,
			ApprovedAt: now,
			ExpiresAt:  now.Add(approvalTTL),
		}
		if err := approvals.Write(approvalPath, newRec); err != nil {
			logger.Printf("ConfirmWrite: approval write failed (continuing): %v", err)
		} else {
			logger.Printf("ConfirmWrite: APPROVED; approval recorded (valid until %s)",
				newRec.ExpiresAt.Format(time.RFC3339))
		}
		return true
	}
}

// runApproval handles `rein approval {status|clear}` — the operator's
// escape hatches for inspecting or revoking the cached human approval.
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
	default:
		fmt.Fprintf(os.Stderr, "rein approval: unknown subcommand %q (want status|clear)\n", sub)
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
// invoked. Fallback: walk /proc to find `git push` or `git send-pack` in the
// ancestor chain (Linux only; macOS would need a libproc port).
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

	if runtime.GOOS != "linux" {
		// macOS process-tree introspection needs libproc; not implemented
		// in Phase 0. Without the shim signal, default to read.
		logger.Printf("no REIN_GIT_OP and platform %q not supported for proc-tree fallback; defaulting to read", runtime.GOOS)
		return false
	}

	if write, src := detectFromProcTree(); write {
		logger.Printf("write intent: process-tree fallback found %q in ancestor chain", src)
		return true
	}
	return false
}

// detectFromProcTree walks the process tree up to a fixed depth looking
// for a `git push` or `git send-pack` invocation. Returns the matching
// cmdline (as a single string) for log purposes. Linux-only.
//
// We trust the chain if the ancestor at any level is `git push` /
// `git send-pack`. We don't try to verify the chain's authenticity (e.g.,
// by checking that intermediate processes are git's transport helpers) —
// this is a routing signal, not a security boundary. An attacker who can
// spoof their argv to fake a git push only gets the wrong tier minted;
// they cannot exceed the role's permissions ceiling enforced server-side.
const procTreeDepth = 6

func detectFromProcTree() (bool, string) {
	pid := os.Getpid()
	for i := 0; i < procTreeDepth; i++ {
		ppid, err := readPPid(pid)
		if err != nil || ppid <= 1 {
			return false, ""
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ppid))
		if err != nil {
			return false, ""
		}
		args := splitCmdline(cmdline)
		if isGitVerb(args, "push") || isGitVerb(args, "send-pack") {
			return true, strings.Join(args, " ")
		}
		pid = ppid
	}
	return false, ""
}

func readPPid(pid int) (int, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				return strconv.Atoi(f[1])
			}
		}
	}
	return 0, fmt.Errorf("no PPid for pid %d", pid)
}

// splitCmdline parses a /proc/<pid>/cmdline NUL-separated buffer into argv.
func splitCmdline(b []byte) []string {
	s := string(b)
	s = strings.TrimRight(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
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

// installShim writes the rein-git and rein-gh shim binaries to a known
// location under the state dir and prints the PATH-prepend instruction.
// Idempotent — running install-shim repeatedly is safe.
func installShim() error {
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	shimDir := filepath.Join(stateDir, "shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		return fmt.Errorf("create shim dir: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	selfDir := filepath.Dir(self)

	shims := []struct{ name, intent string }{
		{"rein-git", "git"},
		{"rein-gh", "gh"},
	}
	for _, s := range shims {
		src, err := locateBinary(s.name, selfDir)
		if err != nil {
			return err
		}
		dst := filepath.Join(shimDir, s.intent)
		if err := copyFile(src, dst, 0o700); err != nil {
			return fmt.Errorf("install %s: %w", s.intent, err)
		}
		fmt.Printf("installed shim: %s -> %s\n", dst, s.name)
	}

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
	return nil
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
// which we escape via the '...'\''... idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
