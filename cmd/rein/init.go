// `rein init`
//
// First-run local scaffolding (Phase 0.5 CP1). Idempotent: re-running is
// safe and re-runs are cheap if --skip-mint-check is set.
//
// What it does:
//
//   - Creates $XDG_STATE_HOME/rein and $XDG_CONFIG_HOME/rein with 0700.
//   - Validates that the REIN_* env vars resolve to a usable App config.
//   - Confirms the rein-git and rein-gh shim binaries exist next to the
//     running rein binary, then runs installShim() to place shims under
//     the state dir's shim subdirectory.
//   - Symlinks the running rein binary to ~/.local/bin/rein so `rein` is
//     reachable from a fresh terminal (closes issue #14).
//   - Mints a real read-only installation token to prove the App
//     credentials work. Skippable via --skip-mint-check to avoid hammering
//     GitHub's installation-token rate limit during dev iteration.
//   - Scaffolds ~/.config/rein/dev-session.yaml if absent. Never
//     overwrites an existing session file.
//
// What it does NOT do at CP1:
//
//   - GitHub App manifest flow (CP4-5). Assumes the App already exists
//     and its credentials live in REIN_* env vars (typically via
//     ./dev-env in this repo).
//   - Shell-rc alias (CP3).
//   - Diagnostics (CP2 — `rein doctor`).

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"crypto/rand"
	"encoding/hex"
	"regexp"

	"golang.org/x/term"

	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/session"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("rein init", flag.ContinueOnError)
	var skipMint bool
	var noSymlink bool
	var noAlias bool
	var aliasFlag bool
	var requireSandbox bool
	var shellOverride string
	var skipAudit bool
	var force bool
	var expectedOwner string
	var manifestPort int
	var repoFlag string
	var assumeYes bool
	fs.BoolVar(&skipMint, "skip-mint-check", false, "skip the App credentials network check (useful for repeated re-runs; GitHub's installation-token mint has secondary rate limits)")
	fs.BoolVar(&noSymlink, "no-symlink", false, "skip creating the ~/.local/bin/rein symlink")
	fs.BoolVar(&aliasFlag, "alias", false, "install the `alias claude='rein run -- claude'` block to your shell rc (opt-in; default is NOT to install)")
	fs.BoolVar(&noAlias, "no-alias", false, "force-skip the shell alias (wins if both --alias and --no-alias are given)")
	fs.BoolVar(&requireSandbox, "require-sandbox", false, "hard-fail (non-zero exit) if the sandbox stack is unhealthy; default is a soft warning (init still finishes)")
	fs.StringVar(&shellOverride, "shell", "", "shell to target for alias setup (bash|zsh|fish); defaults to $SHELL")
	fs.BoolVar(&skipAudit, "skip-audit", false, "create only the primary GitHub App (skip the audit App; subsequent `rein init` will create it)")
	fs.BoolVar(&force, "force", false, "ignore state.json and run the manifest flow from scratch (existing Apps at GitHub are NOT deleted)")
	fs.StringVar(&expectedOwner, "owner", "", "expected GitHub owner login (user or org); if set, manifest flow refuses to persist if the App was created under a different account")
	fs.IntVar(&manifestPort, "port", 0, "pin the manifest-flow callback port (default: random ephemeral); set this on headless/remote machines so you can `ssh -L <port>:127.0.0.1:<port>` before running init")
	fs.StringVar(&repoFlag, "repo", "", "repo (owner/name) to scaffold the dev session against; non-interactive override for the \"which repo?\" prompt")
	fs.BoolVar(&assumeYes, "yes", false, "accept defaults and never prompt (non-interactive); required in headless/CI so init never blocks on a prompt")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println("rein init: setting up local scaffolding")

	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	fmt.Printf("  state dir:  %s\n", stateDir)

	configDir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create config dir %s: %w", configDir, err)
	}
	fmt.Printf("  config dir: %s\n", configDir)

	// Bridge: decide between manifest flow and env-var (Phase 0) path
	// before touching env vars. LoadAppConfig errors when REIN_APP_* are
	// absent, but the new-user manifest path EXPECTS them absent — so
	// the dispatch sits above LoadAppConfig and only the env-driven
	// branches load it.
	state, stateErr := appsetup.ReadState(configDir)
	envPresent, envClientID, envInstallationID := envSnapshot()
	action, explain := appsetup.DecideBridge(state, stateErr, envPresent, envClientID, envInstallationID, force)
	fmt.Printf("  setup:      %s\n", explain)

	// appCfgPtr is non-nil after env loading succeeds; nil if we're on
	// the manifest-flow path (no env, installation_id not yet known).
	// The mint check / scaffold-session / alias steps consult it.
	// appKS is the keystore the mint check builds NewClient against.
	var appCfgPtr *githubapp.Config
	var appKS keystore.Keystore

	switch action {
	case appsetup.BridgeStateCorrupt, appsetup.BridgeManagedExternallyMissingEnv:
		return errors.New(explain)

	case appsetup.BridgeEnvOverrideMismatch:
		fmt.Fprintf(os.Stderr, "  WARN:       %s\n", explain)
		// Fall through to env-driven path.
		cfg, ks, err := loadAppConfigForInit()
		if err != nil {
			return err
		}
		appCfgPtr = &cfg
		appKS = ks

	case appsetup.BridgeEnvOverrideMatch, appsetup.BridgeUseState:
		cfg, ks, err := loadAppConfigForInit()
		if err != nil {
			// Post-manifest-flow UX: state.json records audit_done
			// (or primary_done) but the user hasn't set
			// REIN_APP_INSTALLATION_ID yet. LoadAppConfig fails with
			// "missing env var ..." — instead of surfacing that as an
			// error, print the same install-deep-link hint doctor
			// uses and exit 0. The user is in a known intermediate
			// state, not a broken one.
			if action == appsetup.BridgeUseState && state.Primary != nil && state.Primary.InstallationID == 0 {
				if hint, ok := appsetup.PostManifestInstallHint(configDir); ok {
					fmt.Println()
					fmt.Println(hint)
					return nil
				}
			}
			return err
		}
		appCfgPtr = &cfg
		appKS = ks

	case appsetup.BridgeWriteEnvMarker:
		cfg, ks, err := loadAppConfigForInit()
		if err != nil {
			return err
		}
		appCfgPtr = &cfg
		appKS = ks
		if err := appsetup.WriteEnvMarker(configDir, envClientID, envInstallationID); err != nil {
			return fmt.Errorf("write env marker state.json: %w", err)
		}
		fmt.Println("              wrote managed_externally marker (state.json)")

	case appsetup.BridgeRunManifest, appsetup.BridgeForce, appsetup.BridgeResumeManifest:
		ks := keystore.NewFileKeystore(configDir)
		if expectedOwner == "" {
			fmt.Fprintln(os.Stderr, "WARN: --owner not set; cannot detect 'wrong account' footgun (see docs/init-manifest-design.md §Security considerations)")
		}
		// 25min budget: 10min per browser round, plus headroom for two
		// rounds and conversion.
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
		defer cancel()
		if err := appsetup.RunManifestFlow(ctx, appsetup.RunOptions{
			ConfigDir:     configDir,
			Keystore:      ks,
			SkipAudit:     skipAudit,
			Force:         force,
			Stdout:        os.Stdout,
			Stderr:        os.Stderr,
			ExpectedOwner: expectedOwner,
			Port:          manifestPort,
		}); err != nil {
			return err
		}
		// Manifest flow leaves installation_id unknown; the user must
		// install the App on a repo and supply REIN_APP_INSTALLATION_ID
		// (or wait for Stage 2's install polling). appCfg stays nil;
		// the mint check below skips when it sees nil.
	}

	if appCfgPtr != nil {
		appCfg := *appCfgPtr
		fmt.Printf("  app:        client_id=%s installation_id=%d\n", appCfg.ClientID, appCfg.InstallationID)
		// PEM path no longer lives in githubapp.Config (the keystore is
		// the source of truth). Pre-flight via env directly so the user
		// gets a clear "file missing" error here rather than a deeper
		// keystore error at first mint.
		pemPath := os.Getenv("REIN_APP_PRIVATE_KEY_PATH")
		if _, err := os.Stat(pemPath); err != nil {
			return fmt.Errorf("private key %s: %w (REIN_APP_PRIVATE_KEY_PATH points somewhere unreadable)", pemPath, err)
		}
	}

	// Resolve the running binary's real path (through any symlinks the
	// user reached us via). The shim install + the ~/.local/bin symlink
	// should both target the real file, not another symlink.
	realSelf, err := resolveSelf()
	if err != nil {
		return err
	}
	selfDir := filepath.Dir(realSelf)

	// installShimFiles depends on rein-git and rein-gh being next to the
	// rein binary. Pre-flight check so the user gets a clear "go build"
	// hint rather than a deep error from inside the install.
	for _, name := range []string{"rein-git", "rein-gh"} {
		if _, err := os.Stat(filepath.Join(selfDir, name)); err != nil {
			return fmt.Errorf("shim binary %s not found next to %s; run `go build -o bin/ ./...` first", name, realSelf)
		}
	}

	// Use installShimFiles (silent) rather than installShim (chatty) so
	// init can roll the result into its tight progress output. The
	// activation help printed by `rein install-shim` is replaced by
	// init's end-of-run "Next:" summary.
	shimDir, installed, err := installShimFiles()
	if err != nil {
		return fmt.Errorf("install shim: %w", err)
	}
	fmt.Printf("  shim:       %d binaries under %s\n", len(installed), shimDir)

	if !noSymlink {
		if err := ensureReinOnPath(realSelf); err != nil {
			return err
		}
	} else {
		fmt.Println("  symlink:    skipped (--no-symlink)")
	}

	switch {
	case appCfgPtr == nil:
		fmt.Println("  mint check: skipped (manifest flow leaves installation_id unknown until the App is installed on a repo)")
	case skipMint:
		fmt.Println("  mint check: skipped (--skip-mint-check)")
	default:
		fmt.Println("  mint check: minting a read-only installation token to verify App credentials")
		client, err := githubapp.NewClient(*appCfgPtr, appKS, config.AppKeystoreRole)
		if err != nil {
			return fmt.Errorf("build app client: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
		defer cancel()
		_, expiresAt, err := client.MintReadOnlyToken(ctx)
		if err != nil {
			return fmt.Errorf("mint failed (check REIN_APP_CLIENT_ID, REIN_APP_INSTALLATION_ID, REIN_APP_PRIVATE_KEY_PATH against the actual GitHub App + installation): %w", err)
		}
		fmt.Printf("              ok (token expires %s)\n", expiresAt.Format(time.RFC3339))
	}

	sessionPath, err := session.DefaultFilePath()
	if err != nil {
		return err
	}
	switch _, err := os.Stat(sessionPath); {
	case err == nil:
		fmt.Printf("  session:    existing file kept at %s\n", sessionPath)
	case os.IsNotExist(err):
		// Resolve the repo to scaffold against. Precedence: --repo flag >
		// interactive prompt. The prompt only fires on a real terminal
		// with --yes unset (resolveRepoForSession gates on that), so
		// headless/CI/piped runs fall through and, with no --repo, leave
		// the session unscaffolded rather than blocking. init must never
		// hang on a prompt (onboarding-ux-design.md §7). (REIN_TEST_REPO_A
		// is deliberately NOT read here — a test var must not drive
		// production session scope; see #40.)
		repo := resolveRepoForSession(repoFlag, os.Stdin, assumeYes)
		if repo == "" {
			fmt.Printf("  session:    not scaffolded (pass --repo owner/name to scaffold)\n")
		} else {
			if err := scaffoldSessionFile(sessionPath, repo); err != nil {
				return fmt.Errorf("scaffold session file: %w", err)
			}
			fmt.Printf("  session:    scaffolded at %s (repos: [%s])\n", sessionPath, repo)
			fmt.Println("              note: no issue bound — git push (writes) are BLOCKED until an issue is bound.")
			fmt.Println("              agent-declared issue support is coming (#35); reads work without it. Uncomment `issue:` in the file to enable writes now.")
		}
	default:
		return fmt.Errorf("stat %s: %w", sessionPath, err)
	}

	// Alias is OPT-IN (onboarding-ux-design.md decision 4): default is NOT
	// to install. aliasDecision is a pure function of the flags + tty so
	// the full matrix is unit-testable; here we feed it the live gates and,
	// when it says "prompt", ask on a real terminal defaulting to NO.
	aliasActive := false
	aliasNoted := false
	installAlias, promptAlias := aliasDecision(aliasFlag, noAlias, assumeYes, stdinIsTerminal(os.Stdin))
	if installAlias || promptAlias {
		// Build the plan ONCE (not once per branch) so the rc named in the
		// prompt is exactly the rc that gets edited, and a buildAliasPlan
		// error surfaces the same way whether we prompt or install.
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home dir: %w", err)
		}
		shell := detectShell(shellOverride)
		plan, err := buildAliasPlan(shell, home)
		switch {
		case err != nil && installAlias:
			// The user EXPLICITLY asked (--alias): can't target their shell
			// is a hard error, surfaced loudly.
			return err
		case err != nil:
			// Prompt path: the alias is opt-in and the user hasn't even said
			// yes yet. Don't block the rest of init over an optional feature
			// they may decline — degrade to skip-with-note.
			fmt.Fprintf(os.Stderr, "  alias:      skipped (couldn't target shell %q: %v)\n", shell, err)
			installAlias = false
			aliasNoted = true
		default:
			if promptAlias {
				// Name the concrete rc so the user knows exactly what will be
				// edited. Default NO (do not assume they want it).
				q := fmt.Sprintf("Add alias claude='rein run -- claude' to %s?", plan.rcPath)
				installAlias = promptYesNo(os.Stdout, os.Stdin, q, false)
			}
			if installAlias {
				fmt.Printf("  alias:      target shell=%s, rc=%s\n", plan.shell, plan.rcPath)
				outcome, err := installShellAlias(plan)
				if err != nil {
					return fmt.Errorf("install shell alias: %w", err)
				}
				fmt.Printf("              %s\n", outcome.summary)
				aliasActive = outcome.active
				aliasNoted = true
				// Fire the --no-symlink coupling WARN only when something was
				// freshly written; re-runs that report "already current" don't
				// need the operator's attention again.
				if outcome.changed && noSymlink {
					fmt.Fprintln(os.Stderr, "  WARN:       --no-symlink + alias means the alias relies on `rein` being on $PATH some other way; verify in a fresh shell")
				}
			}
		}
	}
	if !installAlias && !aliasNoted {
		fmt.Println("  alias:      not installed (opt-in; enable with `rein init --alias`)")
	}

	// Sandbox-stack health (onboarding-ux-design.md decision 2 / §3 step 1).
	// SOFT-BLOCK by default: run the same srt preflight `rein doctor`
	// surfaces (reuse sandboxDoctorChecks so there's one detection path),
	// then decide via a pure function. init has already done ALL its other
	// setup at this point; a failing sandbox never aborts that work.
	sandboxResults := make([]checkResult, 0, 4)
	for _, c := range sandboxDoctorChecks() {
		sandboxResults = append(sandboxResults, c())
	}
	healthy, failMsg := sandboxHealthOutcome(sandboxResults, requireSandbox)

	fmt.Println()
	fmt.Println("rein init: done.")
	fmt.Println("Next:")
	fmt.Printf("  - %s is repo-scoped; reads work now. To enable writes (git push), uncomment `issue:` in it with a real issue number (agent-declared issues: #35)\n", sessionPath)
	if aliasActive {
		fmt.Println("  - open a new shell (or `source` your rc) so the `claude` alias is live, then run `claude`")
	} else {
		fmt.Println("  - run `rein run -- claude` (or another agent) to launch with rein in effect")
		fmt.Println("  - (optional) enable the `claude` alias with `rein init --alias`")
	}
	fmt.Println("  - run `rein doctor` to verify everything is wired up")

	// LOUD, specific warning when the sandbox stack is unhealthy. Printed
	// last so it's the final thing the user sees. failMsg is non-empty
	// whenever a check failed (soft or hard); only the return value differs.
	if !healthy {
		fmt.Fprintln(os.Stderr)
		fmt.Fprint(os.Stderr, failMsg)
	}
	if !healthy && requireSandbox {
		return errors.New("sandbox stack unhealthy and --require-sandbox set")
	}
	return nil
}

