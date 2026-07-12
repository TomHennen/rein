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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/broker"
	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/declare"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/runscope"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/internal/tokencache"
	"github.com/TomHennen/rein/internal/ui/grant"
)

// dispatchRun routes `rein run`. CP4 flips the default: sandboxed (srt) mode is
// now the DEFAULT (where srt is healthy — runSandboxed hard-gates preflight and
// fails closed, never silently dropping to unsandboxed on a real repo). Direct
// mode moves behind an explicit --direct/--no-sandbox flag with a loud banner
// (the reduced-protection, throwaway-only path). The mode flag, if present, must
// come BEFORE the "--" separator:
//
//	rein run -- <cmd>              # sandboxed (default)
//	rein run --sandbox -- <cmd>    # sandboxed (explicit; alias for the default)
//	rein run --direct -- <cmd>     # DIRECT/unsandboxed (explicit opt-in + banner)
//	rein run --no-sandbox -- <cmd> # alias for --direct
func dispatchRun(argv []string) (int, error) {
	mode, cmdline, err := parseRunMode(argv)
	if err != nil {
		return 2, err
	}
	if mode == modeDirect {
		// Banner only AFTER the command shape validated (parseRunMode returns a
		// usage error first), so a usage error never prints the scary warning.
		printDirectModeBanner(os.Stderr)
		return runWrapped(append([]string{"--"}, cmdline...))
	}
	// Sandboxed (default + --sandbox). runSandboxed runs preflight and fails
	// closed (with a `rein doctor` pointer) if srt is unhealthy — it does NOT
	// fall back to unsandboxed mode on its own (design §2-3; CP3 fallback rule).
	return runSandboxed(cmdline)
}

// runMode is the sandbox-vs-direct decision for `rein run`.
type runMode int

const (
	modeSandbox runMode = iota
	modeDirect
)

// parseRunMode decides the run mode from the leading flag and validates the
// command shape, returning the mode and the cmdline (the argv AFTER "--"). The
// default (no recognized mode flag) is modeSandbox — the CP4 flip. A usage error
// is returned for a bad command shape BEFORE any side effect, so dispatchRun can
// reject it without printing the direct-mode banner.
func parseRunMode(argv []string) (runMode, []string, error) {
	mode := modeSandbox
	rest := argv
	if len(argv) > 0 {
		switch argv[0] {
		case "--sandbox":
			rest = argv[1:]
		case "--direct", "--no-sandbox":
			mode = modeDirect
			rest = argv[1:]
		}
	}
	cmdline, err := parseRunArgs(rest)
	if err != nil {
		return mode, nil, err
	}
	return mode, cmdline, nil
}

// printDirectModeBanner is the loud, unmissable warning for the explicit
// unsandboxed path. rein cannot detect whether the target is a throwaway repo,
// so this banner + the explicit --direct flag ARE the throwaway gate: the human
// is trusted to heed it (hard constraint #1 + the CP3 fallback rule).
func printDirectModeBanner(w io.Writer) {
	fmt.Fprintln(w, "===============================================================")
	fmt.Fprintln(w, "rein: WARNING — DIRECT (UNSANDBOXED) MODE")
	fmt.Fprintln(w, "  The agent runs OUTSIDE the srt sandbox. It shares your user")
	fmt.Fprintln(w, "  account: it CAN read your ambient credentials (gh login, SSH")
	fmt.Fprintln(w, "  keys, ~/.netrc, keyrings) and the rein-brokered token is only")
	fmt.Fprintln(w, "  hidden by process boundaries, not a sandbox. Use this ONLY on a")
	fmt.Fprintln(w, "  THROWAWAY repo. For real work, drop --direct to run sandboxed.")
	fmt.Fprintln(w, "===============================================================")
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
	case "session":
		if err := runSession(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "rein session: %v\n", err)
			os.Exit(1)
		}
	case "run":
		code, err := dispatchRun(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "rein run: %v\n", err)
			os.Exit(code)
		}
		os.Exit(code)
	case "declare":
		code, err := runDeclare(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "rein declare: %v\n", err)
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
	fmt.Fprintln(os.Stderr, "  rein approval notice --run-id <id>        (show a pending install notice; grants nothing)")
	fmt.Fprintln(os.Stderr, "  rein session show                         (standing scope ceiling + live runs' expansions)")
	fmt.Fprintln(os.Stderr, "  rein session add-repo <owner/name>        (validated widening of the standing ceiling)")
	fmt.Fprintln(os.Stderr, "  rein declare <issue-number> [--repo owner/name]  (declare the issue this run's work is for)")
	fmt.Fprintln(os.Stderr, "  rein run -- <cmd> [args...]                (sandboxed by default)")
	fmt.Fprintln(os.Stderr, "  rein run --direct -- <cmd> [args...]       (unsandboxed; throwaway repos only)")
}

