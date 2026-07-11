package main

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/approvals"
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
	// CP4: the classic ~/.gitconfig is hidden too (it leaks the developer's
	// email + credential.helper config; GIT_CONFIG_GLOBAL redirects git away
	// from it, and this denyRead stops a raw `cat`).
	if !set["/home/someone/.gitconfig"] {
		t.Errorf("~/.gitconfig missing from deny-read set: %v", paths)
	}
}

// TestInSandboxSelfGrantStructurallyFails documents and verifies the CP4
// approval invariant: an in-sandbox process CANNOT grant its own write.
//
// The structural argument (no test can exercise a live sandbox here, so this
// asserts the properties the argument rests on):
//
//   - The only thing that flips the proxy's writesApproved flag is cfg.Approve
//     returning true, and that runs on rein's foreground /dev/tty in the PARENT
//     process — outside the sandbox, with no IPC channel into the in-memory flag.
//   - grant.ObtainApproval also honors an on-disk approval RECORD first, so the
//     load-bearing property is that the record lives under stateDir, which is
//     (a) in the sandbox deny-read set (invisible in-sandbox) AND (b) outside
//     the sandbox's ONLY writable bind-mount (the working tree). So an
//     in-sandbox process can neither read nor forge the record.
//   - There is NO control socket in the in-process model (see runbroker package
//     doc), so the daemon-era "#12 control socket reachable in-sandbox" vector
//     is closed structurally — there is nothing to reach.
func TestInSandboxSelfGrantStructurallyFails(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	t.Setenv("XDG_STATE_HOME", "/home/someone/.local/state")
	t.Setenv("XDG_CONFIG_HOME", "/home/someone/.config")

	stateDir := "/home/someone/.local/state/rein"
	workTree := "/home/someone/work/throwaway-repo" // the sandbox's writable mount

	// (a) The approval record + run context + write-token ledger all live under
	// stateDir — never under the writable working tree.
	for _, p := range []string{
		approvals.RunApprovalPath(stateDir, "run-1"),
		approvals.RunContextPath(stateDir, "run-1"),
		approvals.WriteTokenLedgerPath(stateDir, "run-1"),
	} {
		if !strings.HasPrefix(p, stateDir+string(filepath.Separator)) {
			t.Errorf("approval-related path %q is not under stateDir %q", p, stateDir)
		}
		if strings.HasPrefix(p, workTree+string(filepath.Separator)) {
			t.Errorf("approval-related path %q is under the writable working tree %q — an in-sandbox process could forge it", p, workTree)
		}
	}

	// (b) stateDir is in the sandbox deny-read set — invisible in-sandbox.
	deny, err := credentialDenyReadPaths(stateDir)
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	var sawState bool
	for _, p := range deny {
		if p == stateDir {
			sawState = true
		}
	}
	if !sawState {
		t.Errorf("stateDir %q not in deny-read set; the sandbox could read the approval record: %v", stateDir, deny)
	}
}

// TestCredentialDenyReadHidesClaudeWorkArtifacts is the CP4.5 regression: the
// wrapped agent's OAuth file (~/.claude/.credentials.json) stays readable so the
// agent can authenticate, but the developer's cross-project Claude work history
// (history.jsonl, projects/, sessions/) is hidden so a prompt-injected agent
// can't read it and exfiltrate via the extra egress the operator opened.
func TestCredentialDenyReadHidesClaudeWorkArtifacts(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	t.Setenv("XDG_CONFIG_HOME", "/home/someone/.config")
	t.Setenv("XDG_STATE_HOME", "/home/someone/.local/state")

	paths, err := credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	for _, want := range []string{
		"/home/someone/.claude/history.jsonl",
		"/home/someone/.claude/projects",
		"/home/someone/.claude/sessions",
		// session-env is hidden AND (as a denyRead dir => writable tmpfs) doubles
		// as the writable scratch claude's SessionStart machinery mkdir's per run;
		// without it the in-sandbox mkdir hits EROFS under the read-only root.
		"/home/someone/.claude/session-env",
	} {
		if !set[want] {
			t.Errorf("claude work artifact %q missing from deny-read set: %v", want, paths)
		}
	}
	// A relocated CLAUDE_CONFIG_DIR must ALSO be hidden, and the legacy ~/.claude
	// default stays hidden too (belt-and-suspenders, mirroring gh/gpg).
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/someone/dotfiles/claude")
	paths2, err := credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set2 := map[string]bool{}
	for _, p := range paths2 {
		set2[p] = true
	}
	for _, want := range []string{
		"/home/someone/dotfiles/claude/projects", // relocated
		"/home/someone/.claude/projects",         // legacy default still hidden
	} {
		if !set2[want] {
			t.Errorf("claude history path %q missing when CLAUDE_CONFIG_DIR set: %v", want, paths2)
		}
	}

	// The agent's OWN credential + settings must NOT be hidden — hiding them would
	// break the agent's ability to authenticate/run.
	for _, mustRead := range []string{
		"/home/someone/.claude/.credentials.json",
		"/home/someone/.claude/settings.json",
		"/home/someone/.claude", // the whole dir must not be tmpfs'd
	} {
		if set[mustRead] {
			t.Errorf("path %q must stay readable in-sandbox but is in the deny-read set", mustRead)
		}
	}
}

