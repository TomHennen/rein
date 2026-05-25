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
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crypto/rand"
	"encoding/hex"
	"regexp"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/session"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("rein init", flag.ContinueOnError)
	var skipMint bool
	var noSymlink bool
	var noAlias bool
	var shellOverride string
	fs.BoolVar(&skipMint, "skip-mint-check", false, "skip the App credentials network check (useful for repeated re-runs; GitHub's installation-token mint has secondary rate limits)")
	fs.BoolVar(&noSymlink, "no-symlink", false, "skip creating the ~/.local/bin/rein symlink")
	fs.BoolVar(&noAlias, "no-alias", false, "skip writing the `alias claude='rein run -- claude'` block to your shell rc")
	fs.StringVar(&shellOverride, "shell", "", "shell to target for alias setup (bash|zsh|fish); defaults to $SHELL")
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

	// Validate env vars before any expensive checks. This is the most
	// common breakage (forgot to `source ./dev-env`), and surfacing it
	// first saves a confusing 401 later.
	appCfg, err := config.LoadAppConfig()
	if err != nil {
		return fmt.Errorf("env validation: %w", err)
	}
	fmt.Printf("  app:        client_id=%s installation_id=%d\n", appCfg.ClientID, appCfg.InstallationID)
	if _, err := os.Stat(appCfg.PrivateKeyPath); err != nil {
		return fmt.Errorf("private key %s: %w (REIN_APP_PRIVATE_KEY_PATH points somewhere unreadable)", appCfg.PrivateKeyPath, err)
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

	if !skipMint {
		fmt.Println("  mint check: minting a read-only installation token to verify App credentials")
		client, err := githubapp.NewClient(appCfg)
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
	} else {
		fmt.Println("  mint check: skipped (--skip-mint-check)")
	}

	sessionPath, err := session.DefaultFilePath()
	if err != nil {
		return err
	}
	switch _, err := os.Stat(sessionPath); {
	case err == nil:
		fmt.Printf("  session:    existing file kept at %s\n", sessionPath)
	case os.IsNotExist(err):
		if err := scaffoldSessionFile(sessionPath, os.Getenv("REIN_TEST_REPO_A")); err != nil {
			return fmt.Errorf("scaffold session file: %w", err)
		}
		fmt.Printf("  session:    scaffolded at %s (issue: 1 — edit to a real issue number)\n", sessionPath)
	default:
		return fmt.Errorf("stat %s: %w", sessionPath, err)
	}

	aliasActive := false
	if !noAlias {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home dir: %w", err)
		}
		shell := detectShell(shellOverride)
		plan, err := buildAliasPlan(shell, home)
		if err != nil {
			return err
		}
		fmt.Printf("  alias:      target shell=%s, rc=%s\n", plan.shell, plan.rcPath)
		outcome, err := installShellAlias(plan)
		if err != nil {
			return fmt.Errorf("install shell alias: %w", err)
		}
		fmt.Printf("              %s\n", outcome.summary)
		aliasActive = outcome.active
		// Fire the --no-symlink coupling WARN only when something was
		// freshly written; re-runs that report "already current" don't
		// need the operator's attention again.
		if outcome.changed && noSymlink {
			fmt.Fprintln(os.Stderr, "  WARN:       --no-symlink + alias means the alias relies on `rein` being on $PATH some other way; verify in a fresh shell")
		}
	} else {
		fmt.Println("  alias:      skipped (--no-alias)")
	}

	fmt.Println()
	fmt.Println("rein init: done.")
	fmt.Println("Next:")
	fmt.Printf("  - edit %s to set `issue:` to a real GitHub issue number\n", sessionPath)
	if aliasActive {
		fmt.Println("  - open a new shell (or `source` your rc) so the `claude` alias is live, then run `claude`")
	} else {
		fmt.Println("  - run `rein run -- claude` (or another agent) to launch with rein in effect")
	}
	fmt.Println("  - run `rein doctor` to verify everything is wired up")
	return nil
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

// scaffoldSessionFile writes a starter dev-session.yaml. fallbackRepo is
// the value of REIN_TEST_REPO_A; it must be an "owner/name" slug whose
// characters match repoSlugRe. A fresh random suffix in the session ID
// makes re-runs on different machines produce different IDs.
func scaffoldSessionFile(path, fallbackRepo string) error {
	if strings.TrimSpace(fallbackRepo) == "" {
		return fmt.Errorf("REIN_TEST_REPO_A is empty; cannot scaffold a session without a repo")
	}
	if !repoSlugRe.MatchString(fallbackRepo) {
		return fmt.Errorf("REIN_TEST_REPO_A=%q does not match owner/name with allowed characters [A-Za-z0-9._-]", fallbackRepo)
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
# This file binds an agent run to a scope ceiling (a list of repos) and,
# optionally, a GitHub issue used for the write-approval prompt.
#
# Edit `+"`issue:`"+` to a real issue number on the repo before relying on
# the write-approval flow (otherwise writes are silently un-prompted).
# See internal/session/session.go for field documentation.

id: %s
role: implement
repos:
  - %s
issue: 1
`, sessID, fallbackRepo)

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