// sandboxHealthOutcome is a PURE decision over the sandbox preflight
// results: it never shells out, so it is unit-testable with synthetic
// checkResults. It reports whether the stack is healthy and, when it is
// not, a fully-rendered LOUD warning block (each failing check's name +
// message plus the fix pointer) ready to print to stderr.
//
// requireSandbox does NOT change the message — a failure produces the same
// warning either way. It only governs the caller's exit code: soft-block
// (default) finishes with exit 0 after warning; hard-gate (--require-sandbox)
// returns a non-zero error. The security posture is unchanged: `rein run`
// already fails closed at launch, so this is onboarding-time surfacing, not
// enforcement (onboarding-ux-design.md §7; constraint #3 is about run
// behavior). A result with statusWarn is tolerated — only statusFail counts
// as unhealthy, matching what `rein run --sandbox` hard-gates on.
func sandboxHealthOutcome(results []checkResult, requireSandbox bool) (healthy bool, failMsg string) {
	var failed []checkResult
	for _, r := range results {
		if r.status == statusFail {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return true, ""
	}
	var b strings.Builder
	b.WriteString("WARNING: sandbox stack is UNHEALTHY\n")
	for _, r := range failed {
		fmt.Fprintf(&b, "  [fail] %s: %s\n", r.name, flattenMessage(r.message))
	}
	b.WriteString("  sandboxed `rein run` will NOT work until you fix this; see `rein doctor` and the Prerequisites in README.md\n")
	return false, b.String()
}

// ensureReinOnPath creates or repairs a ~/.local/bin/rein symlink pointing
// at the running binary. ~/.local/bin is on most Linux/macOS distros'
// default PATH via XDG conventions; if it isn't, we warn but don't fail.
//
// Idempotency cases:
//
//   - Symlink already points at realSelf → no-op.
//   - Symlink points elsewhere → replace it (atomic: symlink-to-tmp + rename).
//   - Regular file or directory in the way → refuse to clobber; tell the
//     user to remove it and re-run. Better to fail loudly than overwrite
//     a real binary a user put there.
func ensureReinOnPath(realSelf string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home dir: %w", err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", binDir, err)
	}
	linkPath := filepath.Join(binDir, "rein")

	info, err := os.Lstat(linkPath)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		// Compare by fully-resolved target (EvalSymlinks) rather than the
		// raw Readlink string, so a relative-or-non-canonical symlink
		// that actually points at the right file is recognized as
		// already-correct rather than being re-linked on every init.
		resolved, rerr := filepath.EvalSymlinks(linkPath)
		if rerr == nil && resolved == realSelf {
			fmt.Printf("  symlink:    %s -> %s (already correct)\n", linkPath, realSelf)
			return nil
		}
		// Capture the raw target string for the log message before we
		// replace the link — useful for the operator to see what was
		// there before. Readlink failing is non-fatal; we just lose the
		// "was" annotation.
		existing, _ := os.Readlink(linkPath)
		if err := replaceSymlink(realSelf, linkPath); err != nil {
			return err
		}
		if existing != "" {
			fmt.Printf("  symlink:    %s -> %s (was %q; relinked)\n", linkPath, realSelf, existing)
		} else {
			fmt.Printf("  symlink:    %s -> %s (relinked)\n", linkPath, realSelf)
		}
	case err == nil:
		return fmt.Errorf("%s exists and is not a symlink; remove it and re-run `rein init` (or use --no-symlink)", linkPath)
	case os.IsNotExist(err):
		if err := os.Symlink(realSelf, linkPath); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", linkPath, realSelf, err)
		}
		fmt.Printf("  symlink:    %s -> %s\n", linkPath, realSelf)
	default:
		return fmt.Errorf("lstat %s: %w", linkPath, err)
	}

	if !pathContainsDir(binDir) {
		fmt.Fprintf(os.Stderr, "  WARN:       %s is not on $PATH; add it to your shell rc:\n", binDir)
		fmt.Fprintf(os.Stderr, "                export PATH=%q:$PATH\n", binDir)
	}
	return nil
}

