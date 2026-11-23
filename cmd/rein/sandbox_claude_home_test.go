package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestPrepareClaudeOverlaySeeds asserts the #94 overlay: created 0700, host
// ~/.claude/.credentials.json copied in 0600. ~/.claude.json is NOT copied (only its
// onboarding keys, see TestSeedClaudeOnboarding), and rein authors NO settings.json (it must not weaken claude's own
// permission posture — the host's is likewise never copied).
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
	// A host settings.json that must NOT be copied.
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

	// rein authors NO settings.json (it reads nothing from claude's settings and must
	// not weaken claude's permission posture); the host's is not copied either.
	if _, err := os.Stat(filepath.Join(overlay, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("rein must NOT author/copy settings.json into the overlay (err=%v)", err)
	}

	// No host ~/.claude.json here, so nothing is seeded.
	if _, err := os.Stat(filepath.Join(overlay, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf(".claude.json must not appear without a host one (err=%v)", err)
	}
}

// TestPrepareClaudeOverlayNoHostCreds: absent host creds is NOT an error (rein
// guards GitHub creds, not claude auth) — the overlay is still created and no
// credentials file is seeded (and rein authors no settings.json either).
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
	if _, err := os.Stat(filepath.Join(overlay, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("rein must NOT author a settings.json; err=%v", err)
	}
	if _, err := os.Stat(overlay); err != nil {
		t.Errorf("overlay dir must still be created: %v", err)
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

func TestSeedClaudeOnboarding(t *testing.T) {
	const hostJSON = `{"theme":"dark","hasCompletedOnboarding":true,"lastOnboardingVersion":"2.1.0","projects":{"/secret/repo":{}},"oauthAccount":{"email":"x"}}`
	read := func(t *testing.T, p string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	setup := func(t *testing.T, host, overlayJSON string) (home, cfg string) {
		t.Helper()
		home = t.TempDir()
		overlay := filepath.Join(home, "overlay")
		if err := os.MkdirAll(overlay, 0o700); err != nil {
			t.Fatal(err)
		}
		if host != "" {
			if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(host), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg = filepath.Join(overlay, ".claude.json")
		if overlayJSON != "" {
			if err := os.WriteFile(cfg, []byte(overlayJSON), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		seedClaudeOnboarding(discardLogger(), home, overlay)
		return home, cfg
	}

	t.Run("fresh overlay gets only the onboarding keys", func(t *testing.T) {
		_, cfg := setup(t, hostJSON, "")
		got := read(t, cfg)
		if len(got) != 3 || got["theme"] != "dark" || got["hasCompletedOnboarding"] != true || got["lastOnboardingVersion"] != "2.1.0" {
			t.Errorf("overlay = %v, want exactly the three onboarding keys", got)
		}
		if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
		}
	})
	t.Run("merges into an un-onboarded overlay, keeping its content", func(t *testing.T) {
		_, cfg := setup(t, hostJSON, `{"userID":"sandbox","projects":{"/tmp/x":{}}}`)
		got := read(t, cfg)
		if got["userID"] != "sandbox" || got["hasCompletedOnboarding"] != true {
			t.Errorf("overlay = %v", got)
		}
		if p, _ := got["projects"].(map[string]any); len(p) != 1 || p["/tmp/x"] == nil {
			t.Errorf("overlay projects = %v, want the overlay's own only", got["projects"])
		}
	})
	t.Run("an onboarded overlay is left alone", func(t *testing.T) {
		const own = `{"theme":"light","hasCompletedOnboarding":true}`
		_, cfg := setup(t, hostJSON, own)
		if data, _ := os.ReadFile(cfg); string(data) != own {
			t.Errorf("overlay rewritten: %s", data)
		}
	})
	t.Run("unparseable overlay is left alone", func(t *testing.T) {
		_, cfg := setup(t, hostJSON, `{not json`)
		if data, _ := os.ReadFile(cfg); string(data) != `{not json` {
			t.Errorf("overlay rewritten: %s", data)
		}
	})
	t.Run("no host file, nothing written", func(t *testing.T) {
		_, cfg := setup(t, "", "")
		if _, err := os.Stat(cfg); !os.IsNotExist(err) {
			t.Errorf("overlay .claude.json created without a host one (err=%v)", err)
		}
	})
	t.Run("symlinked overlay file is not followed", func(t *testing.T) {
		home := t.TempDir()
		overlay := filepath.Join(home, "overlay")
		if err := os.MkdirAll(overlay, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(hostJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "victim")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(overlay, ".claude.json")); err != nil {
			t.Fatal(err)
		}
		seedClaudeOnboarding(discardLogger(), home, overlay)
		if data, _ := os.ReadFile(target); string(data) != "{}" {
			t.Errorf("wrote through a symlink: %s", data)
		}
	})
}
