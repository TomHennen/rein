// `rein doctor`
//
// Read-only diagnostics for a rein install. Each check prints a status
// marker (ok / warn / fail) plus a one-line explanation. Checks run
// independently — a failure in one does not skip later ones. Exits 0 if
// all green; 1 if any check is red.
//
// What it catches (Phase 0.5 CP2): rein not on PATH, stale shim binaries,
// broken App credentials, missing/invalid session file, $TMUX-not-set
// (which silently disables the tmux-popup grant layer), stale or
// signature-mismatched approval cache, stale gh-shim cache. Same checks
// that consumed time during CP6 e2e debugging.
//
// What it does NOT do: anything requiring an actual git push or gh call.
// The mint check is a direct GitHub API call, not a git/gh op — that's
// in. Doctor is deliberately passive.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jferrl/go-githubauth"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/nono"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/internal/tokencache"
)

// checkStatus is the per-check verdict. Three values matching PLAN-0.5
// CP2's green/yellow/red framing; rendered with terminal colors when
// stdout is a TTY and NO_COLOR is unset.
type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

type checkResult struct {
	name    string
	status  checkStatus
	message string
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("rein doctor", flag.ContinueOnError)
	var fix bool
	fs.BoolVar(&fix, "fix", false, "apply the SAFE (no-privilege) remediations with consent — reinstall shims, refresh the PATH symlink, clear a stale cache. Privileged/external steps (apt/npm/AppArmor/NTP) are shown, never run (onboarding-ux-design.md §6).")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// PLAN-0.5 CP2 enumerates checks in this order. Operators learn the
	// order from the docs; don't reshuffle for code-organization reasons.
	checks := []func() checkResult{
		checkReinOnPath,
		checkShimFreshness,
		checkAppKeyReadable,
		checkAppMint,
		checkSessionFile,
		checkTmuxEnv,
		checkApprovalCache,
		checkGhShimCache,
	}
	// Sandbox (srt) preflight: same checks `rein run --sandbox` hard-gates on,
	// surfaced here read-only. A [fail] here means sandboxed mode will refuse to
	// launch — doctor is where the operator learns why before they hit it.
	checks = append(checks, sandboxDoctorChecks()...)
	// nono preflight: same rows `rein run --nono` hard-gates on (design pivot §2.3).
	// Shown alongside srt during the pivot — srt stays the default until P3, so
	// doctor surfaces both backends rather than selecting one.
	checks = append(checks, nonoDoctorChecks()...)

	results := runDoctorChecks(checks)
	printDoctorTable(results)

	if !fix {
		// Read-only run: nudge toward --fix when there are safe fixes to make.
		if n := countNoPrivFixes(results); n > 0 {
			fmt.Printf("\n%d safe fix(es) available — run `rein doctor --fix` to apply (privileged/external steps stay guide-only).\n", n)
		}
		return doctorVerdict(results)
	}

	// --fix: apply the no-priv tier (with consent), guide the rest, then
	// re-check so the operator sees the post-fix state.
	fmt.Println("\napplying fixes (no-privilege tier only; privileged/external steps are shown, NOT run):")
	applied := applyRemediations(results, os.Stdin, os.Stdout)
	if applied > 0 {
		fmt.Println("\nre-checking:")
		results = runDoctorChecks(checks)
		printDoctorTable(results)
	}
	return doctorVerdict(results)
}

// runDoctorChecks executes each check closure in order.
func runDoctorChecks(checks []func() checkResult) []checkResult {
	results := make([]checkResult, 0, len(checks))
	for _, c := range checks {
		results = append(results, c())
	}
	return results
}

// printDoctorTable renders the aligned marker | name | message table.
func printDoctorTable(results []checkResult) {
	nameWidth := 0
	for _, r := range results {
		if len(r.name) > nameWidth {
			nameWidth = len(r.name)
		}
	}
	for _, r := range results {
		fmt.Printf("%s  %-*s  %s\n", marker(r.status), nameWidth, r.name+":", flattenMessage(r.message))
	}
}

// doctorVerdict returns the exit-status error (non-nil iff any check failed).
func doctorVerdict(results []checkResult) error {
	fails := 0
	for _, r := range results {
		if r.status == statusFail {
			fails++
		}
	}
	fmt.Println()
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed", fails)
	}
	fmt.Println("rein doctor: ok")
	return nil
}

