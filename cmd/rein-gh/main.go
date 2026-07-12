// rein-gh is a thin wrapper around the system `gh`. On each invocation it
// classifies the subcommand as read or write, then routes:
//
//   - Read (or unknown): fetch a cached read-tier token (or mint+cache one
//     if stale), set GH_TOKEN, exec real gh. The cached token has only
//     {issues:read, pulls:read, contents:read, metadata:read} so an exfil
//     during the cache window grants read-only capability.
//
//   - Write: mint a fresh implement-tier token (no caching), set GH_TOKEN,
//     fork the real gh, wait for it to exit, best-effort revoke the token,
//     and return gh's exit code. The effective TTL of the write token is
//     "gh process lifetime + revoke latency" — typically <1s, far below
//     GitHub's ~1h floor.
//
// Misclassification at this layer routes to the safer tier (read) for
// unknowns: write ops that aren't in the table will fail with a clear
// 403 from GitHub rather than silently using a write-capable token.
// Cannot exceed the token's permission ceiling either way.
//
// Installed at the front of $PATH alongside rein-git by `rein
// install-shim`.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/declare"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/runscope"
	"github.com/TomHennen/rein/internal/session"
)

const (
	// refreshSkew refreshes the cached read token when less than this much
	// life remains, so a long-running gh subcommand doesn't time out
	// mid-call.
	refreshSkew = 5 * time.Minute

	// mintTimeout caps each mint and revoke call. gh users feel this
	// directly on cache-miss; keep tight.
	mintTimeout = 5 * time.Second

	// approvalTTL mirrors cmd/rein's value. It is stamped into
	// Record.ExpiresAt as a sweep/status heuristic ONLY — NOT a re-prompt
	// trigger (the run lifetime is the bound). Kept in sync manually for
	// Phase 0; CP6+ should centralize.
	approvalTTL = 4 * time.Hour

	// reentryEnv is set by the shim before exec'ing the real gh, so any
	// re-entrant shim invocation (gh forking itself internally) skips the
	// mint+revoke cycle and reuses the GH_TOKEN already in the env.
	// Without this we mint twice and revoke twice per user-visible gh
	// invocation — wasteful + extra GitHub API pressure.
	//
	// Security note: this is a performance optimization, not a security
	// boundary. An attacker who can set env vars in the user's shell can
	// already read GH_TOKEN directly; setting reentryEnv lets them skip
	// the mint+revoke cycle but gains no capability beyond what they had
	// from reading the env. The shape-B token-via-env exposure is the
	// real surface; Phase 1 sandbox composition closes it.
	reentryEnv = "REIN_GH_SHIM_ACTIVE"
)

func main() {
	stateDir, err := config.StateDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein-gh: %v\n", err)
		os.Exit(127)
	}
	logger, closeLog, err := openLog(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein-gh: %v\n", err)
		os.Exit(127)
	}
	defer closeLog()

	realGh, err := findRealGh(os.Args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein-gh: %v\n", err)
		os.Exit(127)
	}

	cls := classify(os.Args[1:])
	logger.Printf("invocation: classified=%q argv=%q", cls, os.Args[1:])

	// Re-entrant invocation: a parent shim already set GH_TOKEN. gh
	// internally forks itself for some operations; the child would
	// otherwise mint+revoke its own token, doubling API pressure per
	// user-visible gh command. Skip and exec directly.
	//
	// Carry the parent shim's GH_TOKEN through (that is the whole point of
	// the re-entry guard) but rebuild the rest from baseEnv, so the OTHER
	// three credential vars are still stripped. On the legitimate path this
	// is a no-op — the parent already scrubbed them. It matters only when
	// the guard is set with a stale or hand-crafted environment, where it
	// keeps three of the four vars from riding through unexamined.
	if os.Getenv(reentryEnv) != "" {
		logger.Printf("re-entrant invocation detected (%s set); execing real gh directly", reentryEnv)
		env := baseEnv()
		if parentToken := os.Getenv("GH_TOKEN"); parentToken != "" {
			env = append(env, "GH_TOKEN="+parentToken)
		}
		argv := append([]string{realGh}, os.Args[1:]...)
		if err := syscall.Exec(realGh, argv, env); err != nil {
			fmt.Fprintf(os.Stderr, "rein-gh: exec %s: %v\n", realGh, err)
			os.Exit(127)
		}
	}

	switch cls {
	case "write":
		os.Exit(runWrite(realGh, os.Args[1:], stateDir, logger))
	case "local":
		runLocalAndExec(realGh, os.Args[1:], logger)
	default: // "read" or "unknown"
		runReadAndExec(realGh, os.Args[1:], stateDir, logger)
	}
}

