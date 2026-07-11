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
//   - Approvals are per-run (keyed by REIN_RUN_ID, generated here): the
//     human prompt fires on the FIRST write of THIS run and stays silent
//     for subsequent writes for the run's lifetime (no clock TTL). Each
//     `rein run` is isolated — approving one does not approve another.
//     This run's approval files are cleared on exit; orphans from killed
//     runs are swept on the next launch.
//
//   - /dev/tty inside the wrapped agent: empirical question. Three
//     outcomes depending on what the agent does with the controlling
//     terminal — documented to the operator at startup so they know
//     what to look for.

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/session"
)

// installIDTimeout caps the pre-launch installation-id lookups as a whole.
// One cheap App-JWT GET per session repo per `rein run` launch (not per git
// op; sessions hold a handful of repos at most), so keep it modest but
// generous enough for a cold JWT mint + the round-trips.
const installIDTimeout = 10 * time.Second

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

	// Best-effort sweep of orphaned per-run approval/run-context files
	// from runs whose owning `rein run` is gone (dead pid, or unknown pid
	// older than the backstop). This is the SIGKILL backstop — deferred
	// clear-on-exit handles the catchable-exit paths. Non-fatal: a sweep
	// hiccup must never block a launch.
	if err := approvals.Sweep(stateDir, 24*time.Hour, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "rein: warning: orphan approval sweep: %v\n", err)
	}
	// Migration: silently remove any pre-upgrade global approval.json.
	// Nothing reads it anymore (approvals are per-run now).
	_ = os.Remove(filepath.Join(stateDir, "approval.json"))

	// Validate session — same loader used elsewhere, so the same
	// REIN_SESSION_FILE override and env-fallback rules apply.
	sess, sessSource, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return 1, fmt.Errorf("load session: %w (see README for dev-session.yaml format)", err)
	}

	// One-time, user-facing note if the env App identity is half-configured
	// (some REIN_APP_* set, some missing) — surfaced here at launch rather
	// than from the per-git-op credential helper, which would spam stderr.
	config.WarnPartialAppEnv(os.Stderr)

	// Eagerly resolve + cache the installation id for EVERY session repo on
	// the manifest-flow (state.json) path, BEFORE the child starts, so a 404
	// (App not installed / repo not in the installation) fails loud here
	// instead of degrading to a TM-G8 placeholder inside the child's first
	// git op (issue #44 D4). No-op on the env path.
	// This single cache write covers every later helper / rein-gh invocation
	// (shims only run inside the PATH this wrapper sets up — single writer).
	ctx, cancel := context.WithTimeout(context.Background(), installIDTimeout)
	err = resolveAndCacheInstallID(ctx, sess, fetchRepoInstallationID)
	cancel()
	if err != nil {
		return 1, err
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

	// Generate this run's nonce and clear its per-run approval/run-context
	// files on exit. Fail-closed: a crypto/rand error aborts the launch
	// rather than running with an empty/guessable run id (which would make
	// approvals globally reusable again). ClearRun is idempotent and
	// mirrors the tempdir cleanup above: deferred-only is sufficient for
	// SIGTERM-to-rein and normal exit (cmd.Wait returns, defers fire). The
	// signal goroutine below only FORWARDS SIGTERM — adding a competing
	// clear there would race this defer. Terminal SIGINT (Ctrl-C) is NOT
	// trapped (see package doc): it reaches rein via the shared process
	// group and kills it under the default disposition, so THIS defer does
	// not fire on that path — nor does SIGKILL. Both are covered by the
	// launch Sweep above (the dead owning pid is detected and its files
	// swept). That is the intended division: catchable normal/ SIGTERM exit
	// -> this defer; uncatchable/untrapped (SIGINT, SIGKILL) -> launch Sweep.
	runID, err := newRunID()
	if err != nil {
		return 1, fmt.Errorf("generate run id: %w", err)
	}
	defer func() { _ = approvals.ClearRun(stateDir, runID) }()

	// Build the wrapped child's env.
	env := os.Environ()
	env = setEnv(env, "PATH", shimDir+":"+os.Getenv("PATH"))
	env = setEnv(env, "GIT_CONFIG_GLOBAL", gitConfigPath)
	env = setEnv(env, "GIT_CONFIG_SYSTEM", "/dev/null")
	// Per-run approval scoping: the child's credential helper and rein-gh
	// shim inherit these and key their approval lookup/record to this run.
	// No REIN_RUN_ID in a child means it was invoked outside `rein run`,
	// where the helper fails closed (re-prompts every write).
	env = setEnv(env, "REIN_RUN_ID", runID)
	env = setEnv(env, "REIN_RUN_PID", strconv.Itoa(os.Getpid()))

	// Scrub ambient GitHub tokens from the wrapped child. The agent must
	// use only rein-brokered credentials, never a long-lived token the
	// developer happens to have in their shell. Safe: git ignores these
	// (it authenticates via the credential helper), and gh ops go through
	// the rein-gh shim, which mints + sets its OWN GH_TOKEN, overriding
	// any inherited value. This closes the easiest Shape B bypass — an
	// agent reading the ambient token. It does NOT remove gh's stored
	// login (keyring); a determined same-UID agent can still reach that,
	// which is what the Phase 1 sandbox is for (issue #7).
	var scrubbed []string
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		if os.Getenv(name) != "" {
			scrubbed = append(scrubbed, name)
		}
		env = unsetEnv(env, name)
	}

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
	if len(scrubbed) > 0 {
		fmt.Fprintf(os.Stderr, "  scrubbed from child env: %s (agent uses rein-brokered creds only)\n", strings.Join(scrubbed, ", "))
	}
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

	// The child is gone — the reliable operation-complete signal (issue
	// #20). Best-effort revoke any write tokens minted during this run,
	// tightening a successful push's effective write-token lifetime from
	// GitHub's native ~1h down to the run duration. The deferred
	// approvals.ClearRun (registered above) removes the ledger file
	// afterward. SIGINT/SIGKILL skip this path; those tokens live to
	// their native TTL (the accepted floor) and the orphaned ledger is
	// reaped by the next launch's Sweep.
	revokeRunWriteTokens(stateDir, runID, productionRevoke(sess), time.Now())

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

