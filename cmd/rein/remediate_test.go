package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/ghsession"
	"github.com/TomHennen/rein/internal/tokencache"
)

// TestRemediationTiers is the security-critical invariant for `doctor --fix`:
// only the no-privilege tier is auto-runnable, and every privileged/external
// step (the sandbox stack) is guide-only with NO apply function — so rein can
// never silently `apt`/`npm`/AppArmor on the user's behalf (design §4.5, §7).
func TestRemediationTiers(t *testing.T) {
	cases := []struct {
		name     string
		in       checkResult
		wantOK   bool
		wantTier remedyTier
	}{
		{"path-warn", checkResult{"rein on PATH", statusWarn, "differs"}, true, remedyNoPriv},
		{"path-fail", checkResult{"rein on PATH", statusFail, "not on PATH"}, true, remedyNoPriv},
		{"shim", checkResult{"shim freshness", statusWarn, "stale"}, true, remedyNoPriv},
		{"gh-cache-expired", checkResult{"gh-shim cache", statusWarn, "expired at ..."}, true, remedyNoPriv},
		// An ABSENT cache is normal — no fix offered (would be noise, and there
		// is nothing stale to clear).
		{"gh-cache-absent", checkResult{"gh-shim cache", statusWarn, "absent (next gh read will mint)"}, false, 0},
		{"sandbox-srt", checkResult{"sandbox: srt present", statusFail, "npm i -g ...@0.0.63"}, true, remedyPrivileged},
		{"sandbox-bwrap", checkResult{"sandbox: bwrap userns", statusFail, "sudo sysctl ..."}, true, remedyPrivileged},
		{"session", checkResult{"session", statusFail, "no session"}, true, remedyGuide},
		{"app-key", checkResult{"app key", statusFail, "chmod 600"}, true, remedyGuide},
		{"app-creds", checkResult{"app credentials", statusFail, "mint failed"}, true, remedyGuide},
		{"tmux", checkResult{"$TMUX", statusWarn, "unset"}, true, remedyGuide},
		// Green checks and undefined ones get no remediation.
		{"ok", checkResult{"rein on PATH", statusOK, "/usr/bin/rein"}, false, 0},
		{"approval-cache", checkResult{"approval cache", statusWarn, "no active runs"}, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rem, ok := remediationFor(c.in)
			if ok != c.wantOK {
				t.Fatalf("remediationFor(%+v) ok = %v, want %v", c.in, ok, c.wantOK)
			}
			if !ok {
				return
			}
			if rem.tier != c.wantTier {
				t.Errorf("tier = %v, want %v", rem.tier, c.wantTier)
			}
			// THE invariant: apply is non-nil iff no-priv. A privileged/guide
			// remedy must never carry a runnable action.
			if c.wantTier == remedyNoPriv && rem.apply == nil {
				t.Errorf("no-priv remedy has no apply func")
			}
			if c.wantTier != remedyNoPriv && rem.apply != nil {
				t.Errorf("tier %v carries an apply func — privileged/guide steps must NEVER auto-run", c.wantTier)
			}
			// Privileged/guide remedies must carry guidance text to print.
			if c.wantTier != remedyNoPriv && rem.guide == "" {
				t.Errorf("tier %v has no guide text", c.wantTier)
			}
		})
	}
}

// TestSessionRemediationBranchesOnStatus guards the dead-end-guidance bug: a
// MISSING session (fail) must point at `rein init --repo`, but an EXISTING
// session with the retired `issue:` field (warn) must NOT — init never
// rewrites an existing session, so that would loop. The warn case guides the
// one-line manual edit instead.
func TestSessionRemediationBranchesOnStatus(t *testing.T) {
	missing, ok := remediationFor(checkResult{"session", statusFail, "no session file"})
	if !ok || missing.tier != remedyGuide {
		t.Fatalf("missing session: want guide remedy, got ok=%v tier=%v", ok, missing.tier)
	}
	if !strings.Contains(missing.guide, "rein init") {
		t.Errorf("missing-session guide should point at rein init, got %q", missing.guide)
	}

	retired, ok := remediationFor(checkResult{"session", statusWarn, "`issue: 5` is IGNORED"})
	if !ok || retired.tier != remedyGuide {
		t.Fatalf("retired-field session: want guide remedy, got ok=%v tier=%v", ok, retired.tier)
	}
	if strings.Contains(retired.guide, "rein init") {
		t.Errorf("retired-field guide must NOT suggest `rein init` (init won't rewrite an existing session — a loop); got %q", retired.guide)
	}
	if !strings.Contains(retired.guide, "issue:") {
		t.Errorf("retired-field guide should tell the user to remove the issue: line, got %q", retired.guide)
	}
}