// runLocalAndExec handles the subcommands that manage gh's OWN local state
// rather than talking to GitHub on rein's behalf: auth, config, alias,
// extension. They get baseEnv() — every ambient credential still stripped, so
// nothing leaks — but NO GH_TOKEN of any kind, not even the placeholder.
//
// Injecting a token here was a bug (found in review of #57): gh 2.67 refuses
// to run `gh auth login`/`logout` while GH_TOKEN is set —
//
//	"The value of the GH_TOKEN environment variable is being used for
//	 authentication. To have GitHub CLI store credentials instead, first
//	 clear the value from the environment."
//
// — so the fail-closed placeholder blocked the exact command a user needs to
// REPAIR a setup where rein cannot mint. The recovery path must stay open.
//
// It does not stay open for the AGENT. When REIN_RUN_ID is set we are inside a
// `rein run`, so the caller is the agent, not the human, and the whole `auth`
// noun is refused: see refuseAgentAuth.
func runLocalAndExec(realGh string, args []string, logger *log.Logger) {
	if noun := firstNoun(args); noun == "auth" && os.Getenv("REIN_RUN_ID") != "" {
		refuseAgentAuth(os.Stderr, args)
		logger.Printf("local tier: refused `gh %s` inside a rein run (agent must not manage the developer's gh credentials)", argSummary(args))
		os.Exit(1)
	}
	logger.Printf("local tier: execing gh with no GH_TOKEN (local/credential-management subcommand)")
	argv := append([]string{realGh}, args...)
	if err := syscall.Exec(realGh, argv, baseEnv()); err != nil {
		fmt.Fprintf(os.Stderr, "rein-gh: exec %s: %v\n", realGh, err)
		os.Exit(127)
	}
}

// refuseAgentAuth explains why `gh auth ...` is denied inside a `rein run`.
//
// The whole noun is refused, not just the mutating verbs, because every verb
// under it either mutates the developer's credential store or reads it back:
//
//   - login / logout / refresh: re-authenticate or de-authenticate the HUMAN's
//     gh install. An agent acquiring the developer's login is the thing rein
//     exists to prevent.
//   - setup-git: rewrites git's credential.helper — literal broker
//     displacement (design §5.3, TM-G8).
//   - token: PRINTS the token gh would use. Verified on gh 2.67. rein run
//     scrubs the ambient token vars from the agent's env, so gh falls back to
//     hosts.yml and prints the developer's stored PAT straight to the agent's
//     stdout. That is a direct exfil of the credential the scope ceiling
//     exists to replace.
//   - status (--show-token): same exfil, one flag away.
//
// There is no legitimate agent use for any of them: an agent's credentials
// come from rein, never from gh's keyring. Shape B limit, unchanged: a
// determined agent can invoke the real gh directly and reach hosts.yml. This
// closes the shim path, which is the path a well-behaved (or
// prompt-injected-but-not-adversarial) agent actually takes.
func refuseAgentAuth(w io.Writer, args []string) {
	fmt.Fprintf(w, "rein-gh: refusing `gh %s` inside a rein run.\n", argSummary(args))
	fmt.Fprintln(w, "         `gh auth` manages the DEVELOPER's GitHub credentials: it can log in or out as")
	fmt.Fprintln(w, "         them, print their stored token, or repoint git's credential helper away from")
	fmt.Fprintln(w, "         rein. An agent has no business doing any of that — your credentials are brokered")
	fmt.Fprintln(w, "         by rein and scoped to this session's issue and repos.")
	fmt.Fprintln(w, "         If you need different access, ask the human to change the session.")
}

