package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/session"
)

// TestScaffoldSessionFile_RepoOnly verifies the scaffolded dev-session.yaml
// is repo-scoped: it contains the repo, carries NO active `issue:` line
// (decision A / #35 — the issue is agent-declared at runtime, not
// pre-picked at init), and is valid YAML that internal/session can load.
func TestScaffoldSessionFile_RepoOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-session.yaml")
	const repo = "octo-org/octo-repo"

	if err := scaffoldSessionFile(path, repo); err != nil {
		t.Fatalf("scaffoldSessionFile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scaffolded file: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, repo) {
		t.Errorf("scaffolded file missing repo %q:\n%s", repo, body)
	}

	// No `issue:` field AT ALL (issue #35 retired it — agent-declared,
	// never pre-configured; even a commented opt-in hint would teach the
	// wrong model now).
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "issue:") {
			t.Errorf("scaffolded file has an ACTIVE issue line (the field is retired):\n  %q", ln)
		}
	}
	// The scaffold should instead teach the declare flow.
	if !strings.Contains(body, "rein declare") {
		t.Errorf("scaffolded file should teach the `rein declare <n>` flow:\n%s", body)
	}

	// Must be loadable by the real session loader with the expected scope.
	s, err := session.LoadFromFile(path)
	if err != nil {
		t.Fatalf("session.LoadFromFile on scaffolded file: %v", err)
	}
	if len(s.Repos) != 1 || s.Repos[0] != repo {
		t.Errorf("loaded session repos = %v, want [%s]", s.Repos, repo)
	}
	if s.Issue != 0 {
		t.Errorf("loaded session Issue = %d, want 0 (no issue bound)", s.Issue)
	}
	if s.ID == "" {
		t.Errorf("loaded session has empty ID")
	}
}

// TestScaffoldSessionFile_RejectsBadSlug verifies the owner/name slug
// validation still fails closed on a malformed repo (e.g. one that could
// break out of the YAML scalar).
func TestScaffoldSessionFile_RejectsBadSlug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-session.yaml")

	for _, bad := range []string{
		"",
		"noslash",
		"owner/name\ninjected: true",
		"owner/na me",
		"owner/name:tag",
	} {
		if err := scaffoldSessionFile(path, bad); err == nil {
			t.Errorf("scaffoldSessionFile(%q) = nil error, want rejection", bad)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("scaffoldSessionFile(%q) wrote a file despite bad slug", bad)
			_ = os.Remove(path)
		}
	}
}

// TestResolveRepoForSession_FlagWins verifies --repo is used when set, and
// that a whitespace-only flag is treated as empty (falling through to the
// non-interactive fallback, which returns "" on a non-tty stdin). There is
// no env fallback — REIN_TEST_REPO_A is deliberately not consulted (#40).
func TestResolveRepoForSession_FlagWins(t *testing.T) {
	// A non-terminal stdin so the prompt branch (if ever reached) can't
	// block. An os.Pipe read end is a real *os.File that is NOT a tty.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })

	// --repo set: used verbatim.
	if got := resolveRepoForSession("flag-owner/flag-repo", r, false); got != "flag-owner/flag-repo" {
		t.Errorf("flag set: got %q, want flag-owner/flag-repo", got)
	}
	// whitespace-only flag is treated as empty; non-tty stdin -> "".
	if got := resolveRepoForSession("   ", r, false); got != "" {
		t.Errorf("blank flag on non-tty: got %q, want \"\"", got)
	}
}

// TestResolveRepoForSession_NonInteractiveFallback verifies the mandatory
// non-interactive fallback (onboarding-ux-design.md §7): with no flag, a
// non-terminal stdin (or --yes) must NOT prompt and must return "" without
// blocking — init then leaves the session unscaffolded.
func TestResolveRepoForSession_NonInteractiveFallback(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })

	// No flag, non-tty stdin: returns "" (no prompt, no block).
	if got := resolveRepoForSession("", r, false); got != "" {
		t.Errorf("no-tty with no flag: got %q, want \"\"", got)
	}
	// --yes forces non-interactive even if stdin were a tty; here stdin is
	// non-tty anyway, but assert the assumeYes gate explicitly.
	if got := resolveRepoForSession("", r, true); got != "" {
		t.Errorf("--yes with no flag: got %q, want \"\"", got)
	}
}