// testPanicHook, when non-nil, is called at the top of the broker path. It is
// a TEST-ONLY seam for the TM-G8 panic-recovery test (issue #61), which has to
// inject a crash on the broker path to prove guardHelperPanic still answers
// git with a credential. It receives the helper's stdout so a test can also
// emit PARTIAL output before panicking and prove those bytes never reach git.
//
// SECURITY REVIEW, please confirm: this is deliberately a package-private var
// with no exported setter, no env-var trigger, and no config/argv path that
// can assign it. Only code compiled into package main can set it, i.e. the
// _test.go files. In a production binary it is nil forever and the branch is a
// single nil check on a path that already does file I/O and network calls. A
// panic seam reachable from the environment WOULD be a real hazard — an
// attacker who could force a panic gets a denied credential (fail-closed), but
// it would still be a free DoS on every git operation. This shape is not
// reachable that way.
var testPanicHook func(out io.Writer)

// runCredentialHelper wires env-derived config to the broker using the
// process's real stdio. guardHelperPanic is the TM-G8 backstop;
// runCredentialHelperEnv is the testable seam beneath it.
func runCredentialHelper(action string) error {
	return guardHelperPanic(action, os.Stdin, os.Stdout, os.Stderr)
}

// guardHelperPanic is the last line of the TM-G8 / hard-constraint-#2 defense
// (issue #61): the credential helper must ALWAYS answer git with a credential
// — never empty, never exit 1.
//
// #45 closed that hole for ERROR RETURNS (every pre-broker failure now routes
// through serveHelperPlaceholder). A PANIC produced the identical outcome by a
// different route: the Go runtime prints a stack to stderr and exits 2 with
// EMPTY STDOUT, git reads "no answer" and falls through to the next credential
// source — the OS keychain or the developer's ambient PAT — and the push it
// was supposed to gate silently succeeds outside rein's scope ceiling. The
// broker has targeted recoveries around DetectWrite/ConfirmWrite/RecordWrite,
// but a panic anywhere else on the path (a nil map, a bad slice index, a
// third-party dependency) was unguarded.
//
// Two buffers make the recovery safe:
//
//   - stdin is read to completion up front (git writes the attribute block
//     then closes), so the recovery can REPLAY the same request through the
//     real broker via serveHelperPlaceholder. Without this the request bytes
//     are gone by the time we recover, and we could not tell a github.com get
//     (must answer) from a non-github get (must stay silent).
//
//   - stdout is buffered and flushed only on a clean return. A panic
//     mid-write would otherwise leave a TRUNCATED credential block on git's
//     stdin, and appending the placeholder after it would produce a
//     double-keyed block whose meaning depends on git's last-key-wins parse.
//     On panic the partial bytes are dropped and the placeholder is written to
//     a clean stdout.
//
// Trade-off worth naming: if a panic happened AFTER a real credential was
// fully written, dropping the buffer downgrades a working credential to the
// placeholder, and the operation fails loudly instead of succeeding. That is
// the fail-closed direction (hard constraint #3), and a helper that crashed
// mid-invocation has already lost its claim to be trusted for that operation.
func guardHelperPanic(action string, in io.Reader, out, diag io.Writer) (err error) {
	req, readErr := io.ReadAll(in)
	if readErr != nil {
		// Mirrors the broker's own stdin-error policy: it cannot tell which
		// host the request was for, so it answers empty rather than risk a
		// Bearer for the wrong host. Preserve that, but say so on stderr.
		fmt.Fprintf(diag, "rein: warning: could not read the credential request from git (%v)\n", readErr)
		req = nil
	}

	var buf bytes.Buffer
	defer func() {
		r := recover()
		if r == nil {
			// Clean run: hand git exactly what the broker produced.
			if _, werr := out.Write(buf.Bytes()); werr != nil && err == nil {
				err = fmt.Errorf("write credential response: %w", werr)
			}
			return
		}
		// Panicked: drop any partial output and answer from a clean stdout.
		//
		// Everything from here on is itself panic-guarded. The diagnostics and
		// openLog run BEFORE the credential is served, and they touch writers
		// and a filesystem we no longer trust — a panicking diag writer or log
		// path would otherwise escape this defer and land right back on the
		// TM-G8 outcome we are here to prevent (non-zero exit, empty stdout).
		// The guard guarantees a credential block reaches git no matter what
		// fails inside the recovery.
		served := false
		defer func() {
			if r2 := recover(); r2 != nil {
				if action == "get" && !served {
					fmt.Fprintf(out, "username=%s\npassword=%s\n\n",
						brokercore.CredentialUsername, brokercore.PlaceholderMintFailed)
				}
				err = nil // exit 0: git must never read this as "no answer"
			}
		}()

		stack := debug.Stack()
		fmt.Fprintf(diag, "rein: internal error in the credential helper (panic: %v)\n", r)
		fmt.Fprintln(diag, "      Serving a placeholder credential so git fails loudly instead of falling back")
		fmt.Fprintln(diag, "      to another credential source (your ambient PAT). Please report this with the")
		fmt.Fprintln(diag, "      stack trace in the helper log.")

		logger, closeLog, lerr := openLog()
		if lerr != nil {
			logger = log.New(io.Discard, "", 0)
			closeLog = func() {}
		}
		defer closeLog()
		logger.Printf("PANIC in credential helper: %v\n%s", r, stack)

		err = servePanicPlaceholder(action, req, out, diag, logger, r)
		served = true
	}()

	err = runCredentialHelperEnv(action, bytes.NewReader(req), &buf, diag)
	return err
}

