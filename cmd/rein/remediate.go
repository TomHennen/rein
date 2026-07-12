// Doctor remediation — the tiered fix engine shared by `rein doctor --fix`
// and `rein init`'s step-1 prereq handling (onboarding-ux-design.md §6).
//
// ONE remediation path, THREE safety tiers:
//
//   - remedyNoPriv     Auto-runnable WITH CONSENT, no privilege required:
//                      reinstall shims, refresh the PATH symlink, clear a
//                      STALE (never a live) cache. `--fix` applies these;
//                      on a tty each is confirmed [Y].
//   - remedyPrivileged Needs sudo / an external installer (apt, npm, the
//                      AppArmor profile, NTP). rein NEVER runs these — it
//                      prints the EXACT command and stops (hard constraint:
//                      no silent privileged/external installs; design §4.5,
//                      §7). The consented-privileged tier is a SEPARATE,
//                      still-open decision (§8.4) and is deliberately NOT
//                      built here.
//   - remedyGuide      Needs a human decision (which repo, which account,
//                      fix an env var). Printed as guidance only.
//
// The privileged/guide text is derived from the check's OWN message
// wherever possible (the srt preflight messages already carry the exact
// `npm i -g …@0.0.63` / `sudo sysctl …` commands), so there is one source
// of truth and no drift-prone hardcoded command strings.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/tokencache"
)

type remedyTier int

const (
	remedyNoPriv remedyTier = iota
	remedyPrivileged
	remedyGuide
)

// remediation is the fix associated with a failing/warning check.
type remediation struct {
	tier  remedyTier
	what  string       // one-line description of the action
	apply func() error // non-nil ONLY for remedyNoPriv
	guide string       // exact command / guidance for the privileged & guide tiers (may be multi-line)
}

// remediationForFunc is the seam the driver calls, so tests can inject fake
// remedies (with spy apply funcs) and exercise applyRemediations' consent
// branches without real filesystem side effects. Production points at
// remediationFor.
var remediationForFunc = remediationFor

// remediationFor maps a non-OK check to its remedy. ok=false when the check
// is green or has no defined remediation (e.g. an informational warn).
func remediationFor(r checkResult) (remediation, bool) {
	if r.status == statusOK {
		return remediation{}, false
	}
	switch {
	case r.name == "rein on PATH":
		return remediation{
			tier: remedyNoPriv,
			what: "refresh the ~/.local/bin/rein PATH symlink",
			apply: func() error {
				self, err := resolveSelf()
				if err != nil {
					return err
				}
				return ensureReinOnPath(self)
			},
		}, true

	case r.name == "shim freshness":
		return remediation{
			tier: remedyNoPriv,
			what: "reinstall the git/gh/rein shims",
			apply: func() error {
				_, _, err := installShimFiles()
				return err
			},
		}, true

	// Cache remediation is scoped to STALE/expired entries only — a live
	// cache is never touched (an absent one is normal and needs no fix).
	case r.name == "gh-shim cache" && strings.Contains(r.message, "expired"):
		return remediation{
			tier:  remedyNoPriv,
			what:  "clear the stale gh-shim token cache (next gh read re-mints)",
			apply: clearStaleGhCache,
		}, true

	// Sandbox stack: every fix is a privileged/external install. Guide-only,
	// NEVER run. The message already names the exact command.
	case strings.HasPrefix(r.name, "sandbox:"):
		return remediation{
			tier:  remedyPrivileged,
			what:  "install/repair the sandbox stack (" + strings.TrimPrefix(r.name, "sandbox: ") + ")",
			guide: r.message,
		}, true

	// Session remediation is GUIDE-only in both shapes. The design lists
	// "(re)write the session" as a no-priv fix, but the two cases that
	// actually arise here both need a human: a MISSING session needs the
	// repo (a decision), and an EXISTING session carrying the retired
	// `issue:` field must not be machine-rewritten (that would clobber the
	// user's comments/formatting) — and init deliberately never rewrites an
	// existing session, so pointing a warn at `rein init` would loop. Branch
	// on status so each guide is actionable.
	case r.name == "session" && r.status == statusWarn:
		return remediation{
			tier:  remedyGuide,
			what:  "clean up the session file",
			guide: "edit your dev-session.yaml and remove the `issue:` line — it is IGNORED (the issue is agent-declared via `rein declare <n>`, never configured)",
		}, true
	case r.name == "session":
		return remediation{
			tier:  remedyGuide,
			what:  "configure a dev session",
			guide: "run `rein init --repo owner/name` (or `rein init` and answer the repo prompt) to scaffold ~/.config/rein/dev-session.yaml",
		}, true

	case r.name == "app credentials" || r.name == "app key":
		return remediation{
			tier:  remedyGuide,
			what:  "fix the GitHub App credentials",
			guide: r.message,
		}, true

	case r.name == "$TMUX":
		return remediation{
			tier:  remedyGuide,
			what:  "enable the tmux-popup grant layer",
			guide: "run rein inside a tmux session, or use the tty grant layer / `rein approval grant` from another terminal",
		}, true

	default:
		return remediation{}, false
	}
}