// replaceSymlink atomically replaces an existing symlink. os.Symlink errors
// if the destination exists, so we create a uniquely-named sibling symlink
// and rename over the original.
func replaceSymlink(target, link string) error {
	dir, base := filepath.Split(link)
	tmp, err := uniqueTempName(dir, base+".rein-tmp-")
	if err != nil {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("symlink temp: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename symlink into place: %w", err)
	}
	return nil
}

// uniqueTempName returns a path in dir starting with prefix that doesn't
// currently exist. Uses 8 hex chars of crypto/rand entropy — plenty for
// the rare init-while-init race.
func uniqueTempName(dir, prefix string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, prefix+hex.EncodeToString(b[:])), nil
}

// envSnapshot reads REIN_APP_CLIENT_ID and REIN_APP_INSTALLATION_ID
// without erroring on absence. envPresent is true only when BOTH are
// set and INSTALLATION_ID parses as int64. Partially-set or malformed
// env fails closed to envPresent=false; the bridge will then pick the
// fresh-install row, which is the safer default.
//
// Intentionally a 2-of-4 snapshot (client_id + installation_id only).
// The other two REIN_APP_* vars (app_id, private_key_path) are
// validated later in loadAppConfigForInit when the bridge routes to
// an env-driven branch — that's the late-fail path for the partial-env
// case. Don't "fix" this to a full 4-var validation here; the bridge
// needs the partial signal to route correctly first.
func envSnapshot() (envPresent bool, clientID string, installationID int64) {
	clientID = os.Getenv("REIN_APP_CLIENT_ID")
	rawIID := os.Getenv("REIN_APP_INSTALLATION_ID")
	if clientID == "" || rawIID == "" {
		return false, clientID, 0
	}
	n, err := strconv.ParseInt(rawIID, 10, 64)
	if err != nil {
		return false, clientID, 0
	}
	return true, clientID, n
}

