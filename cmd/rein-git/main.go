// rein-git is a thin wrapper around the system `git`. It inspects argv to
// classify the operation as read or write, sets REIN_GIT_OP accordingly,
// and execs the real git. The env var propagates through git's transport
// (git-remote-https et al.) to the credential helper, where the broker
// reads it to decide between a cached read token and a fresh write token.
//
// Installed at the front of $PATH by `rein install-shim` (or `rein run` in
// Phase 1+). When bypassed (agent calls /usr/bin/git directly), the broker's
// process-tree fallback recovers the signal on Linux.
//
// Misclassification here causes a wrong-tier mint, not a security breach —
// GitHub enforces the role's permissions ceiling at the token-mint API.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/TomHennen/rein/internal/agentenv"
	"github.com/TomHennen/rein/internal/gitupstream"
)

// envUpstreamIntentFile is set only for a sandboxed bound checkout. Its presence
// switches on the strip+capture below; unset (direct mode) passes -u through.
const envUpstreamIntentFile = agentenv.EnvUpstreamIntentFile

// writeSubcommands need a write-capable installation token.
var writeSubcommands = map[string]bool{
	"push":      true,
	"send-pack": true, // low-level push counterpart
}

// readSubcommands are unambiguously read-only over the wire. Other
// subcommands (commit, branch, log, etc.) don't touch the network so they
// never invoke the credential helper; we don't bother classifying them.
var readSubcommands = map[string]bool{
	"fetch":     true,
	"pull":      true, // fetch + merge; the network half is fetch
	"clone":     true,
	"ls-remote": true,
	"archive":   true, // git archive over http is read-only
}

func main() {
	args := os.Args[1:]
	op := classify(args)

	realGit, err := findRealGit(os.Args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein-git: %v\n", err)
		os.Exit(127)
	}

	// Sandbox bound checkout only (#64/#102/#119): record what `git push -u` would
	// set (for rein to apply post-run) and strip the flag, so its .git/config write
	// doesn't fault on the read-only pin. Gated on -u present so we synthesize
	// tracking only when real git would; direct mode (env unset) passes -u through.
	if intentFile := os.Getenv(envUpstreamIntentFile); intentFile != "" &&
		findSubcommand(args) == "push" && gitupstream.HasSetUpstream(args) {
		captureUpstreamIntent(args, intentFile, realGit)
		args = dropPushUpstreamFlag(args)
	}

	env := append(os.Environ(), "REIN_GIT_OP="+op)

	// Replace this process with real git. Note: syscall.Exec wants argv[0]
	// to be the program name; we pass realGit so git sees its real path.
	argv := append([]string{realGit}, args...)
	if err := syscall.Exec(realGit, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "rein-git: exec %s: %v\n", realGit, err)
		os.Exit(127)
	}
}

// captureUpstreamIntent appends what `git push -u` would have set to the
// rendezvous file. Best-effort: any failure is silently skipped, never blocking
// the push (the operator just doesn't get tracking set).
func captureUpstreamIntent(args []string, intentFile, realGit string) {
	in, ok := gitupstream.ParsePush(args, func() (string, error) {
		out, err := exec.Command(realGit, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	})
	if !ok {
		return
	}
	f, err := os.OpenFile(intentFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, gitupstream.EncodeLine(in))
}

// dropPushUpstreamFlag removes `-u` / `--set-upstream` from a `git push` argv.
// Both are valueless flags meaning "record the pushed branch as upstream" — a
// .git/config write that is read-only in the sandbox. Exact-token match only:
// a refspec or branch can never be literally "-u"/"--set-upstream".
func dropPushUpstreamFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "-u" || a == "--set-upstream" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// classify scans argv past git's global options to find the subcommand and
// returns "read", "write", or "unknown".
//
// Fail-closed: when in doubt, return "unknown" — the broker treats unknown
// as read, which causes a push to surface a 403 rather than silently grant
// write capability.
func classify(args []string) string {
	sub := findSubcommand(args)
	switch {
	case writeSubcommands[sub]:
		return "write"
	case readSubcommands[sub]:
		return "read"
	default:
		return "unknown"
	}
}

// findSubcommand walks argv and returns the first non-option token, taking
// into account git's documented global options. Returns "" if none found.
//
// The set of global options is from `git --help` (git 2.43). Options that
// take an argument in a separate token (`--git-dir /path`) consume the next
// arg. Options in the `--name=value` form do not.
func findSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" {
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if optionConsumesNextArg(a) {
			i++ // skip the value
		}
	}
	return ""
}

// optionsThatTakeArg lists the git global options whose value is provided
// as a separate argv token (not `--name=value`). Source: `git --help`
// "GIT COMMAND OPTIONS" section, git 2.43, plus empirical testing.
//
// Some entries (e.g. --exec-path, --list-cmds) are documented only in the
// `=value` form but listed here for safety: if a user writes the separate-
// arg form and git rejects it, the shim merely classifies as "unknown" —
// which the broker treats as read — and execs real git, which surfaces
// its own error. Misclassification at this layer is always fail-closed at
// worst; over-skipping is preferable to letting an unrecognized arg slip
// past and produce a phantom subcommand match.
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
	// --name=value form never consumes the next arg.
	if strings.Contains(a, "=") {
		return false
	}
	return optionsThatTakeArg[a]
}

// findRealGit finds the system git, excluding this shim's own dir so we don't
// recurse. The dir comes from os.Executable() (falling back to shimPath): when
// invoked as bare `git`, os.Args[0] is just "git" and would resolve to the CWD,
// making the PATH scan re-find the shim and add a wasteful self-exec hop.
// REIN_REAL_GIT overrides everything (tests; distros without git on PATH).
func findRealGit(shimPath string) (string, error) {
	if override := os.Getenv("REIN_REAL_GIT"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("REIN_REAL_GIT=%q does not exist", override)
	}

	shimAbs := shimPath
	if exe, err := os.Executable(); err == nil {
		shimAbs = exe
	} else if abs, err := filepath.Abs(shimPath); err == nil {
		shimAbs = abs
	}
	if resolved, err := filepath.EvalSymlinks(shimAbs); err == nil {
		shimAbs = resolved
	}
	shimDir := filepath.Dir(shimAbs)

	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if abs == shimDir {
			continue
		}
		cand := filepath.Join(dir, "git")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", errors.New("no git binary found on PATH (excluding rein-git's own directory)")
}