// firstNoun returns the first non-option token in args (the gh noun), or "".
func firstNoun(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// tokenEnvVars is the canonical set of ambient GitHub credential env vars.
// It MUST stay in sync with cmd/rein/run.go's scrub list: that is what
// `rein run` strips from a wrapped agent's environment, and the shim has to
// strip exactly the same set for the scope ceiling to hold when the shim is
// invoked OUTSIDE `rein run` (issue #57).
var tokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// baseEnv returns the process environment with every ambient GitHub
// credential var REMOVED. Every exec of the real gh must start from this,
// never from a raw os.Environ() (issue #57).
//
// Before this existed, each leg that failed to mint appended nothing to
// os.Environ(), so a GH_TOKEN/GITHUB_TOKEN exported in the developer's own
// shell was inherited straight through to gh: a write that rein could not
// broker then executed with the user's full-scope PAT, defeating the whole
// scope ceiling. (`rein run` children and sandboxed runs were never exposed
// — run.go scrubs these vars and the srt env allowlist drops them. The
// exposed case is the shim on PATH outside `rein run`.)
//
// Removing rather than overwriting also closes a subtler hazard, and the
// mechanism differs between this file's two exec paths, which is exactly why
// "just append and let the last one win" was never safe:
//
//   - syscall.Exec (the read/local/re-entry paths) hands the slice to execve
//     untouched, duplicate keys and all. Go's os.Getenv is FIRST-wins, so an
//     inherited GH_TOKEN earlier in the block SHADOWED the minted token we
//     appended after it: the read tier's success leg was serving reads with
//     the developer's ambient PAT while looking rein-scoped.
//   - exec.Command (the write path) runs dedupEnv before execve, which keeps
//     the LAST occurrence — so the same append DID override there.
//
// One append, two opposite outcomes. Stripping first collapses both to a
// single entry that is unambiguously ours, whichever exec path runs.
func baseEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name, _, ok := strings.Cut(kv, "=")
		if ok && slices.Contains(tokenEnvVars, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// placeholderToken is the deliberately-invalid GH_TOKEN handed to gh on any
// leg where rein declined or failed to broker a real one.
const placeholderToken = "rein-placeholder-denied"

// denyEnv is the fail-closed environment: no inherited GitHub credential of
// any kind, plus a deliberately-invalid GH_TOKEN so gh fails LOUDLY instead
// of silently falling back to the user's hosts.yml login. This mirrors the
// credential helper's TM-G8 placeholder contract — always hand the tool
// SOMETHING that visibly fails, never nothing.
//
// Shape B limit (unchanged): a determined agent can `unset GH_TOKEN` and
// re-run, reaching hosts.yml. This is a fail-closed gate for well-behaved
// agents; adversarial bypass requires the Shape A sandbox.
func denyEnv() []string {
	env := baseEnv()
	env = append(env, "GH_TOKEN="+placeholderToken)
	// Short-circuit the shim's re-entry guard on the denial path too, so gh
	// forking itself internally doesn't re-trigger the prompt and emit a
	// duplicate helpful-stderr message.
	env = append(env, reentryEnv+"=1")
	return env
}

// runReadAndExec mints (or reads from cache) a read-tier token, sets
// GH_TOKEN, and execs the real gh. Does not return on success.
//
// On a read-tier mint failure it fails CLOSED (issue #57): gh is exec'd with
// denyEnv, never with the user's inherited credentials. See readTierToken.
func runReadAndExec(realGh string, args []string, stateDir string, logger *log.Logger) {
	token := readTierToken(stateDir, logger)
	var env []string
	if token != "" {
		env = append(baseEnv(), "GH_TOKEN="+token, reentryEnv+"=1")
	} else {
		fmt.Fprintln(os.Stderr, "rein-gh: could not broker a read-tier token; running gh with a placeholder credential.")
		fmt.Fprintln(os.Stderr, "         gh will fail to authenticate rather than silently using your own GitHub credentials. Run `rein doctor`.")
		env = denyEnv()
	}
	argv := append([]string{realGh}, args...)
	if err := syscall.Exec(realGh, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "rein-gh: exec %s: %v\n", realGh, err)
		os.Exit(127)
	}
}

// readTierToken returns a cached or freshly-minted read-only gh token, or ""
// if config / mint fails. "" means the caller MUST fail closed (denyEnv).
//
// "" used to mean "exec gh with the untouched environment and let it fall
// back to its own auth". That was the read-tier half of issue #57: a read the
// operator believes is rein-scoped (read-only, session repos only) instead
// executed with their full-scope PAT, so a mint failure — or an agent that
// induced one — silently widened the read ceiling from "this session's repos"
// to "everything the human can see". The scope ceiling is the product;
// degrading out of it silently is the bug, whichever tier it happens on. gh
// still surfaces a clear auth error (the stated intent of the old behavior);
// it just no longer succeeds with the wrong credential first.
func readTierToken(stateDir string, logger *log.Logger) string {
	appCfg, ks, _, rscope, err := loadAppCfgWithSession(logger)
	if err != nil {
		logger.Printf("read tier: %v; execing gh without GH_TOKEN", err)
		return ""
	}
	client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
	if err != nil {
		logger.Printf("read tier: NewClient failed: %v", err)
		return ""
	}
	token, _, err := ghsession.EnsureFresh(
		ghsession.ReadCachePathForScope(stateDir, rscope.Key()),
		client.MintGhReadOnlyToken,
		client.RevokeToken,
		refreshSkew,
		mintTimeout,
		logger,
	)
	if err != nil {
		logger.Printf("read tier: ensure-fresh failed: %v; execing gh without GH_TOKEN", err)
		return ""
	}
	return token
}

// loadAppCfgWithSession returns the App config overridden with the
// session's repo, plus the keystore the mint path reads the PEM from
// and the resolved session itself. Returning the session lets callers
// reuse the exact session this loaded for downstream decisions (e.g.
// the write-approval gate) rather than reloading and risking divergence
// across two reads of dev-session.yaml.
// CP4 sessions have one repo; CP5+ multi-repo will require per-call
// repo selection.
func loadAppCfgWithSession(logger *log.Logger) (githubapp.Config, keystore.Keystore, session.Session, *runscope.Resolver, error) {
	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		return githubapp.Config{}, nil, session.Session{}, nil, err
	}
	sess, src, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return githubapp.Config{}, nil, session.Session{}, nil, fmt.Errorf("load session: %w", err)
	}
	// Effective ceiling, not just the standing session (issue #69): a gh
	// write/read against a repo the human approved as a mid-run scope
	// expansion must be covered by the token this shim mints. A state-dir
	// hiccup degrades to the standing ceiling — never wider.
	//
	// The SAME resolver is returned so callers scope-tag the gh-read cache by
	// this exact ceiling (issue #95) — the token's scope and its cache file's
	// key must agree, or a narrower earlier run's cached token could be served
	// here. A zero stateDir gives a resolver with no run context (runID ""),
	// whose Key is the standing repos — matching the fallback mint scope.
	stateDir, serr := config.StateDir()
	if serr != nil {
		stateDir = ""
	}
	rscope := runscope.New(sess, stateDir, os.Getenv("REIN_RUN_ID"))
	appCfg.RepoNames = sess.BareRepoNames()
	if serr == nil {
		appCfg.RepoNames = rscope.BareNames()
	}
	logger.Printf("session: id=%q repos=%v source=%s", sess.ID, sess.Repos, src)
	return appCfg, ks, sess, rscope, nil
}

