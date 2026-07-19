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
		cwd, _ := os.Getwd()
		return 1, fmt.Errorf("load session: %w (see README for dev-session.yaml format)\n      %s", err, noSessionHint(cwd))
	}

	// One-time, user-facing note if the env App identity is half-configured
	// (some REIN_APP_* set, some missing) — surfaced here at launch rather
	// than from the per-git-op credential helper, which would spam stderr.
	config.WarnPartialAppEnv(os.Stderr)

	// Eagerly verify that the App's installation COVERS every session repo,
	// BEFORE the child starts, so a 404 (App not installed / repo not in the
	// installation) fails loud here instead of degrading to a TM-G8 placeholder
	// inside the child's first git op (issue #44 D4). This runs on BOTH config
	// sources — an installation id being present in the env is not evidence the
	// installation covers these repos (issue #68). Only the resulting state.json
	// cache write is state-path-only; that single write covers every later
	// helper / rein-gh invocation (shims only run inside the PATH this wrapper
	// sets up — single writer).
	ctx, cancel := context.WithTimeout(context.Background(), installIDTimeout)
	err = resolveAndCacheInstallID(ctx, sess, newAppInstallProber)
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

	// Ambient GitHub tokens were already removed from rein's own process at startup
	// (scrubAmbientTokensFromSelf, §8), so they are absent from this child env too.
	// The agent must use only rein-brokered credentials, never a long-lived token the
	// developer happens to have in their shell. Belt-and-suspenders: unset again (a
	// no-op) and report what was removed. This does NOT remove gh's stored login
	// (keyring); a determined same-UID agent can still reach that — that is what the
	// sandbox is for (issue #7).
	for _, name := range ambientGitHubTokenNames {
		env = unsetEnv(env, name)
	}
	scrubbed := ambientTokenNamesRemoved

	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Tell the operator what's about to happen so they know what to
	// look for when their wrapped agent (claude, etc.) hits a write.
	fmt.Fprintln(os.Stderr, "rein: launching wrapped process with:")
	fmt.Fprintf(os.Stderr, "  session: %s (role=%s, repos=%v) [source=%s]\n",
		sess.ID, sess.Role, sess.Repos, sessSource)
	fmt.Fprintf(os.Stderr, "  PATH-front shim dir: %s\n", shimDir)
	fmt.Fprintf(os.Stderr, "  per-process git config: %s\n", gitConfigPath)
	fmt.Fprintln(os.Stderr, "  (your real ~/.gitconfig is layered in via include.path)")
	if len(scrubbed) > 0 {
		fmt.Fprintf(os.Stderr, "  scrubbed from child env: %s (agent uses rein-brokered creds only)\n", strings.Join(scrubbed, ", "))
	}
	fmt.Fprintln(os.Stderr)
	sess.WarnIgnoredIssue(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Writes are LOCKED until the agent declares its issue:  rein declare <n>")
	fmt.Fprintln(os.Stderr, "  then push to agent/<n>/<nonce>. The declaration will prompt on THIS terminal")
	fmt.Fprintln(os.Stderr, "  (or a tmux popup); approving covers all writes for this run.")
	fmt.Fprintln(os.Stderr)
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

// installProber is the App-JWT surface the pre-launch coverage check needs:
// one GET per session repo to learn which installation covers it, plus (on the
// env path only) a GET /app to learn the App's slug for the refusal deep-link.
// *githubapp.AppClient satisfies this directly; tests inject a fake, which is
// why the check takes a FACTORY rather than reaching for NewAppClient itself.
//
// Note on cost: AppClient deliberately reads the PEM and mints a fresh App JWT
// INSIDE each call ("never holds key material across calls" — see its doc), so
// constructing the prober once per launch does not make the PEM read or the JWT
// signing once-per-launch. That is by design and left alone: this is a
// once-per-launch path over a handful of repos, and making it "mint once" would
// mean adding TTL-aware token caching to a deliberately stateless security
// component for a saving too small to measure.
type installProber interface {
	RepoInstallationID(ctx context.Context, owner, repo string) (int64, error)
	AppSlug(ctx context.Context) (string, error)
}

// installProberFactory builds the prober for a launch. Injected so tests can
// stub the network; production wires newAppInstallProber.
type installProberFactory func(clientID string, ks keystore.Keystore, roleName string) (installProber, error)

// newAppInstallProber is the production installProberFactory: an App-JWT client
// (no installation id required — the id is what we're discovering).
// REIN_GITHUB_API_BASE overrides the API root (empty -> api.github.com), the
// same escape hatch the appsetup conversion path exposes for testing.
func newAppInstallProber(clientID string, ks keystore.Keystore, roleName string) (installProber, error) {
	return githubapp.NewAppClient(clientID, ks, roleName, os.Getenv("REIN_GITHUB_API_BASE"))
}

// fetchRepoInstallationID is the one-shot install-coverage probe used by the
// #69 scope-expansion paths (declare, run_sandboxed, session): build the App-JWT
// prober via newAppInstallProber and GET the installation covering owner/repo.
// A non-nil error means the App is not installed on that repo (404) or the
// lookup failed — the caller fails loud with the install deep-link. It reuses
// newAppInstallProber so there is a single App-JWT client construction seam.
func fetchRepoInstallationID(ctx context.Context, clientID string, ks keystore.Keystore, roleName, owner, repo string) (int64, error) {
	prober, err := newAppInstallProber(clientID, ks, roleName)
	if err != nil {
		return 0, err
	}
	return prober.RepoInstallationID(ctx, owner, repo)
}

// resolveAndCacheInstallID verifies that the App's installation actually COVERS
// every repo in the session, on BOTH config sources, and — on the manifest-flow
// state path only — caches the resolved id in state.json.
//
// The verification is the point (issue #44 D4); the caching is a state-path
// side effect. An installation id merely being PRESENT (which is always true on
// the env path, where REIN_APP_INSTALLATION_ID supplies it) says nothing about
// whether that installation's selected-repo list includes the session's repos.
// Skipping the probes on the env path was issue #68: launch succeeded and the
// uncovered repo surfaced only as a TM-G8 placeholder inside the agent — the
// outcome design.md §4.2.4 / design.md:581 requires we refuse at launch instead.
//
// So: ALWAYS one App-JWT GET per session repo, whatever the source.
//
//   - 404 on ANY repo -> fail loud naming that repo, with an install
//     deep-link, and DO NOT launch.
//   - Repos resolving to DIFFERENT ids -> fail loud. The session is
//     single-owner (session.Validate) and an installation is single-owner,
//     so all lookups must agree on one id.
//   - Transient (non-404) lookup errors degrade to a warning as long as SOME
//     id is available (resolved from another repo, or already known) — a
//     GitHub blip must not ground a session a known id would have served.
//     All-transient with NO id at all -> fail closed.
//
// The two sources then differ in exactly one place, the id we already hold:
//
//   - State path: the known id is state.json's cached one. A probed id that
//     differs is a REFRESH (uninstall/reinstall rotates the id) — rewrite
//     state.json, don't error.
//   - Env path: the known id is REIN_APP_INSTALLATION_ID, which the operator
//     asserted explicitly. A probed id that differs means the env var
//     CONTRADICTS GitHub — mints would be scoped to an installation that is
//     not the one covering these repos. Fail loud; there is no state.json to
//     own and env is not ours to rewrite.
func resolveAndCacheInstallID(ctx context.Context, sess session.Session, newProber installProberFactory) error {
	appCfg, ks, source, err := config.ResolveApp()
	if err != nil {
		// No env AND no state -> genuine config error; don't launch.
		return fmt.Errorf("resolve App config: %w", err)
	}

	// One prober per launch, reused for every repo probe (and the env-path slug
	// lookup below) rather than reconstructed per repo.
	prober, err := newProber(appCfg.ClientID, ks, config.AppKeystoreRole)
	if err != nil {
		return fmt.Errorf("build App client: %w", err)
	}

	// Source-specific inputs: the id we already hold, and — built lazily, only
	// if we actually have to refuse — how we name the App and where we send the
	// operator to fix coverage. State path reads both from state.json; env path
	// has no state.json, so it asks GitHub for the slug (AppSlug is the only way
	// to learn it on the env path) and degrades to the generic installations page
	// if that lookup fails. Lazy because it costs a GET and the happy path — the
	// installation covers everything — must not pay for an error message.
	var (
		configDir string         // state path only (WriteState target)
		s         appsetup.State // state path only
		knownID   int64          // id already in hand (env var, or cached state)
	)
	coverageFixHint := func() (appLabel, installURL string) {
		if source == config.SourceEnv {
			slug, err := prober.AppSlug(ctx)
			if err != nil || slug == "" {
				// Slug lookup failed. The refusal still stands — only the
				// quality of the pointer degrades. Never let this turn a clean
				// refusal into a confusing lookup error.
				return "the App (client id " + appCfg.ClientID + ")",
					"https://github.com/settings/installations"
			}
			return "App " + slug, "https://github.com/apps/" + slug + "/installations/new"
		}
		htmlURL := s.Primary.HTMLURL
		if htmlURL == "" {
			htmlURL = "https://github.com/apps/" + s.Primary.Slug
		}
		return "App " + s.Primary.Slug, htmlURL + "/installations/new"
	}

	if source == config.SourceEnv {
		// Non-zero: LoadAppConfig rejects a missing, unparseable, or <= 0
		// REIN_APP_INSTALLATION_ID, so SourceEnv always carries a real id.
		knownID = appCfg.InstallationID
	} else {
		configDir, err = config.ConfigDir()
		if err != nil {
			return err
		}
		s, err = appsetup.ReadState(configDir)
		if err != nil {
			return fmt.Errorf("read state.json: %w", err)
		}
		if s.Primary == nil {
			return fmt.Errorf("state.json has no primary App record; run `rein init`")
		}
		knownID = s.Primary.InstallationID
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

		id, err := prober.RepoInstallationID(ctx, owner, repo)
		if err != nil {
			if errors.Is(err, githubapp.ErrAppNotInstalled) {
				// 404 is definitive: the App is not installed on this repo (or
				// the repo is not in the installation's selected-repo list).
				// Fail loud with the deep-link; don't launch.
				appLabel, installURL := coverageFixHint()
				return fmt.Errorf("%s is not installed on %s/%s; install it (or add the repo to the installation) at %s, then re-run",
					appLabel, owner, repo, installURL)
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
		// Every lookup failed transiently. If we already have an id, the
		// session can run from it — degrade to a warning and proceed rather
		// than blocking launch on a hiccup. Only when we have NO id to fall
		// back to (state path, first fetch) do we fail closed. On the env path
		// knownID is always non-zero, so this is always the warn-and-proceed.
		if knownID != 0 {
			fmt.Fprintf(os.Stderr, "rein: warning: could not verify installation coverage of session repos (%v); proceeding with installation id %d\n",
				lastErr, knownID)
			return nil
		}
		return fmt.Errorf("fetch installation id for session repos %v: %w", sess.Repos, lastErr)
	}

	if source == config.SourceEnv {
		// Env path: nothing to cache. The only thing left to check is that the
		// operator's asserted id agrees with GitHub — if it doesn't, every mint
		// this session makes would target the WRONG installation. Fail loud.
		if resolvedID != knownID {
			return fmt.Errorf("REIN_APP_INSTALLATION_ID=%d contradicts GitHub: %s resolves to installation %d; fix REIN_APP_INSTALLATION_ID, then re-run",
				knownID, resolvedRepo, resolvedID)
		}
		return nil
	}

	// State path: a changed id is a refresh, not an error (uninstall/reinstall
	// rotates it). Rewrite state.json only when it actually changed.
	if resolvedID != knownID {
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
// network failure, or an unexpected revoke status all degrade to "token lives
// to its native TTL," never a user-facing error.
//
// The ledger is DEDUPED BY TOKEN VALUE first (issue #67). The proxy memoizes
// one write token for the whole run, but brokercore appends a ledger entry on
// every write-serving request (cache hit or fresh mint), so a run with 3 pushes
// ledgers the SAME token 6 times (info/refs + receive-pack per push). Revoking
// it once succeeds and the rest 404 ("already gone"), which used to print a
// warning per duplicate and a nonsense "revoked 1 of 6".
//
// Deduping at the CONSUMER is the deliberate layer choice. The two alternatives
// are both worse:
//   - At append time: AppendWriteToken would have to read-modify-write the
//     ledger, losing the single atomic O_APPEND that lets concurrent helper
//     invocations (parallel pushes) append without clobbering each other.
//   - At record time (suppressing the repeat RecordWrite in internal/proxy):
//     that couples the ledger to in-memory session state. AppendWriteToken is
//     best-effort and its error is swallowed (TM-G8 — the token must reach the
//     client regardless), so if the FIRST append failed, every later one would
//     be suppressed as a duplicate and a LIVE token would never be ledgered,
//     hence never revoked. Fail-open. The repeated appends are the at-least-once
//     margin that heals a transient append failure; the duplicates are cheap.
//
// Deduping here instead keys on what the ledger ACTUALLY contains, covers BOTH
// callers (exit-revoke and the OnExpire path), is robust to duplicates from any
// other source, and still revokes a legitimately-distinct second token (a
// post-expiry or post-backoff re-mint).
func revokeRunWriteTokens(stateDir, runID string, revoke revokeTokenFunc, now time.Time) {
	entries, err := approvals.ReadWriteTokens(stateDir, runID)
	if err != nil || len(entries) == 0 {
		return
	}
	var revoked, total int
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Token == "" || seen[e.Token] {
			continue
		}
		seen[e.Token] = true
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
// ambientGitHubTokenNames are the env vars carrying a GitHub token. rein mints its
// own credentials via the App key and never consumes these, so it removes them from
// its OWN process env at startup (scrubAmbientTokensFromSelf) — see §8.
var ambientGitHubTokenNames = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// Captured at startup by scrubAmbientTokensFromSelf, BEFORE the env-unset: the names
// (for the child-scrub reassurance message) and values (for the fail-closed launch
// self-check). The values live in rein's heap only — ptrace-protected, never back in
// env/argv — which is the whole co-located-broker discipline (§8).
var (
	ambientTokenNamesRemoved  []string
	ambientTokenValuesRemoved []string
)

// scrubAmbientTokensFromSelf removes the operator's ambient GitHub tokens from rein's
// OWN process env at startup (§8 co-located-broker hardening): nono has no PID
// namespace, so the agent could read /proc/<rein-pid>/environ if nono's environ block
// ever regressed — rein must expose no real credential there. rein mints via the App
// key and never reads these, so removal is safe. Idempotent; call once, early in main.
func scrubAmbientTokensFromSelf() {
	if ambientTokenNamesRemoved != nil || ambientTokenValuesRemoved != nil {
		return
	}
	ambientTokenNamesRemoved = []string{}
	ambientTokenValuesRemoved = []string{}
	for _, name := range ambientGitHubTokenNames {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			ambientTokenNamesRemoved = append(ambientTokenNamesRemoved, name)
			ambientTokenValuesRemoved = append(ambientTokenValuesRemoved, v)
		}
		os.Unsetenv(name)
	}
}

// ambientTokenValues returns the ambient GitHub token values removed from rein's
// process at startup, for the co-located-broker launch self-check (§8): these must
// not survive into the sandbox child's argv or env (agent-readable via /proc).
func ambientTokenValues() []string { return ambientTokenValuesRemoved }

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