// installIDLookup fetches the installation id for owner/repo using an
// App-JWT GET. Injected so tests can stub the network call with an
// httptest server; production wires fetchRepoInstallationID.
type installIDLookup func(ctx context.Context, clientID string, ks keystore.Keystore, roleName, owner, repo string) (int64, error)

// fetchRepoInstallationID is the production installIDLookup: build an App-JWT
// client (no installation id required) and GET /repos/{owner}/{repo}/installation.
// REIN_GITHUB_API_BASE overrides the API root (empty -> api.github.com), the
// same escape hatch the appsetup conversion path exposes for testing.
func fetchRepoInstallationID(ctx context.Context, clientID string, ks keystore.Keystore, roleName, owner, repo string) (int64, error) {
	c, err := githubapp.NewAppClient(clientID, ks, roleName, os.Getenv("REIN_GITHUB_API_BASE"))
	if err != nil {
		return 0, err
	}
	return c.RepoInstallationID(ctx, owner, repo)
}

// resolveAndCacheInstallID ensures state.json carries a fresh installation id
// for the session repos when running off the manifest-flow state path.
//
// No-op on the env path (id already present in the env config). On the state
// path it ALWAYS performs one App-JWT GET per session repo and rewrites
// state.json only when the id changed — this one rule covers both first-fetch
// and stale-id refresh (uninstall/reinstall rotates the id).
//
// EVERY repo in sess.Repos is probed, not just the first: mints are lazy
// closures scoped to all session repos, so a repo the installation's
// selected-repo list excludes would otherwise surface only as a TM-G8
// placeholder inside the agent — design.md §4.2.4 requires a loud
// launch-time error instead (issue #44 D4). 404 on ANY repo -> fail loud
// naming that repo, with the install deep-link, and DO NOT launch. The
// session is single-owner (session.Validate) and an installation is
// single-owner, so all lookups must agree on one id; a mismatch is also a
// loud error. Transient (non-404) lookup errors degrade to a warning as
// long as SOME id is available (resolved from another repo, or cached) —
// a GitHub blip must not ground a session a cached id would have served.
func resolveAndCacheInstallID(ctx context.Context, sess session.Session, lookup installIDLookup) error {
	appCfg, ks, source, err := config.ResolveApp()
	if err != nil {
		// No env AND no state -> genuine config error; don't launch.
		return fmt.Errorf("resolve App config: %w", err)
	}
	if source == config.SourceEnv {
		// Env path: installation id is already present and authoritative;
		// there is no state.json to own. Skip the GETs entirely.
		return nil
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	s, err := appsetup.ReadState(configDir)
	if err != nil {
		return fmt.Errorf("read state.json: %w", err)
	}
	if s.Primary == nil {
		return fmt.Errorf("state.json has no primary App record; run `rein init`")
	}

	var (
		resolvedID   int64  // first successfully resolved id this launch
		resolvedRepo string // repo that resolved it (for the mismatch error)
		lastErr      error  // last transient lookup error (for the all-failed message)
	)
	for _, r := range sess.Repos {
		owner, _, ok := strings.Cut(r, "/")
		repo := bareRepoName(r)
		if !ok || owner == "" || repo == "" {
			return fmt.Errorf("session repo %q is not owner/name", r)
		}

		id, err := lookup(ctx, appCfg.ClientID, ks, config.AppKeystoreRole, owner, repo)
		if err != nil {
			if errors.Is(err, githubapp.ErrAppNotInstalled) {
				// 404 is definitive: the App is not installed on this repo (or
				// the repo is not in the installation's selected-repo list).
				// Fail loud with the deep-link; don't launch.
				htmlURL := s.Primary.HTMLURL
				if htmlURL == "" {
					htmlURL = "https://github.com/apps/" + s.Primary.Slug
				}
				return fmt.Errorf("App %s is not installed on %s/%s; install it (or add the repo to the installation) at %s/installations/new, then re-run",
					s.Primary.Slug, owner, repo, htmlURL)
			}
			// Transient (non-404) error for THIS repo: warn and keep probing
			// the rest — a definitive 404 on a later repo must still refuse
			// the launch. Whether the launch can proceed at all is decided
			// after the loop, based on what id (if any) we ended up with.
			fmt.Fprintf(os.Stderr, "rein: warning: could not verify installation coverage of %s/%s (%v); git operations on it may fail mid-session if the installation does not cover it\n", owner, repo, err)
			lastErr = err
			continue
		}

		if resolvedID == 0 {
			resolvedID, resolvedRepo = id, owner+"/"+repo
		} else if id != resolvedID {
			// Same owner ⇒ same installation is the invariant; two ids means
			// the state is inconsistent in a way mints cannot serve. Fail loud.
			return fmt.Errorf("session repos resolve to different installation ids (%s -> %d, %s/%s -> %d); a single-owner session must map to one installation — check the App's installations, then re-run",
				resolvedRepo, resolvedID, owner, repo, id)
		}
	}

	if resolvedID == 0 {
		// Every lookup failed transiently. If we already have a cached id,
		// the session can run from it — degrade to a warning and proceed
		// rather than blocking launch on a hiccup. Only when we have NO id
		// to fall back to (first fetch) do we fail closed.
		if s.Primary.InstallationID != 0 {
			fmt.Fprintf(os.Stderr, "rein: warning: could not refresh installation id (%v); proceeding with cached id %d\n",
				lastErr, s.Primary.InstallationID)
			return nil
		}
		return fmt.Errorf("fetch installation id for session repos %v: %w", sess.Repos, lastErr)
	}

	if resolvedID != s.Primary.InstallationID {
		s.Primary.InstallationID = resolvedID
		if err := appsetup.WriteState(configDir, s); err != nil {
			return fmt.Errorf("cache installation id: %w", err)
		}
	}
	return nil
}

// revokeWriteTokenTimeout caps each best-effort exit-time revoke. Tight:
// the user is waiting for `rein run` to return.
const revokeWriteTokenTimeout = 5 * time.Second

// revokeTokenFunc revokes a single installation token server-side.
// Injected so tests can stub the network call (mirrors installIDLookup).
type revokeTokenFunc func(ctx context.Context, token string) error

// productionRevoke builds the real revokeTokenFunc: resolve the App config,
// set RepoName from the session (ResolveApp leaves it empty on the
// state.json path, and NewClient requires it even though RevokeToken — a
// self-authenticating DELETE /installation/token — ignores it), construct a
// client, and revoke. Any failure is returned for the caller to log; it is
// never fatal (the token expires on its own ~1h TTL). Mirrors the broker's
// revoke closure in main.go.
func productionRevoke(sess session.Session) revokeTokenFunc {
	return func(ctx context.Context, token string) error {
		appCfg, ks, _, err := config.ResolveApp()
		if err != nil {
			return err
		}
		appCfg.RepoNames = sess.BareRepoNames()
		client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
		if err != nil {
			return err
		}
		return client.RevokeToken(ctx, token)
	}
}

// revokeRunWriteTokens drains this run's write-token ledger and best-effort
// revokes every still-valid token (issue #20). Already-expired entries are
// skipped (revoke is pointless). The ledger FILE is left for the caller's
// deferred approvals.ClearRun to remove — the single per-run-file lifecycle
// owner. Best-effort throughout: a missing/empty ledger, a client-build or
// network failure, or a non-204 revoke all degrade to "token lives to its
// native TTL," never a user-facing error.
func revokeRunWriteTokens(stateDir, runID string, revoke revokeTokenFunc, now time.Time) {
	entries, err := approvals.ReadWriteTokens(stateDir, runID)
	if err != nil || len(entries) == 0 {
		return
	}
	var revoked, total int
	for _, e := range entries {
		if e.Token == "" {
			continue
		}
		total++
		if !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt) {
			continue // already expired; nothing to revoke
		}
		ctx, cancel := context.WithTimeout(context.Background(), revokeWriteTokenTimeout)
		rerr := revoke(ctx, e.Token)
		cancel()
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "rein: warning: exit-revoke of a write token failed (best-effort; it expires on its own): %v\n", rerr)
			continue
		}
		revoked++
	}
	if total > 0 {
		fmt.Fprintf(os.Stderr, "rein: revoked %d of %d write token(s) on exit\n", revoked, total)
	}
}

// newRunID returns a per-run nonce: 16 bytes from crypto/rand encoded as
// 22 chars of base64url (no padding, no slashes — filename-safe). The
// randomness is what makes a stale approval file from a crashed run
// unreusable: no future run shares the id.
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
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

// unsetEnv removes every entry for name from env, returning a new slice.
func unsetEnv(env []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
