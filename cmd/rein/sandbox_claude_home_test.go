package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestPrepareClaudeOverlaySeeds asserts the #94 overlay: created 0700, host
// ~/.claude/.credentials.json copied in 0600, and rein's own minimal settings.json
// authored (NOT the host's). ~/.claude.json is deliberately NOT seeded.
func TestPrepareClaudeOverlaySeeds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	// Host creds present (0600, user-owned) — the seed source.
	hostClaude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(hostClaude, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostClaude, ".credentials.json"), []byte(`{"token":"host-oauth"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A host settings.json that must NOT be copied verbatim.
	if err := os.WriteFile(filepath.Join(hostClaude, "settings.json"), []byte(`{"theme":"host-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	overlay, err := prepareClaudeOverlay(discardLogger(), home)
	if err != nil {
		t.Fatalf("prepareClaudeOverlay: %v", err)
	}
	if want := filepath.Join(home, ".config", "rein-sandbox-home", ".claude"); overlay != want {
		t.Errorf("overlay = %q, want %q (sibling of ConfigDir, not under it)", overlay, want)
	}

	// Overlay dir is 0700.
	fi, err := os.Stat(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("overlay mode = %o, want 0700 (holds the OAuth token)", fi.Mode().Perm())
	}

	// .credentials.json seeded fresh from host, 0600.
	credBytes, err := os.ReadFile(filepath.Join(overlay, ".credentials.json"))
	if err != nil {
		t.Fatalf("read seeded creds: %v", err)
	}
	if string(credBytes) != `{"token":"host-oauth"}` {
		t.Errorf("seeded creds = %q, want the host token copied verbatim", credBytes)
	}
	if cfi, _ := os.Stat(filepath.Join(overlay, ".credentials.json")); cfi.Mode().Perm() != 0o600 {
		t.Errorf("seeded creds mode = %o, want 0600", cfi.Mode().Perm())
	}

	// settings.json is rein's own minimal one, NOT the host's.
	sBytes, err := os.ReadFile(filepath.Join(overlay, "settings.json"))
	if err != nil {
		t.Fatalf("read overlay settings: %v", err)
	}
	if string(sBytes) != sandboxClaudeSettings {
		t.Errorf("overlay settings = %q, want rein's minimal settings %q", sBytes, sandboxClaudeSettings)
	}

	// .claude.json is NOT seeded (regenerated in-sandbox; seeding would leak host history).
	if _, err := os.Stat(filepath.Join(overlay, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf(".claude.json must NOT be seeded into the overlay (err=%v)", err)
	}
}

// TestPrepareClaudeOverlayNoHostCreds: absent host creds is NOT an error (rein
// guards GitHub creds, not claude auth) — the overlay is still created + settings
// authored, and no credentials file is seeded.
func TestPrepareClaudeOverlayNoHostCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	// No ~/.claude at all.

	overlay, err := prepareClaudeOverlay(discardLogger(), home)
	if err != nil {
		t.Fatalf("prepareClaudeOverlay with no host creds must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(overlay, ".credentials.json")); !os.IsNotExist(err) {
		t.Errorf("no host creds => overlay must have no seeded creds; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(overlay, "settings.json")); err != nil {
		t.Errorf("settings.json must still be authored: %v", err)
	}
}

// TestPrepareClaudeOverlayRefreshesCreds: the seed overwrites a stale overlay
// credential on every launch (token freshness).
func TestPrepareClaudeOverlayRefreshesCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	hostClaude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(hostClaude, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostClaude, ".credentials.json"), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-seed the overlay with a STALE credential.
	overlayDir := filepath.Join(home, ".config", "rein-sandbox-home", ".claude")
	if err := os.MkdirAll(overlayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, ".credentials.json"), []byte("stale-old-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareClaudeOverlay(discardLogger(), home); err != nil {
		t.Fatalf("prepareClaudeOverlay: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(overlayDir, ".credentials.json"))
	if string(got) != "fresh" {
		t.Errorf("overlay creds = %q, want the host token refreshed in (not the stale one)", got)
	}
}

// TestPrepareClaudeOverlayFailsClosedOnSymlink: a symlinked overlay dir aborts the
// launch rather than seeding an OAuth token through the link.
func TestPrepareClaudeOverlayFailsClosedOnSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	// Make the overlay path a symlink to an attacker-controlled dir.
	elsewhere := t.TempDir()
	parent := filepath.Join(home, ".config", "rein-sandbox-home")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(parent, ".claude")); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareClaudeOverlay(discardLogger(), home); err == nil {
		t.Error("prepareClaudeOverlay accepted a symlinked overlay dir; it must fail closed")
	}
}

// TestPrepareClaudeOverlayFailsClosedOnSymlinkedParent: MkdirAll follows symlinks on
// PARENTS, so a symlinked ~/.config/rein-sandbox-home could redirect the seeded token
// into a non-owned dir while the freshly-created leaf still looks fine. The parent
// check must catch it before any credential is written.
func TestPrepareClaudeOverlayFailsClosedOnSymlinkedParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	// ~/.config/rein-sandbox-home is a symlink to an elsewhere dir we "control".
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(home, ".config", "rein-sandbox-home")); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareClaudeOverlay(discardLogger(), home); err == nil {
		t.Error("prepareClaudeOverlay accepted a symlinked PARENT dir; it must fail closed")
	}
	// And it must not have seeded a credential through the symlink.
	if _, err := os.Stat(filepath.Join(elsewhere, ".claude", ".credentials.json")); !os.IsNotExist(err) {
		t.Errorf("a credential was seeded through the symlinked parent (err=%v)", err)
	}
}

// TestPrepareClaudeOverlayRejectsLooseHostCreds: a group/world-readable host token is
// rejected (keystore bar) rather than propagated into the overlay.
func TestPrepareClaudeOverlayRejectsLooseHostCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	hostClaude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(hostClaude, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostClaude, ".credentials.json"), []byte("tok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareClaudeOverlay(discardLogger(), home); err == nil {
		t.Error("prepareClaudeOverlay accepted a group/world-readable host credential; it must fail closed")
	}
}