// TestPromptWithDefault_NonInteractive drives the input helper with a
// non-terminal reader. It must return the default without blocking on
// EOF, and honor a typed line + the Enter-accepts-default path.
//
// NOTE: this exercises the reader plumbing, not a genuine tty. The faithful
// check of interactive behavior (a human typing at /dev/tty) is a
// manual/pexpect test; a unit test cannot honestly stand in for a real
// terminal.
func TestPromptWithDefault_NonInteractive(t *testing.T) {
	// EOF immediately (empty reader) -> default, no block.
	if got := promptWithDefault(io.Discard, strings.NewReader(""), "Repo?", "def-owner/def-repo"); got != "def-owner/def-repo" {
		t.Errorf("empty reader: got %q, want default", got)
	}
	// Bare newline (Enter) -> default.
	if got := promptWithDefault(io.Discard, strings.NewReader("\n"), "Repo?", "def-owner/def-repo"); got != "def-owner/def-repo" {
		t.Errorf("bare newline: got %q, want default", got)
	}
	// A typed value -> that value (trimmed).
	if got := promptWithDefault(io.Discard, strings.NewReader("  typed-owner/typed-repo  \n"), "Repo?", "def"); got != "typed-owner/typed-repo" {
		t.Errorf("typed value: got %q, want typed-owner/typed-repo", got)
	}
	// A typed value with no trailing newline (still returned).
	if got := promptWithDefault(io.Discard, strings.NewReader("x-owner/x-repo"), "Repo?", "def"); got != "x-owner/x-repo" {
		t.Errorf("no-newline value: got %q, want x-owner/x-repo", got)
	}
}

// TestSandboxHealthOutcome verifies the PURE sandbox-health decision over
// synthetic checkResults (no real srt shell-out): all-ok is healthy with no
// message; a statusFail is unhealthy and renders a warning block either way,
// with requireSandbox governing only the caller's exit (asserted separately
// in the message content — the function itself returns the same failMsg for
// soft and hard). A statusWarn is tolerated (only statusFail counts).
func TestSandboxHealthOutcome(t *testing.T) {
	allOK := []checkResult{
		{"sandbox: srt present", statusOK, "found"},
		{"sandbox: srt version", statusOK, "0.0.63"},
		{"sandbox: seccomp", statusOK, "available"},
		{"sandbox: bwrap userns", statusOK, "ok"},
	}
	if healthy, msg := sandboxHealthOutcome(allOK, false); !healthy || msg != "" {
		t.Errorf("all-ok soft: got healthy=%v msg=%q, want healthy=true msg=\"\"", healthy, msg)
	}
	if healthy, msg := sandboxHealthOutcome(allOK, true); !healthy || msg != "" {
		t.Errorf("all-ok hard: got healthy=%v msg=%q, want healthy=true msg=\"\"", healthy, msg)
	}

	// A warn-only result is still healthy: only statusFail gates.
	warnOnly := []checkResult{
		{"sandbox: srt present", statusOK, "found"},
		{"sandbox: seccomp", statusWarn, "kernel seccomp probe inconclusive"},
	}
	if healthy, msg := sandboxHealthOutcome(warnOnly, false); !healthy || msg != "" {
		t.Errorf("warn-only: got healthy=%v msg=%q, want healthy=true msg=\"\"", healthy, msg)
	}

	// A failing check: unhealthy in both modes, and the warning block names
	// the failing check + its message + the fix pointer.
	withFail := []checkResult{
		{"sandbox: srt present", statusFail, "srt not found on PATH"},
		{"sandbox: bwrap userns", statusOK, "ok"},
	}
	softHealthy, softMsg := sandboxHealthOutcome(withFail, false)
	if softHealthy {
		t.Errorf("soft with fail: got healthy=true, want false")
	}
	if softMsg == "" {
		t.Fatalf("soft with fail: want a non-empty warning block")
	}
	for _, want := range []string{"sandbox: srt present", "srt not found on PATH", "rein doctor", "README.md", "will NOT work"} {
		if !strings.Contains(softMsg, want) {
			t.Errorf("soft warning block missing %q:\n%s", want, softMsg)
		}
	}

	hardHealthy, hardMsg := sandboxHealthOutcome(withFail, true)
	if hardHealthy {
		t.Errorf("hard with fail: got healthy=true, want false")
	}
	// The message is identical soft vs hard — requireSandbox only changes
	// the caller's exit code, not the surfaced warning.
	if hardMsg != softMsg {
		t.Errorf("hard warning block differs from soft:\n hard=%q\n soft=%q", hardMsg, softMsg)
	}
}

