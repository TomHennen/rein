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
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/ui/grant"
)

const (
	// refreshSkew refreshes the cached read token when less than this much
	// life remains, so a long-running gh subcommand doesn't time out
	// mid-call.
	refreshSkew = 5 * time.Minute

	// mintTimeout caps each mint and revoke call. gh users feel this
	// directly on cache-miss; keep tight.
	mintTimeout = 5 * time.Second

	// approvalTTL mirrors cmd/rein's value. Kept in sync manually for
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
	if os.Getenv(reentryEnv) != "" {
		logger.Printf("re-entrant invocation detected (%s set); execing real gh directly", reentryEnv)
		argv := append([]string{realGh}, os.Args[1:]...)
		if err := syscall.Exec(realGh, argv, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "rein-gh: exec %s: %v\n", realGh, err)
			os.Exit(127)
		}
	}

	switch cls {
	case "write":
		os.Exit(runWrite(realGh, os.Args[1:], stateDir, logger))
	default: // "read" or "unknown"
		runReadAndExec(realGh, os.Args[1:], stateDir, logger)
	}
}

// runReadAndExec mints (or reads from cache) a read-tier token, sets
// GH_TOKEN, and execs the real gh. Does not return on success.
func runReadAndExec(realGh string, args []string, stateDir string, logger *log.Logger) {
	token := readTierToken(stateDir, logger)
	env := os.Environ()
	if token != "" {
		env = append(env, "GH_TOKEN="+token, reentryEnv+"=1")
	}
	argv := append([]string{realGh}, args...)
	if err := syscall.Exec(realGh, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "rein-gh: exec %s: %v\n", realGh, err)
		os.Exit(127)
	}
}