// execGhWithoutToken is the denial path: fork real gh with denyEnv — every
// ambient GitHub credential stripped, plus a deliberately-invalid GH_TOKEN so
// gh doesn't fall back to the user's hosts.yml and silently succeed with
// their PAT. This mirrors the credential helper's TM-G8 placeholder behavior:
// always emit SOMETHING the operator can see fail, rather than no token at
// all.
//
// It previously appended the placeholder to a raw os.Environ() and merely
// blanked GITHUB_TOKEN, leaving GH_ENTERPRISE_TOKEN / GITHUB_ENTERPRISE_TOKEN
// inherited and relying on last-key-wins to mask GH_TOKEN. denyEnv removes
// all four outright (issue #57).
func execGhWithoutToken(realGh string, args []string) int {
	cmd := exec.Command(realGh, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = denyEnv()
	return exitCodeFromRunErr(cmd.Run())
}

// argSummary returns a short representation of the gh subcommand for
// log/prompt clarity. "issue create" rather than "issue create --title
// 'long thing' --body 'lots of text'".
func argSummary(args []string) string {
	out := ""
	count := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if count == 0 {
			out = a
		} else {
			out += " " + a
		}
		count++
		if count >= 2 {
			break
		}
	}
	if out == "" {
		return "<unknown>"
	}
	return out
}