// servePanicPlaceholder replays the buffered request through the SAME route
// every other pre-broker failure uses (serveHelperPlaceholder), so a panic
// yields the identical contract: the TM-G8 placeholder on a github.com get,
// the protocol's empty "not my host" block elsewhere, no-ops for store/erase,
// and exit 0 throughout.
//
// It is itself panic-guarded. If even serveHelperPlaceholder blows up, the
// last resort writes the placeholder credential block directly — the same
// brokercore constants the broker would have used — because emitting SOMETHING
// on a github.com get is the whole invariant. We cannot parse the host in that
// state, so the direct write is limited to `get` and unconditional; a
// placeholder credential offered to a non-github host is a loud auth failure
// there, never a leak (the value is non-secret and useless).
func servePanicPlaceholder(action string, req []byte, out, diag io.Writer, logger *log.Logger, cause any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC while serving the panic placeholder: %v", r)
			if action == "get" {
				fmt.Fprintf(out, "username=%s\npassword=%s\n\n",
					brokercore.CredentialUsername, brokercore.PlaceholderMintFailed)
			}
			err = nil // exit 0: git must never read this as "no answer"
		}
	}()

	hint := "rein: the credential below is a placeholder — the operation will fail rather than silently use another credential."
	perr := serveHelperPlaceholder(action, bytes.NewReader(req), out, diag, logger,
		fmt.Errorf("panic in credential helper: %v", cause), hint)
	if perr != nil {
		// serveHelperPlaceholder only errors on broker programming bugs. Even
		// then, do not surface a non-nil error: main would exit 1 with the
		// stdout we just wrote, and git treats a failed helper as "no answer".
		logger.Printf("serveHelperPlaceholder failed after panic: %v", perr)
	}
	return nil
}