// useColor reports whether to emit ANSI escapes. Honors NO_COLOR
// (no-color.org) and CLICOLOR_FORCE (the conventional override for
// pipeline use cases like `rein doctor | less -R`). Otherwise
// colorizes only when stdout is a character device.
func useColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" {
		return true
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// flattenMessage collapses embedded newlines and runs of whitespace so a
// multi-line upstream error (like GitHub's JSON 401 body) renders as one
// scannable line in the doctor table.
func flattenMessage(m string) string {
	return strings.Join(strings.Fields(m), " ")
}

// labelWidth is the visible-character width of the widest status label
// ("[fail]"). marker right-pads narrower labels with spaces so the
// status column aligns regardless of color.
const labelWidth = 6

func marker(s checkStatus) string {
	var label, color string
	switch s {
	case statusOK:
		label, color = "[ok]", "\033[32m"
	case statusWarn:
		label, color = "[warn]", "\033[33m"
	case statusFail:
		label, color = "[fail]", "\033[31m"
	default:
		label, color = "[?]", ""
	}
	pad := strings.Repeat(" ", labelWidth-len(label))
	if useColor() && color != "" {
		return color + label + "\033[0m" + pad
	}
	return label + pad
}

// checkReinOnPath verifies that `rein` on the user's PATH resolves to
// the same binary that's currently running (typically because `rein init`
// linked ~/.local/bin/rein → the dev checkout). Symlinks on either side
// are resolved before comparing so the same file under different paths
// is recognized as a match.
func checkReinOnPath() checkResult {
	self, err := resolveSelf()
	if err != nil {
		return checkResult{"rein on PATH", statusFail, fmt.Sprintf("cannot locate self: %v", err)}
	}
	found, err := exec.LookPath("rein")
	if err != nil {
		return checkResult{"rein on PATH", statusFail, fmt.Sprintf("not on PATH (running from %s); add ~/.local/bin to PATH or re-run `rein init`", self)}
	}
	resolved, rerr := filepath.EvalSymlinks(found)
	if rerr != nil {
		resolved = found
	}
	if resolved != self {
		return checkResult{"rein on PATH", statusWarn, fmt.Sprintf("`which rein` (%s) differs from running binary (%s); re-run `rein init` to refresh the ~/.local/bin/rein symlink", found, self)}
	}
	return checkResult{"rein on PATH", statusOK, found}
}

// checkShimFreshness compares each shim binary's mtime to the source
// binary it was copied from. Stale shims happen when the developer
// rebuilds the rein source but forgets to re-run `rein install-shim`
// (or `rein init`) — the wrapped agent then runs the old shim.
//
// Missing source binaries (e.g. user installed globally without keeping
// a build tree) yields a warn — we can't verify freshness, but the
// shim itself is present.
func checkShimFreshness() checkResult {
	stateDir, err := config.StateDir()
	if err != nil {
		return checkResult{"shim freshness", statusFail, fmt.Sprintf("state dir: %v", err)}
	}
	shimDir := filepath.Join(stateDir, "shim")
	self, err := resolveSelf()
	if err != nil {
		return checkResult{"shim freshness", statusFail, fmt.Sprintf("cannot locate self: %v", err)}
	}
	selfDir := filepath.Dir(self)

	// (source-binary, installed-shim-name).
	pairs := []struct{ src, shim string }{
		{"rein", "rein"},
		{"rein-git", "git"},
		{"rein-gh", "gh"},
	}
	var stale []string
	var noSrc bool
	for _, p := range pairs {
		shimPath := filepath.Join(shimDir, p.shim)
		shimStat, err := os.Stat(shimPath)
		if err != nil {
			return checkResult{"shim freshness", statusFail, fmt.Sprintf("shim %s missing; run `rein install-shim`", shimPath)}
		}
		srcStat, err := os.Stat(filepath.Join(selfDir, p.src))
		if err != nil {
			noSrc = true
			continue
		}
		if shimStat.ModTime().Before(srcStat.ModTime()) {
			stale = append(stale, p.shim)
		}
	}
	if len(stale) > 0 {
		return checkResult{"shim freshness", statusWarn, fmt.Sprintf("shims %v older than source; re-run `rein install-shim`", stale)}
	}
	if noSrc {
		return checkResult{"shim freshness", statusWarn, fmt.Sprintf("source binaries not next to %s; can't verify freshness", self)}
	}
	return checkResult{"shim freshness", statusOK, fmt.Sprintf("3 shims under %s up to date", shimDir)}
}

