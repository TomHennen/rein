package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/appsetup"
)

// writeRaw writes content to path, creating the parent dir.
func writeRaw(t *testing.T, path, content string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// clearAppEnv unsets every REIN_APP_* / REIN_TEST_REPO_A var via t.Setenv
// so a test starts from a known-empty env regardless of the developer's
// sourced ./dev-env. t.Setenv restores them at test end.
func clearAppEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
		"REIN_TEST_REPO_A",
	} {
		t.Setenv(k, "")
	}
}

// useConfigDir points ConfigDir() at a fresh temp dir for the test.
func useConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// ConfigDir() returns <XDG_CONFIG_HOME>/rein.
	cd, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	return cd
}

func TestResolveApp_EnvWins(t *testing.T) {
	clearAppEnv(t)
	useConfigDir(t)
	t.Setenv("REIN_APP_CLIENT_ID", "Iv23li-env")
	t.Setenv("REIN_APP_PRIVATE_KEY_PATH", "/does/not/need/to/exist.pem")
	t.Setenv("REIN_APP_INSTALLATION_ID", "4242")
	t.Setenv("REIN_TEST_REPO_A", "owner/name")

	cfg, ks, source, err := ResolveApp()
	if err != nil {
		t.Fatalf("ResolveApp: %v", err)
	}
	if source != SourceEnv {
		t.Errorf("source = %v, want SourceEnv", source)
	}
	if cfg.ClientID != "Iv23li-env" {
		t.Errorf("ClientID = %q", cfg.ClientID)
	}
	if cfg.InstallationID != 4242 {
		t.Errorf("InstallationID = %d, want 4242", cfg.InstallationID)
	}
	if ks == nil {
		t.Error("keystore should be non-nil")
	}
}

func TestResolveApp_StateUncached_NilError(t *testing.T) {
	clearAppEnv(t)
	configDir := useConfigDir(t)
	if err := appsetup.WriteState(configDir, appsetup.State{
		Phase: appsetup.PhaseAuditDone,
		Primary: &appsetup.AppRecord{
			ClientID:       "Iv23li-state",
			InstallationID: 0, // uncached
			Slug:           "rein-test",
		},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	cfg, ks, source, err := ResolveApp()
	if err != nil {
		t.Fatalf("ResolveApp must NOT error on uncached install-id: %v", err)
	}
	if source != SourceState {
		t.Errorf("source = %v, want SourceState", source)
	}
	if cfg.ClientID != "Iv23li-state" {
		t.Errorf("ClientID = %q", cfg.ClientID)
	}
	if cfg.InstallationID != 0 {
		t.Errorf("InstallationID = %d, want 0 (uncached)", cfg.InstallationID)
	}
	if ks == nil {
		t.Error("keystore should be non-nil even when uncached")
	}
}

func TestResolveApp_StateCached(t *testing.T) {
	clearAppEnv(t)
	configDir := useConfigDir(t)
	if err := appsetup.WriteState(configDir, appsetup.State{
		Phase: appsetup.PhasePrimaryDone,
		Primary: &appsetup.AppRecord{
			ClientID:       "Iv23li-state",
			InstallationID: 12345,
		},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	cfg, _, source, err := ResolveApp()
	if err != nil {
		t.Fatalf("ResolveApp: %v", err)
	}
	if source != SourceState {
		t.Errorf("source = %v, want SourceState", source)
	}
	if cfg.InstallationID != 12345 {
		t.Errorf("InstallationID = %d, want 12345", cfg.InstallationID)
	}
}

func TestResolveApp_NoConfig_FailsClosed(t *testing.T) {
	clearAppEnv(t)
	useConfigDir(t) // empty dir, no state.json

	_, _, source, err := ResolveApp()
	if err == nil {
		t.Fatal("ResolveApp should fail closed when no env and no state")
	}
	if source != SourceNone {
		t.Errorf("source = %v, want SourceNone", source)
	}
}

func TestResolveApp_CorruptState_FailsClosed(t *testing.T) {
	clearAppEnv(t)
	configDir := useConfigDir(t)
	// Write garbage to state.json so ReadState returns a parse error.
	if err := writeRaw(t, appsetup.StatePath(configDir), "{not json"); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}

	_, _, _, err := ResolveApp()
	if err == nil {
		t.Fatal("ResolveApp should fail closed on corrupt state.json")
	}
	// A parse error must surface the real reason ("state.json unreadable"),
	// NOT the generic "run rein init" remediation — init would not fix a
	// corrupt file. Distinguishing this is the point of Fix 1.
	if !strings.Contains(err.Error(), "state.json unreadable") {
		t.Errorf("corrupt-state error should name the unreadable file, got: %v", err)
	}
	if strings.Contains(err.Error(), "rein init") {
		t.Errorf("corrupt-state error must not suggest `rein init`, got: %v", err)
	}
}

func TestResolveApp_NonManifestPhase_FailsClosed(t *testing.T) {
	clearAppEnv(t)
	configDir := useConfigDir(t)
	// A managed_externally marker is not a manifest phase.
	if err := appsetup.WriteState(configDir, appsetup.State{
		Phase:  appsetup.PhaseManagedExternally,
		Source: appsetup.SourceEnv,
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	_, _, _, err := ResolveApp()
	if err == nil {
		t.Fatal("ResolveApp should fail closed on a non-manifest phase")
	}
}
