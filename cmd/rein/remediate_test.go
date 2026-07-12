package main

import "testing"

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
