package config

import (
	"path/filepath"
	"testing"
)

// TestSandboxClaudeHomeDir_OverrideWins pins the #176 override: when
// EnvSandboxClaudeHomeDir is set, the overlay path is that value verbatim — so
// the journey harness can point a test run at a throwaway overlay instead of the
// developer's real, persistent ~/.config/rein-sandbox-home (which a test once
// wiped). It must win over XDG_CONFIG_HOME.
func TestSandboxClaudeHomeDir_OverrideWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/base")
	t.Setenv(EnvSandboxClaudeHomeDir, "/tmp/throwaway/rein-sandbox-home/.claude")

	got, err := SandboxClaudeHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/throwaway/rein-sandbox-home/.claude" {
		t.Errorf("override must be returned verbatim, got %q", got)
	}

	// A blank/whitespace override is ignored — falls through to XDG.
	t.Setenv(EnvSandboxClaudeHomeDir, "   ")
	got, err = SandboxClaudeHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/xdg/base", "rein-sandbox-home", ".claude"); got != want {
		t.Errorf("blank override must fall through to XDG, got %q want %q", got, want)
	}
}

// TestSandboxClaudeHomeDir_DerivesFromXDG: without the override, the overlay is
// $XDG_CONFIG_HOME/rein-sandbox-home/.claude — a SIBLING of ConfigDir, both under
// the same XDG base. That shared base is exactly why the fix is an overlay-only
// override, not XDG isolation: moving XDG would also move the App config.
func TestSandboxClaudeHomeDir_DerivesFromXDG(t *testing.T) {
	t.Setenv(EnvSandboxClaudeHomeDir, "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/base")

	got, err := SandboxClaudeHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/xdg/base", "rein-sandbox-home", ".claude"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	cfg, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(cfg) != "/xdg/base" || filepath.Dir(filepath.Dir(got)) != "/xdg/base" {
		t.Errorf("ConfigDir (%q) and overlay (%q) must share the XDG base", cfg, got)
	}
}