// checkAppKeyReadable verifies REIN_APP_PRIVATE_KEY_PATH points at a
// readable file with strict mode bits.
//
// Post-CP5 mint-path keystore refactor: rein will refuse to mint when
// the PEM has any group/other bits set (keystore.verifyOwnership
// hard-fails). Doctor reports loose mode as red to keep doctor's
// verdict aligned with what mint actually does — a green doctor and a
// refusing mint would be the worst possible UX on exactly the
// situation the operator most needs guidance on.
//
// Post-CP5: if REIN_APP_PRIVATE_KEY_PATH is unset but state.json
// records a manifest-flow phase, check the keystore-managed PEM
// (~/.config/rein/<role>.pem) instead. Otherwise a fresh manifest
// flow setup always tripped this check before the user got around to
// pointing the env var at the keystored file.
func checkAppKeyReadable() checkResult {
	path := os.Getenv("REIN_APP_PRIVATE_KEY_PATH")
	if path == "" {
		if alt, ok := managedPEMPath(); ok {
			path = alt
		} else {
			return checkResult{"app key", statusFail, "no App private key: REIN_APP_PRIVATE_KEY_PATH unset and no keystore-managed PEM (run `rein init`)"}
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return checkResult{"app key", statusFail, fmt.Sprintf("%s: %v", path, err)}
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return checkResult{"app key", statusFail, fmt.Sprintf("%s mode %#o has group/other bits set (rein will refuse to mint); chmod 600 %s", path, mode, path)}
	}
	return checkResult{"app key", statusOK, fmt.Sprintf("%s (mode %#o)", path, mode)}
}

// managedPEMPath returns the path to the keystore-managed primary PEM
// if state.json indicates a manifest-flow setup. Returns ok=false when
// state.json is absent, corrupt, or doesn't carry a manifest phase
// (e.g. it's a managed_externally marker).
func managedPEMPath() (string, bool) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return "", false
	}
	s, err := appsetup.ReadState(configDir)
	if err != nil {
		return "", false
	}
	if !appsetup.IsManifestPhase(s) {
		return "", false
	}
	// Stat-only: avoid keystore.Get so we don't pay the
	// biometric-prompt cost a Phase 1/2 backend would impose.
	// PathOf gives us the same file FileKeystore.Get would open.
	ks := keystore.NewFileKeystore(configDir)
	return ks.PathOf(config.AppKeystoreRole), true
}