// loadAppConfigForInit wraps config.LoadAppConfig with the init-specific
// error prefix. Centralized so the bridge dispatch can call it from
// multiple branches without duplicating the wording.
func loadAppConfigForInit() (githubapp.Config, keystore.Keystore, error) {
	cfg, ks, err := config.LoadAppConfig()
	if err != nil {
		return githubapp.Config{}, nil, fmt.Errorf("env validation: %w", err)
	}
	return cfg, ks, nil
}

// pathContainsDir reports whether dir is one of the entries in $PATH.
// Trailing slashes and empty entries are tolerated.
func pathContainsDir(dir string) bool {
	dir = filepath.Clean(dir)
	for _, p := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == "" {
			continue
		}
		if filepath.Clean(p) == dir {
			return true
		}
	}
	return false
}

// repoSlugRe matches an "owner/name" slug using GitHub's permitted
// character set (alphanumerics, dot, underscore, hyphen). It explicitly
// rejects newlines, colons, or anything that could break out of a YAML
// scalar value when the slug is interpolated into a scaffolded file.
var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// scaffoldSessionFile writes a starter dev-session.yaml scoped to repo.
// repo must be an "owner/name" slug whose characters match repoSlugRe. A
// fresh random suffix in the session ID makes re-runs on different
// machines produce different IDs.
//
// The scaffolded session is REPO-ONLY: it deliberately writes NO active
// `issue:` field (decision A / #35 — the issue is agent-declared at
// runtime, not pre-picked at init). A commented hint line records how to
// opt in to writes manually until agent-declared support lands. Reads
// work with no issue; writes (git push) are blocked until one is bound.
func scaffoldSessionFile(path, repo string) error {
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("repo is empty; cannot scaffold a session without a repo")
	}
	if !repoSlugRe.MatchString(repo) {
		return fmt.Errorf("repo %q does not match owner/name with allowed characters [A-Za-z0-9._-]", repo)
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("session id entropy: %w", err)
	}
	sessID := "sess_dev_init_" + hex.EncodeToString(b[:])

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	body := fmt.Sprintf(`# rein dev session — scaffolded by `+"`rein init`"+`.
# This file binds an agent run to a scope ceiling (a list of repos).
# See internal/session/session.go for field documentation.

id: %s
role: implement
repos:
  - %s
# issue: <n>   # OPTIONAL: writes (git push) are BLOCKED until an issue is bound. Agent-declared issue support is coming (#35); until then, uncomment with a real issue number to enable writes. Reads work without it.
`, sessID, repo)

	// Atomic write: temp file in same dir, then rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), "dev-session.yaml.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// resolveRepoForSession picks the repo slug to scaffold the dev session
