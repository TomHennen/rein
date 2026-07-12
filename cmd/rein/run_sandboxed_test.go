package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/session"
	"github.com/TomHennen/rein/internal/srt"
	"github.com/TomHennen/rein/internal/worktree"
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

// TestSandboxApprove_ConfirmedSetGate asserts the sandbox write hook is
// (a) never nil (runbroker fails closed on nil), (b) DENIES while the
// run's confirmed-issue set is empty (pre-declaration — issue #35 §2),
// (c) ALLOWS once a declare confirmed an issue, and (d) fails closed
// again after a mid-run session edit (signature mismatch).
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

// TestCredentialDenyReadHidesOnDiskKeyringStore is the #46 regression: the
// Secret Service keyring DATABASE ($XDG_DATA_HOME/keyrings, default
// ~/.local/share/keyrings) must be hidden, not just the live D-Bus socket —
// srt's read-only root bind would otherwise leave it readable, and git's
// libsecret credential helper keeps GitHub tokens in it. KWallet's store is
// the same class. Covers both the default and the XDG_DATA_HOME-relocated
// resolution (mirroring the gh/gpg relocation tests).
func TestCredentialDenyReadHidesOnDiskKeyringStore(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	t.Setenv("XDG_CONFIG_HOME", "/home/someone/.config")
	t.Setenv("XDG_DATA_HOME", "") // unset: default resolution

	paths, err := credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	for _, want := range []string{
		"/home/someone/.local/share/keyrings",
		"/home/someone/.local/share/kwalletd",
	} {
		if !set[want] {
			t.Errorf("on-disk keyring store %q missing from deny-read set: %v", want, paths)
		}
	}

	// Relocated XDG_DATA_HOME: the relocated store must be hidden AND the
	// default must stay hidden (belt-and-suspenders, like gh/gpg).
	t.Setenv("XDG_DATA_HOME", "/home/someone/dotfiles/xdgdata")
	paths2, err := credentialDenyReadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set2 := map[string]bool{}
	for _, p := range paths2 {
		set2[p] = true
	}
	for _, want := range []string{
		"/home/someone/dotfiles/xdgdata/keyrings", // relocated
		"/home/someone/.local/share/keyrings",     // default still hidden
		"/home/someone/dotfiles/xdgdata/kwalletd",
	} {
		if !set2[want] {
			t.Errorf("keyring store %q missing when XDG_DATA_HOME set: %v", want, paths2)
		}
	}
}

// TestCredentialDenyReadHidesReinOwnArtifacts covers the #46 follow-up
// comment: rein's OWN on-disk credential artifacts — the PEM keystore, CA,
// and state.json under ConfigDir; the read-token cache, write-token ledger,
// approvals, and audit log under StateDir — must be hidden even when the
// XDG dirs are relocated, and the plain ~/.config/rein and
// ~/.local/state/rein defaults must stay hidden too (a dev who set XDG_*
// after first use could have legacy token files in the default dirs).
func TestCredentialDenyReadHidesReinOwnArtifacts(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	t.Setenv("XDG_CONFIG_HOME", "/home/someone/dotfiles/xdg")
	stateDir := "/home/someone/dotfiles/state/rein" // env-resolved by the caller

	paths, err := credentialDenyReadPaths(stateDir)
	if err != nil {
		t.Fatalf("credentialDenyReadPaths: %v", err)
	}
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	for _, want := range []string{
		"/home/someone/dotfiles/xdg/rein", // ConfigDir (env-resolved): PEM keystore, CA, state.json
		stateDir,                          // StateDir (caller-resolved): token cache + ledgers + audit
		"/home/someone/.config/rein",      // ConfigDir default (belt-and-suspenders)
		"/home/someone/.local/state/rein", // StateDir default (belt-and-suspenders)
	} {
		if !set[want] {
			t.Errorf("rein-own artifact dir %q missing from deny-read set: %v", want, paths)
		}
	}
}

