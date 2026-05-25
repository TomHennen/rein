// `rein run -- <cmd> [args...]`
//
// Wraps a child process with rein's git credential helper, gh shim, and
// session scope-ceiling in effect — without polluting the user's global
// git config. Phase 0's full integration entry point (CP6).
//
// What it does:
//
//   - Validates that the shim dir (~/.local/state/rein/shim) exists and
//     contains the git, gh, and rein binaries (placed by install-shim).
//     Errors out helpfully if not — does NOT auto-install.
//
//   - Allocates a tempdir + writes a per-process git config there. The
//     config `include.path = ~/.gitconfig` so the user's preferences
//     (aliases, editor, signing config) layer in; rein's
//     credential.https://github.com.helper and useHttpPath override.
//
//   - Sets the child's env so it sees:
//       PATH=<shim-dir>:<inherited PATH>     (intercepts git/gh)
//       GIT_CONFIG_GLOBAL=<tempdir>/gitconfig (rein's overrides)
//       GIT_CONFIG_SYSTEM=/dev/null           (no system /etc/gitconfig)
//     REIN_* env vars are inherited as-is (App auth, test repo).
//
//   - Does NOT detach the child's process group: terminal SIGINT
//     reaches both rein and the child naturally. SIGTERM to rein is
//     forwarded to the child explicitly via a relay goroutine.
//
//   - Cleans the tempdir on exit (deferred + signal-handled).
//
// What you should NOT expect:
//
//   - System config (/etc/gitconfig) is invisible to the child. Phase 0
//     scope decision — adding it back is a one-line change if it matters.
//
//   - With CP5.5's approve-once + 4h TTL, the human prompt fires on the
//     FIRST write of the session and stays silent for subsequent ones.
//     PLAN.md's "single prompt presented" describes the first write,
//     not every write.
//
//   - /dev/tty inside the wrapped agent: empirical question. Three
//     outcomes depending on what the agent does with the controlling
//     terminal — documented to the operator at startup so they know
//     what to look for.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/session"
)