// printDeclareHint mirrors cmd/rein's helper (copied to avoid a
// cross-binary dependency): the deny-channel instruction naming the
// exact next step (issue #35 §2).
func printDeclareHint(w io.Writer, runID string) {
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

// runWrite mints a fresh write-tier token (no cache), forks the real gh
// with GH_TOKEN set, waits for it to exit, best-effort revokes the token,
// and returns gh's exit code. On ANY failure to broker that token (config,
// client, mint) it still runs gh, but with the fail-closed denial env — so
// the user gets gh's "needs auth" error instead of a shim error, and never a
// write executed with their own ambient PAT (issue #57).
func runWrite(realGh string, args []string, stateDir string, logger *log.Logger) int {
	appCfg, ks, sess, rscope, cfgErr := loadAppCfgWithSession(logger)
	var token string
	var client *githubapp.Client

	// Gate write mints on the run's CONFIRMED-ISSUE set (issue #35 §2) —
	// the same gate the credential helper uses for git writes, reading
	// the same per-run approval record `rein declare <n>` populates. The
	// shim never prompts: the prompt lives at declare time. Empty set (or
	// no REIN_RUN_ID, or a mid-run session edit) ⇒ deny with the
	// placeholder GH_TOKEN (blocks the hosts.yml fallback) + the stderr
	// hint naming the exact next step.
	//
	// Reuse the session loadAppCfgWithSession already resolved — exactly
	// one session per invocation — so there is no second read of
	// dev-session.yaml that could diverge and let a write token mint
	// without a confirmed issue (fail-closed on the gate).
	if cfgErr == nil {
		runID := os.Getenv("REIN_RUN_ID")
		sig := approvals.SignatureOf(sess)
		if issues := approvals.ConfirmedIssues(stateDir, runID, sig); len(issues) == 0 {
			printDeclareHint(os.Stderr, runID)
			logger.Printf("write tier: no confirmed issue for run %q (gh %s); execing gh with placeholder GH_TOKEN", runID, argSummary(args))
			return execGhWithoutToken(realGh, args)
		}
		// TM-G6 re-check on every write mint (#35 §6): invalidate
		// transferred-issue confirmations; an emptied set denies the write.
		ghReadToken := func(ctx context.Context) (string, error) {
			client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
			if err != nil {
				return "", err
			}
			tok, _, err := ghsession.EnsureFresh(ghsession.ReadCachePathForScope(stateDir, rscope.Key()), client.MintGhReadOnlyToken, client.RevokeToken, refreshSkew, mintTimeout, logger)
			return tok, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rerr := declare.InvalidateTransferred(ctx, stateDir, runID, sess, ghReadToken, logger, os.Stderr)
		cancel()
		if rerr != nil {
			logger.Printf("write tier: transfer re-check emptied the confirmed set (%v); execing gh with placeholder GH_TOKEN", rerr)
			return execGhWithoutToken(realGh, args)
		}
	}

	if cfgErr != nil {
		logger.Printf("write tier: %v; execing gh with the denial env", cfgErr)
	} else if c, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole); err != nil {
		logger.Printf("write tier: NewClient failed: %v; execing gh with the denial env", err)
	} else {
		client = c
		ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
		t, expiresAt, err := client.MintGhSessionToken(ctx)
		cancel()
		if err != nil {
			logger.Printf("write tier: mint failed: %v; execing gh with the denial env", err)
		} else {
			token = t
			logger.Printf("write tier: mint succeeded expires_at=%s ttl=%s token_len=%d",
				expiresAt.Format(time.RFC3339),
				time.Until(expiresAt).Round(time.Second),
				len(token))
		}
	}

	// Every leg that reaches here without a token — cfg load failed, client
	// construction failed, mint failed — must fail CLOSED, exactly like the
	// gate-denied legs above. These previously fell through to exec.Command
	// with a raw os.Environ(), so a GH_TOKEN exported in the developer's
	// shell executed the write with their full-scope PAT: the rein-gh half of
	// issue #57. Nothing was minted here, so there is no token to revoke.
	if token == "" {
		fmt.Fprintln(os.Stderr, "rein-gh: could not broker a write-tier token; running gh with a placeholder credential.")
		fmt.Fprintln(os.Stderr, "         gh will fail to authenticate rather than silently using your own GitHub credentials. Run `rein doctor`.")
		return execGhWithoutToken(realGh, args)
	}

	env := append(baseEnv(), "GH_TOKEN="+token, reentryEnv+"=1")
	cmd := exec.Command(realGh, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	runErr := cmd.Run()

	// Revoke the write token regardless of gh's exit status. (token is
	// non-empty here by the fail-closed return above; client is non-nil
	// whenever a token was minted.)
	if client != nil {
		revokeCtx, cancel := context.WithTimeout(context.Background(), mintTimeout)
		if err := client.RevokeToken(revokeCtx, token); err != nil {
			logger.Printf("write tier: revoke failed (best-effort): %v", err)
		} else {
			logger.Printf("write tier: revoked token (effective TTL ended at gh exit)")
		}
		cancel()
	}

	return exitCodeFromRunErr(runErr)
}

// exitCodeFromRunErr returns 0 on nil error, gh's exit code if the
// process exited, or 127 for shim-level execution failures.
func exitCodeFromRunErr(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 127
}

// findRealGh returns the system `gh` executable, deliberately excluding
// this shim's directory. REIN_REAL_GH env overrides.
func findRealGh(shimPath string) (string, error) {
	if override := os.Getenv("REIN_REAL_GH"); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("REIN_REAL_GH=%q does not exist", override)
	}
	shimAbs, err := filepath.Abs(shimPath)
	if err != nil {
		shimAbs = shimPath
	}
	if resolved, err := filepath.EvalSymlinks(shimAbs); err == nil {
		shimAbs = resolved
	}
	shimDir := filepath.Dir(shimAbs)

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
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
		cand := filepath.Join(dir, "gh")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", errors.New("no gh binary found on PATH (excluding rein-gh's own directory)")
}

func openLog(stateDir string) (*log.Logger, func(), error) {
	path := filepath.Join(stateDir, "gh-shim.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return log.New(f, fmt.Sprintf("[pid %d] ", os.Getpid()), log.LstdFlags|log.LUTC),
		func() { _ = f.Close() },
		nil
}

// ---- classifier ----

// writeSubcommands lists (noun, verb) pairs that mutate state on
// GitHub's side and therefore need the write-tier token. Anything not
// listed defaults to "read" — safer if we misclassify (the operation
// will 403 cleanly rather than silently use a wider token).
//
// Source: `gh --help` recursive walk against gh 2.67 (the version installed
// in this VM). Local/credential-management operations (auth, config, alias,
// extension) are intentionally NOT here — but NOT because "they ignore
// GH_TOKEN anyway, so read is fine", which is what this comment used to claim
// and is demonstrably FALSE: `gh auth login` and `gh auth logout` on gh 2.67
// REFUSE to run while GH_TOKEN is set, and `gh auth token` prints whatever
// token gh would use. That stale claim is what let the read tier's
// fail-closed placeholder block the user's own recovery command. They now
// route to their own "local" tier (classify → runLocalAndExec): no token
// injected at all, ambient credentials still stripped.
var writeSubcommands = map[[2]string]bool{
	{"issue", "create"}:   true,
	{"issue", "edit"}:     true,
	{"issue", "close"}:    true,
	{"issue", "reopen"}:   true,
	{"issue", "comment"}:  true,
	{"issue", "delete"}:   true,
	{"issue", "lock"}:     true,
	{"issue", "unlock"}:   true,
	{"issue", "pin"}:      true,
	{"issue", "unpin"}:    true,
	{"issue", "transfer"}: true,
	{"issue", "develop"}:  true,

	{"pr", "create"}:        true,
	{"pr", "edit"}:          true,
	{"pr", "close"}:         true,
	{"pr", "reopen"}:        true,
	{"pr", "merge"}:         true,
	{"pr", "comment"}:       true,
	{"pr", "lock"}:          true,
	{"pr", "unlock"}:        true,
	{"pr", "ready"}:         true,
	{"pr", "review"}:        true,
	{"pr", "update-branch"}: true,

	{"release", "create"}:       true,
	{"release", "edit"}:         true,
	{"release", "delete"}:       true,
	{"release", "upload"}:       true,
	{"release", "delete-asset"}: true,

	{"repo", "create"}:      true,
	{"repo", "edit"}:        true,
	{"repo", "delete"}:      true,
	{"repo", "fork"}:        true,
	{"repo", "archive"}:     true,
	{"repo", "unarchive"}:   true,
	{"repo", "rename"}:      true,
	{"repo", "set-default"}: true,
	{"repo", "sync"}:        true,

	{"workflow", "run"}:     true,
	{"workflow", "enable"}:  true,
	{"workflow", "disable"}: true,

	{"codespace", "create"}:  true,
	{"codespace", "delete"}:  true,
	{"codespace", "edit"}:    true,
	{"codespace", "stop"}:    true,
	{"codespace", "rebuild"}: true,

	{"gist", "create"}: true,
	{"gist", "edit"}:   true,
	{"gist", "delete"}: true,
	{"gist", "rename"}: true,

	{"project", "create"}:       true,
	{"project", "edit"}:         true,
	{"project", "delete"}:       true,
	{"project", "item-create"}:  true,
	{"project", "item-edit"}:    true,
	{"project", "item-delete"}:  true,
	{"project", "item-archive"}: true,
	{"project", "field-create"}: true,
	{"project", "field-delete"}: true,

	{"label", "create"}: true,
	{"label", "edit"}:   true,
	{"label", "delete"}: true,
	{"label", "clone"}:  true,

	{"ruleset", "create"}: true,
	{"ruleset", "edit"}:   true,
	{"ruleset", "delete"}: true,

	{"variable", "set"}:    true,
	{"variable", "delete"}: true,

	{"secret", "set"}:    true,
	{"secret", "delete"}: true,

	{"gpg-key", "add"}:    true,
	{"gpg-key", "delete"}: true,

	{"ssh-key", "add"}:    true,
	{"ssh-key", "delete"}: true,

	{"cache", "delete"}: true,

	{"run", "cancel"}: true,
	{"run", "rerun"}:  true,
	{"run", "delete"}: true,
}

// localOnlyNouns are the gh nouns that manage gh's OWN local state instead of
// acting on GitHub with rein's credential. They route to the "local" tier:
// exec'd with a scrubbed env and NO GH_TOKEN (see runLocalAndExec).
//
//   - auth:      credential management. Injecting a token BLOCKS `gh auth
//     login`/`logout` outright on gh 2.67, i.e. the user's own
//     recovery command. Refused entirely inside a `rein run`
//     (refuseAgentAuth).
//   - config:    gh's own settings file. Never touches GitHub.
//   - alias:     gh's own alias file. Never touches GitHub.
//   - extension: install/list gh extensions.
//
// `browse` is deliberately NOT here despite the old comment grouping it with
// these: it resolves the repo (and can hit the API to do so) and is not
// credential management, so it stays on the read tier where its behavior is
// unchanged.
var localOnlyNouns = map[string]bool{
	"auth":      true,
	"config":    true,
	"alias":     true,
	"extension": true,
}

// classify returns "read", "write", "local", or "unknown" for an argv (without
// the leading "gh"). Unknown defaults to read at the caller; this
// function reports it separately mostly for logging.
//
// Special-case for `gh api`: classify based on the HTTP method and
// presence of body fields (-f / -F), since plain `gh api foo` is GET
// but `gh api foo -X PUT` or `gh api foo -f name=value` is write.
func classify(args []string) string {
	if len(args) == 0 {
		return "unknown"
	}
	// args[0] starting with - typically means --help or --version, none
	// of which need a tier.
	if strings.HasPrefix(args[0], "-") {
		return "unknown"
	}
	noun := args[0]
	if localOnlyNouns[noun] {
		return "local"
	}
	if noun == "api" {
		return classifyAPI(args[1:])
	}
	// Find the verb: next non-option token, skipping option-value pairs.
	verb := ""
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			verb = a
			break
		}
		if optionConsumesNextArg(a) {
			i++ // skip the value
		}
	}
	if writeSubcommands[[2]string{noun, verb}] {
		return "write"
	}
	return "read"
}

// optionsThatTakeArg lists gh options whose value is in a separate argv
// token (not --name=value). Conservative list — over-skipping just means
// we miss the verb and classify "read" (safe default).
var optionsThatTakeArg = map[string]bool{
	"-R": true, "--repo": true,
	"--hostname": true,
	"--json":     true,
	"-t":         true, "--template": true,
	"-q": true, "--jq": true,
	"-H": true, "--header": true,
	"--input":  true,
	"--method": true, "-X": true,
}

func optionConsumesNextArg(a string) bool {
	if strings.Contains(a, "=") {
		return false
	}
	return optionsThatTakeArg[a]
}

// classifyAPI inspects the args to `gh api ...` and returns "write" if
// the call would mutate (non-GET method, or any -f/-F/--field/--raw-field
// flag implying a POST body), else "read".
func classifyAPI(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-X" || a == "--method":
			// Next arg is the method.
			if i+1 < len(args) {
				m := strings.ToUpper(args[i+1])
				if m == "PUT" || m == "POST" || m == "PATCH" || m == "DELETE" {
					return "write"
				}
			}
			i++ // skip the value
		case strings.HasPrefix(a, "--method="):
			m := strings.ToUpper(strings.TrimPrefix(a, "--method="))
			if m == "PUT" || m == "POST" || m == "PATCH" || m == "DELETE" {
				return "write"
			}
		case a == "-f" || a == "--field" || a == "-F" || a == "--raw-field":
			// Body field present → POST by default.
			return "write"
		case strings.HasPrefix(a, "--field=") || strings.HasPrefix(a, "--raw-field="):
			return "write"
		}
	}
	return "read"
}