// checkAppMint exercises the actual mint path: build a Client from the
// REIN_* env, call MintReadOnlyToken with a tight timeout. Any error
// here is red — without working credentials no agent path works.
//
// Post-CP5: if state.json shows a manifest-flow phase but
// REIN_APP_INSTALLATION_ID is not yet set (because the user hasn't
// installed the App on a repo), report warn instead of fail and point
// at the install deep-link from state.json. This avoids a "doctor
// always fails on fresh manifest-flow setup" surprise.
func checkAppMint() checkResult {
	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		if configDir, derr := config.ConfigDir(); derr == nil {
			if hint, ok := appsetup.PostManifestInstallHint(configDir); ok {
				return checkResult{"app credentials", statusWarn, hint}
			}
		}
		return checkResult{"app credentials", statusFail, err.Error()}
	}
	// State-path-uncached: installation id not yet fetched. Doctor stays
	// read-only — it REPORTS that rein run will fetch it on next launch and
	// does NOT mint (which would require an id) or touch the network/state.
	if appCfg.InstallationID == 0 {
		return checkResult{"app credentials", statusWarn,
			"install-id not cached; `rein run` will fetch it on next launch (App not yet installed, or first run)"}
	}
	// On the state path ResolveApp leaves RepoNames empty; MintReadOnlyToken
	// needs at least one. Set them from the session, matching the helper /
	// rein-gh.
	if len(appCfg.RepoNames) == 0 {
		if sess, _, serr := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A")); serr == nil && len(sess.Repos) > 0 {
			appCfg.RepoNames = sess.BareRepoNames()
		}
	}
	client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
	if err != nil {
		return checkResult{"app credentials", statusFail, err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	_, expiresAt, err := client.MintReadOnlyToken(ctx)
	if err != nil {
		// Two rate-limit signals from GitHub, treated distinctly:
		//
		//   - jferrl's ErrRateLimited (HTTP 429 / 403 with rate-limit
		//     headers): unambiguous — preferred match.
		//   - 401 "Bad credentials": phase0_findings.md observation —
		//     GitHub returns this under secondary rate limit, but also
		//     under genuine credential mismatch. Ambiguous: we say so
		//     in the message rather than overclaiming "wait it out".
		if errors.Is(err, githubauth.ErrRateLimited) {
			return checkResult{"app credentials", statusFail, fmt.Sprintf("mint failed: GitHub rate-limited (resolves in 5-60min, try again later): %v", err)}
		}
		msg := err.Error()
		if strings.Contains(msg, "401") && strings.Contains(msg, "Bad credentials") {
			return checkResult{"app credentials", statusFail, fmt.Sprintf("mint failed with 401 Bad credentials. Most common cause: env vars don't match the App (verify REIN_APP_CLIENT_ID and REIN_APP_INSTALLATION_ID against the GitHub App settings page). Less common: GitHub secondary rate limit on a freshly-working App (resolves 5-60min — suspect this only if `rein init` succeeded recently). Underlying: %v", err)}
		}
		return checkResult{"app credentials", statusFail, fmt.Sprintf("mint failed: %v", err)}
	}
	return checkResult{"app credentials", statusOK, fmt.Sprintf("mint ok (token expires %s)", expiresAt.Format(time.RFC3339))}
}

// checkSessionFile reports where the session is loaded from, and warns
// when the file still carries the RETIRED `issue:` field (issue #35: the
// issue is agent-declared via `rein declare <n>`, never pre-configured;
// a security-looking field that silently gates nothing is exactly what
// doctor exists to surface).
func checkSessionFile() checkResult {
	sess, source, err := session.LoadOrFallback(os.Getenv("REIN_TEST_REPO_A"))
	if err != nil {
		return checkResult{"session", statusFail, err.Error()}
	}
	desc := fmt.Sprintf("%s (id=%s role=%s repos=%v)", source, sess.ID, sess.Role, sess.Repos)
	if sess.Issue != 0 {
		return checkResult{"session", statusWarn, desc + fmt.Sprintf(" — `issue: %d` is IGNORED (issue is agent-declared now; run `rein declare <n>`); remove the field", sess.Issue)}
	}
	return checkResult{"session", statusOK, desc}
}

// checkTmuxEnv inspects whether $TMUX is set in the current environment.
// PLAN-0.5 CP2 originally proposed spawning a child probe to verify env
// propagation, but Go's exec.Command inherits env automatically and
// nothing in cmd/rein scrubs it — the propagation question collapses to
// "is $TMUX set at the doctor's invocation point?". The check exists to
// catch the case from phase0_findings.md where a user attached to tmux
// AFTER launching `rein run`, leaving the wrapped process without $TMUX
// and silently disabling the tmux-popup grant layer.
func checkTmuxEnv() checkResult {
	if os.Getenv("TMUX") == "" {
		return checkResult{"$TMUX", statusWarn, "unset; tmux-popup grant layer will not fire (use the tty layer or `rein approval grant` from another terminal)"}
	}
	return checkResult{"$TMUX", statusOK, "set; tmux-popup grant layer available"}
}

// checkApprovalCache reports on the per-run approval files. Approvals are
// now keyed by REIN_RUN_ID (one per `rein run` invocation), so there is
// no single global record to inspect — doctor summarizes how many runs
// have files on disk and how many are still live. No runs is yellow (an
// expected state outside `rein run`, not an error).
func checkApprovalCache() checkResult {
	stateDir, err := config.StateDir()
	if err != nil {
		return checkResult{"approval cache", statusFail, err.Error()}
	}
	list, err := approvals.List(stateDir)
	if err != nil {
		return checkResult{"approval cache", statusFail, fmt.Sprintf("list runs: %v", err)}
	}
	if len(list) == 0 {
		return checkResult{"approval cache", statusWarn, "no active runs (per-run approvals; first write inside `rein run` will prompt)"}
	}
	live, approved := 0, 0
	for _, st := range list {
		if st.Live {
			live++
		}
		if st.HasApproval {
			approved++
		}
	}
	return checkResult{"approval cache", statusOK, fmt.Sprintf("%d run(s) on disk (%d live, %d approved); see `rein approval status`", len(list), live, approved)}
}

// sandboxDoctorChecks runs the srt sandbox preflight and maps each result into
// a doctor checkResult. These are the exact checks `rein run --sandbox`
// hard-gates on (srt present + pinned version, seccomp availability, bwrap
// userns health, system CA bundle), so a green doctor here means sandboxed
// mode will launch.
func sandboxDoctorChecks() []func() checkResult {
	// Lazy like every other doctor check: srt.Preflight (which shells out to
	// bwrap) runs when the FIRST sandbox check executes in runDoctor's loop, not
	// at slice-build time. A sync.Once shares the single Preflight result across
	// the per-row closures. The row NAMES are Preflight's stable output set;
	// keep this list in sync if Preflight gains a check (a missing name would
	// render as a fail row rather than silently vanish).
	var (
		once   sync.Once
		byName map[string]srt.Check
	)
	load := func() {
		once.Do(func() {
			byName = map[string]srt.Check{}
			for _, c := range srt.Preflight(srt.DefaultEnv()) {
				byName[c.Name] = c
			}
		})
	}
	mk := func(name string) func() checkResult {
		return func() checkResult {
			load()
			c, ok := byName[name]
			if !ok {
				return checkResult{"sandbox: " + name, statusFail, "preflight did not report this check"}
			}
			return checkResult{"sandbox: " + c.Name, srtStatusToDoctor(c.Status), c.Message}
		}
	}
	return []func() checkResult{
		mk("srt present"), mk("srt version"), mk("seccomp"), mk("bwrap userns"),
		mk("system CA bundle"),
	}
}

func srtStatusToDoctor(s srt.Status) checkStatus {
	switch s {
	case srt.StatusOK:
		return statusOK
	case srt.StatusWarn:
		return statusWarn
	default:
		return statusFail
	}
}

// nonoDoctorChecks runs the nono sandbox preflight and maps each result into a
// doctor checkResult, mirroring sandboxDoctorChecks. Same lazy sync.Once shape:
// nono.Preflight (which shells out to nono for the validate rows) runs when the
// FIRST nono check executes in runDoctor's loop, not at slice-build time, and
// the single result is shared across the per-row closures. Keep the row NAMES in
// sync with nono.Preflight's stable output set (a missing name renders as a fail
// row rather than silently vanishing). srt.Status is a sandboxutil alias, so
// srtStatusToDoctor maps nono's Status too.
func nonoDoctorChecks() []func() checkResult {
	var (
		once   sync.Once
		byName map[string]nono.Check
	)
	load := func() {
		once.Do(func() {
			byName = map[string]nono.Check{}
			for _, c := range nono.Preflight(nono.DefaultEnv()) {
				byName[c.Name] = c
			}
		})
	}
	mk := func(name string) func() checkResult {
		return func() checkResult {
			load()
			c, ok := byName[name]
			if !ok {
				return checkResult{name, statusFail, "preflight did not report this check"}
			}
			return checkResult{c.Name, srtStatusToDoctor(c.Status), c.Message}
		}
	}
	return []func() checkResult{
		mk("nono present"), mk("nono profile validate"), mk("rein CA"),
		mk("loopback proxy port"), mk("nono af_unix_mediation"),
	}
}

// checkGhShimCache reports on the gh read-token cache, which the rein-gh
// shim and `rein gh-auth` share. Absent or expired is yellow (rein-gh
// will mint a fresh token on the next gh invocation). Valid is green.
// There is no red here — the gh shim works correctly with or without a
// cache hit.
//
// Since issue #95 the cache is one file PER scope ceiling
// (gh-read-token-<tag>.json), so this globs them all and reports an
// aggregate: green if ANY ceiling has a live token, yellow otherwise.
func checkGhShimCache() checkResult {
	stateDir, err := config.StateDir()
	if err != nil {
		return checkResult{"gh-shim cache", statusFail, err.Error()}
	}
	paths, gerr := filepath.Glob(ghsession.ReadCacheGlob(stateDir))
	if gerr != nil {
		return checkResult{"gh-shim cache", statusWarn, fmt.Sprintf("glob failed: %v (next gh read will mint)", gerr)}
	}
	if len(paths) == 0 {
		return checkResult{"gh-shim cache", statusWarn, "absent (next gh read will mint)"}
	}
	valid, other := 0, 0
	var latest time.Time
	for _, p := range paths {
		e, rerr := tokencache.Read(p)
		if rerr == nil && e.Valid(0) {
			valid++
			if e.ExpiresAt.After(latest) {
				latest = e.ExpiresAt
			}
			continue
		}
		other++
	}
	if valid > 0 {
		// "cached scope(s)" not "the active scope": a green here means at least
		// one per-ceiling cache is warm, NOT necessarily the current run's — a
		// mismatched scope still re-mints on next use (issue #95).
		return checkResult{"gh-shim cache", statusOK, fmt.Sprintf("%d cached scope(s) valid (latest expires %s)", valid, latest.Format(time.RFC3339))}
	}
	return checkResult{"gh-shim cache", statusWarn, fmt.Sprintf("%d cached scope(s) expired/unreadable (next gh read will mint)", other)}
}