// runWrapped is the entry point for the `run` subcommand. argv is the
// args AFTER "run" — so for `rein run -- claude foo`, argv is
// ["--", "claude", "foo"].
//
// Returns (exitCode, error). On normal child exit, error is nil and
// exitCode is the child's. On setup failure, error is non-nil and
// exitCode is 2 (POSIX usage error) or 127 (command not found / shim
// missing).
func runWrapped(argv []string) (int, error) {
	cmdline, err := parseRunArgs(argv)
	if err != nil {
		return 2, err
	}

	// Locate prerequisites — fail-loud if missing rather than auto-install.
	stateDir, err := config.StateDir()
	if err != nil {
		return 1, err
	}
	shimDir := filepath.Join(stateDir, "shim")
	for _, name := range []string{"git", "gh", "rein"} {
		if _, err := os.Stat(filepath.Join(shimDir, name)); err != nil {
			return 127, fmt.Errorf("shim binary %s/%s not found — run 'rein install-shim' first", shimDir, name)
		}
	}

	// Validate session — same loader used elsewhere, so the same
	// REIN_SESSION_FILE override and env-fallback rules apply.
	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return 1, fmt.Errorf("load session: %w (see README for dev-session.yaml format)", err)
	}

	// Allocate tempdir for the per-process git config.
	tempDir, err := os.MkdirTemp("", "rein-run-*")
	if err != nil {
		return 1, fmt.Errorf("create temp dir: %w", err)
	}
	if err := os.Chmod(tempDir, 0o700); err != nil {
		os.RemoveAll(tempDir)
		return 1, err
	}
	// Deferred cleanup is the happy path. Signal handler below also
	// cleans up on early termination.
	defer os.RemoveAll(tempDir)

	gitConfigPath := filepath.Join(tempDir, "gitconfig")
	reinBin := filepath.Join(shimDir, "rein")
	if err := writeRunGitConfig(gitConfigPath, reinBin); err != nil {
		return 1, fmt.Errorf("write git config: %w", err)
	}

	// Build the wrapped child's env.
	env := os.Environ()
	env = setEnv(env, "PATH", shimDir+":"+os.Getenv("PATH"))
	env = setEnv(env, "GIT_CONFIG_GLOBAL", gitConfigPath)
	env = setEnv(env, "GIT_CONFIG_SYSTEM", "/dev/null")

	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Tell the operator what's about to happen so they know what to
	// look for when their wrapped agent (claude, etc.) hits a write.
	fmt.Fprintln(os.Stderr, "rein: launching wrapped process with:")
	fmt.Fprintf(os.Stderr, "  session: %s (role=%s, repos=%v, issue=#%d) [source=%s]\n",
		sess.ID, sess.Role, sess.Repos, sess.Issue, sessSource)
	fmt.Fprintf(os.Stderr, "  PATH-front shim dir: %s\n", shimDir)
	fmt.Fprintf(os.Stderr, "  per-process git config: %s\n", gitConfigPath)
	fmt.Fprintln(os.Stderr, "  (your real ~/.gitconfig is layered in via include.path)")
	fmt.Fprintln(os.Stderr)
	if sess.Issue == 0 && isWriteCapableRole(sess.Role) {
		fmt.Fprintln(os.Stderr, "  WARN: session has no `issue:` field — write ops will mint WITHOUT human confirmation.")
		fmt.Fprintln(os.Stderr)
	} else {
		fmt.Fprintln(os.Stderr, "  First write op will trigger a confirmation prompt. Look for it in:")
		fmt.Fprintln(os.Stderr, "    - your terminal (if the wrapped process inherits /dev/tty)")
		fmt.Fprintln(os.Stderr, "    - a tmux popup (if you're in tmux and /dev/tty is detached)")
		fmt.Fprintln(os.Stderr, "    - a 'rein: write blocked' message in the wrapped process's output")
		fmt.Fprintln(os.Stderr, "      (run 'rein approval grant' in another terminal to approve, then retry)")
		fmt.Fprintln(os.Stderr)
	}
	fmt.Fprintln(os.Stderr, "rein: running:", strings.Join(cmdline, " "))
	fmt.Fprintln(os.Stderr, "---")

	// Start the child. We do NOT set SysProcAttr.Setpgid so terminal
	// SIGINT reaches both rein and the child via the shared process
	// group. SIGTERM sent only to rein won't auto-reach the child;
	// we forward it via the relay goroutine.
	if err := cmd.Start(); err != nil {
		return 127, fmt.Errorf("start child: %w", err)
	}

	// Signal forwarding for the SIGTERM-only-to-parent case.
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
		// Normal nonzero exit — propagate the child's exit code.
		return ee.ExitCode(), nil
	}
	return 1, fmt.Errorf("wait child: %w", waitErr)
}

// parseRunArgs validates "rein run -- <cmd> [args...]" form. The "--"
// separator is required to keep the shape obvious; if you wanted
// `rein run claude foo` to work too, this is the one line to relax.
func parseRunArgs(argv []string) ([]string, error) {
	if len(argv) < 1 || argv[0] != "--" {
		return nil, fmt.Errorf("usage: rein run -- <cmd> [args...] (the '--' separator is required)")
	}
	if len(argv) < 2 {
		return nil, fmt.Errorf("usage: rein run -- <cmd> [args...] (no command supplied)")
	}
	return argv[1:], nil
}

// writeRunGitConfig writes the per-process git config to path.
// `include.path = ~/.gitconfig` layers the user's existing preferences
// (aliases, signing, editor, etc.); rein's credential.* and
// core.useHttpPath override anything the user had set.
func writeRunGitConfig(path, reinBin string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	userGitconfig := filepath.Join(home, ".gitconfig")

	// Two credential.helper entries: the empty string resets the
	// helper chain so any helpers in the included user config are
	// not also tried. Then our helper is appended as the only one.
	body := ""
	if _, err := os.Stat(userGitconfig); err == nil {
		body += "# Layer the user's real ~/.gitconfig in (aliases, editor, signing, etc.).\n"
		body += "# rein's credential.* and useHttpPath settings below override anything\n"
		body += "# the user had set, since this rein config is the GLOBAL config from\n"
		body += "# git's perspective (GIT_CONFIG_GLOBAL points at this file).\n"
		body += "[include]\n"
		body += "  path = " + userGitconfig + "\n\n"
	}
	body += "[credential \"https://github.com\"]\n"
	body += "  helper =\n"
	body += "  helper = \"" + reinBin + " credential-helper\"\n"
	body += "  useHttpPath = true\n"

	return os.WriteFile(path, []byte(body), 0o600)
}

// setEnv replaces (or appends) the named env var in env. Returns the
// new slice.
func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