// readTierToken returns a cached or freshly-minted read-only gh token,
// or "" if config / mint fails. Returning "" lets gh fall back to its
// own hosts.yml-based auth and surface its own error rather than a
// shim-level error.
func readTierToken(stateDir string, logger *log.Logger) string {
	appCfg, err := loadAppCfgWithSession(logger)
	if err != nil {
		logger.Printf("read tier: %v; execing gh without GH_TOKEN", err)
		return ""
	}
	client, err := githubapp.NewClient(appCfg)
	if err != nil {
		logger.Printf("read tier: NewClient failed: %v", err)
		return ""
	}
	token, _, err := ghsession.EnsureFresh(
		ghsession.ReadCachePath(stateDir),
		appCfg,
		client.MintGhReadOnlyToken,
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
// session's repo. CP4 sessions have one repo; CP5+ multi-repo will
// require per-call repo selection.
func loadAppCfgWithSession(logger *log.Logger) (githubapp.Config, error) {
	appCfg, err := config.LoadAppConfig()
	if err != nil {
		return githubapp.Config{}, err
	}
	sess, src, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return githubapp.Config{}, fmt.Errorf("load session: %w", err)
	}
	appCfg.RepoName = bareRepoName(sess.Repos[0])
	logger.Printf("session: id=%q repos=%v source=%s", sess.ID, sess.Repos, src)
	return appCfg, nil
}

// execGhWithoutToken is the denial path: fork real gh with a
// deliberately-invalid GH_TOKEN so gh doesn't fall back to the user's
// hosts.yml and silently succeed with the user's PAT. This mirrors the
// credential helper's TM-G8 placeholder behavior: always emit
// SOMETHING the operator can see fail, rather than no token at all.
//
// We also empty GITHUB_TOKEN so tools that read it as a fallback don't
// pick up a different value from the inherited env.
//
// Shape B limit: a determined agent could `unset GH_TOKEN` itself and
// re-run, hitting hosts.yml. This is a fail-closed gate for well-behaved
// agents; adversarial bypass requires Shape A sandbox. Documented in
// internal/ui/grant's package doc.
func execGhWithoutToken(realGh string, args []string) int {
	env := os.Environ()
	env = append(env, "GH_TOKEN=rein-placeholder-denied")
	env = append(env, "GITHUB_TOKEN=")
	// Short-circuit the shim's re-entry guard on the denial path too,
	// so gh forking itself internally doesn't re-trigger the prompt
	// and emit a duplicate helpful-stderr message.
	env = append(env, reentryEnv+"=1")
	cmd := exec.Command(realGh, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
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

// isWriteCapableRole mirrors cmd/rein's helper; we copy to avoid a
// cross-package dependency from a tiny CLI to another tiny CLI.
func isWriteCapableRole(role string) bool {
	switch role {
	case "implement", "triage", "review", "release":
		return true
	}
	return false
}

// bareRepoName extracts the "name" half of "owner/name". The App
// installation already pins the owner, so the mint API only accepts the
// bare name.
func bareRepoName(ownerSlashName string) string {
	_, name, ok := strings.Cut(ownerSlashName, "/")
	if !ok {
		return ownerSlashName
	}
	return name
}

// runWrite mints a fresh write-tier token (no cache), forks the real gh
// with GH_TOKEN set, waits for it to exit, best-effort revokes the
// token, and returns gh's exit code. On mint failure we still exec gh
// (without GH_TOKEN) so the user gets gh's "needs auth" error instead
// of a shim error.
func runWrite(realGh string, args []string, stateDir string, logger *log.Logger) int {
	appCfg, cfgErr := loadAppCfgWithSession(logger)
	var token string
	var client *githubapp.Client

	// Gate write mints behind the human-in-the-loop approval flow, the
	// same gate the credential helper uses for git writes. Closes the
	// CP5 hole where gh writes minted without prompting.
	if cfgErr == nil {
		sess, _, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
		if err == nil && sess.Issue != 0 {
			cfg := grant.Config{
				StateDir:      stateDir,
				TTL:           approvalTTL,
				PromptTimeout: 60 * time.Second,
				Logger:        logger,
			}
			req := grant.Request{
				Session: sess,
				Action:  fmt.Sprintf("gh %s (write)", argSummary(args)),
				Repo:    sess.Repos[0],
			}
			if !grant.ObtainApproval(context.Background(), req, cfg) {
				logger.Printf("write tier: human approval denied; execing gh without GH_TOKEN")
				return execGhWithoutToken(realGh, args)
			}
		} else if err == nil && sess.Issue == 0 && isWriteCapableRole(sess.Role) {
			logger.Printf("WARN: write tier proceeding without confirmation (session %q has no `issue:` field)", sess.ID)
		}
	}

	if cfgErr != nil {
		logger.Printf("write tier: %v; execing gh without GH_TOKEN", cfgErr)
	} else if c, err := githubapp.NewClient(appCfg); err != nil {
		logger.Printf("write tier: NewClient failed: %v; execing gh without GH_TOKEN", err)
	} else {
		client = c
		ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
		t, expiresAt, err := client.MintGhSessionToken(ctx)
		cancel()
		if err != nil {
			logger.Printf("write tier: mint failed: %v; execing gh without GH_TOKEN", err)
		} else {
			token = t
			logger.Printf("write tier: mint succeeded expires_at=%s ttl=%s token_len=%d",
				expiresAt.Format(time.RFC3339),
				time.Until(expiresAt).Round(time.Second),
				len(token))
		}
	}

	env := os.Environ()
	if token != "" {
		env = append(env, "GH_TOKEN="+token, reentryEnv+"=1")
	}
	cmd := exec.Command(realGh, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	runErr := cmd.Run()

	// Revoke the write token regardless of gh's exit status.
	if token != "" && client != nil {
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
// Source: `gh --help` recursive walk against gh 2.67 (the version
// installed in this VM). Local-only operations (alias, config,
// extension install, auth login, browse) are intentionally NOT here —
// they ignore GH_TOKEN anyway, so "read" is fine.
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

	{"project", "create"}:        true,
	{"project", "edit"}:          true,
	{"project", "delete"}:        true,
	{"project", "item-create"}:   true,
	{"project", "item-edit"}:     true,
	{"project", "item-delete"}:   true,
	{"project", "item-archive"}:  true,
	{"project", "field-create"}:  true,
	{"project", "field-delete"}:  true,

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

// classify returns "read", "write", or "unknown" for an argv (without
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
	"--hostname":  true,
	"--json":      true,
	"-t":          true, "--template": true,
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