// TestAliasDecision walks the full flag/tty matrix for the OPT-IN alias
// decision (onboarding-ux-design.md decision 4). No real tty is needed —
// aliasDecision is pure; the isTTY gate is passed in. Genuine interactive
// prompting is a manual/pexpect concern.
func TestAliasDecision(t *testing.T) {
	cases := []struct {
		name                           string
		aliasFlag, noAlias, yes, isTTY bool
		wantInstall, wantPrompt        bool
	}{
		// Default: neither flag, non-interactive -> off, no prompt.
		{"default no-flags non-tty", false, false, false, false, false, false},
		// Default on a real tty (no --yes) -> prompt (install resolved later).
		{"tty no-flags", false, false, false, true, false, true},
		// --alias forces install, no prompt.
		{"alias flag", true, false, false, false, true, false},
		{"alias flag on tty", true, false, false, true, true, false},
		// --no-alias forces skip.
		{"no-alias flag", false, true, false, false, false, false},
		{"no-alias flag on tty", false, true, false, true, false, false},
		// Both flags: --no-alias wins (explicit opt-out beats opt-in).
		{"alias + no-alias -> off", true, true, false, false, false, false},
		{"alias + no-alias on tty -> off", true, true, false, true, false, false},
		// --yes with --alias still installs (explicit opt-in honored).
		{"yes + alias", true, false, true, true, true, false},
		// --yes alone: non-interactive default off, never prompts.
		{"yes alone on tty", false, false, true, true, false, false},
		{"yes alone non-tty", false, false, true, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotInstall, gotPrompt := aliasDecision(c.aliasFlag, c.noAlias, c.yes, c.isTTY)
			if gotInstall != c.wantInstall || gotPrompt != c.wantPrompt {
				t.Errorf("aliasDecision(alias=%v,noAlias=%v,yes=%v,tty=%v) = (install=%v,prompt=%v), want (install=%v,prompt=%v)",
					c.aliasFlag, c.noAlias, c.yes, c.isTTY, gotInstall, gotPrompt, c.wantInstall, c.wantPrompt)
			}
			// Invariant: never install AND prompt at once.
			if gotInstall && gotPrompt {
				t.Errorf("aliasDecision returned install=true AND prompt=true — mutually exclusive")
			}
		})
	}
}