// clearStaleGhCache removes the gh-shim token cache ONLY when it is
// stale/expired/corrupt. A live (still-valid) cache is left untouched, and an
// absent one is a no-op. This is the "refresh a stale cache" no-priv remedy —
// scoped so it can never discard a usable token or a live approval.
func clearStaleGhCache() error {
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	path := ghsession.ReadCachePath(stateDir)
	e, rerr := tokencache.Read(path)
	if errors.Is(rerr, os.ErrNotExist) {
		return nil // nothing to clear
	}
	if rerr == nil && e.Valid(0) {
		return nil // LIVE — never delete a valid cache
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// countNoPrivFixes reports how many results have an auto-runnable no-priv
// remedy — used to decide whether to nudge the user toward `--fix`.
func countNoPrivFixes(results []checkResult) int {
	n := 0
	for _, r := range results {
		if rem, ok := remediationFor(r); ok && rem.tier == remedyNoPriv {
			n++
		}
	}
	return n
}

// applyRemediations walks the results and applies/guides each non-OK check's
// remedy. It returns the number of no-priv fixes actually applied.
//
// Consent model (§7): --fix IS the consent to run no-priv fixes. On a real
// terminal each no-priv fix is additionally confirmed [Y] (default yes) so
// the user can decline individual ones. On a NON-terminal (CI/piped) it never
// blocks — the flag alone authorizes the safe fixes. Privileged/external
// steps are ALWAYS printed and NEVER run, regardless of consent.
func applyRemediations(results []checkResult, in *os.File, w io.Writer) (applied int) {
	interactive := stdinIsTerminal(in)
	confirm := func(q string) bool { return promptYesNo(w, in, q, true) }
	return applyRemediationsCore(results, interactive, confirm, w)
}

// applyRemediationsCore is the I/O-free driver (the tty probe and the concrete
// prompt are lifted out as `interactive` + `confirm`), so every consent branch
// is unit-testable: --fix non-tty applies without prompting; interactive +
// decline skips; a privileged remedy is print-only and its apply is never
// invoked (it is nil — the switch never reaches it).
func applyRemediationsCore(results []checkResult, interactive bool, confirm func(string) bool, w io.Writer) (applied int) {
	for _, r := range results {
		rem, ok := remediationForFunc(r)
		if !ok {
			continue
		}
		switch rem.tier {
		case remedyNoPriv:
			if interactive && !confirm(fmt.Sprintf("  apply fix — %s?", rem.what)) {
				fmt.Fprintf(w, "  [skip]   %s (declined)\n", r.name)
				continue
			}
			fmt.Fprintf(w, "  [fix]    %s: %s …\n", r.name, rem.what)
			if err := rem.apply(); err != nil {
				fmt.Fprintf(w, "           FAILED: %v\n", err)
				continue
			}
			fmt.Fprintln(w, "           done.")
			applied++
		case remedyPrivileged:
			fmt.Fprintf(w, "  [manual] %s needs a privileged/external step — rein will NOT run it. Run it yourself:\n", r.name)
			printGuide(w, rem.guide)
		case remedyGuide:
			fmt.Fprintf(w, "  [guide]  %s:\n", r.name)
			printGuide(w, rem.guide)
		}
	}
	return applied
}

// printGuide writes possibly-multi-line guidance indented under a header.
func printGuide(w io.Writer, guide string) {
	for _, line := range strings.Split(strings.TrimRight(guide, "\n"), "\n") {
		fmt.Fprintf(w, "           %s\n", strings.TrimSpace(line))
	}
}