// TestSandboxBannerHintsAndEgress asserts the CP4.5 banner additions: the extra
// egress hosts are surfaced, and the one-line "run without rein" bypass hint is
// present (\claude for bash/zsh, command claude for fish).
func TestSandboxBannerHintsAndEgress(t *testing.T) {
	var buf bytes.Buffer
	sess := session.Session{ID: "sess_test", Role: "implement", Repos: []string{"owner/repo"}}
	printSandboxBanner(&buf, sess, "file:/x", "/run/s.sock", "/work", []string{"api.anthropic.com", "registry.npmjs.org"}, []string{"claude", "-p", "hi"})
	out := buf.String()

	if !strings.Contains(out, "api.anthropic.com") || !strings.Contains(out, "registry.npmjs.org") {
		t.Errorf("banner should list extra egress domains; got:\n%s", out)
	}
	if !strings.Contains(out, "NOT injected") {
		t.Errorf("banner should clarify extra egress is not injected; got:\n%s", out)
	}
	if !strings.Contains(out, `\claude`) || !strings.Contains(out, "command claude") {
		t.Errorf("banner missing the bypass hint (\\claude / command claude); got:\n%s", out)
	}
}

// TestParseRunModeRouting locks in the CP4 default-mode flip and the
// validate-before-banner ordering: bare `run --` is sandboxed, --direct/
// --no-sandbox select direct mode, --sandbox stays sandboxed, and a bad command
// shape returns a usage error (so dispatchRun never prints the direct banner).
func TestParseRunModeRouting(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		wantMode runMode
		wantCmd  []string
		wantErr  bool
	}{
		{"default is sandboxed", []string{"--", "claude", "foo"}, modeSandbox, []string{"claude", "foo"}, false},
		{"explicit --sandbox", []string{"--sandbox", "--", "claude"}, modeSandbox, []string{"claude"}, false},
		{"--direct selects direct", []string{"--direct", "--", "claude"}, modeDirect, []string{"claude"}, false},
		{"--no-sandbox aliases direct", []string{"--no-sandbox", "--", "claude"}, modeDirect, []string{"claude"}, false},
		{"missing -- is a usage error", []string{"claude"}, modeSandbox, nil, true},
		{"--direct without command is a usage error", []string{"--direct"}, modeDirect, nil, true},
		{"--direct without -- is a usage error", []string{"--direct", "claude"}, modeDirect, nil, true},
		{"no command at all", []string{}, modeSandbox, nil, true},
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
			// A usage error must be returned even for the direct mode, so the
			// caller rejects it BEFORE printing the banner.
			if tc.wantErr && cmd != nil {
				t.Errorf("cmdline = %v on error, want nil", cmd)
			}
		})
	}
}

// TestAuditLogPathIsUnderDenyReadSet pins the containment relationship the
// audit trail's integrity rests on (design §6, audit #44 §2): the per-run
// audit log must live UNDER a path that credentialDenyReadPaths puts in the
// sandbox deny-read set. Existing tests pin "stateDir is denied" and the
// code puts the log under stateDir, but nothing tied the two together — a
// future "move the audit log to ~/.rein-audit" refactor would leave the
// agent able to read (and groom against) its own audit trail with every
// test green.
func TestAuditLogPathIsUnderDenyReadSet(t *testing.T) {
	t.Setenv("HOME", "/home/someone")

	stateDir := filepath.Join(t.TempDir(), "state", "rein")
	denies, err := credentialDenyReadPaths(stateDir)
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}

	p := auditLogPath(stateDir, "run-abc123")
	if !filepath.IsAbs(p) {
		t.Fatalf("auditLogPath returned a relative path %q; stateDir was absolute", p)
	}
	under := false
	for _, d := range denies {
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			under = true
			break
		}
	}
	if !under {
		t.Errorf("audit log path %q is not under any deny-read path; the sandboxed agent could read its own audit trail.\ndeny-read set: %v", p, denies)
	}
}