// runCredentialHelperEnv is the env-driven helper core.
//
// TM-G8 / hard-constraint #2: on the github.com `get` path this function must
// NEVER return an error (main turns that into exit 1 with EMPTY stdout). When
// a credential helper errors or answers empty, git treats it as "no answer"
// and FALLS THROUGH to the next credential source — the next configured
// helper, the OS keychain, or a terminal prompt. A corrupted session file or
// a missing state dir could then let a push silently succeed with the
// developer's ambient PAT: exactly the broker displacement TM-G8 exists to
// prevent (design §5.3, validated §12.1). So every environmental pre-broker
// failure is routed THROUGH the broker via serveHelperPlaceholder (loud
// placeholder credential on stdout, real diagnosis on stderr), and
// observability-only failures (the helper log) degrade with a warning. The
// only error returns left are broker-level programming bugs (nil
// Logger/MintRead), never environmental failures.
func runCredentialHelperEnv(action string, in io.Reader, out, diag io.Writer) error {
	logger, closeLog, err := openLog()
	if err != nil {
		// Fail-open on observability: a helper that can't open its own log
		// must still answer git (TM-G8). Warn on stderr and run unlogged.
		fmt.Fprintf(diag, "rein: warning: helper log unavailable (%v); continuing without logging\n", err)
		logger = log.New(io.Discard, "", 0)
		closeLog = func() {}
	}
	defer closeLog()

	stateDir, err := config.StateDir()
	if err != nil {
		// Without a state dir the broker's bookkeeping (read cache, approval
		// records, write-token ledger) has nowhere to live — running the full
		// broker would scatter relative paths. Fail closed on the credential
		// (placeholder, never a real token) but never empty/exit 1.
		return serveHelperPlaceholder(action, in, out, diag, logger,
			fmt.Errorf("state dir unavailable: %w", err),
			fmt.Sprintf("rein: state dir unavailable (%v).\n      Check $HOME / $XDG_STATE_HOME and directory permissions, then retry.", err))
	}

	// ResolveApp is the read-only resolver: env -> state.json+keystore ->
	// fail. It NEVER touches the network — rein run's eager step owns the
	// install-id fetch. On the state path InstallationID may be 0 (uncached);
	// that is NOT an error here. The only error case is "no config at all"
	// (no env AND no state).
	appCfg, ks, _, rerr := config.ResolveApp()
	if rerr != nil {
		return serveHelperPlaceholder(action, in, out, diag, logger,
			fmt.Errorf("ResolveApp failed: %w", rerr),
			fmt.Sprintf("rein: not configured (%v).", rerr))
	}

	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		// Malformed/invalid dev-session.yaml, REIN_SESSION_FILE naming a
		// missing file, or no session file + no fallback repo: the "no
		// session is active" state. Refuse to mint (no scope ceiling exists)
		// but keep the refusal inside rein's domain via the placeholder —
		// an error return here would hand the push to the developer's
		// ambient credentials instead (issue #45). The error text carries
		// the session file path and parse/read detail.
		return serveHelperPlaceholder(action, in, out, diag, logger,
			fmt.Errorf("session load failed: %w", err),
			fmt.Sprintf("rein: cannot load the active session (%v).\n      Fix or remove the session file, or run `rein init` to set up; `rein doctor` diagnoses.", err))
	}
	logger.Printf("session: id=%q role=%q repos=%v source=%s", sess.ID, sess.Role, sess.Repos, sessSource)

	return runCredentialHelperWithConfig(action, in, out, diag, appCfg, ks, sess, stateDir, logger)
}

// serveHelperPlaceholder answers a helper invocation whose pre-broker setup
// failed (no state dir, no App config, no loadable session). It routes the
// request through the REAL broker with a minter that fails with cause, so:
//
//   - a github.com `get` yields the TM-G8 placeholder credential
//     (rein-placeholder-mint-failed) on stdout with exit 0 — never empty
//     stdout, never exit 1. git treats an erroring/empty helper as "no
//     answer" and falls through to other credential sources, so an error
//     here could silently ride the developer's ambient PAT; the placeholder
//     keeps the failure loud and inside rein's domain, and stderr carries
//     the helpful part.
//   - non-github.com hosts still get the protocol's empty "not my host"
//     block, and store/erase remain no-ops — identical to a healthy helper.
//
// hint is printed to diag only when the broker actually falls back to the
// placeholder (github.com get), so non-github invocations stay byte-identical
// on stderr too. ReadCachePath is intentionally unset: a helper in a failed
// state must never serve a stale cached token.
func serveHelperPlaceholder(action string, in io.Reader, out, diag io.Writer, logger *log.Logger, cause error, hint string) error {
	logger.Printf("%v; serving TM-G8 placeholder for github.com get", cause)
	failMint := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		return "", time.Time{}, cause
	})
	return broker.RunCredentialHelper(action, in, out, broker.Config{
		MintRead:    failMint,
		MintWrite:   failMint,
		MintTimeout: mintTimeout,
		Logger:      logger,
		Diag:        &hintFirstWriter{w: diag, hint: hint},
	})
}

