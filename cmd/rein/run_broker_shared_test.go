package main

import (
	"io"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/session"
)

// funcID returns a comparable identity for a mint method value. Go func
// values are not comparable with ==, but two method expressions naming the
// SAME method share a code pointer, so this pins "which tier the sandbox
// selected" without a live mint. The wire-level proof that each tier carries
// the right PERMISSIONS lives in internal/githubapp/mint_scope_test.go (which
// has the httptest seam); this test proves the sandbox WIRES those tiers.
func funcID(f interface{}) uintptr { return reflect.ValueOf(f).Pointer() }

// TestSandboxMintTiersAreGhCapable pins the fix for the in-sandbox gh bug: the
// sandbox injecting proxy carries EVERY github request (git AND gh/REST/
// GraphQL), so its read/write tiers must be the gh-capable ones
// (issues+pull_requests), not the contents-only git tiers. With the
// contents-only tiers, `git push` landed but `gh pr create`/`gh issue
// comment`/any issue-or-PR API write 403'd, and even issue/PR READS failed —
// while the injected contract promised approving covers ALL writes.
//
// It also pins the read/write TIER SPLIT: the read tier must be a READ mint
// (never MintGhSessionToken), so a token cached/exfiltrated on the read path
// grants read-only capability.
func TestSandboxMintTiersAreGhCapable(t *testing.T) {
	// The write tier is the implement-role write mint (contents+issues+
	// pull_requests write) — matches direct mode's gh write path.
	if funcID(sandboxWriteMint) != funcID((*githubapp.Client).MintGhSessionToken) {
		t.Error("sandbox WRITE tier is not MintGhSessionToken; gh issue/PR writes will 403 in-sandbox")
	}
	// The read tier is the all-read gh mint (contents+issues+pull_requests read).
	if funcID(sandboxReadMint) != funcID((*githubapp.Client).MintGhReadOnlyToken) {
		t.Error("sandbox READ tier is not MintGhReadOnlyToken; gh issue/PR reads will 403 in-sandbox")
	}
	// Regression guard: neither tier is the contents-only git mint — that is
	// the exact pre-fix state that broke gh in-sandbox.
	if funcID(sandboxReadMint) == funcID((*githubapp.Client).MintReadOnlyToken) {
		t.Error("sandbox READ tier reverted to the contents-only MintReadOnlyToken (no issues:read)")
	}
	if funcID(sandboxWriteMint) == funcID((*githubapp.Client).MintWriteToken) {
		t.Error("sandbox WRITE tier reverted to the contents-only MintWriteToken (no issues/pull_requests:write)")
	}
	// TIER SPLIT: the read tier must never be the write mint — a read-path
	// token must not confer write capability.
	if funcID(sandboxReadMint) == funcID((*githubapp.Client).MintGhSessionToken) {
		t.Error("sandbox READ tier is the WRITE mint; the read/write split is broken")
	}
}

// TestSandboxApprove_ConfirmedSetGate pins the run write gate: no writes until a
// declare confirms an issue, and a mid-run scope edit invalidates the record.
func TestSandboxApprove_ConfirmedSetGate(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	stateDir := t.TempDir()
	sess := session.Session{ID: "s", Role: "implement", Repos: []string{"o/r"}}
	approve := buildSandboxApprove(sess, stateDir, "run-1", logger)
	if approve == nil {
		t.Fatal("buildSandboxApprove returned nil — runbroker would fail closed, but a real Approve must be wired")
	}
	if approve("o/r") {
		t.Error("empty confirmed set must DENY (writes locked until `rein declare <n>`)")
	}
	// A declare confirms an issue → the gate opens for this run.
	ci := approvals.ConfirmedIssue{Number: 73, Repo: "o/r", Title: "t", State: "open", ConfirmedAt: time.Now()}
	if err := approvals.AppendConfirmedIssue(stateDir, "run-1", approvals.SignatureOf(sess), sess.ID, ci, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !approve("o/r") {
		t.Error("confirmed set non-empty must ALLOW writes for the run")
	}
	// Mid-run session edit → different signature → whole record invalid.
	edited := sess
	edited.Repos = []string{"o/r", "o/extra"}
	approveEdited := buildSandboxApprove(edited, stateDir, "run-1", logger)
	if approveEdited("o/r") {
		t.Error("mid-run scope edit must invalidate the record, issue set included")
	}
}

// TestParseRunModeRouting pins `rein run` mode routing after the P3 cutover:
// nono is the default and sole sandbox backend; --sandbox/--nono are no-op
// aliases for it; --direct/--no-sandbox opt out; and a bad command shape is a
// usage error even for direct mode (so the caller rejects it before the banner).
func TestParseRunModeRouting(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		wantMode runMode
		wantCmd  []string
		wantErr  bool
	}{
		{"default is sandboxed (nono)", []string{"--", "claude", "foo"}, modeNono, []string{"claude", "foo"}, false},
		{"explicit --sandbox aliases nono", []string{"--sandbox", "--", "claude"}, modeNono, []string{"claude"}, false},
		{"explicit --nono aliases nono", []string{"--nono", "--", "claude"}, modeNono, []string{"claude"}, false},
		{"--direct selects direct", []string{"--direct", "--", "claude"}, modeDirect, []string{"claude"}, false},
		{"--no-sandbox aliases direct", []string{"--no-sandbox", "--", "claude"}, modeDirect, []string{"claude"}, false},
		{"missing -- is a usage error", []string{"claude"}, modeNono, nil, true},
		{"--direct without command is a usage error", []string{"--direct"}, modeDirect, nil, true},
		{"--direct without -- is a usage error", []string{"--direct", "claude"}, modeDirect, nil, true},
		{"no command at all", []string{}, modeNono, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, cmd, err := parseRunMode(tc.argv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %v, want %v", mode, tc.wantMode)
			}
			if !tc.wantErr && strings.Join(cmd, " ") != strings.Join(tc.wantCmd, " ") {
				t.Errorf("cmdline = %v, want %v", cmd, tc.wantCmd)
			}
			if tc.wantErr && cmd != nil {
				t.Errorf("cmdline = %v on error, want nil", cmd)
			}
		})
	}
}

// TestAuditLogPathUnderStateDir pins the containment relationship the audit
// trail's integrity rests on (design §6): the per-run audit log must live UNDER
// stateDir, which nono's default-deny filesystem keeps unreadable in-sandbox. A
// future "move the audit log to ~/.rein-audit" refactor would otherwise leave
// the agent able to read (and groom against) its own audit trail.
func TestAuditLogPathUnderStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state", "rein")
	p := auditLogPath(stateDir, "run-abc123")
	if !filepath.IsAbs(p) {
		t.Fatalf("auditLogPath returned a relative path %q; stateDir was absolute", p)
	}
	if p != stateDir && !strings.HasPrefix(p, stateDir+string(filepath.Separator)) {
		t.Errorf("audit log path %q is not under stateDir %q; the sandboxed agent could read its own audit trail (stateDir is default-denied under nono)", p, stateDir)
	}
}
