package main

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
)

// TestPreflightGateFailsClosed asserts the launch gate: any hard (StatusFail)
// check makes printPreflightAndOK return false, which runSandboxed turns into a
// refusal to launch (no silent drop to unsandboxed mode).
func TestPreflightGateFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		checks []srt.Check
		wantOK bool
	}{
		{
			name: "all ok",
			checks: []srt.Check{
				{Name: "srt present", Status: srt.StatusOK, Message: "/usr/bin/srt"},
				{Name: "seccomp", Status: srt.StatusOK, Message: "present"},
			},
			wantOK: true,
		},
		{
			name: "warn does not block",
			checks: []srt.Check{
				{Name: "clock skew", Status: srt.StatusWarn, Message: "no reference"},
			},
			wantOK: true,
		},
		{
			name: "seccomp fail blocks",
			checks: []srt.Check{
				{Name: "srt present", Status: srt.StatusOK, Message: "/usr/bin/srt"},
				{Name: "seccomp", Status: srt.StatusFail, Message: "missing"},
			},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := printPreflightAndOK(&buf, tc.checks)
			if got != tc.wantOK {
				t.Errorf("printPreflightAndOK = %v, want %v", got, tc.wantOK)
			}
			// Every check must be surfaced (loud-degrade).
			for _, c := range tc.checks {
				if !strings.Contains(buf.String(), c.Name) {
					t.Errorf("preflight output did not mention %q", c.Name)
				}
			}
		})
	}
}

// TestSandboxApproveNeverNilAndDeniesWithoutIssue asserts the sandbox write hook
// is (a) never nil (runbroker fails closed on nil) and (b) DENIES when the
// session binds no issue — reads flow, writes blocked, never silently allowed.
func TestSandboxApproveNeverNilAndDeniesWithoutIssue(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	noIssue := session.Session{ID: "s", Role: "implement", Repos: []string{"o/r"}, Issue: 0}
	approve := buildSandboxApprove(noIssue, t.TempDir(), "run-1", logger)
	if approve == nil {
		t.Fatal("buildSandboxApprove returned nil — runbroker would fail closed, but a real Approve must be wired")
	}
	if approve("o/r") {
		t.Error("no-issue session approved a write; sandboxed mode must DENY (reads flow, writes blocked)")
	}
}

// TestCredentialDenyReadFailsClosedWithoutHome asserts the sandbox refuses to
// assemble the deny-read set when $HOME is unresolvable (empty $HOME while XDG_*
// still resolves) — otherwise it would launch with ~/.ssh etc. exposed while
// every other check stayed green. Fail closed, don't return a partial list.
func TestCredentialDenyReadFailsClosedWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	// XDG dirs still resolve, mirroring the reachable fail-open scenario.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := credentialDenyReadPaths(t.TempDir()); err == nil {
		t.Error("credentialDenyReadPaths returned nil error with $HOME unset; it must fail closed rather than drop the home credential stores")
	}

	// Sanity: with a home it returns the stores including ~/.ssh.
	t.Setenv("HOME", "/home/someone")
	paths, err := credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error with HOME set: %v", err)
	}
	var sawSSH bool
	for _, p := range paths {
		if p == "/home/someone/.ssh" {
			sawSSH = true
		}
	}
	if !sawSSH {
		t.Errorf("~/.ssh missing from deny-read set: %v", paths)
	}
}

// TestCredentialDenyReadCoversRelocatedStores is the F1 regression: a developer
// who relocated their credential stores via XDG_CONFIG_HOME / GH_CONFIG_DIR /
// GNUPGHOME must have the RELOCATED paths hidden in-sandbox, not just the plain
// ~/.config defaults. Otherwise `ls $HOME` inside the sandbox finds the
// relocated gh config and reads the OAuth token.
func TestCredentialDenyReadCoversRelocatedStores(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	xdg := "/home/someone/dotfiles/xdg"
	ghDir := "/home/someone/dotfiles/ghconfig"
	gpgDir := "/home/someone/dotfiles/gnupg"
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("GH_CONFIG_DIR", ghDir)
	t.Setenv("GNUPGHOME", gpgDir)

	paths, err := credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	// The relocated stores must all be present.
	for _, want := range []string{
		ghDir,                                  // GH_CONFIG_DIR
		gpgDir,                                 // GNUPGHOME
		filepath.Join(xdg, "git"),              // XDG_CONFIG_HOME/git
		filepath.Join(xdg, "rein-credentials"), // XDG_CONFIG_HOME/rein-credentials
	} {
		if !set[want] {
			t.Errorf("relocated store %q missing from deny-read set: %v", want, paths)
		}
	}
	// And the ~/.config defaults are still present (belt-and-suspenders).
	for _, want := range []string{"/home/someone/.config/gh", "/home/someone/.gnupg", "/home/someone/.ssh"} {
		if !set[want] {
			t.Errorf("default store %q missing from deny-read set", want)
		}
	}
}