// TestApplyRemediationsCore_Consent exercises the consent-sensitive driver
// directly (the tty probe + concrete prompt are lifted out): --fix non-tty
// applies the no-priv action WITHOUT prompting; a privileged remedy is
// print-only and its apply is never invoked; the interactive-decline path
// skips the action. These are the security-relevant branches.
func TestApplyRemediationsCore_Consent(t *testing.T) {
	prev := remediationForFunc
	t.Cleanup(func() { remediationForFunc = prev })

	var noPrivCalls int
	remediationForFunc = func(r checkResult) (remediation, bool) {
		switch r.name {
		case "noPriv":
			return remediation{tier: remedyNoPriv, what: "do safe thing",
				apply: func() error { noPrivCalls++; return nil }}, true
		case "priv":
			// A privileged remedy carries NO apply func — the switch can
			// never run it. guide is print-only.
			return remediation{tier: remedyPrivileged, what: "sandbox", guide: "sudo do-it"}, true
		}
		return remediation{}, false
	}
	results := []checkResult{{"noPriv", statusFail, ""}, {"priv", statusFail, ""}}

	// 1) NON-interactive (--fix, no tty): applies no-priv without prompting;
	//    privileged is [manual] print-only; confirm is never consulted.
	noPrivCalls = 0
	var buf bytes.Buffer
	confirmCalled := false
	applied := applyRemediationsCore(results, false, func(string) bool { confirmCalled = true; return false }, &buf)
	if confirmCalled {
		t.Error("non-interactive run must NOT prompt (the flag is the consent; blocking would violate §7)")
	}
	if noPrivCalls != 1 {
		t.Errorf("no-priv apply calls = %d, want 1", noPrivCalls)
	}
	if applied != 1 {
		t.Errorf("applied = %d, want 1", applied)
	}
	if !strings.Contains(buf.String(), "[manual]") {
		t.Errorf("privileged remedy must be [manual] print-only:\n%s", buf.String())
	}

	// 2) interactive + confirm YES: applies.
	noPrivCalls = 0
	buf.Reset()
	if applied = applyRemediationsCore(results, true, func(string) bool { return true }, &buf); applied != 1 || noPrivCalls != 1 {
		t.Errorf("interactive-yes: applied=%d calls=%d, want 1/1", applied, noPrivCalls)
	}

	// 3) interactive + DECLINE: the action is skipped, nothing runs.
	noPrivCalls = 0
	buf.Reset()
	applied = applyRemediationsCore(results, true, func(string) bool { return false }, &buf)
	if noPrivCalls != 0 {
		t.Errorf("a declined fix must NOT run: apply calls = %d", noPrivCalls)
	}
	if applied != 0 {
		t.Errorf("declined: applied = %d, want 0", applied)
	}
	if !strings.Contains(buf.String(), "[skip]") {
		t.Errorf("a declined fix must print [skip]:\n%s", buf.String())
	}
}

// TestClearStaleGhCache checks the "refresh a stale cache" no-priv remedy is
// scoped so it can NEVER discard a usable token: it refuses a VALID cache,
// removes a stale/expired one, and no-ops on an absent one.
func TestClearStaleGhCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	stateDir, err := config.StateDir()
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	// A per-scope gh-read cache file (issue #95); clearStaleGhCache globs all
	// gh-read-token*.json, so any scoped filename exercises the same path.
	path := ghsession.ReadCachePathForScope(stateDir, "owner/alpha")

	// absent -> no-op, no error.
	if err := clearStaleGhCache(); err != nil {
		t.Errorf("absent cache: clearStaleGhCache err = %v, want nil", err)
	}

	// VALID -> must be preserved.
	if err := tokencache.Write(path, tokencache.Entry{Token: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("write valid: %v", err)
	}
	if err := clearStaleGhCache(); err != nil {
		t.Errorf("valid cache: err = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a VALID cache must NOT be deleted, but it's gone: %v", err)
	}

	// STALE (expired) -> removed.
	if err := tokencache.Write(path, tokencache.Entry{Token: "tok", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := clearStaleGhCache(); err != nil {
		t.Errorf("stale cache: err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a STALE cache must be removed, stat err = %v (want not-exist)", err)
	}
}

// TestNoPrivFixesNeverExecInGuideOnly is a belt-and-suspenders sweep: across a
// realistic result set, applyRemediations must never be able to run a
// privileged step. We assert structurally that no privileged/guide remedy has
// an apply func (the driver only ever calls apply on remedyNoPriv).
func TestPrivilegedRemediesHaveNoApply(t *testing.T) {
	privileged := []checkResult{
		{"sandbox: srt present", statusFail, "x"},
		{"sandbox: srt version", statusFail, "x"},
		{"sandbox: seccomp", statusFail, "x"},
		{"sandbox: bwrap userns", statusFail, "x"},
		{"sandbox: system CA bundle", statusFail, "x"},
	}
	for _, r := range privileged {
		rem, ok := remediationFor(r)
		if !ok || rem.tier != remedyPrivileged {
			t.Fatalf("%s: want privileged remedy, got ok=%v tier=%v", r.name, ok, rem.tier)
		}
		if rem.apply != nil {
			t.Errorf("%s: privileged remedy MUST NOT have an apply func", r.name)
		}
	}
}
