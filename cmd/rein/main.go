// rein is the credential broker CLI.
//
// Phase 0 subcommands:
//   - credential-helper {get|store|erase}: drives the git credential-helper
//     protocol; reads target App config from REIN_* env vars; routes
//     between read and write tiers per the REIN_GIT_OP env (set by the
//     rein-git shim) with a process-tree fallback on Linux.
//   - install-shim: writes the rein-git shim binary to a known location
//     and prints the PATH-prepend instruction.
//
// Future checkpoints add sessions, scope ceilings, prompts, and a top-level
// `rein run` wrapper that does the helper + shim wiring per-process.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/broker"
	"github.com/TomHennen/rein/internal/githubapp"
)

const (
	// mintTimeout caps each installation-token mint. Git users feel this
	// latency directly when the helper is invoked, so keep it tight.
	mintTimeout = 5 * time.Second
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
}

// runCredentialHelper wires env-derived config to the broker. All errors
// returned here are programming/config errors — credential-mint failures
// are handled inside the broker per TM-G8.
func runCredentialHelper(action string) error {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}

	logger, closeLog, err := openLog()
	if err != nil {
		return err
	}
	defer closeLog()

	client, err := githubapp.NewClient(cfg.app)
	if err != nil {
		return err
	}

	mintRead := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		return client.MintReadOnlyToken(ctx)
	})
	mintWrite := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		return client.MintWriteToken(ctx)
	})

	stateDir, err := reinStateDir()
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
	})
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

// installShim writes the rein-git binary to a known location under the
// state dir and prints the PATH-prepend instruction. Idempotent.
func installShim() error {
	stateDir, err := reinStateDir()
	if err != nil {
		return err
	}
	shimDir := filepath.Join(stateDir, "shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		return fmt.Errorf("create shim dir: %w", err)
	}
	shimDst := filepath.Join(shimDir, "git")

	// Locate the rein-git binary built alongside rein. Heuristic: look for
	// it next to the current executable.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(self), "rein-git")
	if _, err := os.Stat(candidate); err != nil {
		// Last resort: PATH search (might find a globally-installed one).
		if p, err := exec.LookPath("rein-git"); err == nil {
			candidate = p
		} else {
			return fmt.Errorf("rein-git binary not found next to %s or on PATH; build it first (go build -o bin/rein-git ./cmd/rein-git)", self)
		}
	}

	if err := copyFile(candidate, shimDst, 0o700); err != nil {
		return fmt.Errorf("install shim: %w", err)
	}

	fmt.Printf("installed shim: %s\n", shimDst)
	fmt.Printf("                (intercepts `git` and sets REIN_GIT_OP)\n\n")
	fmt.Println("To activate, prepend the shim dir to $PATH before launching agents:")
	fmt.Printf("  export PATH=%s:$PATH\n\n", shellQuote(shimDir))
	fmt.Println("Verify with:")
	fmt.Println("  which git    # should resolve to the shim")
	fmt.Println()
	fmt.Println("CP6's `rein run` wrapper will set PATH per-wrapped-process.")
	return nil
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

type config struct {
	app githubapp.Config
}

func loadConfigFromEnv() (config, error) {
	required := []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
		"REIN_TEST_REPO_A",
	}
	for _, k := range required {
		if os.Getenv(k) == "" {
			return config{}, fmt.Errorf("missing env var %s (did you source ./dev-env?)", k)
		}
	}
	installationID, err := strconv.ParseInt(os.Getenv("REIN_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return config{}, fmt.Errorf("REIN_APP_INSTALLATION_ID not an int64: %w", err)
	}

	slug := os.Getenv("REIN_TEST_REPO_A")
	_, repoName, ok := strings.Cut(slug, "/")
	if !ok || repoName == "" {
		return config{}, fmt.Errorf("REIN_TEST_REPO_A %q is not owner/name", slug)
	}

	return config{
		app: githubapp.Config{
			ClientID:       os.Getenv("REIN_APP_CLIENT_ID"),
			PrivateKeyPath: os.Getenv("REIN_APP_PRIVATE_KEY_PATH"),
			InstallationID: installationID,
			RepoName:       repoName,
		},
	}, nil
}

// reinStateDir is $XDG_STATE_HOME/rein (defaulting to ~/.local/state/rein).
// Created with mode 0700 on first use.
func reinStateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "rein")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

// openLog returns a logger writing to <state-dir>/helper.log.
func openLog() (*log.Logger, func(), error) {
	dir, err := reinStateDir()
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