// TestSandboxBannerHintsAndEgress asserts the CP4.5 banner additions: the extra
// egress hosts are surfaced, and the one-line "run without rein" bypass hint is
// present (\claude for bash/zsh, command claude for fish).
func TestSandboxBannerHintsAndEgress(t *testing.T) {
	var buf bytes.Buffer
	sess := session.Session{ID: "sess_test", Role: "implement", Repos: []string{"owner/repo"}}
	printSandboxBanner(&buf, sess, "file:/x", "/run/s.sock", "/work", []string{"api.anthropic.com", "registry.npmjs.org"}, []string{"claude", "-p", "hi"}, false, []string{"/home/x/.claude", "/home/x/go"}, "", worktree.Result{}, "/tmp/clone", "", "")
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

// TestSandboxBannerHomeDenialUX asserts the #59 first-class denial-UX
// requirement: the default banner states $HOME is hidden, lists the
// allow-backs, and shows the EXACT remediation syntax (REIN_SANDBOX_ALLOW_READ
// and the REIN_SANDBOX_SHOW_HOME kill switch) so a broken-path discovery loop
// is self-serve. With the kill switch on, the banner instead warns loudly that
// $HOME is visible.
func TestSandboxBannerHomeDenialUX(t *testing.T) {
	sess := session.Session{ID: "sess_test", Role: "implement", Repos: []string{"owner/repo"}}

	var buf bytes.Buffer
	printSandboxBanner(&buf, sess, "file:/x", "/run/s.sock", "/work", nil, []string{"claude"}, false, []string{"/home/x/.claude", "/home/x/go"}, "", worktree.Result{}, "/tmp/clone", "", "")
	out := buf.String()
	for _, want := range []string{
		"$HOME is HIDDEN",
		"/home/x/.claude, /home/x/go",       // the allow-back list is surfaced
		"REIN_SANDBOX_ALLOW_READ=/abs/path", // exact remediation syntax
		"REIN_SANDBOX_SHOW_HOME=1",          // kill switch named
		"NOT recommended",                   // ...and warned about

		// #63: the banner must state the THREE distinct $HOME behaviors, not
		// collapse them. Each was verified against real srt 0.0.63 + bwrap:
		// hidden reads ENOENT; hidden writes land in a scratch tmpfs and
		// SUCCEED, then vanish at teardown; writes to an allowed-back path
		// (a read-only bind) fail with EROFS. Collapsing them — as the
		// original "reads see an empty dir; writes are discarded" line did —
		// mis-states two of the three and hides the fact that actually
		// matters: nothing outside the working tree survives.
		"READS of a hidden path FAIL LOUDLY",
		"SILENTLY SUCCEED",
		"DISCARDED at run end",
		"ERROR (read-only)",
		"NOTHING under $HOME PERSISTS",
		"/work", // ...and the working tree is named as the place that does
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default banner missing %q; got:\n%s", want, out)
		}
	}

	buf.Reset()
	printSandboxBanner(&buf, sess, "file:/x", "/run/s.sock", "/work", nil, []string{"claude"}, true, nil, "", worktree.Result{}, "/tmp/clone", "", "")
	out = buf.String()
	if !strings.Contains(out, "$HOME is VISIBLE") {
		t.Errorf("show-home banner must warn $HOME is visible; got:\n%s", out)
	}
	if strings.Contains(out, "$HOME is HIDDEN") {
		t.Errorf("show-home banner must not claim $HOME is hidden; got:\n%s", out)
	}

	buf.Reset()
	printShowHomeWarning(&buf)
	if !strings.Contains(buf.String(), "READABLE in-sandbox") {
		t.Errorf("kill-switch warning missing; got:\n%s", buf.String())
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

// TestSandboxBannerListsWritableCheckouts (#64) is the security-disclosure
// test. Binding the developer's REAL checkout writable is a genuinely new
// exposure: the tree may hold uncommitted human work, and a prompt-injected
// agent can modify it. rein's mitigation is not obscurity (the agent MUST be
// told the paths or it cannot use them) — it is that the human sees, at launch,
// exactly which real trees went in and can Ctrl-C if one is a surprise. If this
// test ever goes quiet, the widening became invisible.
func TestSandboxBannerListsWritableCheckouts(t *testing.T) {
	var buf bytes.Buffer
	sess := session.Session{ID: "sess_test", Role: "implement", Repos: []string{"owner/a", "owner/b"}}
	wt := worktree.Result{
		WorkTreeRepo: "owner/a",
		Bindings: []worktree.Binding{
			{Repo: "owner/b", Path: "/srv/dev/b", Mode: "rw", Source: "session"},
		},
	}
	printSandboxBanner(&buf, sess, "file:/x", "/run/s.sock", "/srv/dev/a",
		nil, []string{"claude"}, false, []string{"/home/x/.claude"}, "", wt, "/tmp/rein-agent-tmp-1", "", "")
	out := buf.String()

	for _, want := range []string{
		"AGENT-WRITABLE",                // the loud header
		"owner/b  ->  /srv/dev/b",       // the exact real tree, named
		"(rw, from session)",            // ...and where the mapping came from
		"modify ANY writable file",      // ...and what is at stake
		".git is pinned",                // the hardening is disclosed
		"NOT risk-free",                 // ...but honestly, not overclaimed
		"issue #76",                     // ...naming the tracked residual
		"/srv/dev/a",                    // the working tree
		"[owner/a]",                     // ...with its autodetected repo
		"REIN_EPHEMERAL_CLONE_DIR",      // the mid-run fallback, named
		"/tmp/rein-agent-tmp-1",         // ...with its actual path
		"never inside the working tree", // ...and the nesting warning (mocks §7)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q; got:\n%s", want, out)
		}
	}

	// With nothing mapped, the loud block must NOT appear (no false alarm), but
	// the ephemeral-clone guidance still must (it is how a mid-run repo works).
	buf.Reset()
	printSandboxBanner(&buf, sess, "file:/x", "/run/s.sock", "/srv/dev/a",
		nil, []string{"claude"}, false, nil, "", worktree.Result{}, "/tmp/rein-agent-tmp-1", "", "")
	out = buf.String()
	if strings.Contains(out, "AGENT-WRITABLE") {
		t.Errorf("no checkouts mapped, but the banner cried wolf:\n%s", out)
	}
	if !strings.Contains(out, "REIN_EPHEMERAL_CLONE_DIR") {
		t.Errorf("banner must always name the ephemeral clone dir; got:\n%s", out)
	}
}

// TestMappedCheckoutUnderCredentialDenyFailsClosed (#64 x #59): a `worktrees:`
// entry is a WIDENING — it must be held to the same authoritative-deny rule as
// every other widening path. Mapping a "checkout" that sits at or under a
// credential store (say ~/.ssh, or rein's own key dir) would make srt re-bind
// that path READ-WRITE over the deny tmpfs, handing the agent the very secrets
// the deny exists to hide. srt.Build is the enforcement point; this pins that
// the worktree path (which rein passes through ExtraAllowWrite) is covered by
// it and cannot become a bypass.
func TestMappedCheckoutUnderCredentialDenyFailsClosed(t *testing.T) {
	home := t.TempDir()
	credStore := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(filepath.Join(credStore, "checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "dev", "a")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := srt.Build(srt.Params{
		SocketPath:         "/run/user/1000/rein/x/proxy.sock",
		WorkingTree:        work,
		ExtraAllowWrite:    []string{filepath.Join(credStore, "checkout")}, // the mapped "worktree"
		DenyReadCredStores: []string{credStore},
		DenyReadHome:       home,
	})
	if err == nil {
		t.Fatal("a mapped checkout under a credential-store deny was accepted; srt would re-bind it READ-WRITE and re-expose the store")
	}
	if !strings.Contains(err.Error(), "authoritative deny-read") {
		t.Fatalf("unexpected error (want the fail-closed deny-overlap refusal): %v", err)
	}
}

// TestSandboxBannerEphemeralCwd (#64): when the cwd checkout is unhardenable and
// falls back to an ephemeral working tree, the banner must (a) say the REAL tree
// was NOT bound and why, (b) name the throwaway tree, (c) tell the human nothing
// local persists and the push is the only durable artifact, and (d) NOT claim
// "only the working tree persists" (which would be a lie — the tree is a
// throwaway). It must also name the opt-in to bind the real tree anyway.
func TestSandboxBannerEphemeralCwd(t *testing.T) {
	var buf bytes.Buffer
	sess := session.Session{ID: "sess_test", Role: "implement", Repos: []string{"owner/a"}}
	printSandboxBanner(&buf, sess, "file:/x", "/run/s.sock", "/tmp/rein-ephemeral-work-9",
		nil, []string{"claude"}, false, []string{"/home/x/.claude"}, "", worktree.Result{},
		"/tmp/rein-agent-tmp-1", "/home/x/super", "owner/a")
	out := buf.String()

	for _, want := range []string{
		"YOUR REAL CHECKOUT WAS NOT BOUND",          // the real tree was declined
		"/home/x/super",                             // ...named
		"EPHEMERAL throwaway tree",                  // the substitute, named
		"/tmp/rein-ephemeral-work-9",                // ...its path
		"clones owner/a there and PUSHES",           // clone-and-push instruction
		"NOTHING local PERSISTS",                    // the honest persistence story
		"ONLY durable artifact is the agent's PUSH", // ...restated
		"REIN_SANDBOX_ALLOW_UNHARDENED_GIT=1",       // the opt-in to bind the real tree
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ephemeral-cwd banner missing %q; got:\n%s", want, out)
		}
	}
	// It must NOT tell the human the working tree persists — that is the exact lie
	// the ephemeral special-casing exists to avoid.
	if strings.Contains(out, "Only the working tree does") {
		t.Errorf("ephemeral banner falsely claims the working tree persists; got:\n%s", out)
	}
}