// against, applying the precedence: --repo flag > interactive prompt. A
// non-empty --repo wins outright.
//
// The prompt fires ONLY when stdin is a real terminal AND assumeYes is
// false (onboarding-ux-design.md §7: non-interactive fallback is
// mandatory). In headless/CI/piped runs (no tty) or under --yes, it does
// NOT prompt and returns "" — init then leaves the session unscaffolded
// with a graceful note rather than blocking. init must never hang on a
// prompt.
//
// stdin is passed in (rather than read from os.Stdin directly) so tests
// can drive the no-tty path with a non-terminal reader; the tty gate is
// derived from that same file descriptor when it is an *os.File.
func resolveRepoForSession(repoFlag string, stdin *os.File, assumeYes bool) string {
	if r := strings.TrimSpace(repoFlag); r != "" {
		return r
	}
	// No --repo: prompt only if we can, else fall back to "".
	if assumeYes || !stdinIsTerminal(stdin) {
		return ""
	}
	// Enter-accepts-default; default here is "" (skip scaffolding), so a
	// bare Enter is a graceful no-op rather than an error.
	return strings.TrimSpace(promptWithDefault(os.Stdout, stdin, "Which repo should the agent work on? (owner/name, Enter to skip)", ""))
}

// stdinIsTerminal reports whether f is a real interactive terminal. Used
// to gate prompts: a non-tty (headless SSH -L flow with no controlling
// terminal on stdin, CI, or a pipe) must never see a blocking prompt.
//
// Note: an interactive ssh session HAS a tty, so this isatty check (not
// the browser-headless SSH_CONNECTION heuristic in internal/appsetup) is
// the correct gate for prompts — a remote user at a real terminal should
// still be prompted.
func stdinIsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// aliasDecision is a PURE decision (no I/O, no tty probe of its own) over
// the alias flags + gates. It returns whether to install the alias outright
// and whether the caller should instead PROMPT the user.
//
// The alias is OPT-IN (onboarding-ux-design.md decision 4):
//
//   - --no-alias wins over everything: explicit opt-out beats opt-in, so
//     --alias + --no-alias -> skip (no error; documented precedence).
//   - --alias (without --no-alias) -> install, no prompt.
//   - neither flag, non-interactive (assumeYes OR not a tty) -> skip, no
//     prompt: the mandatory non-interactive fallback (§7) — never block.
//   - neither flag, real tty, --yes unset -> prompt (caller asks, default NO).
//
// When prompt is true, install is false; the caller resolves the real
// answer via promptYesNo. When prompt is false, install is authoritative.
func aliasDecision(aliasFlag, noAliasFlag, assumeYes, isTTY bool) (install bool, prompt bool) {
	if noAliasFlag {
		return false, false
	}
	if aliasFlag {
		return true, false
	}
	// Neither flag: opt-in default is OFF. Only a genuinely interactive run
	// (real tty, --yes unset) gets to ask; everything else skips silently.
	if assumeYes || !isTTY {
		return false, false
	}
	return false, true
}