// TestPromptYesNo drives the yes/no helper with a non-terminal reader: it
// must return the default without blocking on EOF, and parse the standard
// affirmatives/negatives (case-insensitive), with empty/garbage -> default.
//
// NOTE: like promptWithDefault this exercises the reader plumbing, not a
// genuine tty. Real interactive behavior is a manual/pexpect concern.
func TestPromptYesNo(t *testing.T) {
	// Empty reader (EOF) -> default, no block. Assert both defaults.
	if got := promptYesNo(io.Discard, strings.NewReader(""), "Add alias?", false); got != false {
		t.Errorf("empty reader def=false: got %v, want false", got)
	}
	if got := promptYesNo(io.Discard, strings.NewReader(""), "Add alias?", true); got != true {
		t.Errorf("empty reader def=true: got %v, want true", got)
	}
	// Bare newline (Enter) -> default.
	if got := promptYesNo(io.Discard, strings.NewReader("\n"), "Add alias?", false); got != false {
		t.Errorf("bare newline def=false: got %v, want false", got)
	}
	// Affirmatives -> true regardless of default.
	for _, in := range []string{"y\n", "Y\n", "yes\n", "YES\n", "  yes  \n"} {
		if got := promptYesNo(io.Discard, strings.NewReader(in), "Add alias?", false); got != true {
			t.Errorf("input %q: got %v, want true", in, got)
		}
	}
	// Negatives -> false regardless of default.
	for _, in := range []string{"n\n", "N\n", "no\n", "NO\n", "  no  \n"} {
		if got := promptYesNo(io.Discard, strings.NewReader(in), "Add alias?", true); got != false {
			t.Errorf("input %q: got %v, want false", in, got)
		}
	}
	// Garbage -> default.
	if got := promptYesNo(io.Discard, strings.NewReader("maybe\n"), "Add alias?", true); got != true {
		t.Errorf("garbage def=true: got %v, want true", got)
	}
}

// TestStdinIsTerminal_NonTTY verifies the isatty gate returns false for a
// non-terminal file (a pipe) and for nil — the two ways init must decline
// to prompt.
func TestStdinIsTerminal_NonTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })

	if stdinIsTerminal(r) {
		t.Errorf("stdinIsTerminal(pipe) = true, want false")
	}
	if stdinIsTerminal(nil) {
		t.Errorf("stdinIsTerminal(nil) = true, want false")
	}
}

// TestResolveStateApp_NoEnv is the regression guard for the manifest-flow
// steady-state `rein init` bug: after the App is installed (install-id
// cached), a re-run with NO REIN_APP_* env vars must resolve App config
// from state.json + the managed keystore and NOT demand env vars. Before
// the fix, init's BridgeUseState path called the env-only loader and
// hard-failed on "missing env var REIN_APP_CLIENT_ID".
func TestResolveStateApp_NoEnv(t *testing.T) {
	for _, k := range []string{
		"REIN_APP_CLIENT_ID", "REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID", "REIN_TEST_REPO_A",
	} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	configDir := filepath.Join(dir, "rein")

	// Cached install-id: the steady state a re-run hits.
	if err := appsetup.WriteState(configDir, appsetup.State{
		Phase: appsetup.PhaseAuditDone,
		Primary: &appsetup.AppRecord{
			ClientID:       "Iv23li-state",
			InstallationID: 12345,
		},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	cfg, ks, awaitInstall, err := resolveStateApp()
	if err != nil {
		t.Fatalf("resolveStateApp must succeed with no REIN_APP_* env: %v", err)
	}
	if awaitInstall {
		t.Error("awaitInstall = true, want false (install-id is cached)")
	}
	if cfg.ClientID != "Iv23li-state" || cfg.InstallationID != 12345 {
		t.Errorf("cfg = %+v, want client_id=Iv23li-state installation_id=12345", cfg)
	}
	if ks == nil {
		t.Error("keystore must be non-nil on the state path")
	}
}

// TestResolveStateApp_UncachedAwaitsInstall verifies the App-created-but-
// not-yet-installed state: install-id is 0, so awaitInstall is true and the
// caller prints the install hint instead of trying to mint.
func TestResolveStateApp_UncachedAwaitsInstall(t *testing.T) {
	for _, k := range []string{
		"REIN_APP_CLIENT_ID", "REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID", "REIN_TEST_REPO_A",
	} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	configDir := filepath.Join(dir, "rein")

	if err := appsetup.WriteState(configDir, appsetup.State{
		Phase: appsetup.PhaseAuditDone,
		Primary: &appsetup.AppRecord{
			ClientID:       "Iv23li-state",
			InstallationID: 0, // uncached: App not yet installed on a repo
		},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	_, _, awaitInstall, err := resolveStateApp()
	if err != nil {
		t.Fatalf("resolveStateApp must NOT error on uncached install-id: %v", err)
	}
	if !awaitInstall {
		t.Error("awaitInstall = false, want true (install-id uncached)")
	}
}