// hintFirstWriter prepends a failure-specific hint line before the first
// write. The broker writes to Diag only when a github.com get actually falls
// back to the placeholder, which scopes the hint to exactly the invocations
// where the failure is user-visible.
type hintFirstWriter struct {
	w       io.Writer
	hint    string
	printed bool
}

func (h *hintFirstWriter) Write(p []byte) (int, error) {
	if !h.printed {
		h.printed = true
		fmt.Fprintln(h.w, h.hint)
	}
	return h.w.Write(p)
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
	if testPanicHook != nil {
		testPanicHook(out)
	}
	// The run's EFFECTIVE ceiling (issue #69): the session's standing repos
	// UNION the scope expansions the human approved for THIS run. Every
	// scope-sensitive surface below reads through it — the scope check
	// (InScope) and the token scope (RepoNames) must agree, or an in-scope
	// repo gets a token that doesn't cover it.
	//
	// Scoping the App config here (rather than at the caller) is deliberate:
	// this is the ONE place that knows both the session and the run id.
	rscope := runscope.New(sess, stateDir, os.Getenv("REIN_RUN_ID"))
	appCfg.RepoNames = rscope.BareNames()

	mintRead := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
		if err != nil {
			return "", time.Time{}, err
		}
		return client.MintReadOnlyToken(ctx)
	})
	mintWrite := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		// TM-G6 re-check on every write-token mint (#35 §6): a confirmed
		// issue whose canonical URL now 3xx's was transferred — its
		// confirmation is invalidated; an emptied set fails the mint (the
		// broker serves the TM-G8 placeholder, never an error/empty).
		if runID := os.Getenv("REIN_RUN_ID"); runID != "" {
			ghReadToken := func(ctx context.Context) (string, error) {
				client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
				if err != nil {
					return "", err
				}
				tok, _, err := ghsession.EnsureFresh(ghsession.ReadCachePath(stateDir), client.MintGhReadOnlyToken, client.RevokeToken, 5*time.Minute, mintTimeout, logger)
				return tok, err
			}
			if err := declare.InvalidateTransferred(ctx, stateDir, runID, sess, ghReadToken, logger, diag); err != nil {
				return "", time.Time{}, err
			}
		}
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
		MintRead:    mintRead,
		MintWrite:   mintWrite,
		MintTimeout: mintTimeout,
		Logger:      logger,
		Diag:        diag,
		// The read-token cache is keyed BY SCOPE: a token minted before a
		// scope expansion does not cover the newly-approved repo, and the
		// helper is a fresh process per git op, so an in-memory bust is not
		// available — a distinct cache file per ceiling is the equivalent
		// (issue #69). Same ceiling => same file => caching behaves as before.
		ReadCachePath: filepath.Join(stateDir, "cache", "read-token-"+scopeCacheTag(rscope)+".json"),
		DetectWrite:   func() bool { return detectWriteIntent(logger) },
		Revoke:        revoke,
		InScope:       rscope.Contains,
		// EmptyPathScope stays "" (= allow): existing test setups
		// without useHttpPath=true continue to work. install-shim's
		// instructions recommend setting useHttpPath for strict
		// enforcement.
		ConfirmWrite: buildConfirmWrite(sess, stateDir, diag, logger),
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

// scopeCacheTag is a short, filesystem-safe fingerprint of the run's
// effective ceiling, used to key the read-token cache FILE. Two different
// ceilings never share a cache file, so a token minted before a scope
// expansion can never be served after it (issue #69).
func scopeCacheTag(rs *runscope.Resolver) string {
	sum := sha256.Sum256([]byte(rs.Key()))
	return hex.EncodeToString(sum[:])[:12]
}