// promptYesNo writes a yes/no question to w, reads a single line from in,
// and returns the parsed answer — or def when the line is empty
// (Enter-accepts-default) or the read fails (EOF/closed reader). Like
// promptWithDefault it performs a blocking read, so callers MUST gate it
// behind stdinIsTerminal; on a non-terminal reader the read returns quickly
// (EOF) and def is returned WITHOUT blocking.
//
// Accepted (case-insensitive): y/yes -> true, n/no -> false. Anything else
// (including a bare Enter) falls back to def.
func promptYesNo(w io.Writer, in io.Reader, question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Fprintf(w, "%s %s: ", question, hint)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// promptWithDefault writes prompt to w, reads a single line from in, and
// returns the trimmed line — or def if the line is empty (Enter-accepts-
// default) or the read fails (EOF/closed reader). It performs a blocking
// read, so callers MUST gate it behind stdinIsTerminal; on a non-terminal
// reader the read returns quickly (EOF) rather than hanging, and def is
// returned.
//
// This is a small, testable free-text helper: drive it with a
// non-terminal io.Reader in tests to exercise the default/no-block path.
// The genuinely-interactive behavior (a human typing at /dev/tty) is best
// verified by a manual/pexpect test — a unit test cannot honestly stand
// in for a real tty.
func promptWithDefault(w io.Writer, in io.Reader, prompt, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(w, "%s: ", prompt)
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}
