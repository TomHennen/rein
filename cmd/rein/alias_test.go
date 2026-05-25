package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpliceManagedBlock(t *testing.T) {
	snippet := aliasBeginMarker + "\nalias claude='rein run -- claude'\n" + aliasEndMarker

	t.Run("appends to empty file", func(t *testing.T) {
		got, err := spliceManagedBlock(nil, snippet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := snippet + "\n"
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("appends with leading newline when prior file lacks one", func(t *testing.T) {
		got, err := spliceManagedBlock([]byte("# no trailing newline"), snippet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "# no trailing newline\n" + snippet + "\n"
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("replaces existing block in place", func(t *testing.T) {
		before := "# pre-content\n" + aliasBeginMarker + "\nalias claude='OLD'\n" + aliasEndMarker + "\n# post-content\n"
		got, err := spliceManagedBlock([]byte(before), snippet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "# pre-content\n" + snippet + "\n# post-content\n"
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("preserves surrounding content unchanged", func(t *testing.T) {
		before := "export FOO=bar\n" + aliasBeginMarker + "\nalias claude='OLD'\n" + aliasEndMarker + "\nalias ll='ls -la'\n"
		got, err := spliceManagedBlock([]byte(before), snippet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(got), "export FOO=bar\n") {
			t.Errorf("pre-content lost: %q", got)
		}
		if !strings.Contains(string(got), "alias ll='ls -la'\n") {
			t.Errorf("post-content lost: %q", got)
		}
	})

	t.Run("errors when BEGIN present without END", func(t *testing.T) {
		before := aliasBeginMarker + "\nalias claude='OLD'\n# (no END marker)\n"
		_, err := spliceManagedBlock([]byte(before), snippet)
		if err == nil {
			t.Fatal("expected error for malformed file")
		}
	})

	t.Run("re-splice is idempotent", func(t *testing.T) {
		first, err := spliceManagedBlock(nil, snippet)
		if err != nil {
			t.Fatalf("first splice: %v", err)
		}
		second, err := spliceManagedBlock(first, snippet)
		if err != nil {
			t.Fatalf("second splice: %v", err)
		}
		if string(first) != string(second) {
			t.Errorf("re-splice changed content:\nfirst:  %q\nsecond: %q", first, second)
		}
	})

	t.Run("strips duplicate managed blocks", func(t *testing.T) {
		// A buggy prior run wrote two blocks; splice should leave
		// exactly one and self-heal.
		dup := aliasBeginMarker + "\nalias claude='OLD1'\n" + aliasEndMarker + "\n# middle\n" + aliasBeginMarker + "\nalias claude='OLD2'\n" + aliasEndMarker + "\n# end\n"
		got, err := spliceManagedBlock([]byte(dup), snippet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Exactly one BEGIN should remain.
		if n := strings.Count(string(got), aliasBeginMarker); n != 1 {
			t.Errorf("expected 1 BEGIN, got %d:\n%s", n, got)
		}
		if !strings.Contains(string(got), "# middle") || !strings.Contains(string(got), "# end") {
			t.Errorf("non-managed content lost: %q", got)
		}
		if strings.Contains(string(got), "OLD1") || strings.Contains(string(got), "OLD2") {
			t.Errorf("old content not stripped: %q", got)
		}
	})

	t.Run("errors on malformed file with surrounding content", func(t *testing.T) {
		before := "export FOO=bar\n" + aliasBeginMarker + "\nalias claude='OLD'\n# missing END\nalias ll=ls\n"
		_, err := spliceManagedBlock([]byte(before), snippet)
		if err == nil {
			t.Fatal("expected error for BEGIN-without-END")
		}
	})
}

func TestHasForeignClaudeAlias(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty file", "", false},
		{"unrelated content only", "export FOO=bar\nalias ll='ls -la'\n", false},
		{"managed block only", aliasBeginMarker + "\nalias claude='rein run -- claude'\n" + aliasEndMarker + "\n", false},
		{"bash alias outside block", "alias claude='/some/other/thing'\n", true},
		{"bash alias with space syntax", "alias claude /some/other/thing\n", true},
		{"zsh alias same as bash", "alias claude='custom'\n", true},
		{"fish function", "function claude\n    /usr/bin/claude $argv\nend\n", true},
		{"commented-out alias does not count", "# alias claude='example'\n", false},
		{"partial name match does not count", "function claudette\n  echo hi\nend\n", false},
		{"foreign alias outside but managed block also present", aliasBeginMarker + "\nalias claude='rein run -- claude'\n" + aliasEndMarker + "\nalias claude='OTHER'\n", true},
		{"managed block does not trigger after strip", "alias ll='ls'\n" + aliasBeginMarker + "\nalias claude='rein run -- claude'\n" + aliasEndMarker + "\nexport BAR=x\n", false},
		{"leading whitespace alias still detected", "    alias claude='x'\n", true},
		{"zsh global alias -g", "alias -g claude='custom'\n", true},
		{"zsh global alias -gs claude", "alias -gs claude='custom'\n", true},
		{"POSIX function claude()", "claude() { /usr/bin/claude $@; }\n", true},
		{"POSIX function claude () with space", "claude () { echo hi; }\n", true},
		{"function-like name but different", "claudette() { echo hi; }\n", false},
		{"malformed file (BEGIN no END) treated as foreign", aliasBeginMarker + "\nalias claude='x'\n", true},
		{"duplicate managed blocks do not trigger", aliasBeginMarker + "\nalias claude='r'\n" + aliasEndMarker + "\n" + aliasBeginMarker + "\nalias claude='r'\n" + aliasEndMarker + "\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasForeignClaudeAlias([]byte(c.body))
			if got != c.want {
				t.Errorf("got %v, want %v\nbody:\n%s", got, c.want, c.body)
			}
		})
	}
}

func TestDetectShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if got := detectShell(""); got != "zsh" {
		t.Errorf("env-based: got %q, want zsh", got)
	}
	if got := detectShell("fish"); got != "fish" {
		t.Errorf("override: got %q, want fish", got)
	}
	t.Setenv("SHELL", "/some/weird/exotic-shell")
	if got := detectShell(""); got != "bash" {
		t.Errorf("unknown fallback: got %q, want bash", got)
	}
	t.Setenv("SHELL", "")
	if got := detectShell(""); got != "bash" {
		t.Errorf("empty SHELL: got %q, want bash", got)
	}
}

func TestBuildAliasPlan(t *testing.T) {
	t.Run("bash", func(t *testing.T) {
		p, err := buildAliasPlan("bash", "/h")
		if err != nil {
			t.Fatal(err)
		}
		if p.rcPath != "/h/.bashrc" {
			t.Errorf("rc: %q", p.rcPath)
		}
		if !strings.Contains(p.snippet, "alias claude='rein run -- claude'") {
			t.Errorf("snippet missing alias line: %q", p.snippet)
		}
		if !strings.Contains(p.bypassHint, "\\claude") {
			t.Errorf("bypass hint: %q", p.bypassHint)
		}
	})
	t.Run("zsh writes to .zshrc", func(t *testing.T) {
		p, err := buildAliasPlan("zsh", "/h")
		if err != nil {
			t.Fatal(err)
		}
		if p.rcPath != "/h/.zshrc" {
			t.Errorf("rc: %q", p.rcPath)
		}
	})
	t.Run("fish uses function with $argv and autoload location", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		p, err := buildAliasPlan("fish", "/h")
		if err != nil {
			t.Fatal(err)
		}
		if p.rcPath != "/h/.config/fish/functions/claude.fish" {
			t.Errorf("rc: %q (expected fish autoload location)", p.rcPath)
		}
		if !strings.Contains(p.snippet, "function claude") || !strings.Contains(p.snippet, "$argv") {
			t.Errorf("fish snippet wrong: %q", p.snippet)
		}
		if !strings.Contains(p.bypassHint, "command claude") {
			t.Errorf("fish bypass hint: %q", p.bypassHint)
		}
	})
	t.Run("fish honors XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
		p, err := buildAliasPlan("fish", "/h")
		if err != nil {
			t.Fatal(err)
		}
		if p.rcPath != "/custom/xdg/fish/functions/claude.fish" {
			t.Errorf("rc: %q", p.rcPath)
		}
	})
	t.Run("unknown shell errors", func(t *testing.T) {
		_, err := buildAliasPlan("nushell", "/h")
		if err == nil {
			t.Fatal("expected error for unknown shell")
		}
	})
}

func TestInstallShellAlias_lifecycle(t *testing.T) {
	home := t.TempDir()
	plan, err := buildAliasPlan("bash", home)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("create on absent file", func(t *testing.T) {
		out, err := installShellAlias(plan)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !out.active || !out.changed {
			t.Errorf("expected active+changed, got %+v", out)
		}
		body, err := os.ReadFile(plan.rcPath)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.Contains(string(body), aliasBeginMarker) {
			t.Errorf("managed block missing: %q", body)
		}
	})

	t.Run("re-run on current file is no-change", func(t *testing.T) {
		out, err := installShellAlias(plan)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !out.active {
			t.Errorf("expected active, got %+v", out)
		}
		if out.changed {
			t.Errorf("expected no-change on idempotent re-run, got %+v", out)
		}
	})

	t.Run("update after hand-edit of managed block", func(t *testing.T) {
		body, _ := os.ReadFile(plan.rcPath)
		mutated := strings.ReplaceAll(string(body), "rein run -- claude", "MUTATED")
		if err := os.WriteFile(plan.rcPath, []byte(mutated), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := installShellAlias(plan)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !out.changed {
			t.Errorf("expected changed after hand-edit, got %+v", out)
		}
		body, _ = os.ReadFile(plan.rcPath)
		if strings.Contains(string(body), "MUTATED") {
			t.Errorf("hand-edit not corrected: %q", body)
		}
	})

	t.Run("foreign alias outside managed block is refused", func(t *testing.T) {
		body, _ := os.ReadFile(plan.rcPath)
		withForeign := "alias claude=/other/thing\n" + string(body)
		if err := os.WriteFile(plan.rcPath, []byte(withForeign), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := installShellAlias(plan)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if out.active || out.changed {
			t.Errorf("expected !active and !changed on foreign collision, got %+v", out)
		}
		body2, _ := os.ReadFile(plan.rcPath)
		if !strings.Contains(string(body2), "alias claude=/other/thing") {
			t.Errorf("foreign alias should be preserved: %q", body2)
		}
	})
}

func TestInstallShellAlias_fishFunctionsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	plan, err := buildAliasPlan("fish", home)
	if err != nil {
		t.Fatal(err)
	}
	if plan.rcPath != filepath.Join(home, ".config", "fish", "functions", "claude.fish") {
		t.Fatalf("unexpected fish path: %s", plan.rcPath)
	}
	out, err := installShellAlias(plan)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !out.active || !out.changed {
		t.Errorf("expected active+changed, got %+v", out)
	}
	body, err := os.ReadFile(plan.rcPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "function claude") || !strings.Contains(string(body), "$argv") {
		t.Errorf("fish function body missing: %q", body)
	}
}

func TestInstallShellAlias_preservesMode(t *testing.T) {
	home := t.TempDir()
	plan, err := buildAliasPlan("bash", home)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-create with a non-default mode so we can verify it's kept.
	if err := os.WriteFile(plan.rcPath, []byte("# user content\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := installShellAlias(plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(plan.rcPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode not preserved: got %o, want 0640", info.Mode().Perm())
	}
}
