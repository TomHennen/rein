package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	// No ACTIVE issue field. A commented hint (a line whose issue token is
	// preceded by `#`) is fine; an uncommented `issue:` at the start of a
	// (trimmed) line is not — that would bind the session to a bogus issue
	// and change write-approval behavior.
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "issue:") {
			t.Errorf("scaffolded file has an ACTIVE issue line (must be commented out):\n  %q", ln)
		}
	}
	// The commented hint should still be present so the manual opt-in path
	// is discoverable.
	if !strings.Contains(body, "# issue:") {
		t.Errorf("scaffolded file missing the commented `# issue:` hint:\n%s", body)
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