// buildConfirmWrite returns the direct-mode credential helper's write
// gate (issue #35 §2): a pure READ of the run's confirmed-issue set. The
// helper no longer prompts inline — the prompt lives at declare time
// (`rein declare <n>`). Empty set (or no run id, or a mid-run session
// edit) ⇒ deny: the core serves the TM-G8 placeholder (never empty) and
// the stderr hint names the exact next step, which git forwards to the
// agent (the #45/TM-G8 diag channel).
//
// Never nil: EVERY direct-mode write is gated on a confirmed issue now
// (one model, both modes — the old issue-less "mint without
// confirmation" direct path is retired with sess.Issue).
//
// diag is the helper's user-facing stderr-like sink (the same seam the
// broker's mint-failed diagnostic uses; git forwards it to the caller).
// It must never be stdout — that is the credential protocol.
func buildConfirmWrite(sess session.Session, stateDir string, diag io.Writer, logger *log.Logger) func(repo string) bool {
	runID := os.Getenv("REIN_RUN_ID")
	sig := approvals.SignatureOf(sess)
	return func(repo string) bool {
		if issues := approvals.ConfirmedIssues(stateDir, runID, sig); len(issues) > 0 {
			logger.Printf("write gate: run %s has %d confirmed issue(s); allowing write to %q", runID, len(issues), repo)
			return true
		}
		printDeclareHint(diag, runID)
		logger.Printf("write gate: no confirmed issue for run %q; returning placeholder (repo=%q)", runID, repo)
		return false
	}
}

// printDeclareHint is the direct-mode deny-channel instruction (issue
// #35 §2): git forwards helper stderr to the caller, so a cooperative
// agent learns the exact next step instead of guessing.
func printDeclareHint(w io.Writer, runID string) {
	if w == nil {
		return // nil Diag discards, same contract as broker.Config.Diag
	}
	fmt.Fprintln(w)
	if runID == "" {
		fmt.Fprintln(w, "rein: write blocked — this operation ran OUTSIDE `rein run` (no REIN_RUN_ID),")
		fmt.Fprintln(w, "  so no issue can be declared for it. Launch your agent via `rein run -- <cmd>`,")
		fmt.Fprintln(w, "  then run `rein declare <n>` and retry.")
	} else {
		fmt.Fprintln(w, "rein: no issue declared for this run — writes are locked.")
		fmt.Fprintln(w, "  Run: rein declare <n>   (the issue number this work is for)")
		fmt.Fprintln(w, "  approve on your terminal, then retry this operation.")
	}
	fmt.Fprintln(w)
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
	case "notice":
		// The out-of-process INSTALL NOTICE surface (issue #69), run by the
		// tmux popup. It renders the pending notice from the run-context
		// snapshot and waits for an acknowledgement. It carries NO approval
		// authority: it cannot write the approval record, and no answer to
		// it grants anything — the real scope-expansion prompt fires when
		// the agent retries its declare.
		if runID == "" {
			return errors.New("approval notice requires --run-id")
		}
		logger, closeLog, err := openLog()
		if err != nil {
			return err
		}
		defer closeLog()
		n, err := grant.NoticeFromRunContext(stateDir, runID)
		if err != nil {
			return err
		}
		cfg := grant.Config{StateDir: stateDir, RunID: runID, PromptTimeout: 10 * time.Minute, Logger: logger}
		if err := grant.AcknowledgeInstallNotice(context.Background(), cfg, n); err != nil {
			// No tty (not in a popup): print it plainly instead.
			grant.WriteInstallNotice(os.Stdout, n)
		}
		return nil
	default:
		fmt.Fprintf(os.Stderr, "rein approval: unknown subcommand %q (want status|clear|grant|notice)\n", sub)
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
			fmt.Printf("  session:    %s role=%s repos=%v\n", s.ID, s.Role, s.Repos)
			if pi := st.Context.PendingIssue; pi != nil {
				fmt.Printf("  pending:    #%d %q in %s (awaiting confirmation)\n", pi.Number, pi.Title, pi.Repo)
			}
		} else {
			fmt.Println("  session:    <no run context on disk>")
		}
		if st.HasApproval {
			fmt.Printf("  approval:   VALID (first confirmed %s)\n", st.Approval.ApprovedAt.Format(time.RFC3339))
			for _, ci := range st.Approval.Issues {
				fmt.Printf("    issue:    #%d %q in %s (confirmed %s)\n", ci.Number, ci.Title, ci.Repo, ci.ConfirmedAt.Format(time.RFC3339))
			}
			if len(st.Approval.Issues) == 0 {
				fmt.Println("    (no confirmed issues — writes locked until `rein declare <n>`)")
			}
		} else {
			fmt.Println("  approval:   none (agent must run `rein declare <n>`)")
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
